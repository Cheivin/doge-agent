package ai

import "testing"

// TestAbortMarkerDroppedFromHistory: an aborted run leaves a pure-marker
// assistant message (no text, no tool calls, no reasoning). Sending it back is
// rejected by gateways (OpenAI-compat "Format Error" 400 for null content with
// no tool_calls; Anthropic rejects empty content arrays), so both adapters
// must drop it from the request history. Regression for the field issue where
// an injected async follow-up after a user abort always failed with 400.
func TestAbortMarkerDroppedFromHistory(t *testing.T) {
	aborted := &Message{Role: RoleAssistant, Content: []ContentBlock{}, StopReason: StopReasonAborted, ErrorMessage: "aborted"}
	ctx := &Context{
		SystemPrompt: "sys",
		Messages: []*Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}},
			aborted,
			{Role: RoleUser, Content: []ContentBlock{TextBlock("[远端任务 x 完成] result")}},
		},
	}

	// OpenAI-completions: no assistant entry with null content.
	model := &Model{ID: "m", API: APIOpenAICompletions, Provider: "p", Reasoning: true}
	msgs := convertCompletionsMessages(model, ctx)
	for _, m := range msgs {
		item, ok := m.(map[string]any)
		if !ok || item["role"] != "assistant" {
			continue
		}
		if item["content"] == nil {
			t.Fatalf("assistant with null content leaked into request: %#v", item)
		}
	}
	if len(msgs) != 3 { // system + user + user (marker dropped)
		t.Fatalf("openai message count = %d, want 3 (abort marker dropped): %#v", len(msgs), msgs)
	}

	// Anthropic: no assistant entry with an empty content array.
	blocks := convertAnthropicMessages(ctx.Messages)
	for _, m := range blocks {
		item, ok := m.(map[string]any)
		if !ok || item["role"] != "assistant" {
			continue
		}
		if c, ok := item["content"].([]any); ok && len(c) == 0 {
			t.Fatalf("assistant with empty content leaked into request: %#v", item)
		}
	}
}

// TestAbortMarkerWithToolCallsKept: an aborted assistant that still carries
// ANSWERED tool calls (results exist) is not a pure marker and must be kept.
func TestAbortMarkerWithToolCallsKept(t *testing.T) {
	ctx := &Context{
		Messages: []*Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}},
			{Role: RoleAssistant, Content: []ContentBlock{ToolCallBlock("c1", "bash", map[string]any{"command": "ls"})}},
			{Role: RoleToolResult, ToolCallID: "c1", ToolName: "bash", Content: []ContentBlock{TextBlock("out")}},
			{Role: RoleUser, Content: []ContentBlock{TextBlock("next")}},
		},
	}
	msgs := convertCompletionsMessages(&Model{API: APIOpenAICompletions}, ctx)
	assistant := 0
	for _, m := range msgs {
		if item, ok := m.(map[string]any); ok && item["role"] == "assistant" {
			assistant++
		}
	}
	if assistant != 1 {
		t.Fatalf("assistant messages = %d, want 1 (kept)", assistant)
	}
}
