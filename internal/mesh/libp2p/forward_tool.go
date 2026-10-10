//go:build mesh

package libp2p

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"doge/internal/agent"
	"doge/internal/coding/tools"
	"doge/internal/mesh"
)

// ForwardTool builds the mesh_forward agent tool backed by the given mesh. It
// lets the agent manage port forwarding while executing a task (e.g. pull a
// remote docker daemon to a local port) without the user switching terminals.
func ForwardTool(m mesh.Mesh) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "mesh_forward",
		Label:       "Manage P2P port forwarding",
		Description: "List, add, remove, enable or disable P2P port forwarding rules. mode=expose publishes a local service (local = host:port or unix:///path) to the mesh; mode=listen bridges a local bind:port (or unix socket) to a peer's exposed service (peer + remote). Changes are persisted and hot-applied to the running mesh.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":  map[string]any{"type": "string", "enum": []string{"list", "add", "remove", "enable", "disable", "reload"}, "description": "operation to perform"},
				"name":    map[string]any{"type": "string", "description": "rule name (add/remove/enable/disable)"},
				"mode":    map[string]any{"type": "string", "enum": []string{"expose", "listen"}, "description": "add: expose (publish local) or listen (bridge to a remote service)"},
				"local":   map[string]any{"type": "string", "description": "add expose: local service address, host:port or unix:///path"},
				"bind":    map[string]any{"type": "string", "description": "add listen: bind interface (0.0.0.0, 127.0.0.1, an IP, or unix:///path)"},
				"port":    map[string]any{"type": "integer", "description": "add listen: TCP port to bind"},
				"peer":    map[string]any{"type": "string", "description": "add listen: target peer id, or a node NAME (alias) as shown by mesh_peers — resolved to the peer id when the rule is stored"},
				"remote":  map[string]any{"type": "string", "description": "add listen: remote exposed service name"},
				"enabled": map[string]any{"type": "boolean", "description": "add: start the rule immediately (default true)"},
			},
			"required": []string{"action"},
		},
		Execute: func(_ string, params map[string]any, _ context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			action, _ := params["action"].(string)
			switch action {
			case "list":
				return listForwardResult(m.ForwardList()), nil
			case "add":
				rule := mesh.ForwardRule{
					Name:    strParam(params["name"]),
					Mode:    strParam(params["mode"]),
					Local:   strParam(params["local"]),
					Bind:    strParam(params["bind"]),
					Port:    intParam(params["port"]),
					Peer:    strParam(params["peer"]),
					Remote:  strParam(params["remote"]),
					Enabled: true,
				}
				if v, ok := params["enabled"].(bool); ok {
					rule.Enabled = v
				}
				// listen 规则的 peer 支持节点名：解析成真实 peer ID 再落盘
				// （配置里存别名无法拨号）；重名在此处即时报错
				if rule.Mode == "listen" && rule.Peer != "" {
					resolved, err := resolvePeerRef(m, rule.Peer)
					if err != nil {
						return tools.ToolError("mesh_forward add: " + err.Error()), nil
					}
					rule.Peer = resolved
				}
				if err := m.ForwardAdd(rule); err != nil {
					return tools.ToolError("mesh_forward add: " + err.Error()), nil
				}
				return listForwardResult(m.ForwardList()), nil
			case "remove":
				name := strParam(params["name"])
				if err := m.ForwardRemove(name); err != nil {
					return tools.ToolError("mesh_forward remove: " + err.Error()), nil
				}
				return tools.ToolResult(fmt.Sprintf("Removed forward rule %q", name)), nil
			case "enable":
				name := strParam(params["name"])
				if err := m.ForwardSetEnabled(name, true); err != nil {
					return tools.ToolError("mesh_forward enable: " + err.Error()), nil
				}
				return tools.ToolResult(fmt.Sprintf("Enabled forward rule %q", name)), nil
			case "disable":
				name := strParam(params["name"])
				if err := m.ForwardSetEnabled(name, false); err != nil {
					return tools.ToolError("mesh_forward disable: " + err.Error()), nil
				}
				return tools.ToolResult(fmt.Sprintf("Disabled forward rule %q", name)), nil
			case "reload":
				if err := m.ForwardReload(); err != nil {
					return tools.ToolError("mesh_forward reload: " + err.Error()), nil
				}
				return listForwardResult(m.ForwardList()), nil
			default:
				return tools.ToolError("mesh_forward: unknown action " + strParam(params["action"])), nil
			}
		},
	}
}

// listForwardResult renders the rule table for tool results.
func listForwardResult(rules []mesh.ForwardRule) *agent.AgentToolResult {
	if len(rules) == 0 {
		return tools.ToolResult("No forward rules.")
	}
	sorted := append([]mesh.ForwardRule(nil), rules...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	b.WriteString("Forward rules:\n")
	for _, r := range sorted {
		state := "enabled"
		if !r.Enabled {
			state = "disabled"
		}
		switch r.Mode {
		case "expose":
			fmt.Fprintf(&b, "  %-16s expose  local=%s  [%s]\n", r.Name, r.Local, state)
		case "listen":
			fmt.Fprintf(&b, "  %-16s listen  bind=%s:%d -> %s/%s  [%s]\n", r.Name, r.Bind, r.Port, r.Peer, r.Remote, state)
		default:
			fmt.Fprintf(&b, "  %-16s %s  [%s]\n", r.Name, r.Mode, state)
		}
	}
	return tools.ToolResult(b.String())
}

func strParam(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intParam(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case int64:
		return int(n)
	}
	return 0
}
