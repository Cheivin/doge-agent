package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"doge/internal/ai"
)

func TestCtrlOThroughUpdate(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{}, sess: &fakeSession{}, input: newInputBox(), cwd: "/tmp"}
	tr := &ai.Message{Role: ai.RoleToolResult, Content: []ai.ContentBlock{ai.TextBlock("hello-doge")}}
	m.messages = append(m.messages, tr)
	m.width = 100
	m.height = 24

	// 通过 Update 的键路径触发 ctrl+o
	_, _ = m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if !m.expandedTools[toolFoldKey(tr)] {
		t.Fatal("ctrl+o via Update did not expand")
	}
	view := m.View().Content
	if !strings.Contains(view, "收起") {
		t.Errorf("view missing 收起: %q", view)
	}
}

func TestCopyLastReply(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{},
		sess: &fakeSession{}, input: newInputBox(), cwd: "/tmp"}
	if got := m.lastAssistantText(); got != "" {
		t.Fatalf("expected no assistant reply yet, got %q", got)
	}
	m.messages = append(m.messages,
		&ai.Message{Role: ai.RoleUser, Content: []ai.ContentBlock{ai.TextBlock("hi")}},
		&ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{
			{Type: ai.ContentTypeThinking, Thinking: "hmm"},
			{Type: ai.ContentTypeText, Text: "第一段"},
			{Type: ai.ContentTypeText, Text: "第二段"},
		}},
	)
	if got := m.lastAssistantText(); got != "第一段\n第二段" {
		t.Fatalf("lastAssistantText = %q, want 第一段\\n第二段", got)
	}

	// ctrl+y returns a clipboard command and sets the status; it ignores the
	// thinking block and only copies the reply's text.
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+y should return a command")
	}
	if !strings.Contains(m.status, "剪贴板") {
		t.Errorf("status = %q, want clipboard hint", m.status)
	}

	// With no assistant message, the key shows a hint and no command.
	m2 := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{},
		sess: &fakeSession{}, input: newInputBox(), cwd: "/tmp"}
	if _, cmd := m2.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}); cmd != nil {
		t.Error("ctrl+y with no reply should not emit a command")
	}
}

func TestInputBoxPaste(t *testing.T) {
	b := newInputBox()
	b.Focus()
	b.Paste("第一行\n第二行\r\n第三行")
	if b.Value() != "第一行\n第二行\n第三行" {
		t.Errorf("value = %q, want CRLF normalized and newlines preserved", b.Value())
	}
	b.Paste(" 尾部")
	if b.Value() != "第一行\n第二行\n第三行 尾部" {
		t.Errorf("paste at cursor = %q", b.Value())
	}
}

func TestPasteThroughUpdate(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{},
		sess: &fakeSession{}, input: newInputBox(), cwd: "/tmp"}
	m.input.Focus()
	_, _ = m.Update(tea.PasteMsg{Content: "粘贴内容：code foo"})
	if m.input.Value() != "粘贴内容：code foo" {
		t.Fatalf("input = %q, want pasted text inserted", m.input.Value())
	}
	// Pasting while a permission prompt is pending is ignored (keys are too).
	m.pendingPermission = &permissionAskMsg{}
	_, cmd := m.Update(tea.PasteMsg{Content: "ignored"})
	if cmd != nil {
		t.Fatal("expected nil cmd")
	}
	if m.input.Value() != "粘贴内容：code foo" {
		t.Fatalf("paste must not mutate input while permission pending, got %q", m.input.Value())
	}
}
