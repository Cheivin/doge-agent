//go:build mesh

package libp2p

import (
	"context"
	"fmt"
	"strings"

	"doge/internal/agent"
	"doge/internal/coding/tools"
	"doge/internal/mesh"
)

// RemoteAgentStatusTool builds the remote_agent_status agent tool: it lists
// the asynchronous remote_agent tasks in flight (task id, peer, status,
// progress) so the master can orchestrate several concurrent remote tasks.
func RemoteAgentStatusTool(b *Bridge) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "remote_agent_status",
		Label:       "List in-flight remote tasks",
		Description: "List asynchronous remote_agent tasks dispatched to remote mesh nodes: task id, peer, status (running/completed/error/aborted/recovered) and latest progress. Pass task=<id> to inspect one task.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{"type": "string", "description": "optional task id to inspect"},
			},
		},
		Execute: func(_ string, params map[string]any, _ context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			if taskID := strParam(params["task"]); taskID != "" {
				info, ok := b.Status(taskID)
				if !ok {
					return tools.ToolResult("No in-flight remote task " + taskID), nil
				}
				return tools.ToolResult(formatInflight(info)), nil
			}
			list := b.ListInflight()
			if len(list) == 0 {
				return tools.ToolResult("No in-flight remote tasks."), nil
			}
			var sb strings.Builder
			sb.WriteString("In-flight remote tasks:\n")
			for _, info := range list {
				fmt.Fprintf(&sb, "  %s  %-10s  peer=%s  %s\n", info.TaskID, info.Status, info.Peer, info.Progress)
			}
			return tools.ToolResult(sb.String()), nil
		},
	}
}

func formatInflight(info mesh.TaskStatus) string {
	return fmt.Sprintf("task=%s status=%s peer=%s dispatched=%s progress=%s",
		info.TaskID, info.Status, info.Peer, info.Dispatched.Format("15:04:05"), info.Progress)
}
