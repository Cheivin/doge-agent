package coding

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"doge/internal/agent"
	"doge/internal/ai"
)

// PrintMode is the non-interactive mode.
type PrintMode string

const (
	PrintModeText PrintMode = "text"
	PrintModeJSON PrintMode = "json"
)

// RunPrintMode sends one prompt and prints the result:
//   - text: only the final assistant text (like the print mode)
//   - json: one JSON object per agent event
//
// Returns the process exit code.
func RunPrintMode(session *AgentSession, message string, mode PrintMode) int {
	exitCode := 0
	if mode == PrintModeJSON {
		unsub, _ := session.Subscribe(jsonPrintListener(os.Stdout), ConsumerHeadless)
		defer unsub()
	}

	// An empty prompt (e.g. `doge` with stdin piped and no arguments) must
	// not start an agent run — that would spend an LLM call and materialize
	// a session file for no conversation at all.
	if strings.TrimSpace(message) == "" {
		fmt.Fprintln(os.Stderr, "doge: empty prompt (pass a message or run interactively)")
		return 1
	}

	if err := session.Prompt(message); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	session.WaitForIdle()

	// Text mode: print only the final assistant text.
	if mode == PrintModeText {
		printFinalAssistantText(session, os.Stdout, &exitCode)
	}
	return exitCode
}

// printFinalAssistantText prints the last assistant message text (
// mode semantics: only the final text, non-error).
func printFinalAssistantText(session *AgentSession, out *os.File, exitCode *int) {
	messages := session.CurrentMessages()
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != ai.RoleAssistant {
			continue
		}
		if m.StopReason == ai.StopReasonError || m.StopReason == ai.StopReasonAborted {
			fmt.Fprintf(os.Stderr, "doge: %s\n", m.ErrorMessage)
			*exitCode = 1
			return
		}
		text := m.Text()
		if text != "" {
			fmt.Fprintln(out, text)
		}
		return
	}
}

// jsonPrintListener serializes every agent event as a JSON line
// (print --mode json).
func jsonPrintListener(out *os.File) agent.AgentListener {
	return func(ev agent.AgentEvent) error {
		entry := map[string]any{
			"type": ev.Type,
		}
		if ev.Message != nil {
			entry["message"] = ev.Message
		}
		if ev.Messages != nil {
			entry["messages"] = ev.Messages
		}
		if ev.ToolResults != nil {
			entry["toolResults"] = ev.ToolResults
		}
		if ev.ToolName != "" {
			entry["toolName"] = ev.ToolName
			entry["toolCallId"] = ev.ToolCallID
		}
		if ev.Result != nil {
			entry["toolResult"] = ev.Result
			entry["isError"] = ev.IsError
		}
		data, err := json.Marshal(entry)
		if err == nil {
			fmt.Fprintln(out, string(data))
		}
		return nil
	}
}
