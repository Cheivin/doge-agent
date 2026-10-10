//go:build mesh

package libp2p

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"doge/internal/agent"
	"doge/internal/coding/tools"
	"doge/internal/mesh"
)

// RemoteTool builds the remote_tool agent tool: it invokes a node-level tool
// on a connected peer DIRECTLY (one MsgToolCall/MsgToolResult round trip)
// instead of dispatching a remote agent task — no LLM on the remote side, no
// tokens, deterministic. The typical use is a precise forward/listen binding
// on a remote node (tool=mesh_forward); a peer advertises this capability in
// its profile (tags["remoteTools"]=true when its mesh.json sets
// allowRemoteTools).
func RemoteTool(m mesh.Mesh, b *Bridge) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "remote_tool",
		Label:       "Call a tool on a remote node directly",
		Description: "Execute a node-level tool on another mesh node DIRECTLY and return its output — no remote agent run, no tokens. Tools: mesh_forward (list/add/remove/enable/disable/reload forwarding rules on THAT node — use it for precise forward/listen bindings, e.g. expose one of the peer's local services to the mesh, or bind a listen port on the peer), mesh_peers (list the mesh from that node's point of view), remote_agent_status (its in-flight remote tasks). Direct calls are allowed by default (same-token admission; a peer can opt out with allowRemoteTools:false or restrict with trustedPeers) — mesh_peers shows the remoteTools flag. Use remote_agent instead when you need an agent to think and act over many steps on the remote node.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"peer": map[string]any{"type": "string", "description": "target peer id, or a node NAME (alias) as shown by mesh_peers; duplicate names are rejected — then use the exact peer id"},
				"tool": map[string]any{"type": "string", "enum": []string{"mesh_forward", "mesh_peers", "remote_agent_status"}, "description": "node-level tool to run on the peer"},
				"args": map[string]any{"type": "object", "description": "tool arguments, same schema as the local tool (e.g. mesh_forward: action/mode/local/bind/port/peer/remote/name)"},
			},
			"required": []string{"peer", "tool"},
		},
		Execute: func(_ string, params map[string]any, ctx context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			peerRef := strParam(params["peer"])
			tool := strParam(params["tool"])
			if peerRef == "" || tool == "" {
				return tools.ToolError("remote_tool: peer and tool are required"), nil
			}
			if !directToolAllowlist[tool] {
				return tools.ToolError("remote_tool: tool " + tool + " is not callable directly (allowed: mesh_forward, mesh_peers, remote_agent_status)"), nil
			}
			peer, err := resolvePeerRef(m, peerRef)
			if err != nil {
				return tools.ToolError("remote_tool: " + err.Error()), nil
			}
			args, ok := params["args"].(map[string]any)
			if !ok {
				args = map[string]any{}
			}
			callID := newID()
			payload, _ := json.Marshal(mesh.ToolCallPayload{Tool: tool, Args: args})
			if err := m.Send(peer, mesh.Envelope{Type: mesh.MsgToolCall, ID: callID, To: peer, Payload: payload}); err != nil {
				return tools.ToolError(fmt.Sprintf("remote_tool: send to %s failed: %s (peer disconnected? it should reconnect automatically within seconds)", peer, err)), nil
			}
			res, err := b.WaitForResult(ctx, callID, directToolTimeout)
			if err == ErrTaskTimeout {
				return tools.ToolError(fmt.Sprintf("remote_tool: %s on %s timed out after %s", tool, peer, directToolTimeout)), nil
			}
			if err == ErrTaskAborted {
				return tools.ToolError("remote_tool: aborted"), nil
			}
			if res.Error != "" {
				return tools.ToolError(fmt.Sprintf("remote_tool %s on %s: %s", tool, peer, res.Error)), nil
			}
			return tools.ToolResult(strings.TrimSpace(res.Output)), nil
		},
	}
}
