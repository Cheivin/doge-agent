package agent

import (
	"testing"

	"doge/internal/ai"
)

func TestStripOrphanedToolCalls(t *testing.T) {
	// A well-formed assistant → tool-result sequence is left unchanged.
	asst := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonToolUse,
		Content: []ai.ContentBlock{ai.ToolCallBlock("call_ok", "bash", map[string]any{})}}
	res := &ai.Message{Role: ai.RoleToolResult, ToolCallID: "call_ok",
		Content: []ai.ContentBlock{ai.TextBlock("done")}}
	got := stripOrphanedToolCalls([]*ai.Message{asst, res})
	if len(got) != 2 {
		t.Fatalf("well-formed sequence altered: %d messages", len(got))
	}

	// An aborted assistant message that is only an orphaned tool_call is
	// dropped entirely.
	orphan := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonAborted,
		Content: []ai.ContentBlock{ai.ToolCallBlock("call_orphan", "bash", map[string]any{})}}
	got = stripOrphanedToolCalls([]*ai.Message{orphan})
	if len(got) != 0 {
		t.Fatalf("orphaned tool_call message not dropped: %#v", got)
	}

	// Text + orphaned tool_call: text survives, the tool_call is stripped.
	asst3 := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonAborted,
		Content: []ai.ContentBlock{ai.TextBlock("let me check"), ai.ToolCallBlock("call_orphan", "bash", map[string]any{})}}
	got = stripOrphanedToolCalls([]*ai.Message{asst3})
	if len(got) != 1 || len(got[0].Content) != 1 || got[0].Content[0].Type != ai.ContentTypeText {
		t.Fatalf("text+orphan = %#v, want text only", got)
	}

	// Mixed: an answered call survives, the orphaned one is stripped.
	asst2 := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonToolUse,
		Content: []ai.ContentBlock{
			ai.ToolCallBlock("call_ok", "bash", map[string]any{}),
			ai.ToolCallBlock("call_orphan", "bash", map[string]any{}),
		}}
	res2 := &ai.Message{Role: ai.RoleToolResult, ToolCallID: "call_ok",
		Content: []ai.ContentBlock{ai.TextBlock("done")}}
	got = stripOrphanedToolCalls([]*ai.Message{asst2, res2})
	if len(got) != 2 {
		t.Fatalf("mixed = %d messages, want 2", len(got))
	}
	if len(got[0].Content) != 1 || got[0].Content[0].ToolCallID != "call_ok" {
		t.Fatalf("mixed assistant content = %#v, want only call_ok", got[0].Content)
	}
}

// TestAbortDoesNotPoisonNextTurn: after a run aborts mid-tool-call, the next
// turn must not send the orphaned tool_call to the provider (which would be a
// 400 "Format Error" on OpenAI-compatible endpoints).
func TestAbortDoesNotPoisonNextTurn(t *testing.T) {
	var sawOrphan bool
	scripts := []func(call int, ctx *ai.Context) *ai.Message{
		// Simulate an abort mid-tool-call: the assistant streamed a tool_call
		// and the run stopped before any tool result was produced.
		func(call int, ctx *ai.Context) *ai.Message {
			return &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonAborted,
				Content: []ai.ContentBlock{ai.ToolCallBlock("call_1", "bash", map[string]any{})}}
		},
		// The next turn must receive a history with no orphaned tool_call.
		func(call int, ctx *ai.Context) *ai.Message {
			for _, m := range ctx.Messages {
				if m.Role == ai.RoleAssistant {
					for _, c := range m.Content {
						if c.Type == ai.ContentTypeToolCall {
							sawOrphan = true
						}
					}
				}
			}
			return textStop("ok")
		},
	}
	a := NewAgent(AgentOptions{
		SystemPrompt: "be brief",
		Model:        testModel(),
		StreamFn:     fakeStream(scripts...),
	})
	if err := a.Prompt("first"); err != nil {
		t.Fatalf("prompt 1: %v", err)
	}
	a.WaitForIdle()
	if err := a.Prompt("second"); err != nil {
		t.Fatalf("prompt 2: %v", err)
	}
	a.WaitForIdle()
	if sawOrphan {
		t.Fatal("next turn received an orphaned tool_call (provider would 400)")
	}
}
