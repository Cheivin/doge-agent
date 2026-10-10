package coding

import (
	"strings"
	"testing"

	"doge/internal/ai"
)

func TestShouldCompact(t *testing.T) {
	// 190k tokens in a 200k window with 16k reserve → compact.
	if !ShouldCompact(190000, 200000, 16384) {
		t.Error("190k > 184k should compact")
	}
	if ShouldCompact(100000, 200000, 16384) {
		t.Error("100k < 184k should not compact")
	}
	// Exactly at the threshold → compact .
	if !ShouldCompact(183617, 200000, 16384) {
		t.Error("threshold should compact")
	}
}

func TestFindCutPoint(t *testing.T) {
	// Build entries: user, assistant, toolResult, user, assistant.
	var entries []*SessionEntry
	roles := []string{ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult, ai.RoleUser, ai.RoleAssistant}
	for i, role := range roles {
		entries = append(entries, &SessionEntry{
			ID:   string(rune('a' + i)),
			Type: EntryMessage,
			Message: &ai.Message{Role: role,
				Content: []ai.ContentBlock{ai.TextBlock(strings.Repeat("x", 40))}, // 10 tokens each
			},
		})
	}
	// keepRecentTokens=25: accumulate from the end: e4(10) + e3(10) + e2(10)=30 ≥ 25.
	// Candidate cut at e2 (toolResult) → backtrack to e3 (user). First kept = e3.
	cut, ok := FindCutPoint(entries, 25)
	if !ok {
		t.Fatal("no cut point found")
	}
	if cut != 3 {
		t.Errorf("cut = %d, want 3 (never a tool result)", cut)
	}

	// Small keepRecent keeps more.
	cut, ok = FindCutPoint(entries, 5)
	if !ok || cut != 4 {
		t.Errorf("cut with small keep = %d, %v", cut, ok)
	}
}

func TestBuildSummarizationPrompt(t *testing.T) {
	entries := []*SessionEntry{{
		Type: EntryMessage,
		Message: &ai.Message{Role: ai.RoleUser,
			Content: []ai.ContentBlock{ai.TextBlock("fix the build")}},
	}}
	prompt := BuildSummarizationPrompt(entries, "")
	if !strings.Contains(prompt, "## Goal") || !strings.Contains(prompt, "fix the build") {
		t.Errorf("prompt = %q", prompt)
	}
	if !strings.Contains(prompt, "<conversation>") {
		t.Errorf("missing conversation wrapper: %q", prompt)
	}

	// With a previous summary, use the update prompt.
	prompt2 := BuildSummarizationPrompt(entries, "previous summary")
	if !strings.Contains(prompt2, "## Previous Summary") || !strings.Contains(prompt2, "previous summary") {
		t.Errorf("update prompt = %q", prompt2)
	}
}

func TestSerializeConversation(t *testing.T) {
	entries := []*SessionEntry{
		{Type: EntryMessage, Message: ai.NewUserTextMessage("hello")},
		{Type: EntryMessage, Message: &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonToolUse,
			Content: []ai.ContentBlock{ai.ToolCallBlock("c1", "bash", map[string]any{"command": "ls"})}}},
		{Type: EntryMessage, Message: &ai.Message{Role: ai.RoleToolResult, ToolCallID: "c1", ToolName: "bash",
			Content: []ai.ContentBlock{ai.TextBlock("file list")}}},
	}
	text := SerializeConversation(entries)
	if !strings.Contains(text, "USER: hello") ||
		!strings.Contains(text, "TOOL CALL: bash") ||
		!strings.Contains(text, "TOOL RESULT: file list") {
		t.Errorf("serialized = %q", text)
	}
}
