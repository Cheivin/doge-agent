//go:build mesh

package libp2p

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"

	"doge/internal/agent"
	"doge/internal/mesh"
)

// TestPeerAuthedAndWait 互验门禁判定：peerAuthed 严格只认 authed（不做
// "刚连接即放行"的宽限——那会被重连刷窗口）；waitPeerAdmitted 对已互验
// peer 立即放行，对未互验（未连接的假 peer 经 Connectedness 即时拒绝）不放行
func TestPeerAuthedAndWait(t *testing.T) {
	m := newNode(t, testToken(t), "gate")
	pid := m.host.ID()

	if m.peerAuthed(pid) {
		t.Fatal("unknown peer authed")
	}
	if m.waitPeerAdmitted(pid, 100*time.Millisecond) {
		t.Fatal("unauthenticated peer admitted without waiting for handshake")
	}

	m.mu.Lock()
	m.peers[pid] = &peerState{connectedAt: time.Now()}
	m.mu.Unlock()
	if m.peerAuthed(pid) {
		t.Fatal("fresh unauthenticated peer counted as authed")
	}

	m.mu.Lock()
	m.peers[pid].authed = true
	m.mu.Unlock()
	if !m.peerAuthed(pid) {
		t.Fatal("authed peer not recognized")
	}
	if !m.waitPeerAdmitted(pid, time.Second) {
		t.Fatal("authed peer not admitted")
	}
}

// TestUnauthedStreamRejected 模拟静默攻击者（裸 host，注册握手 handler 但永不
// 回应，连接得以挂住）：其 agent / file 流必须等到互验超时后被 reset，且不
// 产生任何 envelope 分发——门禁是等待语义，攻击者等不到放行，也就无法借
// "刚连接"的宽限窗口调 MsgToolCall / 打 forward。
func TestUnauthedStreamRejected(t *testing.T) {
	victim := newNodeWithStore(t, testToken(t), "victim")
	// 缩短门禁等待，测试不等默认 15s
	victim.authWait = 300 * time.Millisecond

	attacker, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("attacker host: %v", err)
	}
	t.Cleanup(func() { _ = attacker.Close() })
	// 收到挑战只丢弃不回：互验永远完不成，连接也不被主动断开
	attacker.SetStreamHandler(protoHandshake, func(s network.Stream) {
		_, _ = io.Copy(io.Discard, s)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := attacker.Connect(ctx, peer.AddrInfo{ID: victim.host.ID(), Addrs: victim.host.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	victimID := victim.host.ID()
	waitFor(t, 5*time.Second, func() bool {
		victim.mu.Lock()
		defer victim.mu.Unlock()
		_, ok := victim.peers[attacker.ID()]
		return ok
	}, "victim records attacker connection")

	got := make(chan mesh.Envelope, 1)
	unsub := victim.Subscribe(func(env mesh.Envelope) error {
		select {
		case got <- env:
		default:
		}
		return nil
	})
	defer unsub()

	// agent 流：直接发 MsgToolCall（原攻击路径），期待 reset 而非回执。
	// NewStream 失败同样算拒绝（连接已被握手超时断开的情形）。
	rejected := func(proto protocol.ID, write func(network.Stream) error) bool {
		s, err := attacker.NewStream(ctx, victimID, proto)
		if err != nil {
			return true
		}
		defer s.Close()
		if err := write(s); err != nil {
			return true
		}
		var hdr [4]byte
		_, err = io.ReadFull(s, hdr[:])
		return err != nil
	}
	payload, _ := json.Marshal(mesh.ToolCallPayload{Tool: "mesh_forward"})
	if !rejected(protoAgent, func(s network.Stream) error {
		return writeFrame(s, mesh.Envelope{Type: mesh.MsgToolCall, To: victimID.String(), Payload: payload})
	}) {
		t.Fatal("unauthenticated agent stream got a reply frame, want reset")
	}
	if !rejected(protoFile, func(s network.Stream) error {
		return writeFrame(s, fileHeader{ID: "evil", Name: "evil.bin", Size: 1, Purpose: "send"})
	}) {
		t.Fatal("unauthenticated file stream got a reply frame, want reset")
	}

	select {
	case env := <-got:
		t.Fatalf("envelope dispatched for unauthenticated peer: %+v", env)
	default:
	}
}

// TestRemoteToolBusy 并发上限：额度占满时 MsgToolCall 立即回 busy，不排队
func TestRemoteToolBusy(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "busy-a")
	b := newNode(t, tok, "busy-b")
	connectAndWait(t, a, b)
	b.cfg.AllowRemoteTools = true

	// 名字在白名单内、执行阻塞的假工具
	block := make(chan struct{})
	h := newRemoteToolHandler(b, []*agent.AgentTool{blockedForwardTool(block)})
	b.Subscribe(h.Handle)
	for i := 0; i < directToolConcurrency; i++ {
		h.sem <- struct{}{} // 占满并发额度
	}

	resCh := make(chan mesh.Envelope, 4)
	unsub := a.Subscribe(func(env mesh.Envelope) error {
		if env.Type == mesh.MsgError {
			select {
			case resCh <- env:
			default:
			}
		}
		return nil
	})
	defer unsub()

	payload, _ := json.Marshal(mesh.ToolCallPayload{Tool: "mesh_forward"})
	if err := a.Send(b.host.ID().String(), mesh.Envelope{
		Type: mesh.MsgToolCall, To: b.host.ID().String(), Payload: payload,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case env := <-resCh:
		var ep mesh.ErrorPayload
		if err := json.Unmarshal(env.Payload, &ep); err != nil {
			t.Fatal(err)
		}
		if ep.Code != "busy" {
			t.Fatalf("error code = %q, want busy", ep.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no busy reply within 5s")
	}
	close(block) // 放行可能被占用的执行，避免悬挂 goroutine
}
