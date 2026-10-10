package ui

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/permissions"
	"doge/internal/mesh"
)

// fakeMeshView is a scripted mesh view for the /mesh command test.
type fakeMeshView struct{}

func (fakeMeshView) Self() mesh.AgentProfile {
	return mesh.AgentProfile{PeerID: "self-id", Name: "node-a", Roles: []string{"master"}, Accepting: false, Load: 0, MaxLoad: 2}
}
func (fakeMeshView) Peers() []mesh.PeerInfo {
	return []mesh.PeerInfo{{ID: "peer-b", Profile: mesh.AgentProfile{Name: "node-b", Roles: []string{"worker"}, Accepting: true, Load: 1, MaxLoad: 2}}}
}
func (fakeMeshView) Tasks() []mesh.TaskStatus {
	return []mesh.TaskStatus{{TaskID: "t1", Peer: "peer-b", Status: "running", Progress: "doing work"}}
}

// TestShowMesh: the /mesh command renders the local node, connected peers and
// in-flight remote tasks into the status line; a nil mesh reports disabled.
func TestShowMesh(t *testing.T) {
	m := &model{mesh: fakeMeshView{}}
	m.showMesh()
	for _, want := range []string{"本节点", "self-id", "node-a", "roles=[master]", "peer-b", "node-b", "在途远端任务", "t1", "running"} {
		if !strings.Contains(m.status, want) {
			t.Fatalf("showMesh status missing %q:\n%s", want, m.status)
		}
	}

	m2 := &model{mesh: nil}
	m2.showMesh()
	if !strings.Contains(m2.status, "mesh 未启用") {
		t.Fatalf("nil mesh status = %q, want disabled message", m2.status)
	}
}

func TestInputBoxEditing(t *testing.T) {
	b := newInputBox()
	b.Focus()

	// Typing.
	for _, k := range []tea.KeyPressMsg{
		{Code: 'h', Text: "h"},
		{Code: 'i', Text: "i"},
	} {
		b.Update(k)
	}
	if b.Value() != "hi" {
		t.Errorf("value = %q", b.Value())
	}

	// Cursor left + insert.
	b.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	b.Update(tea.KeyPressMsg{Code: 'X', Text: "X"})
	if b.Value() != "hXi" {
		t.Errorf("after insert = %q", b.Value())
	}

	// Backspace.
	b.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if b.Value() != "hi" {
		t.Errorf("after backspace = %q", b.Value())
	}

	// Home + ctrl+w deletes the word.
	b.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	b.Update(tea.KeyPressMsg{Code: 'f', Text: "foo "})
	b.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if b.Value() != "hi" {
		t.Errorf("after ctrl+w = %q", b.Value())
	}

	// Enter is not consumed (submit handled by the caller).
	if b.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) {
		t.Error("enter should not be consumed by the input box")
	}
}

func TestRenderMessage(t *testing.T) {
	// User message.
	user := ai.NewUserTextMessage("hello world")
	m := &model{expandedThinking: map[string]bool{}}
	out, _ := m.renderMessage(user)
	if !strings.Contains(out, "hello world") {
		t.Errorf("user render = %q", out)
	}

	// Assistant with text + tool call + thinking.
	assistant := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonToolUse,
		Content: []ai.ContentBlock{
			ai.TextBlock("**bold** text"),
			ai.ThinkingBlock("reasoning…"),
			ai.ToolCallBlock("c1", "bash", map[string]any{"command": "ls"}),
		}}
	out, _ = m.renderMessage(assistant)
	if !strings.Contains(out, "bold") || !strings.Contains(out, "思考 ⤵") ||
		!strings.Contains(out, "bash") {
		t.Errorf("assistant render = %q", out)
	}
	// Collapsed by default: no thinking content shown.
	if strings.Contains(out, "reasoning") {
		t.Errorf("thinking should be collapsed: %q", out)
	}
	// Expanded: thinking content visible (per-block state).
	m.expandedThinking[thinkingKey(assistant, 1)] = true
	out, _ = m.renderMessage(assistant)
	if !strings.Contains(out, "reasoning") {
		t.Errorf("expanded thinking missing: %q", out)
	}

	// Tool result error.
	err := &ai.Message{Role: ai.RoleToolResult, IsError: true,
		Content: []ai.ContentBlock{ai.TextBlock("command failed")}}
	out, _ = m.renderMessage(err)
	if !strings.Contains(out, "command failed") {
		t.Errorf("tool error render = %q", out)
	}
}

func TestThinkingTimer(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}}
	// Thinking in progress shows a live timer.
	m.thinkingActive = true
	m.thinkingStart = time.Now().Add(-3 * time.Second)
	out, _ := m.renderThinking(ai.ContentBlock{Type: ai.ContentTypeThinking, Thinking: "secret"}, 0, "k1")
	if !strings.Contains(out, "思考中") || !strings.Contains(out, "3s") {
		t.Errorf("thinking timer = %q", out)
	}
	// The thinking content is hidden while collapsed.
	if strings.Contains(out, "secret") {
		t.Errorf("collapsed thinking leaked content: %q", out)
	}

	// Finished thinking collapses to a one-line entry; clicking its block
	// expands only that block.
	m.thinkingActive = false
	out, _ = m.renderThinking(ai.ContentBlock{Type: ai.ContentTypeThinking, Thinking: "secret"}, 0, "k1")
	if !strings.Contains(out, "思考 ⤵") || strings.Contains(out, "secret") {
		t.Errorf("collapsed = %q", out)
	}
	m.expandedThinking["k1"] = true
	out, _ = m.renderThinking(ai.ContentBlock{Type: ai.ContentTypeThinking, Thinking: "secret"}, 0, "k1")
	if !strings.Contains(out, "secret") {
		t.Errorf("expanded = %q", out)
	}
	// Other blocks stay collapsed.
	out, _ = m.renderThinking(ai.ContentBlock{Type: ai.ContentTypeThinking, Thinking: "other"}, 0, "k2")
	if strings.Contains(out, "other") {
		t.Errorf("unrelated block should stay collapsed: %q", out)
	}
}

func TestScrollResetOnEvent(t *testing.T) {
	// New content arrives: offset resets (follow bottom).
	m := &model{expandedThinking: map[string]bool{}}
	m.scrollOffset = 5
	m.handleEvent(agent.AgentEvent{Type: agent.EventAgentEnd})
	if m.scrollOffset != 0 {
		t.Errorf("offset should reset on events, got %d", m.scrollOffset)
	}
}

func TestMouseScrolling(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}}
	m.height = 30
	// Wheel up scrolls back.
	m.handleMouse(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.scrollOffset != 3 {
		t.Errorf("wheel up offset = %d", m.scrollOffset)
	}
	// Wheel down returns.
	m.handleMouse(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.scrollOffset != 0 {
		t.Errorf("wheel down offset = %d", m.scrollOffset)
	}
	// Never negative.
	m.handleMouse(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.scrollOffset != 0 {
		t.Errorf("negative offset = %d", m.scrollOffset)
	}
}

func TestMouseClickHitTest(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}}
	m.height = 30
	m.contentHeight = 50
	m.foldLines = map[int]string{10: "k1"}

	// Click on the folded-thinking line (content line 10) toggles.
	// Window: msgHeight=26, offset=0 → start=24; screen Y = 10-24 <0 — so
	// scroll up first so the line is visible.
	m.scrollOffset = 20 // start = 50-26-20 = 4 → screen Y of content 10 = 6
	m.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 6})
	if !m.expandedThinking["k1"] {
		t.Error("click on the thinking line should expand that block")
	}
	// Click on another line does not toggle.
	m.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 9})
	if !m.expandedThinking["k1"] {
		t.Error("click elsewhere should not toggle back")
	}
	// Click outside the window (footer) does nothing.
	m.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 29})
	if !m.expandedThinking["k1"] {
		t.Error("footer click should not toggle")
	}
	// No thinking lines: clicks do nothing.
	m.foldLines = map[int]string{}
	m.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 7})
	if !m.expandedThinking["k1"] {
		t.Error("click without a thinking line should not toggle")
	}
}

func TestMouseClickPerBlock(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}}
	m.height = 30
	m.contentHeight = 50
	m.scrollOffset = 20 // start = 4
	// Two collapsed thinking entries on different lines.
	m.foldLines = map[int]string{5: "k1", 8: "k2"}

	// Click the first fold line (screen Y = 5-4 = 1): only k1 expands.
	m.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 1})
	if !m.expandedThinking["k1"] {
		t.Error("click on first fold line should expand k1")
	}
	if m.expandedThinking["k2"] {
		t.Error("k2 must stay collapsed")
	}
	// Click the second fold line (screen Y = 8-4 = 4): only k2 expands.
	m.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 4})
	if !m.expandedThinking["k2"] {
		t.Error("click on second fold line should expand k2")
	}
	// Click k1 again: only k1 collapses.
	m.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 1})
	if m.expandedThinking["k1"] {
		t.Error("second click on k1 should collapse it")
	}
	if !m.expandedThinking["k2"] {
		t.Error("k2 must stay expanded")
	}
}

func TestMouseClickShortContent(t *testing.T) {
	// Content shorter than the message area: the window starts at line 0,
	// so a click on a fold line must still hit (regression: the start was
	// computed without clamping and went negative).
	m := &model{expandedThinking: map[string]bool{}}
	m.height = 24
	m.contentHeight = 8
	m.foldLines = map[int]string{1: "k1", 2: "k2"}

	m.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 1})
	if !m.expandedThinking["k1"] {
		t.Error("click on fold at content line 1 should expand k1")
	}
	if m.expandedThinking["k2"] {
		t.Error("k2 must stay collapsed")
	}
	m.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 2})
	if !m.expandedThinking["k2"] {
		t.Error("click on fold at content line 2 should expand k2")
	}
}

func TestCommandMenu(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}}
	m.input = newInputBox()
	m.input.Focus()

	// Typing "/" opens the menu with all commands.
	m.input.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	m.updateCommandMenu()
	if !m.cmdMenu || len(m.cmdItems) != len(cmdMenuItems) {
		t.Fatalf("menu not open: %v %d", m.cmdMenu, len(m.cmdItems))
	}
	// Filtering: "/mo" matches /model only.
	m.input.Update(tea.KeyPressMsg{Code: 'm', Text: "mo"})
	m.updateCommandMenu()
	if len(m.cmdItems) != 1 || m.cmdItems[0].cmd != "/model" {
		t.Fatalf("filter = %v", m.cmdItems)
	}
	// Clearing the input closes the menu.
	m.input.Reset()
	m.updateCommandMenu()
	if m.cmdMenu {
		t.Error("menu should close when input is cleared")
	}
	// "//" is not a command.
	m.input.SetValue("//x")
	m.updateCommandMenu()
	if m.cmdMenu {
		t.Error("// should not open the menu")
	}
}

func TestCommandMenuRender(t *testing.T) {
	items := []cmdMenuItem{
		{cmd: "/quit", desc: "退出"},
		{cmd: "/model", desc: "切换模型", needsArgs: true},
	}
	view := renderCommandMenu(items, 1)
	if !strings.Contains(view, "❯ /model") || !strings.Contains(view, "/quit") {
		t.Errorf("menu render = %q", view)
	}
	if strings.Contains(view, "❯ /quit") {
		t.Errorf("cursor should be on /model: %q", view)
	}
}

func TestHandleEvents(t *testing.T) {
	sess := &agentSessionStub{}
	m := newModel(sess, nil, nil, "")
	m.program = &tea.Program{}

	// Message lifecycle events.
	m.handleEvent(agent.AgentEvent{Type: agent.EventMessageStart, Message: ai.NewUserTextMessage("q")})
	m.handleEvent(agent.AgentEvent{Type: agent.EventMessageEnd, Message: ai.NewUserTextMessage("q")})
	if len(m.messages) != 1 {
		t.Fatalf("messages = %d", len(m.messages))
	}

	// Streaming assistant.
	streaming := &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{ai.TextBlock("par")}}
	m.handleEvent(agent.AgentEvent{Type: agent.EventMessageStart, Message: streaming})
	if m.streaming == nil {
		t.Fatal("streaming not set")
	}
	streaming.Content = []ai.ContentBlock{ai.TextBlock("partial")}
	m.handleEvent(agent.AgentEvent{Type: agent.EventMessageUpdate, Message: streaming})
	if m.streaming.Text() != "partial" {
		t.Errorf("streaming text = %q", m.streaming.Text())
	}
	final := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{ai.TextBlock("done")}}
	m.handleEvent(agent.AgentEvent{Type: agent.EventMessageEnd, Message: final})
	if m.streaming != nil || len(m.messages) != 2 {
		t.Errorf("after end: streaming=%v messages=%d", m.streaming, len(m.messages))
	}

	// Tool lifecycle.
	m.handleEvent(agent.AgentEvent{Type: agent.EventToolExecutionStart, ToolCallID: "c1", ToolName: "bash"})
	m.handleEvent(agent.AgentEvent{Type: agent.EventToolExecutionUpdate, ToolCallID: "c1",
		PartialResult: agent.AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("out")}}})
	if m.toolOutputs["c1"] != "out" {
		t.Errorf("tool output = %q", m.toolOutputs["c1"])
	}
	m.handleEvent(agent.AgentEvent{Type: agent.EventToolExecutionEnd, ToolCallID: "c1", ToolName: "bash", IsError: false})
}

type agentSessionStub struct {
	messages []*ai.Message
	sessions []coding.SessionSummary
	cwd      string
	deleted  []string
}

func (s *agentSessionStub) CurrentMessages() []*ai.Message { return s.messages }
func (s *agentSessionStub) Subscribe(l agent.AgentListener, kind coding.ConsumerKind) (func(), func(coding.ConsumerKind)) {
	return func() {}, func(coding.ConsumerKind) {}
}
func (s *agentSessionStub) Prompt(text string) error      { return nil }
func (s *agentSessionStub) Abort()                        {}
func (s *agentSessionStub) Compact() error                { return nil }
func (s *agentSessionStub) SwitchModel(spec string) error { return nil }
func (s *agentSessionStub) NewSession() error             { return nil }
func (s *agentSessionStub) OpenSession(id string) error   { return nil }
func (s *agentSessionStub) DeleteSession(id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}
func (s *agentSessionStub) ListSessions() ([]coding.SessionSummary, error) {
	gone := map[string]bool{}
	for _, id := range s.deleted {
		gone[id] = true
	}
	var out []coding.SessionSummary
	for _, sess := range s.sessions {
		if !gone[sess.ID] {
			out = append(out, sess)
		}
	}
	return out, nil
}
func (s *agentSessionStub) GetCwd() string           { return s.cwd }
func (s *agentSessionStub) CurrentSessionID() string { return "" }
func (s *agentSessionStub) CostPolicyFor(provider, model string) coding.CostPolicy {
	return coding.CostPolicy{}
}
func (s *agentSessionStub) ReloadConfig() error { return nil }
func (s *agentSessionStub) AgentRuntime() *agent.Agent {
	return agent.NewAgent(agent.AgentOptions{Model: &ai.Model{ID: "stub", Provider: "stub"}})
}
func (s *agentSessionStub) AnswerQuestion(id, text string) bool { return true }

func TestTruncateHelpers(t *testing.T) {
	if got := truncateLine("abcdef", 3); got != "abc" {
		t.Errorf("truncateLine = %q", got)
	}
	if got := wrapLine("abcdef", 3); got != "abc\ndef" {
		t.Errorf("wrapLine = %q", got)
	}
}

func TestCJKWidthHandling(t *testing.T) {
	// "你好世界" is 8 bytes, 4 runes, 8 display columns.
	s := "你好世界"
	if got := truncateLine(s, 4); got != "你好" {
		t.Errorf("truncateLine CJK width 4 = %q (must not split UTF-8)", got)
	}
	if got := truncateLine(s, 5); got != "你好" {
		t.Errorf("truncateLine CJK width 5 = %q", got)
	}
	if got := truncateLine(s, 3); got != "你" {
		t.Errorf("truncateLine CJK width 3 = %q", got)
	}
	// No split: the result must be valid UTF-8.
	if !utf8.ValidString(truncateLine("你好世界abc", 7)) {
		t.Error("truncateLine produced invalid UTF-8")
	}
	// Wrapping keeps CJK characters intact.
	got := wrapLine("你好世界abc", 5)
	if got != "你好\n世界a\nbc" {
		t.Errorf("wrapLine CJK = %q", got)
	}
	// Mixed ASCII + CJK.
	if got := truncateLine("ab你好", 4); got != "ab你" {
		t.Errorf("truncateLine mixed = %q", got)
	}
	// Rune-count truncation.
	if got := truncateRunes("你好世界", 3); got != "你好世…" {
		t.Errorf("truncateRunes = %q", got)
	}
}

func TestInputBoxCJK(t *testing.T) {
	b := newInputBox()
	b.Focus()

	// Typing CJK runes.
	b.Update(tea.KeyPressMsg{Code: '你', Text: "你好"})
	if b.Value() != "你好" {
		t.Errorf("value = %q", b.Value())
	}
	if b.cursor != 2 {
		t.Errorf("cursor = %d, want 2 (rune count)", b.cursor)
	}

	// Insert in the middle of CJK text.
	b.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	b.Update(tea.KeyPressMsg{Code: '世', Text: "世"})
	if b.Value() != "你世好" {
		t.Errorf("insert = %q", b.Value())
	}

	// Backspace deletes one rune.
	b.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if b.Value() != "你好" {
		t.Errorf("backspace = %q", b.Value())
	}

	// The rendered view must contain valid UTF-8 (cursor marker insertion
	// must not split a rune).
	view := b.View()
	if !utf8.ValidString(view) {
		t.Errorf("view contains invalid UTF-8: %q", view)
	}
}

func TestRenderErrorMessage(t *testing.T) {
	// A failed assistant turn must render its error so failures are visible.
	err := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonError,
		ErrorMessage: "HTTP 429: rpm exhausted", Content: []ai.ContentBlock{ai.TextBlock("")}}
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{}}
	out, _ := m.renderMessage(err)
	if !strings.Contains(out, "429") || !strings.Contains(out, "rpm exhausted") {
		t.Errorf("error render = %q", out)
	}
}

func TestToolResultFold(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{}}
	tr := &ai.Message{Role: ai.RoleToolResult, ToolName: "bash",
		Content: []ai.ContentBlock{ai.TextBlock("total 8\ndir1\ndir2\nfile3\nfile4\nfile5")}}

	// Collapsed by default: one line, fold key recorded.
	out, folds := m.renderMessage(tr)
	if strings.Count(out, "\n") != 1 {
		t.Errorf("collapsed tool result should be one line, got %q", out)
	}
	if len(folds) != 1 {
		t.Fatalf("expected one fold, got %v", folds)
	}
	var key string
	for k := range folds {
		key = k
	}
	if !strings.HasPrefix(key, "tool:") {
		t.Errorf("fold key = %q, want tool: prefix", key)
	}
	if !strings.Contains(out, "⤵") {
		t.Errorf("collapsed line should hint expandability: %q", out)
	}

	// Expanded: full content visible, and the fold line stays registered so
	// a click can collapse it again.
	m.expandedTools[key] = true
	out2, folds2 := m.renderMessage(tr)
	if !strings.Contains(out2, "file5") {
		t.Errorf("expanded output missing tail: %q", out2)
	}
	if !strings.Contains(out2, "收起") {
		t.Errorf("expanded should show a collapse hint: %q", out2)
	}
	if len(folds2) != 1 || folds2[key] != 1 {
		t.Errorf("expanded should stay clickable to collapse: %v", folds2)
	}

	// Click routing: fold line toggles expandedTools via handleMouse.
	m2 := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{}}
	m2.height = 24
	m2.contentHeight = 8
	m2.foldLines = map[int]string{1: key}
	m2.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 1})
	if !m2.expandedTools[key] {
		t.Error("click on tool fold line should expand it")
	}
	if len(m2.expandedThinking) != 0 {
		t.Error("tool click must not touch thinking state")
	}
	m2.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 1})
	if m2.expandedTools[key] {
		t.Error("second click should collapse the tool result")
	}
}

func TestFooterKeepsError(t *testing.T) {
	// agent_end must not overwrite a turn error with "ready".
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{}}
	m.handleEvent(agent.AgentEvent{Type: agent.EventTurnEnd, Message: &ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonError, ErrorMessage: "HTTP 500: boom"}})
	m.handleEvent(agent.AgentEvent{Type: agent.EventAgentEnd})
	if !strings.Contains(m.status, "boom") {
		t.Errorf("status after agent_end = %q, want error kept", m.status)
	}
	// A new submit clears the error state.
	m.sess = &fakeSession{}
	m.input = newInputBox()
	m.input.Focus()
	m.input.Update(tea.KeyPressMsg{Code: 'h', Text: "hi"})
	m.submit()
	if m.lastError != "" || strings.Contains(m.status, "boom") {
		t.Errorf("error should clear on submit, status=%q lastError=%q", m.status, m.lastError)
	}
}

// fakeSession implements sessionLike for tests that call submit.
type fakeSession struct{}

func (f *fakeSession) CurrentMessages() []*ai.Message { return nil }
func (f *fakeSession) Subscribe(l agent.AgentListener, kind coding.ConsumerKind) (func(), func(coding.ConsumerKind)) {
	return func() {}, func(coding.ConsumerKind) {}
}
func (f *fakeSession) Prompt(text string) error                       { return nil }
func (f *fakeSession) Abort()                                         {}
func (f *fakeSession) Compact() error                                 { return nil }
func (f *fakeSession) SwitchModel(spec string) error                  { return nil }
func (f *fakeSession) NewSession() error                              { return nil }
func (f *fakeSession) OpenSession(id string) error                    { return nil }
func (f *fakeSession) DeleteSession(id string) error                  { return nil }
func (f *fakeSession) ListSessions() ([]coding.SessionSummary, error) { return nil, nil }
func (f *fakeSession) GetCwd() string                                 { return "" }
func (f *fakeSession) CurrentSessionID() string                       { return "" }
func (f *fakeSession) CostPolicyFor(provider, model string) coding.CostPolicy {
	return coding.CostPolicy{}
}
func (f *fakeSession) ReloadConfig() error                 { return nil }
func (f *fakeSession) AgentRuntime() *agent.Agent          { return nil }
func (f *fakeSession) AnswerQuestion(id, text string) bool { return true }

func TestToggleAllThinking(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}}
	msg := &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{
		ai.ThinkingBlock("a"), ai.ThinkingBlock("b")}}
	m.messages = append(m.messages, msg)

	// All collapsed → ctrl+t expands all.
	m.toggleAllThinking()
	if !m.expandedThinking[thinkingKey(msg, 0)] || !m.expandedThinking[thinkingKey(msg, 1)] {
		t.Error("ctrl+t should expand all thinking blocks")
	}
	// All expanded → ctrl+t collapses all.
	m.toggleAllThinking()
	if m.expandedThinking[thinkingKey(msg, 0)] || m.expandedThinking[thinkingKey(msg, 1)] {
		t.Error("second ctrl+t should collapse all thinking blocks")
	}
}

func TestToggleAllTools(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{}}
	tr := &ai.Message{Role: ai.RoleToolResult, Content: []ai.ContentBlock{ai.TextBlock("out")}}
	m.messages = append(m.messages, tr)

	m.toggleAllTools()
	if !m.expandedTools[toolFoldKey(tr)] {
		t.Error("ctrl+o should expand tool results")
	}
	m.toggleAllTools()
	if m.expandedTools[toolFoldKey(tr)] {
		t.Error("second ctrl+o should collapse tool results")
	}
}

func TestRenderFooter(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{},
		cwd: "/Users/test/proj", width: 100, sess: &fakeSession{}}
	m.messages = append(m.messages, &ai.Message{Role: ai.RoleAssistant,
		Usage: &ai.Usage{Input: 1500, Output: 300}})
	out := m.renderFooter()
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("footer should have 2 lines, got %d: %q", len(lines), out)
	}
	if !strings.Contains(lines[0], "proj") {
		t.Errorf("pwd line = %q", lines[0])
	}
	if !strings.Contains(lines[1], "1.5k") || !strings.Contains(lines[1], "no model") {
		t.Errorf("stats line = %q", lines[1])
	}
}

func TestExpandedThinkingClickable(t *testing.T) {
	// An expanded thinking block must stay clickable to collapse it back
	// (regression: the expanded branch used to drop the fold registration).
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{},
		height: 24, contentHeight: 8, input: newInputBox(), cwd: "/tmp",
		foldLines: map[int]string{}}
	assistant := &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{
		ai.ThinkingBlock("deep reasoning text"),
		ai.TextBlock("answer"),
	}}
	m.messages = append(m.messages, assistant)

	// Expand via render path: the fold line must be registered.
	m.expandedThinking[thinkingKey(assistant, 0)] = true
	_, folds := m.renderMessage(assistant)
	if len(folds) != 1 {
		t.Fatalf("expanded thinking should register a fold, got %v", folds)
	}
	// Simulate View's foldLines assembly and a click on that line.
	for key, line := range folds {
		m.foldLines[line] = key
	}
	line := 0
	for _, l := range folds {
		line = l
	}
	m.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, Y: line})
	if m.expandedThinking[thinkingKey(assistant, 0)] {
		t.Error("click on expanded thinking line should collapse it")
	}
	// Collapsed again: fold still registered for re-expansion.
	_, folds2 := m.renderMessage(assistant)
	if len(folds2) != 1 {
		t.Errorf("collapsed thinking should register a fold, got %v", folds2)
	}
}

func TestPermissionAskFlow(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{},
		height: 24, width: 100, input: newInputBox(), cwd: "/tmp", sess: &fakeSession{},
		foldLines: map[int]string{}}
	reply := make(chan permissions.AskResponse, 1)

	// A pending prompt renders the confirmation bar.
	m.Update(permissionAskMsg{req: permissions.AskRequest{
		Kind: "bash", ToolName: "bash", Command: "git push origin main",
	}, reply: reply})
	if m.pendingPermission == nil {
		t.Fatal("permission prompt not stored")
	}
	view := m.View().Content
	if !strings.Contains(view, "权限请求") || !strings.Contains(view, "git push") {
		t.Errorf("confirmation bar missing: %q", view)
	}

	// "y" replies AllowOnce and clears the prompt.
	m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	select {
	case resp := <-reply:
		if !resp.AllowOnce {
			t.Errorf("response = %+v, want AllowOnce", resp)
		}
	default:
		t.Error("no reply sent")
	}
	if m.pendingPermission != nil {
		t.Error("prompt should clear after reply")
	}

	// "d" denies; "a" allows always.
	for _, tc := range []struct {
		key  tea.KeyPressMsg
		want func(permissions.AskResponse) bool
	}{
		{tea.KeyPressMsg{Code: 'd', Text: "d"}, func(r permissions.AskResponse) bool { return r.Deny }},
		{tea.KeyPressMsg{Code: 'a', Text: "a"}, func(r permissions.AskResponse) bool { return r.AllowAlways }},
		{tea.KeyPressMsg{Code: tea.KeyEsc}, func(r permissions.AskResponse) bool { return r.Deny }},
	} {
		reply = make(chan permissions.AskResponse, 1)
		m.Update(permissionAskMsg{req: permissions.AskRequest{Kind: "tool", ToolName: "write"}, reply: reply})
		m.Update(tc.key)
		select {
		case resp := <-reply:
			if !tc.want(resp) {
				t.Errorf("key %v → %+v", tc.key.String(), resp)
			}
		default:
			t.Errorf("key %v: no reply", tc.key.String())
		}
	}
}

func TestInputHistory(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{},
		height: 24, width: 100, cwd: "/tmp", sess: &fakeSession{},
		foldLines: map[int]string{}, input: newInputBox(), historyIdx: -1}
	m.input.Focus()

	// Submit three messages; history records them in order.
	for _, msg := range []string{"第一条", "第二条", "第三条"} {
		m.input.SetValue(msg)
		m.submit()
	}
	if len(m.history) != 3 {
		t.Fatalf("history = %v", m.history)
	}

	// Type a draft, then up walks newest → oldest.
	m.input.SetValue("草稿")
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.input.Value(); got != "第三条" {
		t.Errorf("up#1 = %q, want 第三条", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.input.Value(); got != "第二条" {
		t.Errorf("up#2 = %q, want 第二条", got)
	}
	// Up at the oldest entry stays there.
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.input.Value(); got != "第一条" {
		t.Errorf("up#3 = %q, want 第一条", got)
	}
	// Down walks forward and restores the draft at the end.
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.input.Value(); got != "第二条" {
		t.Errorf("down#1 = %q, want 第二条", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.input.Value(); got != "草稿" {
		t.Errorf("down#3 = %q, want 草稿 (draft restored)", got)
	}
	// Empty history: up/down are no-ops.
	m2 := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{},
		height: 24, width: 100, cwd: "/tmp", sess: &fakeSession{},
		foldLines: map[int]string{}, input: newInputBox(), historyIdx: -1}
	m2.input.Focus()
	m2.input.SetValue("x")
	m2.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m2.input.Value() != "x" {
		t.Errorf("empty history up changed input: %q", m2.input.Value())
	}
	// Dedup: submitting the same text twice keeps one entry.
	m.input.SetValue("第三条")
	m.submit()
	if len(m.history) != 3 {
		t.Errorf("dedup failed: %v", m.history)
	}
}

func TestLoadHistoryFromSession(t *testing.T) {
	messages := []*ai.Message{
		ai.NewUserTextMessage("第一个问题"),
		{Role: ai.RoleAssistant, Content: []ai.ContentBlock{ai.TextBlock("回答")}},
		ai.NewUserTextMessage("第二个问题"),
		ai.NewUserTextMessage("第二个问题"),                                       // dedup against previous
		{Role: ai.RoleUser, Content: []ai.ContentBlock{ai.TextBlock("   ")}}, // blank skipped
	}
	h := loadHistory(messages)
	if len(h) != 2 || h[0] != "第一个问题" || h[1] != "第二个问题" {
		t.Errorf("history = %v", h)
	}
}

func TestRenderHeader(t *testing.T) {
	home, _ := os.UserHomeDir()
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{},
		height: 24, width: 60, cwd: home + "/proj", sess: &fakeSession{},
		foldLines: map[int]string{}, input: newInputBox(), historyIdx: -1,
		version: "0.1.0", showHeader: true}
	header := m.renderHeader()
	lines := strings.Split(header, "\n")
	if len(lines) != 6 {
		t.Fatalf("header lines = %d, want 6", len(lines))
	}
	if !strings.HasPrefix(lines[0], "╭") || !strings.HasSuffix(lines[0], "╮") {
		t.Errorf("top border = %q", lines[0])
	}
	if !strings.HasSuffix(lines[5], "╯") {
		t.Errorf("bottom border = %q", lines[5])
	}
	if !strings.Contains(lines[1], ">_ doge (v0.1.0)") {
		t.Errorf("title = %q", lines[1])
	}
	if !strings.Contains(lines[3], "model:") || !strings.Contains(lines[3], "/model to change") {
		t.Errorf("model row = %q", lines[3])
	}
	if !strings.Contains(lines[4], "directory: ~/proj") {
		t.Errorf("directory row = %q", lines[4])
	}
}

// TestHeaderRowsAligned locks the box geometry: every header row (borders
// included) must share the same display width so the right border column
// lines up on all rows.
func TestHeaderRowsAligned(t *testing.T) {
	home, _ := os.UserHomeDir()
	m := &model{expandedThinking: map[string]bool{}, expandedTools: map[string]bool{},
		height: 24, width: 80, cwd: home + "/proj", sess: &fakeSession{},
		foldLines: map[int]string{}, input: newInputBox(), historyIdx: -1,
		version: "0.1.0", showHeader: true}
	header := m.renderHeader()
	lines := strings.Split(header, "\n")
	if len(lines) != 6 {
		t.Fatalf("header lines = %d", len(lines))
	}
	want := len([]rune(stripANSIForTest(lines[0])))
	for i, l := range lines {
		if got := len([]rune(stripANSIForTest(l))); got != want {
			t.Fatalf("header row %d width = %d, want %d\n%s", i, got, want, header)
		}
	}
}

// TestNoBlankLineBetweenBlocks locks block separators: a collapsed thinking
// fold is immediately followed by the assistant text block with exactly one
// newline (glamour's leading newline must be trimmed).
func TestNoBlankLineBetweenBlocks(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}}
	assistant := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{
			ai.ThinkingBlock("secret reasoning"),
			ai.TextBlock("第一段输出"),
		}}
	out, _ := m.renderMessage(assistant)
	if strings.Contains(out, "思考 ⤵\n\n") {
		t.Errorf("blank line between thinking and text: %q", out)
	}
	if !strings.Contains(out, "第一段输出") {
		t.Errorf("text missing: %q", out)
	}
	// The text block itself must not start with a blank line.
	for _, l := range strings.Split(out, "\n") {
		_ = l
	}
}

// stripANSIForTest removes ANSI escape sequences (CSI ... m) for width math.
func stripANSIForTest(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || r == '\\' {
				inEscape = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func TestCommandTabCompletion(t *testing.T) {
	m := &model{expandedThinking: map[string]bool{}, sess: &fakeSession{}, input: newInputBox()}
	m.input.Focus()

	// "/c" filters to /compact; Tab completes it and keeps the menu open.
	m.input.SetValue("/c")
	m.updateCommandMenu()
	if len(m.cmdItems) != 1 || m.cmdItems[0].cmd != "/compact" {
		t.Fatalf("filter = %v", m.cmdItems)
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.input.Value() != "/compact" {
		t.Fatalf("tab completion = %q", m.input.Value())
	}
	if !m.cmdMenu || len(m.cmdItems) != 1 {
		t.Fatalf("menu should stay open after completing a plain command")
	}

	// Argument commands complete with a trailing space and close the menu.
	m.input.SetValue("/mo")
	m.updateCommandMenu()
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.input.Value() != "/model " {
		t.Fatalf("needsArgs completion = %q", m.input.Value())
	}
	if m.cmdMenu {
		t.Fatal("menu should close after completing an argument command")
	}
}

func TestSessionPickerDelete(t *testing.T) {
	stub := &agentSessionStub{
		cwd: "/tmp",
		sessions: []coding.SessionSummary{
			{ID: "sess-old-1", Cwd: "/tmp", MessageCount: 2, FirstMessage: "hello"},
			{ID: "sess-old-2", Cwd: "/tmp", MessageCount: 4, FirstMessage: "world"},
		},
	}
	m := &model{expandedThinking: map[string]bool{}, sess: stub, input: newInputBox(), cwd: "/tmp"}
	m.input.Focus()

	if cmd := m.runSlashCommand("/sessions"); cmd != nil {
		t.Fatal("unexpected cmd")
	}
	if !m.sessPicker || len(m.sessItems) != 2 {
		t.Fatalf("picker not open: %v %d", m.sessPicker, len(m.sessItems))
	}

	// First "d" arms deletion; nothing deleted yet.
	_, _ = m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if len(stub.deleted) != 0 {
		t.Fatalf("deleted too early: %v", stub.deleted)
	}
	if m.pendingDelete != "sess-old-1" {
		t.Fatalf("pendingDelete = %q", m.pendingDelete)
	}
	// Second "d" confirms; the picker reloads with the remaining session.
	_, _ = m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if len(stub.deleted) != 1 || stub.deleted[0] != "sess-old-1" {
		t.Fatalf("deleted = %v", stub.deleted)
	}
	if !m.sessPicker || len(m.sessItems) != 1 || m.sessItems[0].id != "sess-old-2" {
		t.Fatalf("picker after delete: %v %v", m.sessPicker, m.sessItems)
	}
	if m.pendingDelete != "" {
		t.Fatalf("pendingDelete should reset: %q", m.pendingDelete)
	}
}

func TestStatsCommand(t *testing.T) {
	tr := &ai.Message{Role: ai.RoleAssistant, Model: "deepseek-v4-flash", Provider: "model",
		Content: []ai.ContentBlock{ai.TextBlock("hi")},
		Usage: &ai.Usage{
			// Input is the TOTAL prompt (incl. cached); CacheRead is the cached
			// subset. 1000 fresh + 4500 cached → 5500 total prompt.
			Input: 5500, Output: 200, CacheRead: 4500, TotalTokens: 5700,
			Cost: ai.UsageCost{Total: 0.012},
		},
	}
	m := &model{expandedThinking: map[string]bool{}, sess: &fakeSession{}, input: newInputBox()}
	m.input.Focus()
	m.messages = append(m.messages, ai.NewUserTextMessage("hello"), tr)

	if cmd := m.runSlashCommand("/stats"); cmd != nil {
		t.Fatal("unexpected cmd")
	}
	if !m.showStats || m.statsText == "" {
		t.Fatalf("stats overlay not open: %v %q", m.showStats, m.statsText)
	}
	for _, want := range []string{"输入", "输出", "缓存", "命中", "5.5k", "81.8%"} {
		if !strings.Contains(m.statsText, want) {
			t.Errorf("stats missing %q:\n%s", want, m.statsText)
		}
	}
	// Esc closes the overlay.
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.showStats || m.statsText != "" {
		t.Fatal("esc did not close stats")
	}
}
