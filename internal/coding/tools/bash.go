package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
)

// BashTool runs a shell command with a timeout. Output is streamed as
// partial updates and truncated to the output limits; the whole process
// group is killed on timeout or abort.
func BashTool(ops *Operations) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "bash",
		Label:       "Bash",
		Description: "Run a shell command. Use a timeout to prevent hangs. The working directory is the session cwd.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "The shell command to run"},
				"timeout": map[string]any{"type": "number", "description": "Timeout in seconds (default 120)"},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			command, _ := params["command"].(string)
			if command == "" {
				return ToolError("bash: command is required"), nil
			}
			timeoutSec := intParam(params, "timeout", 120)
			if timeoutSec <= 0 {
				timeoutSec = 120
			}

			execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
			defer cancel()

			// Stream output as partial updates (100ms throttle).
			var acc strings.Builder
			var lastEmit time.Time
			emitAccumulated := func(force bool) {
				if acc.Len() == 0 {
					return
				}
				if !force && time.Since(lastEmit) < 100*time.Millisecond {
					return
				}
				partial := acc.String()
				acc.Reset()
				lastEmit = time.Now()
				if onUpdate != nil {
					onUpdate(agent.AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(partial)}})
				}
			}

			result, execErr := ops.Exec(execCtx, command, ExecOptions{
				SessionEnv: []string{
					"DOGE_SESSION_ID=" + ops.SessionID,
					"DOGE_SESSION_FILE=" + ops.SessionFile,
				},
				OnOutput: func(chunk []byte) {
					acc.Write(chunk)
					emitAccumulated(false)
				},
			})
			emitAccumulated(true)

			if execErr != nil {
				if ctx.Err() != nil {
					return ToolError("bash: aborted"), nil
				}
				return ToolError(fmt.Sprintf("bash: %v", execErr)), nil
			}

			output := truncateOutput(acc.String())
			if output == "" {
				output = result.Stdout
			}
			if result.ExitCode != 0 {
				msg := fmt.Sprintf("bash: command exited with code %d", result.ExitCode)
				if result.TimedOut {
					msg = fmt.Sprintf("bash: command timed out after %ds", timeoutSec)
				}
				if output != "" {
					msg += "\n" + output
				}
				return ToolError(msg), nil
			}
			if output == "" {
				output = "(no output)"
			}
			return ToolResult(output), nil
		},
	}
}
