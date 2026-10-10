//go:build mesh

package libp2p

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/mesh"
)

// directToolTimeout bounds one direct tool call end to end; the tools that
// may be called this way are quick node-level config/inspect operations.
const directToolTimeout = time.Minute

// directToolConcurrency 同时执行的直接调用上限：MsgToolCall 每条起一个
// goroutine，无上限会被 flood 占满节点
const directToolConcurrency = 8

// directToolAllowlist is the only tool set a peer may invoke directly on this
// node. Session tools (read/bash/...) stay behind the remote-task permission
// levels on purpose: a direct call must never become a channel that bypasses
// remoteTaskLevel.
var directToolAllowlist = map[string]bool{
	"mesh_forward":        true,
	"mesh_peers":          true,
	"remote_agent_status": true,
}

// peerTrusted reports whether from may act on this node: the corresponding
// allow switch must be on, and when a trustedPeers whitelist is configured the
// sender must be on it. Shared by remote tasks and direct tool calls.
func peerTrusted(allow bool, trustedPeers []string, from string) bool {
	if !allow {
		return false
	}
	if len(trustedPeers) == 0 {
		return true
	}
	for _, p := range trustedPeers {
		if p == from {
			return true
		}
	}
	return false
}

// remoteToolHandler executes a peer's direct tool call (MsgToolCall) against
// this node's registered node-level tools — no remote agent session, no LLM
// run on either side. The master reads the tool set from the profile
// (tags["remoteTools"]) and sends precise tool invocations instead of
// dispatching a full remote_agent task.
type remoteToolHandler struct {
	m     *meshNode
	tools []*agent.AgentTool
	sem   chan struct{} // 并发执行上限
}

func newRemoteToolHandler(m *meshNode, tools []*agent.AgentTool) *remoteToolHandler {
	return &remoteToolHandler{m: m, tools: tools, sem: make(chan struct{}, directToolConcurrency)}
}

// Handle processes MsgToolCall envelopes; execution runs in its own goroutine
// (bounded by sem, overflow is rejected immediately with busy).
func (h *remoteToolHandler) Handle(env mesh.Envelope) error {
	if env.Type != mesh.MsgToolCall {
		return nil
	}
	select {
	case h.sem <- struct{}{}:
		go func() {
			defer func() { <-h.sem }()
			h.handle(env)
		}()
	default:
		h.replyError(env.From, env.ID, "busy", "too many concurrent direct tool calls")
	}
	return nil
}

func (h *remoteToolHandler) handle(env mesh.Envelope) {
	if !peerTrusted(h.m.cfg.AllowRemoteTools, h.m.cfg.TrustedPeers, env.From) {
		h.replyError(env.From, env.ID, "denied", "peer not authorized for direct tool calls")
		return
	}
	var cp mesh.ToolCallPayload
	if err := json.Unmarshal(env.Payload, &cp); err != nil {
		h.replyError(env.From, env.ID, "bad-request", "invalid tool call payload: "+err.Error())
		return
	}
	if !directToolAllowlist[cp.Tool] {
		h.replyError(env.From, env.ID, "denied", "tool "+cp.Tool+" is not allowed for direct remote execution")
		return
	}
	tool := h.findTool(cp.Tool)
	if tool == nil {
		h.replyError(env.From, env.ID, "not-found", "no tool "+cp.Tool+" registered on this node")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), directToolTimeout)
	defer cancel()
	res, err := tool.Execute(env.ID, cp.Args, ctx, nil)
	if err != nil {
		h.replyError(env.From, env.ID, "failed", err.Error())
		return
	}
	h.reply(env.From, env.ID, mesh.ToolResultPayload{Status: "completed", Output: renderToolText(res)})
}

func (h *remoteToolHandler) findTool(name string) *agent.AgentTool {
	for _, t := range h.tools {
		if t != nil && t.Name == name {
			return t
		}
	}
	return nil
}

// reply reports a tool execution outcome to the calling peer.
func (h *remoteToolHandler) reply(to, callID string, res mesh.ToolResultPayload) {
	data, _ := json.Marshal(res)
	_ = h.m.Send(to, mesh.Envelope{Type: mesh.MsgToolResult, To: to, ReplyTo: callID, Payload: data})
}

// replyError sends an immediate rejection tied to the call id.
func (h *remoteToolHandler) replyError(to, callID, code, msg string) {
	data, _ := json.Marshal(mesh.ErrorPayload{Code: code, Message: msg})
	_ = h.m.Send(to, mesh.Envelope{Type: mesh.MsgError, To: to, ReplyTo: callID, Payload: data})
}

// renderToolText concatenates the text blocks of a tool result for the wire.
func renderToolText(res *agent.AgentToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if c.Type == ai.ContentTypeText {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}
