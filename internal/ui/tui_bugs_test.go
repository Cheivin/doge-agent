package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// recordingSession wraps agentSessionStub and records abort/prompt calls.
type recordingSession struct {
	*agentSessionStub
	aborted  int
	prompted int
}

func (s *recordingSession) Abort() { s.aborted++ }
func (s *recordingSession) Prompt(text string) error {
	s.prompted++
	return nil
}

// Bug: successful slash commands leave "✓ ..." status, keeping busy() true so
// the spinner animates forever even though nothing is running.
func TestBusyClearedAfterCompletedStatus(t *testing.T) {
	m := newModel(&agentSessionStub{}, nil, nil, "")
	for _, status := range []string{
		"✓ 已重载 models.json / auth.json / permission.json",
		"✓ 已切换模型",
		"✓ 已开始新会话",
		"✓ bash", // tool execution ended
	} {
		m.status = status
		if m.busy() {
			t.Errorf("busy() = true for completed status %q; spinner would be stuck", status)
		}
	}
	// Genuinely in-progress states must stay busy.
	for _, status := range []string{"…", "→ bash", "压缩中…"} {
		m.status = status
		if !m.busy() {
			t.Errorf("busy() = false for in-progress status %q", status)
		}
	}
}

// Bug: ESC while idle claims "中断请求已发送" and calls Abort though no run is
// active.
func TestEscIdleDoesNotAbort(t *testing.T) {
	sess := &recordingSession{agentSessionStub: &agentSessionStub{}}
	m := newModel(sess, nil, nil, "")
	m.status = "✓ 已重载 models.json / auth.json / permission.json"

	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if sess.aborted != 0 {
		t.Errorf("ESC called Abort while idle (aborted=%d)", sess.aborted)
	}
	if m.status != "✓ 已重载 models.json / auth.json / permission.json" {
		t.Errorf("ESC overwrote idle status with %q", m.status)
	}
}

// Guard: ESC during an active run must still abort.
func TestEscAbortsActiveRun(t *testing.T) {
	sess := &recordingSession{agentSessionStub: &agentSessionStub{}}
	m := newModel(sess, nil, nil, "")
	m.runActive = true
	m.status = "→ bash"

	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if sess.aborted != 1 {
		t.Errorf("ESC did not abort active run (aborted=%d)", sess.aborted)
	}
	if !strings.Contains(m.status, "中断请求已发送") {
		t.Errorf("ESC while running = status %q, want abort message", m.status)
	}
}

// Bug: Shift+Enter should insert a newline instead of submitting.
func TestInputBoxShiftEnterNewline(t *testing.T) {
	b := newInputBox()
	b.Focus()
	b.SetValue("abc")

	if !b.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}) {
		t.Fatal("shift+enter not consumed by the input box")
	}
	if b.Value() != "abc\n" {
		t.Fatalf("value = %q, want abc\\n", b.Value())
	}
	// Plain enter still submits (handled by the model, not the input box).
	if b.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) {
		t.Fatal("plain enter must not be consumed by the input box")
	}
}

// Bug: up/down in a multiline input must move the cursor between lines, not
// browse history.
func TestInputBoxMultilineCursor(t *testing.T) {
	b := newInputBox()
	b.Focus()
	b.SetValue("line one\nline two\nline three")
	b.cursor = 4 // line 0, offset 4 (after "line")

	b.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	// line 1 starts at index 9; offset clamped to min(4, len("line two"))=4.
	if b.cursor != 13 {
		t.Errorf("down: cursor = %d, want 13", b.cursor)
	}
	b.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	// line 2 starts at index 18; offset 4.
	if b.cursor != 22 {
		t.Errorf("down: cursor = %d, want 22", b.cursor)
	}
	// At the last line, down is a no-op.
	b.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if b.cursor != 22 {
		t.Errorf("down past end: cursor = %d, want 22", b.cursor)
	}
	b.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if b.cursor != 13 {
		t.Errorf("up: cursor = %d, want 13", b.cursor)
	}
	b.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if b.cursor != 4 {
		t.Errorf("up: cursor = %d, want 4", b.cursor)
	}
}

// Bug: pasting multiline text must keep the newlines instead of folding them
// into spaces.
func TestInputBoxMultilinePaste(t *testing.T) {
	b := newInputBox()
	b.Focus()
	b.Paste("a\r\nb\nc")
	if b.Value() != "a\nb\nc" {
		t.Errorf("paste = %q, want a\\nb\\nc", b.Value())
	}
}

// Bug: shift+enter in the model must insert a newline without submitting.
func TestModelShiftEnterInsertsNewline(t *testing.T) {
	sess := &recordingSession{agentSessionStub: &agentSessionStub{}}
	m := newModel(sess, nil, nil, "")
	m.input.Focus()
	m.input.SetValue("hello")

	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})

	if sess.prompted != 0 {
		t.Fatalf("shift+enter submitted the prompt (prompted=%d)", sess.prompted)
	}
	if !strings.Contains(m.input.Value(), "\n") {
		t.Fatalf("shift+enter did not insert a newline: %q", m.input.Value())
	}
}
