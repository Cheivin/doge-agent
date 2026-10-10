//go:build mesh

package libp2p

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"doge/internal/agent"
	"doge/internal/coding"
	"doge/internal/coding/tools"
	"doge/internal/mesh"
)

// RemoteAgentTool builds the remote_agent agent tool (P4). It dispatches a
// task to a remote node and returns the result synchronously (blocking until
// the result arrives) or asynchronously (immediate ack; the result is later
// injected as a follow-up message). sess is the master's current session:
// the context snapshot is prepared from it out-of-band, never exposed to the
// model.
func RemoteAgentTool(m mesh.Mesh, b *Bridge, sess *coding.AgentSession) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "remote_agent",
		Label:       "Run a task on a remote mesh node",
		Description: "Run a task ON another doge node (the mesh slave) and get its result — inspecting that node's system, listing/reading files, running commands there. This is the ONLY way to act on a remote node; there is no shell or CLI for it. Use mesh_peers first to discover nodes and their peer ids / roles / accepting status. PREFER async=true: dispatch, END your turn, and wait for the completion notification (a follow-up message starts the next turn) — never poll with bash sleep + remote_agent_status. Use async=false only when you must inline-wait for the result (the tool blocks until it returns). By default it REUSES the most recent remote session on the target (continuity); pass session=\"new\" for independent tasks, or session=<id> to continue a specific one. context prepares the master's context for a NEW remote session (summary default / fresh / fork); ignored when continuing. A remote task may ask you questions (contact_supervisor); answer them when prompted.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"peer":    map[string]any{"type": "string", "description": "target peer id, or a node NAME (alias) as shown by mesh_peers; duplicate names are rejected — then use the exact peer id; empty selects one by roles"},
				"roles":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "capability roles the target should declare (used when peer is empty)"},
				"task":    map[string]any{"type": "string", "description": "the task to execute on the remote node"},
				"context": map[string]any{"type": "string", "enum": []string{"fresh", "summary", "fork"}, "description": "context to send: summary (default) | fresh | fork (ignored when continuing a session)"},
				"session": map[string]any{"type": "string", "description": "remote session strategy: empty (default) = reuse the most recent session with this peer; \"new\" = force a fresh session; an id = continue that specific session"},
				"async":   map[string]any{"type": "boolean", "description": "true = return immediately with a task id and notify later; false (default) = block for the result"},
				"model":   map[string]any{"type": "string", "description": "optional model hint for the remote side (provider/model)"},
				"cwd":     map[string]any{"type": "string", "description": "remote working directory (alias or absolute path; must be in the remote node's cwds whitelist; ignored when continuing a session)"},
				"files":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "staged upload ids (mesh_file) transferred to the remote node's staging area before the task runs; the remote task sees reference lines pointing at them"},
				"timeout": map[string]any{"type": "integer", "description": "seconds to wait for the result before interrupting (0 = default)"},
			},
			"required": []string{"task"},
		},
		Execute: func(_ string, params map[string]any, ctx context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			peer := strParam(params["peer"])
			roles := strSliceParam(params["roles"])
			task := strings.TrimSpace(strParam(params["task"]))
			contextMode := strParam(params["context"])
			session := strParam(params["session"])
			async := boolParam(params["async"])
			model := strParam(params["model"])
			cwd := strParam(params["cwd"])
			timeoutSec := intParam(params["timeout"])

			if task == "" {
				return tools.ToolError("remote_agent: task is required"), nil
			}
			if contextMode == "" {
				contextMode = "summary"
			}
			if contextMode != "fresh" && contextMode != "summary" && contextMode != "fork" {
				return tools.ToolError("remote_agent: context must be fresh, summary or fork"), nil
			}

			// 1. Select the target: direct peer reference (peer id or node
			// name — ambiguous names are rejected immediately), or by roles
			// among accepting peers. An explicit peer is sent as-is; the
			// slave's authorization (and its bad-cwd rejection) is the
			// authoritative gate.
			if peer == "" {
				peer = selectPeer(m, roles)
				if peer == "" {
					hint := ""
					if len(roles) > 0 {
						hint = fmt.Sprintf(" declaring roles %v", roles)
					}
					return tools.ToolError(fmt.Sprintf("remote_agent: no connected accepting peer%s found (run `doge mesh peers` to list nodes)", hint)), nil
				}
			} else if resolved, err := resolvePeerRef(m, peer); err != nil {
				return tools.ToolError("remote_agent: " + err.Error()), nil
			} else {
				peer = resolved
			}

			// 1b. Optional file hand-off: push each staged upload into the
			// target's staging area (/doge/file/0.1) before dispatching, so the
			// remote task can read the files locally.
			var fileRefs []mesh.RemoteFileRef
			for _, id := range strSliceParam(params["files"]) {
				node, ok := m.(*meshNode)
				if !ok {
					return tools.ToolError("remote_agent: file transfer requires the libp2p node"), nil
				}
				store := node.uploadStore()
				if store == nil {
					return tools.ToolError("remote_agent: this node has no upload store (mesh file transfer disabled)"), nil
				}
				meta, err := store.Meta(id)
				if err != nil {
					return tools.ToolError(fmt.Sprintf("remote_agent: unknown upload %q (stage it with mesh_file first)", id)), nil
				}
				ref, ack, err := node.SendFile(ctx, peer, meta)
				if err != nil {
					return tools.ToolError(fmt.Sprintf("remote_agent: file %s to %s failed: %v", meta.Name, peer, err)), nil
				}
				if !ack.OK {
					return tools.ToolError(fmt.Sprintf("remote_agent: file %s rejected by %s (%s): %s", meta.Name, peer, ack.Code, ack.Message)), nil
				}
				fileRefs = append(fileRefs, ref)
			}

			// 3. Prepare the context snapshot (wire field; the model never sees
			// it) and dispatch.
			taskID := newID()
			// Route this task's questions to the dispatching session, so a
			// slave asking for permission mid-run reaches the window the user
			// is viewing (not blocked by this sync wait).
			b.SetTaskSession(taskID, sess)
			// Session strategy: by default REUSE this master session's most
			// recent remote-task session with the peer (keyed per master
			// session, so two sessions dispatching to the same node each keep
			// their own conversation). Pass session="new" to force a
			// brand-new session (independent tasks), or session=<id> to
			// continue a specific one.
			masterSessID := sess.CurrentSession().Header.ID
			session = strParam(params["session"])
			if session == "" {
				session = b.LastSession(masterSessID, peer)
			} else if session == "new" {
				session = ""
			}
			tp := mesh.TaskPayload{
				Task: task, Context: contextMode, Model: model, Cwd: cwd, Roles: roles, Timeout: timeoutSec,
				Session: session,
				Files:   fileRefs,
			}
			if snap, err := buildSnapshot(sess, contextMode); err != nil {
				return tools.ToolError("remote_agent: " + err.Error()), nil
			} else {
				tp.Snapshot = snap
			}
			payload, _ := json.Marshal(tp)
			if err := m.Send(peer, mesh.Envelope{Type: mesh.MsgTask, ID: taskID, To: peer, Payload: payload}); err != nil {
				return tools.ToolError("remote_agent: dispatch to " + peer + " failed: " + err.Error()), nil
			}

			// 3. Async: register (with the dispatching session as the follow-up
			// target) and return immediately. The registry watchdog uses the
			// same timeout, so a lost result never shows "running" forever.
			timeout := time.Duration(timeoutSec) * time.Second
			if timeout <= 0 {
				timeout = defaultSyncTimeout
			}
			if async {
				b.RegisterInflight(taskID, peer, truncateTask(task), sess, timeout)
				return tools.ToolResult(fmt.Sprintf("Remote task dispatched to %s (async). task=%s You will be notified when it completes; use remote_agent_status to check.", peer, taskID)), nil
			}

			// 4. Sync: block for the result; on timeout or a local abort (user
			// interrupted the run) interrupt the slave so the remote task stops
			// too instead of leaving it running.
			res, werr := b.WaitForResult(ctx, taskID, timeout)
			if werr == ErrTaskTimeout {
				_ = m.Send(peer, mesh.Envelope{Type: mesh.MsgInterrupt, To: peer, ReplyTo: taskID})
				return tools.ToolError(fmt.Sprintf("Remote task %s on %s timed out after %s; interrupt sent to the remote node.", taskID, peer, timeout)), nil
			}
			if werr == ErrTaskAborted {
				_ = m.Send(peer, mesh.Envelope{Type: mesh.MsgInterrupt, To: peer, ReplyTo: taskID})
				return tools.ToolError(fmt.Sprintf("Remote task %s on %s aborted: the local run was interrupted; interrupt sent to the remote node.", taskID, peer)), nil
			}
			// Remember the session for this master session + peer (sync path;
			// async is remembered by the bridge on the result).
			b.rememberSession(masterSessID, peer, res.Session)
			switch res.Status {
			case "error":
				return tools.ToolError(fmt.Sprintf("Remote task %s on %s failed: %s", taskID, peer, res.Error)), nil
			case "aborted":
				return tools.ToolError(fmt.Sprintf("Remote task %s on %s was aborted: %s", taskID, peer, res.Error)), nil
			}
			out := res.Output
			if res.Truncated {
				out += "\n[output truncated on the remote side; use session-snap to pull the full session]"
			}
			return tools.ToolResult(fmt.Sprintf("Remote task %s on %s completed (session=%s):\n%s", taskID, peer, res.Session, out)), nil
		},
	}
}

// selectPeer chooses a target for role-based routing: a connected, accepting
// peer that is under its load cap, preferring the least loaded.
func selectPeer(m mesh.Mesh, roles []string) string {
	best := ""
	bestLoad := -1
	for _, p := range m.Peers() {
		if !p.Profile.Accepting {
			continue
		}
		if p.Profile.MaxLoad > 0 && p.Profile.Load >= p.Profile.MaxLoad {
			continue
		}
		if len(roles) > 0 && !rolesMatch(p.Profile.Roles, roles) {
			continue
		}
		if best == "" || p.Profile.Load < bestLoad {
			best = p.ID
			bestLoad = p.Profile.Load
		}
	}
	return best
}

// rolesMatch reports whether the profile declares any of the requested roles.
func rolesMatch(profileRoles, want []string) bool {
	for _, w := range want {
		for _, r := range profileRoles {
			if r == w {
				return true
			}
		}
	}
	return false
}

// buildSnapshot prepares the context snapshot for the given mode. fresh is
// empty; summary gzip-compresses a generated summary of the master session;
// fork gzip-compresses the recent transcript messages. The model never sees
// the raw bytes.
func buildSnapshot(sess *coding.AgentSession, mode string) ([]byte, error) {
	if sess == nil || mode == "fresh" {
		return nil, nil
	}
	switch mode {
	case "summary":
		summary, err := sess.SummarizeContext()
		if err != nil {
			return nil, fmt.Errorf("summary snapshot: %w", err)
		}
		if strings.TrimSpace(summary) == "" {
			return nil, nil
		}
		return compressSnapshot([]byte(summary))
	case "fork":
		msgs := sess.RecentMessages()
		if len(msgs) == 0 {
			return nil, nil
		}
		data, err := json.Marshal(msgs)
		if err != nil {
			return nil, err
		}
		return compressSnapshot(data)
	}
	return nil, nil
}

func truncateTask(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:120] + "…"
}

func strSliceParam(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func boolParam(v any) bool {
	b, _ := v.(bool)
	return b
}
