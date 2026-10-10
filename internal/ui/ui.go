// Package ui implements the interactive bubbletea-based mode.
package ui

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	glamour "charm.land/glamour/v2"
	"golang.org/x/text/width"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/permissions"
	"doge/internal/mcp"
	"doge/internal/mesh"
)

// cmdMenuItem is one entry of the slash-command menu.
type cmdMenuItem struct {
	cmd  string
	desc string
	// needsArgs commands are completed into the input for editing instead
	// of executed directly.
	needsArgs bool
}

// sessItem is one entry of the /sessions picker.
type sessItem struct {
	id    string
	label string
}

var cmdMenuItems = []cmdMenuItem{
	{cmd: "/quit", desc: "退出"},
	{cmd: "/exit", desc: "退出"},
	{cmd: "/model", desc: "切换模型 provider/model[:级别]", needsArgs: true},
	{cmd: "/compact", desc: "压缩上下文"},
	{cmd: "/new", desc: "开始全新会话"},
	{cmd: "/sessions", desc: "切换同目录历史会话"},
	{cmd: "/open", desc: "打开指定会话 <id>", needsArgs: true},
	{cmd: "/stats", desc: "会话 token 统计"},
	{cmd: "/reload", desc: "重新加载 models/auth/permission 配置"},
	{cmd: "/help", desc: "命令帮助"},
}

// sessionMsg carries agent events into the tea event loop.
type sessionMsg struct {
	ev agent.AgentEvent
}

// statusMsg reports an async error to the UI.
type statusMsg struct {
	err error
}

// tickMsg drives the thinking-in-progress timer refresh.
type tickMsg struct{}

// permissionAskMsg asks the user to allow/deny a tool call. The reply
// channel is written once by the key handler.
type permissionAskMsg struct {
	req   permissions.AskRequest
	reply chan permissions.AskResponse
}

// permissionTimeoutMsg clears a stale permission prompt whose ask handler
// timed out.
type permissionTimeoutMsg struct {
	reply chan permissions.AskResponse
}

// meshQuestionMsg surfaces a remote-task question (a mesh slave asking the
// master for a decision). The answer is written out-of-band via
// AgentSession.AnswerQuestion, never through the agent loop.
type meshQuestionMsg struct {
	question agent.Question
}

// permissionAskHandler implements permissions.AskHandler on the tea loop.
type permissionAskHandler struct {
	program *tea.Program
}

func (h *permissionAskHandler) Ask(req permissions.AskRequest) (permissions.AskResponse, error) {
	reply := make(chan permissions.AskResponse, 1)
	h.program.Send(permissionAskMsg{req: req, reply: reply})
	select {
	case resp := <-reply:
		return resp, nil
	case <-time.After(10 * time.Minute):
		h.program.Send(permissionTimeoutMsg{reply: reply})
		return permissions.AskResponse{}, errors.New("permission prompt timed out")
	}
}

// sessionLike is the subset of AgentSession the UI needs (testable).
type sessionLike interface {
	CurrentMessages() []*ai.Message
	Subscribe(l agent.AgentListener, kind coding.ConsumerKind) (func(), func(coding.ConsumerKind))
	Prompt(text string) error
	Abort()
	Compact() error
	SwitchModel(spec string) error
	NewSession() error
	OpenSession(id string) error // switch to a same-cwd historical session
	DeleteSession(id string) error
	ListSessions() ([]coding.SessionSummary, error)
	GetCwd() string
	CurrentSessionID() string
	AgentRuntime() *agent.Agent
	CostPolicyFor(provider, model string) coding.CostPolicy
	ReloadConfig() error
	// AnswerQuestion writes a human's answer to a pending mesh question
	// out-of-band (never through the agent loop).
	AnswerQuestion(id, text string) bool
}

// meshView is the subset of the mesh state the TUI needs (the /mesh command):
// this node's identity, connected peers, and in-flight remote tasks. The mesh
// build's adapter satisfies it structurally, so the UI never depends on libp2p.
type meshView interface {
	Self() mesh.AgentProfile
	Peers() []mesh.PeerInfo
	Tasks() []mesh.TaskStatus
}

// mcpView is the subset of the MCP manager the TUI needs (the /mcp command):
// server status + tools, reconnect, enable/disable and the setup scaffold.
// The internal/mcp Manager satisfies it structurally.
type mcpView interface {
	Enabled() bool
	Servers() []mcp.ServerInfo
	Reconnect(name string) error
	SetEnabled(name string, enabled bool) error
	Setup() error
}

// model is the root bubbletea model: message stream + input + footer.
type model struct {
	sess    sessionLike
	mesh    meshView
	mcp     mcpView
	program *tea.Program

	mu          sync.Mutex
	messages    []*ai.Message     // completed messages
	streaming   *ai.Message       // partial assistant message
	toolOutputs map[string]string // toolCallID → accumulated output

	input    *inputBox
	status   string
	width    int
	height   int
	quitting bool

	// Thinking display state: collapsed by default; while the model thinks,
	// show a live timer.
	thinkingActive bool
	thinkingStart  time.Time

	// scrollOffset is the number of lines scrolled up from the bottom of the
	// message area (0 = follow the latest output).
	scrollOffset int

	// Command menu (slash commands, codex style).
	cmdMenu   bool
	cmdItems  []cmdMenuItem
	cmdCursor int

	// Session picker (/sessions): select a same-cwd historical session.
	sessPicker bool
	sessItems  []sessItem
	sessCursor int
	// showRemoteSessions reveals the slave's remote-task sessions in the
	// picker (hidden by default; press r to toggle).
	showRemoteSessions bool
	// pendingDelete is the session id awaiting a second "d" press to confirm
	// deletion in the picker.
	pendingDelete string

	// showStats renders the /stats report as an overlay above the footer.
	showStats bool
	statsText string

	// Render-time geometry for precise mouse hit-testing.
	contentHeight int
	// foldLines maps a content line in the message view to the key of the
	// collapsed entry on that line (thinking blocks and tool results), for
	// mouse hit-testing. Keys are "tool:…" for tool results, otherwise
	// thinking-block keys.
	foldLines map[int]string

	// msgCache memoizes the rendered text and fold map of each committed
	// message. Streaming chunks and spinner ticks then rejoin cached lines
	// instead of re-rendering the whole transcript every frame; it is cleared
	// whenever expansion state changes (thinking/tool toggles).
	msgCache map[*ai.Message]msgRender

	// bodyLines holds the rendered lines of committed messages (leading
	// block-separator blanks already trimmed), maintained incrementally so a
	// frame only splits the newly committed message instead of the whole
	// transcript. bodyValid is false until the first build and whenever
	// expansion state changes force a rebuild.
	bodyLines []string
	bodyFolds map[int]string // absolute body line → fold key
	bodyValid bool

	// streamCache memoizes the throttled render of the in-progress assistant
	// message (see renderStreaming); the streaming text is cached whole, the
	// committed form goes through msgCache.
	streamCache streamingCache

	// expandedThinking tracks per-thinking-block expansion state;
	// expandedTools tracks per-tool-result expansion state.
	expandedThinking map[string]bool
	expandedTools    map[string]bool

	// lastError keeps the most recent turn error so the footer can show it
	// instead of being overwritten by the "ready" idle state.
	lastError string

	// cwd is the working directory shown in the footer's first line.
	cwd string
	// spinnerIdx drives the working spinner in the status area.
	spinnerIdx int

	// runActive tracks whether an agent run is in progress, derived from the
	// EventAgentStart/EventAgentEnd lifecycle events. It is authoritative for
	// "is something running" (the status string is only a display hint), so
	// the spinner and ESC-abort never fire on stale "✓ …" statuses.
	runActive bool

	// pendingPermission is a tool-call permission prompt awaiting a key
	// response; its reply channel is written by the key handler.
	pendingPermission *permissionAskMsg

	// pendingQuestion is a remote-task question awaiting a text answer in the
	// input box; Enter submits it out-of-band.
	pendingQuestion *meshQuestionMsg

	// Input history: submitted texts browsed with up/down.
	// history[0] is the oldest; historyIdx is the browse cursor (0 = most
	// recent, -1 = not browsing, showing the draft).
	history      []string
	historyIdx   int
	historyDraft string

	// version and showHeader control the codex-style startup banner.
	version    string
	showHeader bool
}

// msgRender is the memoized output of renderMessage for one committed
// message: its rendered text and the fold lines it contributes.
type msgRender struct {
	text  string
	folds map[string]int
}

// Run starts the interactive UI for a session and blocks until exit.
func Run(sess *coding.AgentSession, mesh meshView, mcp mcpView, version, initialMessage string) {
	m := newModel(sess, mesh, mcp, version)
	// v2 is declarative: alt screen, mouse mode, and keyboard enhancements
	// are requested per-frame from View(); no program options needed here.
	p := tea.NewProgram(m)
	m.program = p
	sess.SetPermissionAskHandler(&permissionAskHandler{program: p})

	if initialMessage != "" {
		m.input.SetValue(initialMessage)
		m.submit()
	}
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
	}
}

func newModel(sess sessionLike, mesh meshView, mcp mcpView, version string) *model {
	cwd, _ := os.Getwd()
	m := &model{
		sess:             sess,
		mesh:             mesh,
		mcp:              mcp,
		toolOutputs:      map[string]string{},
		input:            newInputBox(),
		expandedThinking: map[string]bool{},
		expandedTools:    map[string]bool{},
		foldLines:        map[int]string{},
		msgCache:         map[*ai.Message]msgRender{},
		cwd:              cwd,
		historyIdx:       -1,
		version:          version,
		showHeader:       true,
	}
	m.input.SetWidth(80)
	// Restore the transcript.
	m.messages = append(m.messages, sess.CurrentMessages()...)
	// Input history starts from the restored session's user messages, so
	// up/down browsing works right after --continue.
	m.history = loadHistory(m.messages)
	// Wire agent events into the tea loop. Remote-task questions are routed
	// through their own message so the input box can answer them out-of-band
	// (a plain sessionMsg would drop the pending-question state).
	sess.Subscribe(func(ev agent.AgentEvent) error {
		if ev.Type == agent.EventQuestion && ev.Question != nil {
			m.program.Send(meshQuestionMsg{question: *ev.Question})
			return nil
		}
		m.program.Send(sessionMsg{ev: ev})
		return nil
	}, coding.ConsumerInteractive)
	return m
}

func (m *model) Init() tea.Cmd {
	m.input.Focus()
	return nil
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(msg.Width - 2)
	case tea.KeyPressMsg:
		// A pending permission prompt intercepts keys before the editor.
		if m.pendingPermission != nil {
			switch msg.String() {
			case "y":
				m.pendingPermission.reply <- permissions.AskResponse{AllowOnce: true}
			case "a":
				m.pendingPermission.reply <- permissions.AskResponse{AllowAlways: true}
			case "d", "esc", "ctrl+c":
				m.pendingPermission.reply <- permissions.AskResponse{Deny: true}
			default:
				return m, nil
			}
			m.pendingPermission = nil
			return m, nil
		}
		// A pending remote-task question: Enter submits the answer out-of-band
		// (never a prompt), esc/ctrl+c cancels the prompt (the master bridge
		// times out and tells the slave). Other keys type into the input.
		if m.pendingQuestion != nil {
			switch msg.String() {
			case "enter":
				text := strings.TrimSpace(m.input.Value())
				m.input.Reset()
				if text != "" {
					m.sess.AnswerQuestion(m.pendingQuestion.question.ID, text)
					m.status = muted("已回答远端任务提问")
				}
				m.pendingQuestion = nil
				return m, nil
			case "esc", "ctrl+c":
				m.pendingQuestion = nil
				m.status = muted("已取消远端任务提问")
				return m, nil
			}
		}
		// Session picker: "d" deletes the highlighted session (press twice to
		// confirm); intercepted before the input so typing never leaks.
		if m.sessPicker && (msg.String() == "d" || msg.String() == "D") {
			m.pickDelete()
			return m, nil
		}
		// Session picker: "r" toggles the slave's remote-task sessions (hidden
		// by default so they never clutter the user's own session list).
		if m.sessPicker && (msg.String() == "r" || msg.String() == "R") {
			m.showRemoteSessions = !m.showRemoteSessions
			m.loadSessionPicker()
			return m, nil
		}
		switch msg.String() {
		case "ctrl+y":
			// Copy the latest assistant reply to the system clipboard via
			// OSC 52. Terminals that only support OSC 52 writes (most) work;
			// terminals without OSC 52 ignore the sequence (status notes it).
			if text := m.lastAssistantText(); text != "" {
				cmds = append(cmds, tea.SetClipboard(text))
				m.status = muted("已复制最后回复到剪贴板 (OSC 52)")
			} else {
				m.status = muted("还没有可复制的回复")
			}
			return m, tea.Batch(cmds...)
		case "ctrl+c":
			// ctrl+c clears the editor; on empty input it exits
			// (ctrl+d also exits on empty input). The status cue mirrors the
			// readline "^C" echo so the clear is visible.
			if m.input.Value() != "" {
				m.input.Reset()
				m.updateCommandMenu()
				m.status = muted("^C 输入已清空")
				return m, nil
			}
			return m, tea.Quit
		case "ctrl+d":
			if m.input.Value() == "" {
				return m, tea.Quit
			}
		case "ctrl+t":
			// Toggle thinking blocks globally.
			m.toggleAllThinking()
		case "ctrl+o":
			// Toggle tool output expansion globally.
			m.toggleAllTools()
		case "enter":
			if m.sessPicker {
				m.applySessionPicker()
				return m, nil
			}
			if m.cmdMenu && len(m.cmdItems) > 0 {
				selected := m.cmdItems[m.cmdCursor]
				m.cmdMenu = false
				if selected.needsArgs {
					// Complete into the input for editing.
					m.input.SetValue(selected.cmd + " ")
					return m, nil
				}
				// A triggered command clears the input, like submit() does for
				// manually typed commands.
				m.input.Reset()
				return m, m.runSlashCommand(selected.cmd)
			}
			if m.input.Value() == "" {
				return m, nil
			}
			return m, m.submit()
		case "tab":
			// Tab completes the highlighted command into the input without
			// running it: plain commands keep the menu open (Tab, then Enter);
			// argument commands append a space and close the menu.
			if m.cmdMenu && len(m.cmdItems) > 0 {
				selected := m.cmdItems[m.cmdCursor]
				if selected.needsArgs {
					m.input.SetValue(selected.cmd + " ")
					m.cmdMenu = false
				} else {
					m.input.SetValue(selected.cmd)
				}
				m.updateCommandMenu()
				return m, nil
			}
		case "up", "down":
			if m.sessPicker {
				m.pendingDelete = "" // moving on cancels the delete confirm
				if msg.String() == "up" {
					m.sessCursor--
				} else {
					m.sessCursor++
				}
				n := len(m.sessItems)
				if n > 0 {
					m.sessCursor = (m.sessCursor + n) % n
				}
				return m, nil
			}
			if m.cmdMenu {
				if msg.String() == "up" {
					m.cmdCursor--
				} else {
					m.cmdCursor++
				}
				n := len(m.cmdItems)
				if n > 0 {
					m.cmdCursor = (m.cmdCursor + n) % n
				}
				return m, nil
			}
			// Multi-line input: up/down move the cursor between lines instead
			// of browsing history (single-line input keeps history browsing).
			if m.input.Focused() && strings.Contains(m.input.Value(), "\n") {
				m.input.Update(msg)
				return m, nil
			}
			// Input history browsing: up walks older entries,
			// down walks back to the draft.
			if msg.String() == "up" {
				if m.historyIdx == -1 {
					m.historyDraft = m.input.Value()
				}
				if m.historyIdx < len(m.history)-1 {
					m.historyIdx++
					m.input.SetValue(m.history[len(m.history)-1-m.historyIdx])
				}
			} else {
				if m.historyIdx > 0 {
					m.historyIdx--
					m.input.SetValue(m.history[len(m.history)-1-m.historyIdx])
				} else if m.historyIdx == 0 {
					m.historyIdx = -1
					m.input.SetValue(m.historyDraft)
				}
			}
			return m, nil
		case "esc":
			// esc interrupts the current run (and closes the overlays).
			if m.sessPicker {
				m.sessPicker = false
				m.pendingDelete = ""
				return m, nil
			}
			if m.showStats {
				m.showStats = false
				m.statsText = ""
				return m, nil
			}
			if m.cmdMenu {
				m.cmdMenu = false
				return m, nil
			}
			// esc only interrupts an actual run; when idle it does nothing, so
			// it never claims "中断请求已发送" for a run that never started.
			if m.busy() {
				m.status = muted("中断请求已发送 (esc)")
				m.sess.Abort()
			}
		case "pgup":
			m.mu.Lock()
			m.scrollBy(m.height - 2)
			m.mu.Unlock()
		case "pgdn":
			m.mu.Lock()
			m.scrollBy(-(m.height - 2))
			m.mu.Unlock()
		default:
			if m.input.Focused() && m.input.Update(msg) {
				// handled by the input box
			}
			m.updateCommandMenu()
		}
	case tea.PasteMsg:
		// Bracketed paste: insert the pasted text into the input box. Like
		// keys, paste is ignored while a permission prompt is awaiting input.
		if m.pendingPermission != nil {
			return m, nil
		}
		if m.input.Focused() {
			m.input.Paste(msg.Content)
			m.updateCommandMenu()
		}
	case tea.MouseMsg:
		m.handleMouse(msg)
	case sessionMsg:
		m.handleEvent(msg.ev)
	case statusMsg:
		m.status = "✗ " + msg.err.Error()
	case tickMsg:
		// Spinner + thinking timer refresh; re-arm while busy.
	case permissionAskMsg:
		m.pendingPermission = &msg
	case permissionTimeoutMsg:
		if m.pendingPermission != nil && m.pendingPermission.reply == msg.reply {
			m.pendingPermission = nil
		}
	case meshQuestionMsg:
		// Focus the input so the answer can be typed immediately; if another
		// question is already pending, the new one waits (the bridge times out).
		if m.pendingQuestion == nil {
			m.pendingQuestion = &msg
		}
	}

	m.mu.Lock()
	busy := m.busy()
	m.mu.Unlock()
	if busy {
		m.spinnerIdx++
		cmds = append(cmds, tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} }))
	}
	return m, tea.Batch(cmds...)
}

// loadHistory extracts submitted user inputs from the session transcript
// (oldest first, deduped, capped).
func loadHistory(messages []*ai.Message) []string {
	var history []string
	for _, msg := range messages {
		if msg.Role != ai.RoleUser {
			continue
		}
		if t := strings.TrimSpace(msg.Text()); t != "" &&
			(len(history) == 0 || history[len(history)-1] != t) {
			history = append(history, t)
		}
	}
	if len(history) > 100 {
		history = history[len(history)-100:]
	}
	return history
}

// toggleAllThinking expands all thinking blocks when all are collapsed,
// and collapses them all otherwise.
func (m *model) toggleAllThinking() {
	m.mu.Lock()
	defer m.mu.Unlock()
	anyCollapsed := false
	for _, msg := range m.messages {
		for bi, b := range msg.Content {
			if b.Type == ai.ContentTypeThinking && !m.expandedThinking[thinkingKey(msg, bi)] {
				anyCollapsed = true
			}
		}
	}
	if m.streaming != nil {
		for bi, b := range m.streaming.Content {
			if b.Type == ai.ContentTypeThinking && !m.expandedThinking[thinkingKey(m.streaming, bi)] {
				anyCollapsed = true
			}
		}
	}
	for _, msg := range m.messages {
		for bi, b := range msg.Content {
			if b.Type == ai.ContentTypeThinking {
				m.expandedThinking[thinkingKey(msg, bi)] = anyCollapsed
			}
		}
	}
	if m.streaming != nil {
		for bi, b := range m.streaming.Content {
			if b.Type == ai.ContentTypeThinking {
				m.expandedThinking[thinkingKey(m.streaming, bi)] = anyCollapsed
			}
		}
	}
	m.invalidateMsgCache()
}

// toggleAllTools expands all tool results when all are collapsed, and
// collapses them all otherwise.
func (m *model) toggleAllTools() {
	m.mu.Lock()
	defer m.mu.Unlock()
	anyCollapsed := false
	for _, msg := range m.messages {
		if msg.Role == ai.RoleToolResult && !m.expandedTools[toolFoldKey(msg)] {
			anyCollapsed = true
		}
	}
	for _, msg := range m.messages {
		if msg.Role == ai.RoleToolResult {
			m.expandedTools[toolFoldKey(msg)] = anyCollapsed
		}
	}
	m.invalidateMsgCache()
}

// updateCommandMenu opens/closes/filters the slash-command menu based on
// the current input value.
func (m *model) updateCommandMenu() {
	value := m.input.Value()
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") {
		var filtered []cmdMenuItem
		for _, item := range cmdMenuItems {
			if strings.HasPrefix(item.cmd, value) {
				filtered = append(filtered, item)
			}
		}
		m.cmdMenu = len(filtered) > 0
		m.cmdItems = filtered
		if m.cmdCursor >= len(filtered) {
			m.cmdCursor = 0
		}
		return
	}
	m.cmdMenu = false
	m.cmdItems = nil
}

// submit sends the current input to the agent, or handles slash commands.
func (m *model) submit() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	debugLog("submit: %q", text)
	if text == "" {
		return nil
	}
	m.input.Reset()
	// A new user message clears the previous turn's error state.
	m.lastError = ""
	// Record every submitted input (including /commands) in the up/down
	// history, deduped and capped.
	if len(m.history) == 0 || m.history[len(m.history)-1] != text {
		m.history = append(m.history, text)
		if len(m.history) > 100 {
			m.history = m.history[len(m.history)-100:]
		}
	}
	m.historyIdx = -1
	m.historyDraft = ""
	if strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "//") {
		return m.runSlashCommand(text)
	}
	// 用户消息由 agent 事件流渲染（message_start/end），这里不手动追加，
	// 否则会与事件渲染重复。
	m.status = "…"
	// The agent loop blocks until the run finishes (LLM calls, tools), so it
	// must never run on the tea event loop; dispatch it in a goroutine and
	// deliver results back via the program.
	go func() {
		if err := m.sess.Prompt(text); err != nil && m.program != nil {
			m.program.Send(statusMsg{err: err})
		}
	}()
	return nil
}

// runSlashCommand handles built-in commands typed in the input. It returns
// a tea.Cmd; quitting returns tea.Quit directly instead of Send-ing a
// message (Program.Send from inside Update deadlocks: the message channel
// is unbuffered and the event loop is busy running this Update).
func (m *model) runSlashCommand(line string) tea.Cmd {
	name := line
	args := ""
	if idx := strings.IndexAny(line, " \t"); idx >= 0 {
		name = line[:idx]
		args = strings.TrimSpace(line[idx:])
	}
	switch name {
	case "/quit", "/exit":
		return tea.Quit
	case "/help":
		m.status = "命令: /quit /exit /model <spec> /compact /new /sessions /open <id> /stats /reload /help；/ 菜单内 Tab 补全；/sessions 内 d 删除"
	case "/reload":
		if err := m.sess.ReloadConfig(); err != nil {
			m.status = "✗ 重载配置失败: " + err.Error()
			return nil
		}
		m.status = "✓ 已重载 models.json / auth.json / permission.json"
	case "/model":
		if args == "" {
			m.status = "用法: /model <provider/model[:thinking]>"
			return nil
		}
		if err := m.sess.SwitchModel(args); err != nil {
			m.status = "✗ " + err.Error()
			return nil
		}
		m.status = "✓ 已切换模型"
	case "/compact":
		m.status = "压缩中…"
		go func() {
			if err := m.sess.Compact(); err != nil && m.program != nil {
				m.program.Send(statusMsg{err: err})
			}
			m.program.Send(sessionMsg{ev: agent.AgentEvent{Type: agent.EventAgentEnd}})
		}()
	case "/new":
		if err := m.sess.NewSession(); err != nil {
			m.status = "✗ " + err.Error()
			return nil
		}
		m.afterSessionChange()
		m.status = "✓ 已开始新会话（历史在 ~/.doge/agent/sessions/…）"
	case "/open":
		if args == "" {
			m.status = "用法: /open <session-id>（可用 /sessions 选择）"
			return nil
		}
		if err := m.sess.OpenSession(args); err != nil {
			m.status = "✗ " + err.Error()
			return nil
		}
		m.afterSessionChange()
		m.status = "✓ 已切换到会话 #" + shortID(m.sess.CurrentSessionID())
	case "/sessions":
		if !m.loadSessionPicker() {
			m.status = "当前目录没有可切换的历史会话"
		}
	case "/mesh":
		m.showMesh()
	case "/mcp":
		m.showMcp(args)
	case "/stats":
		st := coding.ComputeSessionStats(m.messages, m.sess.CostPolicyFor)
		st.SessionID = m.sess.CurrentSessionID()
		m.showStats = true
		m.statsText = renderStats(st)
	default:
		m.status = "未知命令: " + name + "（/help 查看）"
	}
	return nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// truncateRunesUI shortens s to at most n runes with an ellipsis.
func truncateRunesUI(s string, n int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return strings.TrimSpace(string(runes[:n])) + "…"
}

// renderStats renders the /stats overlay report.
func renderStats(st coding.SessionStats) string {
	var b strings.Builder
	b.WriteString("📊 会话统计")
	if st.SessionID != "" {
		b.WriteString("  #" + shortID(st.SessionID))
	}
	b.WriteString("\n")
	kv := func(k, v string) { b.WriteString(dimText(k+": ") + v + "\n") }
	if st.Model != "" {
		kv("模型", st.Model)
	}
	kv("消息", fmt.Sprintf("%d（用户 %d / 助手 %d / 工具 %d）",
		st.Messages, st.UserMessages, st.AssistantMessages, st.ToolMessages))
	kv("输入", formatTokens(st.InputTokens)+"   输出 "+formatTokens(st.OutputTokens))
	if st.ReasoningTokens > 0 {
		kv("推理", formatTokens(st.ReasoningTokens))
	}
	kv("缓存", fmt.Sprintf("命中 %s (%.1f%%)   写入 %s",
		formatTokens(st.CacheRead), st.CacheHitPct, formatTokens(st.CacheWrite)))
	kv("合计", formatTokens(st.TotalTokens)+" tokens")
	cur := st.Currency
	if cur == "" {
		cur = "$"
	}
	cost := fmt.Sprintf("%s%.4f", cur, st.Cost)
	if st.Cost == 0 {
		cost += "（models.json 未配置单价）"
	}
	kv("花费", cost)
	return strings.TrimSuffix(b.String(), "\n")
}

// afterSessionChange re-seeds the rendered transcript and cwd after /new or
// /open, so the conversation window matches the freshly activated session.
func (m *model) afterSessionChange() {
	m.resetConversation()
	m.messages = append(m.messages, m.sess.CurrentMessages()...)
	m.history = loadHistory(m.messages)
	m.cwd = m.sess.GetCwd()
}

// applySessionPicker selects the picked session (/sessions + Enter).
func (m *model) applySessionPicker() {
	m.sessPicker = false
	m.pendingDelete = ""
	if m.sessCursor < 0 || m.sessCursor >= len(m.sessItems) {
		return
	}
	selected := m.sessItems[m.sessCursor]
	if err := m.sess.OpenSession(selected.id); err != nil {
		m.status = "✗ " + err.Error()
		return
	}
	m.afterSessionChange()
	m.status = "✓ 已切换到会话 #" + shortID(m.sess.CurrentSessionID())
}

// loadSessionPicker (re)populates the /sessions picker with same-cwd
// sessions; it reports whether any session was found. Remote-task sessions are
// hidden by default (press r to reveal) so they never clutter the user's own
// sessions.
func (m *model) loadSessionPicker() bool {
	summaries, err := m.sess.ListSessions()
	if err != nil {
		m.status = "✗ " + err.Error()
		return false
	}
	picker := []sessItem{}
	remoteHidden := 0
	for _, s := range summaries {
		if s.Cwd != m.sess.GetCwd() {
			continue // TUI switches only within the current directory
		}
		if s.Kind == coding.SessionKindRemote {
			if !m.showRemoteSessions {
				remoteHidden++
				continue
			}
		}
		preview := truncateRunesUI(s.FirstMessage, 24)
		label := fmt.Sprintf("#%s  %d 条  %s", shortID(s.ID), s.MessageCount, s.Timestamp.Format("01-02 15:04"))
		if s.Kind == coding.SessionKindRemote {
			label = "[远端] " + label
		}
		if preview != "" {
			label += "  " + preview
		}
		picker = append(picker, sessItem{id: s.ID, label: label})
	}
	if len(picker) == 0 {
		if remoteHidden > 0 {
			// Only remote sessions here: keep the picker open so `r` can
			// reveal them, and tell the user.
			m.sessItems = picker
			m.sessPicker = true
			m.status = fmt.Sprintf("远端任务会话 %d 个已隐藏（按 r 显示）", remoteHidden)
			return true
		}
		m.sessPicker = false
		return false
	}
	m.cmdMenu = false
	m.sessItems = picker
	if m.sessCursor >= len(picker) {
		m.sessCursor = len(picker) - 1
	}
	m.sessPicker = true
	if remoteHidden > 0 {
		m.status = fmt.Sprintf("远端任务会话 %d 个已隐藏（按 r 显示）", remoteHidden)
	}
	return true
}

// showMesh renders the mesh network state (this node, connected peers,
// in-flight remote tasks) into the status area for the /mesh command.
func (m *model) showMesh() {
	if m.mesh == nil {
		m.status = "mesh 未启用（需 -tags mesh 构建并 --mesh 启动）"
		return
	}
	var b strings.Builder
	self := m.mesh.Self()
	fmt.Fprintf(&b, "本节点 %s  %s  roles=[%s]  accepting=%v  load=%d/%d\n",
		self.PeerID, dashOr(self.Name), strings.Join(self.Roles, ","), self.Accepting, self.Load, self.MaxLoad)
	peers := m.mesh.Peers()
	if len(peers) == 0 {
		b.WriteString("无已连接节点\n")
	} else {
		fmt.Fprintf(&b, "节点 %d 个:\n", len(peers))
		for _, p := range peers {
			fmt.Fprintf(&b, "  %s  %s  roles=[%s]  accepting=%v  load=%d/%d\n",
				p.ID, dashOr(p.Profile.Name), strings.Join(p.Profile.Roles, ","),
				p.Profile.Accepting, p.Profile.Load, p.Profile.MaxLoad)
		}
	}
	tasks := m.mesh.Tasks()
	if len(tasks) == 0 {
		b.WriteString("无在途远端任务\n")
	} else {
		fmt.Fprintf(&b, "在途远端任务 %d 个:\n", len(tasks))
		for _, t := range tasks {
			fmt.Fprintf(&b, "  %s  %-10s  peer=%s  %s\n", t.TaskID, t.Status, t.Peer, t.Progress)
		}
	}
	m.status = strings.TrimSuffix(b.String(), "\n")
}

func dashOr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// showMcp renders the MCP state / executes the /mcp subcommands.
//   - /mcp, /mcp status       面板：server 状态、工具数、缓存/错误
//   - /mcp tools [server]     列出工具（可过滤 server）
//   - /mcp reconnect [server] 立即重连（空=全部）
//   - /mcp enable|disable <server> 启用/禁用（写项目 .doge/mcp.json）
//   - /mcp setup              生成项目 .mcp.json 脚手架
func (m *model) showMcp(args string) {
	fields := strings.Fields(args)
	sub := ""
	if len(fields) > 0 {
		sub = fields[0]
	}
	rest := ""
	if len(fields) > 1 {
		rest = strings.Join(fields[1:], " ")
	}
	if m.mcp == nil {
		m.status = "MCP 未初始化（--no-mcp 或配置加载失败）"
		return
	}
	switch sub {
	case "", "status":
		m.showMcpPanel()
	case "tools":
		m.showMcpTools(rest)
	case "reconnect":
		if err := m.mcp.Reconnect(rest); err != nil {
			m.status = "✗ 重连失败: " + err.Error()
			return
		}
		if rest == "" {
			m.status = "✓ 已重连全部 MCP server"
		} else {
			m.status = "✓ 已重连 " + rest
		}
	case "enable", "disable":
		if rest == "" {
			m.status = "用法: /mcp " + sub + " <server>"
			return
		}
		if err := m.mcp.SetEnabled(rest, sub == "enable"); err != nil {
			m.status = "✗ " + err.Error()
			return
		}
		m.status = "✓ " + rest + " 已" + map[bool]string{true: "启用", false: "禁用"}[sub == "enable"]
	case "setup":
		if err := m.mcp.Setup(); err != nil {
			m.status = "✗ " + err.Error()
			return
		}
		m.status = "✓ 已生成 .mcp.json 脚手架并重新发现配置（改好 mcpServers 后 /mcp reconnect）"
	default:
		m.status = "用法: /mcp [status|tools [server]|reconnect [server]|enable <server>|disable <server>|setup]"
	}
}

// showMcpPanel 渲染 server 状态面板
func (m *model) showMcpPanel() {
	if !m.mcp.Enabled() {
		m.status = "未配置 MCP server（在 ~/.doge/agent/mcp.json、<cwd>/.doge/mcp.json 或 .mcp.json 里加 mcpServers；/mcp setup 生成脚手架）"
		return
	}
	var b strings.Builder
	servers := m.mcp.Servers()
	fmt.Fprintf(&b, "MCP 服务器 %d 个（/mcp tools 看工具，/mcp reconnect 重连）:\n", len(servers))
	for _, s := range servers {
		state := "● 已连接"
		if !s.Connected {
			state = "○ 未连接"
		}
		cache := ""
		if s.Cache {
			cache = " 缓存"
		}
		err := ""
		if s.Error != "" {
			err = " 上次错误: " + s.Error
		}
		fmt.Fprintf(&b, "  %-16s %-6s %s%s 工具=%d%s\n", s.Name, s.Transport, state, cache, len(s.Tools), err)
	}
	m.status = strings.TrimSuffix(b.String(), "\n")
}

// showMcpTools 渲染某（或全部）server 的工具列表
func (m *model) showMcpTools(server string) {
	var b strings.Builder
	servers := m.mcp.Servers()
	shown := 0
	for _, s := range servers {
		if server != "" && s.Name != server {
			continue
		}
		if len(s.Tools) == 0 {
			fmt.Fprintf(&b, "%s: 无工具元数据（/mcp reconnect %s 拉取）\n", s.Name, s.Name)
			shown++
			continue
		}
		fmt.Fprintf(&b, "%s (%d):\n", s.Name, len(s.Tools))
		for _, t := range s.Tools {
			desc := t.Description
			if len(desc) > 80 {
				desc = desc[:80] + "…"
			}
			fmt.Fprintf(&b, "  %s__%s  %s\n", s.Name, t.Name, desc)
		}
		shown++
	}
	if shown == 0 {
		m.status = "未找到 server " + server + "（/mcp status 查看列表）"
		return
	}
	m.status = strings.TrimSuffix(b.String(), "\n")
}

// pickDelete deletes the highlighted session in the picker: the first "d"
// arms the deletion, the second confirms it.
func (m *model) pickDelete() {
	if m.sessCursor < 0 || m.sessCursor >= len(m.sessItems) {
		return
	}
	target := m.sessItems[m.sessCursor]
	if m.pendingDelete != target.id {
		m.pendingDelete = target.id
		m.status = muted("再按一次 d 确认删除会话 #" + shortID(target.id) + "（↑/↓ 取消）")
		return
	}
	m.pendingDelete = ""
	if err := m.sess.DeleteSession(target.id); err != nil {
		m.status = "✗ " + err.Error()
		return
	}
	m.status = "✓ 已删除会话 #" + shortID(target.id)
	if !m.loadSessionPicker() {
		m.status = "✓ 已删除会话；当前目录没有其他历史会话"
	}
}

// resetConversation clears the rendered transcript, caches, and expansions so
// a /new session starts as a blank conversation.
func (m *model) resetConversation() {
	m.messages = nil
	m.streaming = nil
	m.toolOutputs = map[string]string{}
	m.msgCache = map[*ai.Message]msgRender{}
	m.bodyLines = nil
	m.bodyFolds = map[int]string{}
	m.bodyValid = false
	m.streamCache = streamingCache{}
	m.expandedThinking = map[string]bool{}
	m.expandedTools = map[string]bool{}
	m.foldLines = map[int]string{}
	m.scrollOffset = 0
	m.lastError = ""
}

// handleMouse maps mouse events: wheel scrolls the message history, a left
// click in the message area toggles expanded thinking.
func (m *model) handleMouse(msg tea.MouseMsg) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scrollBy(3)
		case tea.MouseWheelDown:
			m.scrollBy(-3)
		}
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return
		}
		// Precise hit-test: only a click on a collapsed entry line toggles
		// that entry (thinking block or tool result). The math mirrors View
		// exactly (menu rows + clamping), so screen Y maps 1:1 to content
		// lines.
		if m.height <= 3 {
			return
		}
		overlayLines := 0
		switch {
		case m.cmdMenu && len(m.cmdItems) > 0:
			overlayLines = len(m.cmdItems) + 1
		case m.sessPicker && len(m.sessItems) > 0:
			overlayLines = len(m.sessItems) + 1
		case m.showStats && m.statsText != "":
			overlayLines = strings.Count(m.statsText, "\n") + 1
		}
		msgHeight := m.msgPaneHeight(overlayLines)
		start := m.contentHeight - msgHeight - m.scrollOffset
		if start < 0 {
			start = 0
		}
		mouse := msg.Mouse()
		contentLine := start + mouse.Y
		if key, ok := m.foldLines[contentLine]; ok {
			if strings.HasPrefix(key, toolFoldPrefix) {
				m.expandedTools[key] = !m.expandedTools[key]
			} else {
				m.expandedThinking[key] = !m.expandedThinking[key]
			}
			m.invalidateMsgCache()
		}
	}
}

// scrollBy adjusts the scroll offset by delta lines (positive = up/back).
func (m *model) scrollBy(delta int) {
	m.scrollOffset += delta
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
	// The upper bound is applied in View where the content height is known.
}

// debugLog writes to DOGE_UI_DEBUG when set.
func debugLog(format string, args ...any) {
	if path := os.Getenv("DOGE_UI_DEBUG"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, format+"\n", args...)
			f.Close()
		}
	}
}

// handleEvent applies an agent event to the UI state.
func (m *model) handleEvent(ev agent.AgentEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// New content arrives: follow the bottom again.
	m.scrollOffset = 0
	switch ev.Type {
	case agent.EventAgentStart:
		m.runActive = true
	case agent.EventMessageStart:
		if ev.Message != nil && ev.Message.Role == ai.RoleAssistant {
			m.streaming = ev.Message
		}
	case agent.EventMessageUpdate:
		if ev.Message != nil && ev.Message.Role == ai.RoleAssistant {
			m.streaming = ev.Message
		}
		// Track thinking lifecycle for the timer and collapse state.
		if ev.AssistantMessageEvent != nil {
			switch ev.AssistantMessageEvent.Type {
			case ai.EventThinkingStart:
				m.thinkingActive = true
				m.thinkingStart = time.Now()
			case ai.EventThinkingEnd:
				m.thinkingActive = false
			}
		}
	case agent.EventMessageEnd:
		if ev.Message == nil {
			return
		}
		m.streaming = nil
		m.thinkingActive = false
		switch ev.Message.Role {
		case ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult:
			m.messages = append(m.messages, ev.Message)
			m.appendBodyMessage(ev.Message)
		}
	case agent.EventToolExecutionStart:
		m.toolOutputs[ev.ToolCallID] = ""
		m.status = "→ " + ev.ToolName
	case agent.EventToolExecutionUpdate:
		if ev.PartialResult != nil {
			for _, b := range ev.PartialResult.(agent.AgentToolResult).Content {
				if b.Type == ai.ContentTypeText {
					m.toolOutputs[ev.ToolCallID] += b.Text
				}
			}
		}
	case agent.EventToolExecutionEnd:
		m.status = "✓ " + ev.ToolName
	case agent.EventTurnStart:
		m.status = "…"
	case agent.EventAgentEnd:
		m.runActive = false
		if m.lastError != "" {
			m.status = "✗ " + truncateLine(m.lastError, 80)
		} else {
			m.status = "ready"
		}
	case agent.EventTurnEnd:
		if ev.Message != nil && ev.Message.ErrorMessage != "" {
			m.lastError = ev.Message.ErrorMessage
			m.status = "✗ " + truncateLine(ev.Message.ErrorMessage, 80)
		}
	}
}

// View renders the full screen: message stream, then input, then footer.
// It also declares the terminal features we want per frame (alt screen,
// mouse cell motion, kitty keyboard disambiguation); bubbletea enables them
// when the terminal supports them and no-ops otherwise.
func (m *model) View() tea.View {
	m.mu.Lock()
	defer m.mu.Unlock()

	// The committed transcript is kept as an incremental line buffer so a
	// frame never re-splits the whole session; only the streaming tail and
	// the visible window are assembled per frame.
	if !m.bodyValid {
		m.buildBodyLines()
	}
	streamLines := []string{}
	streamFolds := map[string]int{}
	if m.streaming != nil {
		text, msgFolds := m.renderStreaming(m.streaming)
		streamLines = strings.Split(text, "\n")
		streamFolds = msgFolds
	}
	hdrLines := []string{}
	if m.showHeader {
		hdrLines = strings.Split(m.renderHeader(), "\n")
	}

	// Layout: overlay (command menu / session picker / stats) + messages + input + footer.
	overlayLines := 0
	var overlayView string
	switch {
	case m.cmdMenu && len(m.cmdItems) > 0:
		overlayLines = len(m.cmdItems) + 1
		overlayView = renderCommandMenu(m.cmdItems, m.cmdCursor)
	case m.sessPicker && len(m.sessItems) > 0:
		overlayLines = len(m.sessItems) + 1
		overlayView = renderSessionPicker(m.sessItems, m.sessCursor, m.pendingDelete)
	case m.showStats && m.statsText != "":
		overlayLines = strings.Count(m.statsText, "\n") + 1
		overlayView = m.statsText
	}
	msgHeight := m.msgPaneHeight(overlayLines)
	// Three-segment content: header / committed / streaming. The scroll
	// window is sliced straight from the segments, so per-frame work is
	// O(visible window), not O(transcript).
	nHdr, nCommit, nStream := len(hdrLines), len(m.bodyLines), len(streamLines)
	total := nHdr + nCommit + nStream
	m.contentHeight = total
	// Clamp the stored scroll offset so mouse hit-testing in handleMouse
	// uses the same window as the rendered view.
	if maxOffset := total - msgHeight; maxOffset < 0 {
		m.scrollOffset = 0
	} else if m.scrollOffset > maxOffset {
		m.scrollOffset = maxOffset
	}
	start := total - msgHeight - m.scrollOffset
	if start < 0 {
		start = 0
	}
	end := start + msgHeight
	if end > total {
		end = total
	}
	// Fold hit-testing: only entries inside the visible window are
	// registered (clicks can only land there).
	m.foldLines = map[int]string{}
	for line, key := range m.bodyFolds {
		if abs := nHdr + line; abs >= start && abs < end {
			m.foldLines[abs] = key
		}
	}
	for key, relLine := range streamFolds {
		if abs := nHdr + nCommit + relLine; abs >= start && abs < end {
			m.foldLines[abs] = key
		}
	}
	window := make([]string, 0, msgHeight)
	for i := start; i < end; i++ {
		switch {
		case i < nHdr:
			window = append(window, hdrLines[i])
		case i < nHdr+nCommit:
			window = append(window, m.bodyLines[i-nHdr])
		default:
			window = append(window, streamLines[i-nHdr-nCommit])
		}
	}
	msgView := strings.Join(window, "\n")
	out := msgView
	if out != "" {
		out += "\n"
	}
	if overlayView != "" {
		out += overlayView + "\n"
	}
	if status := m.renderStatus(); status != "" {
		out += status + "\n"
	}
	v := tea.NewView(out + m.input.View() + "\n" + m.renderFooter())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	// Keyboard: v2 always enables basic kitty-protocol disambiguation
	// (flags := 1 in the renderer), which fixes ambiguous esc/ctrl+arrow /
	// modifier reporting on supporting terminals and no-ops elsewhere. The
	// advanced `KeyboardEnhancements` flags are deliberately not requested:
	// ReportEventTypes adds key-release noise and ReportAllKeysAsEscapeCodes
	// turns plain-text keys into escape codes, which would fight the input
	// box's Text-based handling.
	return v
}

// renderHeader renders the codex-style startup banner. It is emitted as the
// first lines of the message stream (see View), so it shows at the top while
// the session is short and scrolls away as content fills the screen
// instead of staying pinned.
func (m *model) renderHeader() string {
	modelID := "no model"
	level := ""
	if rt := m.sess.AgentRuntime(); rt != nil {
		st := rt.State()
		if st.Model != nil {
			modelID = st.Model.ID
		}
		level = st.ThinkingLevel
	}
	modelLine := modelID
	if level != "" && level != "off" {
		modelLine += " " + level
	}
	modelHint := "  /model to change"

	dir := m.cwd
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(dir, home) {
		dir = "~" + strings.TrimPrefix(dir, home)
	}

	title := ">_ doge"
	if m.version != "" {
		title += " (v" + m.version + ")"
	}
	title += " by cheivin"

	// Natural (unstyled, unpadded) content rows; box width follows the
	// longest one so the border hugs the text like codex's banner.
	modelRow := "model:     " + modelLine
	dirRow := "directory: " + dir
	width := displayWidth(title)
	if w := displayWidth(modelRow) + displayWidth(modelHint); w > width {
		width = w
	}
	if w := displayWidth(dirRow); w > width {
		width = w
	}
	const minHeaderWidth, maxHeaderWidth = 48, 96
	if width < minHeaderWidth {
		width = minHeaderWidth
	}
	if width > maxHeaderWidth {
		width = maxHeaderWidth
	}
	if max := m.width - 4; width > max {
		width = max
	}
	if width < 20 {
		width = 20
	}

	// Rows are built as plain text for width math, then styled at the end
	// (ANSI escapes would otherwise pollute displayWidth). The content area
	// inside the box is `width - 2` columns so a row ("| " + content + " |")
	// is exactly width+2 cells — the same total as the horizontal borders.
	pad := func(s string) string {
		padCount := width - 2 - displayWidth(s)
		if padCount < 0 {
			padCount = 0
		}
		return "| " + s + strings.Repeat(" ", padCount) + " |"
	}

	// Model row with the hint right-aligned (plain text first).
	avail := width - 2 - displayWidth(modelRow) - displayWidth(modelHint)
	if avail < 1 {
		modelRow += " " + modelHint
	} else {
		modelRow += strings.Repeat(" ", avail) + modelHint
	}

	rows := []string{
		"\u256d" + strings.Repeat("\u2500", width) + "\u256e",
		pad(title),
		pad(""),
		pad(modelRow),
		pad(dirRow),
		"\u2570" + strings.Repeat("\u2500", width) + "\u256f",
	}
	// Style the hint dim (does not affect display width).
	rows[3] = strings.Replace(rows[3], modelHint, dimText(modelHint), 1)
	return strings.Join(rows, "\n")
}

// spinnerFrames are the working spinner frames.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// busy reports whether the agent is currently doing something that should show
// the working spinner (and allow ESC to abort). Completed states — a "✓ …"
// success status, a "✗ …" error, or an idle model — are never busy, so the
// spinner cannot get stuck after a slash command or a finished tool.
func (m *model) busy() bool {
	if m.runActive || m.streaming != nil || m.thinkingActive {
		return true
	}
	return strings.HasPrefix(m.status, "→") || strings.HasPrefix(m.status, "…") ||
		m.status == "压缩中…"
}

// msgPaneHeight returns the rows available for the message pane: terminal
// height minus the status line, the (possibly multi-line) input, and the
// two-line footer. Mirrored by handleMouse so mouse hit-testing maps 1:1.
func (m *model) msgPaneHeight(overlayLines int) int {
	inputLines := 1
	if m.input != nil {
		inputLines = strings.Count(m.input.View(), "\n") + 1
	}
	h := m.height - 4 - overlayLines - (inputLines - 1)
	if h < 1 {
		h = 1
	}
	return h
}

// renderStatus renders the status line between the messages and the input
// : a permission prompt when one is pending, a spinner
// while working, the error in red otherwise.
func (m *model) renderStatus() string {
	if m.pendingQuestion != nil {
		q := m.pendingQuestion.question
		return warn("❓ 远端任务提问: ") + errText(truncateLine(q.Prompt, 60)) + muted("  [输入回答后回车，esc 取消]")
	}
	if m.pendingPermission != nil {
		req := m.pendingPermission.req
		what := req.ToolName
		if req.Kind == "bash" {
			what = "bash `" + req.Command + "`"
		} else if req.Path != "" {
			what = req.ToolName + " " + req.Path
		}
		return warn("⚠ 权限请求: ") + errText(truncateLine(what, 60)) +
			muted("  [y]一次 [a]总是允许 [d]拒绝 [esc]取消")
	}
	if strings.HasPrefix(m.status, "✗") {
		return errText(m.status)
	}
	if m.busy() {
		spin := spinnerFrames[m.spinnerIdx%len(spinnerFrames)]
		text := m.status
		if text == "…" {
			text = "Working..."
		}
		return accent(spin) + " " + muted(text)
	}
	if m.status == "" || m.status == "ready" {
		return ""
	}
	return muted(m.status)
}

// formatTokens formats a token count compactly (1.5k, 12k, 2.3M).
func formatTokens(count int) string {
	switch {
	case count < 1000:
		return fmt.Sprintf("%d", count)
	case count < 10000:
		return fmt.Sprintf("%.1fk", float64(count)/1000)
	case count < 1000000:
		return fmt.Sprintf("%dk", count/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(count)/1000000)
	}
}

// renderFooter renders the two-line footer: pwd on the first line, token
// stats and the model on the second.
func (m *model) renderFooter() string {
	// Line 1: pwd with ~ for home.
	cwd := m.cwd
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(cwd, home) {
		cwd = "~" + strings.TrimPrefix(cwd, home)
	}
	pwdLine := dimText(truncateLine(cwd, m.width))

	// Line 2: token stats on the left, model + thinking level right-aligned.
	var input, output, cacheRead int
	for _, msg := range m.messages {
		if msg.Role == ai.RoleAssistant && msg.Usage != nil {
			input += msg.Usage.Input
			output += msg.Usage.Output
			cacheRead += msg.Usage.CacheRead
		}
	}
	var parts []string
	if input > 0 {
		parts = append(parts, "↑"+formatTokens(input))
	}
	if output > 0 {
		parts = append(parts, "↓"+formatTokens(output))
	}
	if cacheRead > 0 {
		parts = append(parts, "R"+formatTokens(cacheRead))
	}
	stats := strings.Join(parts, " ")

	right := modelName(m.sess)
	if rt := m.sess.AgentRuntime(); rt != nil {
		if level := rt.State().ThinkingLevel; level != "" && level != "off" {
			right = right + " • " + level
		}
	}
	line := stats
	// Narrow terminals: drop the right-aligned model so the stats don't get
	// squeezed out (graceful degradation, matching picoclaw's cliui).
	if right != "" && m.width >= footerMinWidth {
		pad := m.width - displayWidth(stats) - displayWidth(right)
		if pad < 2 {
			pad = 2
		}
		line += strings.Repeat(" ", pad) + right
	}
	return pwdLine + "\n" + dimText(truncateLine(line, m.width))
}

// footerMinWidth is the minimum terminal width for the two-column footer
// (stats + model). Below it, the model/thinking level is omitted.
const footerMinWidth = 64

// displayWidth returns the terminal display width of a string (CJK = 2).
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// thinkingElapsed returns the seconds since thinking started.
func (m *model) thinkingElapsed() int {
	if m.thinkingStart.IsZero() {
		return 0
	}
	return int(time.Since(m.thinkingStart).Seconds())
}

// renderThinking renders a thinking block: a one-line collapsed entry
// (with the elapsed time while thinking), expanded to full content when the
// specific block is clicked. Returns the rendered text and the content line
// of the collapsed entry (for mouse hit-testing), or -1.
func (m *model) renderThinking(b ai.ContentBlock, startLine int, key string) (string, int) {
	if m.thinkingActive {
		return fmt.Sprintf("\n%s %s", muted("· 思考中"), fmt.Sprintf("%ds", m.thinkingElapsed())), -1
	}
	if m.expandedThinking[key] {
		content := b.Thinking
		if len(content) > 2000 {
			content = truncateRunes(content, 2000)
		}
		content = wrapLine(content, 80)
		// Thinking renders as italic, muted text. The first content
		// line stays registered as a fold target so a click collapses the
		// block again.
		return "\n" + italic(thinkingColor("· "+content)), startLine + 1
	}
	return "\n" + muted("· 思考 ⤵"), startLine + 1
}

// buildBodyLines (re)builds the committed-message line buffer from scratch:
// all messages rendered via the cache, joined, split, and leading blank
// block separators trimmed, with fold positions shifted accordingly. Used on
// the first frame and whenever expansion state changes.
func (m *model) buildBodyLines() {
	var body strings.Builder
	folds := map[string]int{}
	bodyLine := 0
	for _, msg := range m.messages {
		text, msgFolds := m.renderMsgCached(msg)
		for key, relLine := range msgFolds {
			folds[key] = bodyLine + relLine
		}
		bodyLine += strings.Count(text, "\n")
		body.WriteString(text)
	}
	lines := strings.Split(body.String(), "\n")
	trim := 0
	for trim < len(lines) && lines[trim] == "" {
		trim++
	}
	lines = lines[trim:]
	m.bodyLines = lines
	m.bodyFolds = make(map[int]string, len(folds))
	for key, line := range folds {
		m.bodyFolds[line-trim] = key
	}
	m.bodyValid = true
}

// appendBodyMessage appends one newly committed message's rendered lines to
// the incremental body buffer (called from handleEvent under the model
// lock). No-op while the buffer is invalid — the next View rebuilds all.
func (m *model) appendBodyMessage(msg *ai.Message) {
	if !m.bodyValid {
		return
	}
	text, msgFolds := m.renderMsgCached(msg)
	if text == "" {
		return
	}
	start := len(m.bodyLines)
	m.bodyLines = append(m.bodyLines, strings.Split(text, "\n")...)
	if len(msgFolds) > 0 {
		for key, relLine := range msgFolds {
			m.bodyFolds[start+relLine] = key
		}
	}
}

// renderMsgCached returns the memoized render of a committed message,
// computing and caching it on first use. See msgCache.
func (m *model) renderMsgCached(msg *ai.Message) (string, map[string]int) {
	if m.msgCache == nil {
		m.msgCache = map[*ai.Message]msgRender{}
	}
	if c, ok := m.msgCache[msg]; ok {
		return c.text, c.folds
	}
	text, folds := m.renderMessage(msg)
	m.msgCache[msg] = msgRender{text: text, folds: folds}
	return text, folds
}

// invalidateMsgCache drops all memoized message renders; call it whenever
// expansion state changes (thinking/tool toggles) so the next frame
// re-renders affected messages. The incremental body-line buffer is rebuilt
// on the next View as well.
func (m *model) invalidateMsgCache() {
	m.msgCache = map[*ai.Message]msgRender{}
	m.bodyValid = false
}

// lastAssistantText returns the plain text of the most recent assistant
// message (used by ctrl+y to copy the reply to the clipboard), or "" when
// there is no assistant reply yet.
func (m *model) lastAssistantText() string {
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if msg.Role != ai.RoleAssistant {
			continue
		}
		var b strings.Builder
		for _, c := range msg.Content {
			if c.Type == ai.ContentTypeText && c.Text != "" {
				b.WriteString(c.Text)
				b.WriteString("\n")
			}
		}
		return strings.TrimSuffix(b.String(), "\n")
	}
	return ""
}

// renderMessage renders one completed message. Returns the text and a map
// from each collapsed thinking entry's key to its content line (relative to
// this message); the map is nil when there are no folded entries.
func (m *model) renderMessage(msg *ai.Message) (string, map[string]int) {
	switch msg.Role {
	case ai.RoleUser:
		// User messages render as a highlighted box. The leading newline
		// mirrors the other block types so the box starts on its own line
		// instead of being glued to the previous assistant/tool message.
		lines := strings.Split(wrapLine(msg.Text(), 80), "\n")
		for i, l := range lines {
			lines[i] = userMsgBg(l)
		}
		return "\n" + strings.Join(lines, "\n"), nil
	case ai.RoleAssistant:
		// Failed turns render their error so failures are never silent.
		if msg.StopReason == ai.StopReasonError && msg.ErrorMessage != "" {
			return "\n" + errText("Error: "+truncateLine(msg.ErrorMessage, 300)), nil
		}
		var out strings.Builder
		folds := map[string]int{}
		for bi, b := range msg.Content {
			switch b.Type {
			case ai.ContentTypeText:
				if b.Text != "" {
					out.WriteString("\n" + renderMarkdown(b.Text))
				}
			case ai.ContentTypeThinking:
				// Merge consecutive thinking blocks into one entry (some
				// gateways split reasoning into many blocks): collect the
				// run, then render a single fold with the combined text.
				if b.Thinking != "" || m.expandedThinking[thinkingKey(msg, bi)] {
					end := bi
					var combined strings.Builder
					anyExpanded := false
					for ; end < len(msg.Content) && msg.Content[end].Type == ai.ContentTypeThinking; end++ {
						if msg.Content[end].Thinking != "" {
							combined.WriteString(msg.Content[end].Thinking)
						}
						if m.expandedThinking[thinkingKey(msg, end)] {
							anyExpanded = true
						}
					}
					merged := ai.ContentBlock{Type: ai.ContentTypeThinking, Thinking: combined.String()}
					line := strings.Count(out.String(), "\n")
					key := thinkingKey(msg, bi)
					if anyExpanded {
						merged.Thinking = wrapLine(truncateRunes(merged.Thinking, 2000), 80)
						out.WriteString("\n" + italic(thinkingColor("· "+merged.Thinking)))
						// Keep the first line clickable to collapse again.
						folds[key] = line + 1
					} else {
						text, fold := m.renderThinking(merged, line, key)
						if fold >= 0 {
							folds[key] = fold
						}
						out.WriteString(text)
					}
					bi = end - 1
				}
			case ai.ContentTypeToolCall:
				// In-flight tool calls render in the pending state box.
				out.WriteString("\n" + toolBg("pending", " "+bold(b.ToolName)+" "+formatArgs(b.Arguments)))
			}
		}
		return out.String(), folds
	case ai.RoleToolResult:
		// Tool results are collapsed to one line by default; a click on the
		// line expands (or re-collapses) the full output. The state box
		// color follows success/error state.
		if msg.IsError {
			return "\n" + toolBg("error", " "+errText("✗ "+truncateLine(msg.Text(), 200))), nil
		}
		key := toolFoldKey(msg)
		if m.expandedTools[key] {
			text := truncateRunes(msg.Text(), 2000)
			var out strings.Builder
			out.WriteString("\n" + muted("· 收起 ⤴"))
			for _, line := range strings.Split(text, "\n") {
				out.WriteString("\n" + toolBg("success", "  "+line))
			}
			// Keep the 收起 header clickable to collapse again.
			return out.String(), map[string]int{key: 1}
		}
		// The collapsed entry renders as "\n· …", so it sits on content
		// line 1 of this message's render. Only the first line is shown.
		firstLine := msg.Text()
		if i := strings.IndexByte(firstLine, '\n'); i >= 0 {
			firstLine = firstLine[:i]
		}
		return "\n" + muted("· "+truncateLine(firstLine, 100)+" ⤵"), map[string]int{key: 1}
	}
	return "", nil
}

const (
	// streamRenderInterval is the minimum time between full re-renders of the
	// in-progress streaming message (~12 fps), matching the spinner tick
	// cadence. Per-chunk frames reuse the previous render in between.
	streamRenderInterval = 80 * time.Millisecond
	// streamRenderBurst forces an early re-render when the streaming text
	// jumps by more than this many bytes, so a fast burst never lags > a beat.
	streamRenderBurst = 1500
)

// streamingCache memoizes the last rendered streaming assistant message.
type streamingCache struct {
	msg      *ai.Message
	textLen  int
	rendered time.Time
	out      string
	folds    map[string]int
}

// renderStreaming renders the in-progress assistant message, throttled: the
// full markdown render (renderStreamingFull) runs at most every
// streamRenderInterval (or immediately on a large text jump / message switch);
// other frames reuse the previous output. Returns the text and the
// folded-thinking line map, as in renderMessage.
func (m *model) renderStreaming(msg *ai.Message) (string, map[string]int) {
	if msg == nil {
		return "", nil
	}
	textLen := 0
	for _, b := range msg.Content {
		if b.Type == ai.ContentTypeText {
			textLen += len(b.Text)
		}
	}
	c := m.streamCache
	if c.msg == msg && c.out != "" &&
		time.Since(c.rendered) < streamRenderInterval &&
		textLen-c.textLen < streamRenderBurst {
		return c.out, c.folds
	}
	out, folds := m.renderStreamingFull(msg)
	m.streamCache = streamingCache{msg: msg, textLen: textLen, rendered: time.Now(), out: out, folds: folds}
	return out, folds
}

// renderStreamingFull renders the in-progress assistant message without
// throttling (see renderStreaming).
func (m *model) renderStreamingFull(msg *ai.Message) (string, map[string]int) {
	var out strings.Builder
	folds := map[string]int{}
	for bi, b := range msg.Content {
		switch b.Type {
		case ai.ContentTypeText:
			out.WriteString("\n" + renderMarkdown(b.Text))
		case ai.ContentTypeThinking:
			if b.Thinking != "" || m.thinkingActive || m.expandedThinking[thinkingKey(msg, bi)] {
				end := bi
				var combined strings.Builder
				anyExpanded := false
				for ; end < len(msg.Content) && msg.Content[end].Type == ai.ContentTypeThinking; end++ {
					if msg.Content[end].Thinking != "" {
						combined.WriteString(msg.Content[end].Thinking)
					}
					if m.expandedThinking[thinkingKey(msg, end)] {
						anyExpanded = true
					}
				}
				merged := ai.ContentBlock{Type: ai.ContentTypeThinking, Thinking: combined.String()}
				line := strings.Count(out.String(), "\n")
				key := thinkingKey(msg, bi)
				if m.thinkingActive {
					out.WriteString(fmt.Sprintf("\n%s %s", muted("· 思考中"), fmt.Sprintf("%ds", m.thinkingElapsed())))
				} else if anyExpanded {
					out.WriteString("\n" + italic(thinkingColor("· "+wrapLine(truncateRunes(merged.Thinking, 2000), 80))))
					folds[key] = line + 1
				} else {
					text, fold := m.renderThinking(merged, line, key)
					if fold >= 0 {
						folds[key] = fold
					}
					out.WriteString(text)
				}
				bi = end - 1
			}
		case ai.ContentTypeToolCall:
			out.WriteString("\n" + toolBg("pending", " "+bold(b.ToolName)+" "+formatArgs(b.Arguments)))
		}
	}
	return out.String(), folds
}

// thinkingKey identifies a thinking block within a message.
func thinkingKey(msg *ai.Message, blockIndex int) string {
	return fmt.Sprintf("%p:%d", msg, blockIndex)
}

// toolFoldPrefix distinguishes tool-result fold keys from thinking keys in
// the shared foldLines map.
const toolFoldPrefix = "tool:"

// toolFoldKey identifies a tool result message for expansion state.
func toolFoldKey(msg *ai.Message) string {
	return fmt.Sprintf("%s%p", toolFoldPrefix, msg)
}

// renderCommandMenu renders the slash-command picker above the input.
func renderCommandMenu(items []cmdMenuItem, cursor int) string {
	var out strings.Builder
	out.WriteString("命令:\n")
	for i, item := range items {
		marker := "  "
		if i == cursor {
			marker = "❯ "
		}
		out.WriteString(marker + item.cmd + "  " + item.desc + "\n")
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// renderSessionPicker renders the /sessions picker above the input.
func renderSessionPicker(items []sessItem, cursor int, pendingDelete string) string {
	var out strings.Builder
	out.WriteString("会话（当前目录，Enter 打开 / d 删除 / r 显示远端任务会话 / Esc 关闭）:\n")
	for i, item := range items {
		marker := "  "
		if i == cursor {
			marker = "❯ "
		}
		label := item.label
		if item.id == pendingDelete {
			label = "[再按 d 确认删除] " + label
		}
		out.WriteString(marker + label + "\n")
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// renderMarkdown renders assistant text as styled terminal output using
// Glamour. The TermRenderer is created once and reused; any render failure
// falls back to the raw text so a formatting hiccup never hides content.
func renderMarkdown(text string) string {
	if text == "" {
		return ""
	}
	mdRenderOnce.Do(func() {
		mdRenderer, mdRenderErr = glamour.NewTermRenderer(
			glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(-1),        // no client wrap; the terminal wraps
			glamour.WithPreservedNewLines(), // keep the author's blank lines
			glamour.WithEmoji(),
		)
	})
	if mdRenderErr != nil || mdRenderer == nil {
		return text
	}
	out, err := mdRenderer.Render(text)
	if err != nil {
		return text
	}
	// Glamour wraps the document with leading/trailing newlines; message
	// blocks already manage their own "\n" separators, so drop both so a
	// block never renders with an extra blank line before it.
	return strings.Trim(out, "\n")
}

var (
	mdRenderOnce sync.Once
	mdRenderer   *glamour.TermRenderer
	mdRenderErr  error
)

func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	var parts []string
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return truncateLine(strings.Join(parts, " "), 80)
}

func modelName(sess sessionLike) string {
	rt := sess.AgentRuntime()
	if rt == nil {
		return "no model"
	}
	state := rt.State()
	if state.Model == nil {
		return "no model"
	}
	return state.Model.Provider + "/" + state.Model.ID
}

func wrapLine(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out strings.Builder
	w := 0
	for _, r := range s {
		if r == '\n' {
			out.WriteByte('\n')
			w = 0
			continue
		}
		rw := runeWidth(r)
		if w+rw > width {
			out.WriteByte('\n')
			w = 0
		}
		out.WriteRune(r)
		w += rw
	}
	return out.String()
}

func truncateLine(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out strings.Builder
	w := 0
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > width {
			break
		}
		out.WriteRune(r)
		w += rw
	}
	return out.String()
}

// truncateRunes truncates by rune count without splitting UTF-8 sequences.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// runeWidth returns the terminal display width of a rune (CJK = 2).
func runeWidth(r rune) int {
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}
