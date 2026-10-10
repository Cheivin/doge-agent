//go:build mesh

package libp2p

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"doge/internal/agent"
	"doge/internal/guide"
	"doge/internal/mesh"
)

// newToolPair wires a direct tool handler on b (with mesh_forward when
// withTools) and a master bridge on a, then connects them.
func newToolPair(t *testing.T, allow, withTools bool) (*Bridge, *meshNode, *meshNode) {
	t.Helper()
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	connectAndWait(t, a, b)
	b.cfg.AllowRemoteTools = allow
	var tools []*agent.AgentTool
	if withTools {
		tools = []*agent.AgentTool{ForwardTool(b)}
	}
	b.Subscribe(newRemoteToolHandler(b, tools).Handle)
	bridge := NewBridge(a, nil, 0)
	a.Subscribe(bridge.Handle)
	return bridge, a, b
}

// execRemoteTool runs the remote_tool tool on the master against the given
// peer reference (peer id or node name).
func execRemoteTool(t *testing.T, bridge *Bridge, a *meshNode, peerRef, tool string, args map[string]any) string {
	t.Helper()
	rt := RemoteTool(a, bridge)
	params := map[string]any{"peer": peerRef, "tool": tool}
	if args != nil {
		params["args"] = args
	}
	res, err := rt.Execute("call", params, context.Background(), nil)
	if err != nil {
		t.Fatalf("remote_tool %s: %v", tool, err)
	}
	return renderToolText(res)
}

// TestRemoteToolDirectForward: the master adds a forward rule ON the slave via
// remote_tool — no remote agent session involved; the rule must exist on the
// slave afterwards.
func TestRemoteToolDirectForward(t *testing.T) {
	bridge, a, b := newToolPair(t, true, true)

	out := execRemoteTool(t, bridge, a, b.host.ID().String(), "mesh_forward", map[string]any{
		"action": "add", "name": "svc", "mode": "expose", "local": "127.0.0.1:8080",
	})
	if !strings.Contains(out, "svc") {
		t.Fatalf("output = %q, want the rule table", out)
	}
	found := false
	for _, r := range b.ForwardList() {
		if r.Name == "svc" && r.Mode == "expose" && r.Local == "127.0.0.1:8080" {
			found = true
		}
	}
	if !found {
		t.Fatalf("rule not applied on the slave: %v", b.ForwardList())
	}
}

// TestRemoteToolDenied: without allowRemoteTools the slave rejects the call
// immediately (MsgError, no timeout).
func TestRemoteToolDenied(t *testing.T) {
	bridge, a, b := newToolPair(t, false, true)

	out := execRemoteTool(t, bridge, a, b.host.ID().String(), "mesh_forward", map[string]any{
		"action": "list",
	})
	if !strings.Contains(out, "denied") {
		t.Fatalf("output = %q, want a denied rejection", out)
	}
	if len(b.ForwardList()) != 0 {
		t.Fatalf("slave state changed: %v", b.ForwardList())
	}
}

// TestRemoteToolUnknownTool: tools outside the node-level allowlist are
// rejected by the master before sending, and unregistered tools by the slave.
func TestRemoteToolUnknownTool(t *testing.T) {
	bridge, a, b := newToolPair(t, true, false)

	if out := execRemoteTool(t, bridge, a, b.host.ID().String(), "bash", map[string]any{}); !strings.Contains(out, "not callable directly") {
		t.Fatalf("output = %q, want a master-side allowlist rejection", out)
	}
	if out := execRemoteTool(t, bridge, a, b.host.ID().String(), "mesh_forward", map[string]any{"action": "list"}); !strings.Contains(out, "not-found") {
		t.Fatalf("output = %q, want a slave-side not-found rejection", out)
	}
}

// TestRemoteToolPeerNameAlias: a node NAME resolves to its peer id, and an
// unknown name is rejected immediately.
func TestRemoteToolPeerNameAlias(t *testing.T) {
	bridge, a, _ := newToolPair(t, true, true)

	out := execRemoteTool(t, bridge, a, "node-b", "mesh_forward", map[string]any{"action": "list"})
	if strings.Contains(out, "no connected peer") {
		t.Fatalf("unique name not resolved: %q", out)
	}

	out = execRemoteTool(t, bridge, a, "ghost", "mesh_forward", map[string]any{"action": "list"})
	if !strings.Contains(out, "no connected peer named") {
		t.Fatalf("output = %q, want an unknown-name rejection", out)
	}
}

// TestRemoteToolPeerNameAmbiguous: two connected nodes sharing a name make the
// name reference ambiguous — the call is rejected immediately with both peer
// ids listed, before anything is sent.
func TestRemoteToolPeerNameAmbiguous(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b1 := newNode(t, tok, "node-twin")
	b2 := newNode(t, tok, "node-twin")
	connectAndWait(t, a, b1)
	connectAndWait(t, a, b2)
	bridge := NewBridge(a, nil, 0)
	a.Subscribe(bridge.Handle)

	out := execRemoteTool(t, bridge, a, "node-twin", "mesh_forward", map[string]any{"action": "list"})
	if !strings.Contains(out, "ambiguous") {
		t.Fatalf("output = %q, want an ambiguous-name rejection", out)
	}
	if !strings.Contains(out, b1.host.ID().String()) || !strings.Contains(out, b2.host.ID().String()) {
		t.Fatalf("output = %q, want both candidate peer ids listed", out)
	}
}

// TestRemoteToolAllowlistBoundary: the name allowlist is the authoritative
// exposure gate for direct tool calls — even if a non-node-level tool (e.g.
// the built-in guide) is adversarially wired into the slave's MeshTools, the
// call must still be denied. Exposure is opt-in by registration AND by
// allowlist; local conveniences like guide never satisfy the second gate.
func TestRemoteToolAllowlistBoundary(t *testing.T) {
	// 白名单只含节点级 mesh 工具
	for _, name := range []string{"mesh_forward", "mesh_peers", "remote_agent_status"} {
		if !directToolAllowlist[name] {
			t.Fatalf("allowlist missing node tool %q", name)
		}
	}
	// 本地/会话级工具不在白名单：guide、以及全部会话工具
	for _, name := range []string{"guide", "skill", "read", "write", "edit", "bash", "grep", "code", "ls", "find"} {
		if directToolAllowlist[name] {
			t.Fatalf("allowlist must not expose session/local tool %q", name)
		}
	}

	// 就算把 guide 注册进从侧 MeshTools（错误接线），从侧白名单本身也要拒绝：
	// 绕过主侧 remote_tool 的预检，直接发 MsgToolCall 信封
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	connectAndWait(t, a, b)
	b.cfg.AllowRemoteTools = true
	b.Subscribe(newRemoteToolHandler(b, []*agent.AgentTool{guide.Tool()}).Handle)

	replies := make(chan mesh.Envelope, 4)
	unsub := a.Subscribe(func(env mesh.Envelope) error {
		if env.Type == mesh.MsgError {
			replies <- env
		}
		return nil
	})
	defer unsub()
	payload, _ := json.Marshal(mesh.ToolCallPayload{Tool: "guide", Args: map[string]any{}})
	if err := a.Send(b.host.ID().String(), mesh.Envelope{Type: mesh.MsgToolCall, ID: "call-guide", To: b.host.ID().String(), Payload: payload}); err != nil {
		t.Fatal(err)
	}
	select {
	case env := <-replies:
		var ep mesh.ErrorPayload
		if err := json.Unmarshal(env.Payload, &ep); err != nil {
			t.Fatal(err)
		}
		if ep.Code != "denied" || !strings.Contains(ep.Message, "not allowed for direct remote execution") {
			t.Fatalf("error = %s: %s, want a slave-side allowlist rejection", ep.Code, ep.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the slave-side rejection")
	}
}

// TestNodeReconnectWithoutPeerstoreAddrs: after a forced drop, the peer's
// peerstore addresses are wiped (what happens once a network outage outlives
// the 2–15 minute identify TTLs); the reconnect loop must still recover by
// re-dialing the recorded connection addresses.
func TestNodeReconnectWithoutPeerstoreAddrs(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	connectAndWait(t, a, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.reconnectInterval = 500 * time.Millisecond
	go a.startReconnectLoop(ctx)

	bID := b.host.ID()
	if err := a.host.Network().ClosePeer(bID); err != nil {
		t.Logf("ClosePeer: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return !hasPeer(a, bID.String())
	}, "a drops the peer")
	a.host.Peerstore().ClearAddrs(bID)

	waitFor(t, 15*time.Second, func() bool {
		return hasPeerProfile(a, bID.String()) && hasPeerProfile(b, a.host.ID().String())
	}, "peers reconnect via the recorded addresses")
}

// blockedForwardTool 名字在直接调用白名单内、执行阻塞在 done 上的假工具
func blockedForwardTool(done <-chan struct{}) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "mesh_forward",
		Description: "blocked fake for tests",
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			select {
			case <-done:
			case <-ctx.Done():
			}
			return &agent.AgentToolResult{}, nil
		},
	}
}
