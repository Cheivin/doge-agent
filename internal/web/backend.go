package web

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/permissions"
)

// errSessionReadOnly is returned by write operations on a session the window
// does not hold write control of (another window / the TUI is using it).
var errSessionReadOnly = errors.New("会话正在其他窗口使用（只读），可请求接管")

// errRemoteSessionMcp marks per-session MCP toggles on remote-task sessions
// (they execute on the slave under the dispatching node's policy).
var errRemoteSessionMcp = errors.New("远端任务会话不支持单独配置 MCP")

// Backend is the narrow slice of an AgentSession the web console needs. It
// exists so API handlers and the hub can be unit-tested against a stub.
type Backend interface {
	// Prompt starts an agent run with the given input. It returns quickly
	// with "agent is busy" if a run is already in flight; otherwise the run
	// executes asynchronously. PromptWithAttachments carries staged-upload
	// attachments (multimodal images / inline texts / read references).
	Prompt(text string) error
	PromptWithAttachments(text string, atts []coding.Attachment) error
	Abort()
	Compact() error
	SwitchModel(spec string) error
	// SetThinkingLevel switches the reasoning level (minimal/low/medium/high/
	// xhigh) and lands a thinking_level_change entry in the session file.
	SetThinkingLevel(level string) error

	// NewSession starts a fresh empty session; SwitchSession restores an
	// existing one (by id or file path). Both are rejected while a run is
	// in flight. DeleteSession removes a persisted session (never the
	// active one). RewriteTurn rewinds to the messageSeq-th user message,
	// replaces it with text, and re-runs from there.
	NewSession() error
	// NewSessionIn 在指定目录开新会话，空串用当前目录（web 项目切换入口）
	NewSessionIn(cwd string) error
	SwitchSession(idOrPath string) error
	DeleteSession(id string) error
	RewriteTurn(messageSeq int, text string) error

	Status() Status
	Stats() coding.SessionStats
	ListModels() []ModelInfo
	// ListAllModels returns the full catalog, including models that currently
	// lack credentials (config UIs; backs /api/models?all=1).
	ListAllModels() []ModelInfo
	SessionInfo() SessionInfo
	ListSessions() ([]SessionSummary, error)

	// StreamingMessage returns the in-flight partial assistant message, if
	// any (used to resync clients that connect mid-run).
	StreamingMessage() *ai.Message

	// Subscribe registers an event listener on the active session and returns
	// an unsubscribe function plus a setKind function that updates whether a
	// human is interactively present (the web hub uses it as its browser
	// connection count changes).
	Subscribe(l agent.AgentListener) (func(), func(coding.ConsumerKind))

	// InstallPermissionHandler routes "ask" permission decisions (e.g. to the
	// WebSocket hub). Only one handler is active at a time; a later installer
	// replaces the previous one (the TUI does this when both run).
	InstallPermissionHandler(h permissions.AskHandler)

	// AnswerQuestion writes a human's answer to a pending mesh question
	// (surfaced as a "question" agent event). It reports whether the question
	// was still pending.
	AnswerQuestion(id, text string) bool

	// ReloadConfig re-reads models.json, auth.json and permission.json.
	ReloadConfig() error

	// McpInfo reports the current session's per-session MCP toggle state;
	// SetMcpEnabled records the desired state (applied on ReloadConfig — the
	// user chooses when to reload).
	McpInfo() McpSessionState
	SetMcpEnabled(on bool) error

	// SkillsInfo reports the current session's loaded skills (the set behind
	// its skill tool and <available_skills> block).
	SkillsInfo() []coding.SkillInfo
}

// McpSessionState is the per-session MCP view (GET/POST /api/session/mcp):
// whether this process has an mcp toolset at all, the session kind (remote
// sessions are not individually controllable), the desired vs actually
// mounted state, and whether a change awaits a reload.
type McpSessionState struct {
	Available bool   `json:"available"`
	Kind      string `json:"kind,omitempty"`
	Enabled   bool   `json:"enabled"`
	Mounted   bool   `json:"mounted"`
	Pending   bool   `json:"pending"`
}

// Status summarizes the live agent state for /api/status.
type Status struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	// GlobalCwd 是服务进程的启动目录（"全局目录"，新会话不选项目时的落点）
	GlobalCwd    string   `json:"globalCwd,omitempty"`
	Model        string   `json:"model"`
	Thinking     string   `json:"thinking"`
	Streaming    bool     `json:"streaming"`
	PendingTools []string `json:"pendingTools"`
	ErrorMessage string   `json:"errorMessage"`
	Version      string   `json:"version"`
	HasAssistant bool     `json:"hasAssistant"`
	// Writable reports whether this window currently holds write control of
	// its session (false = read-only, another window/TUI owns it).
	Writable bool `json:"writable"`
	// Owner is the current write-holder's id ("" when uncontended).
	Owner string `json:"owner,omitempty"`
}

// ModelInfo is the API representation of an ai.Model.
type ModelInfo struct {
	Spec          string   `json:"spec"`
	Name          string   `json:"name"`
	Provider      string   `json:"provider"`
	ID            string   `json:"id"`
	API           string   `json:"api"`
	ContextWindow int      `json:"contextWindow"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input,omitempty"`     // 支持的输入类型 text/image
	MaxTokens     int      `json:"maxTokens,omitempty"` // 单次输出上限
}

// SessionInfo describes the active session and its committed transcript.
type SessionInfo struct {
	ID       string        `json:"id"`
	Cwd      string        `json:"cwd"`
	FilePath string        `json:"filePath"`
	Messages []*ai.Message `json:"messages"`
	// SourceName / SourcePeerID label a remote-task session with the node that
	// dispatched it (the UIs show them instead of "USER" on message cards).
	SourceName   string `json:"sourceName,omitempty"`
	SourcePeerID string `json:"sourcePeerId,omitempty"`
	// Kind is "" for a user session, "remote" for a slave's remote-task fork.
	Kind string `json:"kind,omitempty"`
}

// SessionSummary lists one persisted session.
type SessionSummary struct {
	ID           string `json:"id"`
	FilePath     string `json:"filePath"`
	Cwd          string `json:"cwd"`
	Timestamp    int64  `json:"timestamp"`
	Name         string `json:"name"`
	MessageCount int    `json:"messageCount"`
	// Kind is "" for a user's own session, "remote" for a slave's
	// remote-task fork (the UIs keep these separate).
	Kind string `json:"kind"`
	// SourceName / SourcePeerID label a remote-task session with the node that
	// dispatched it.
	SourceName   string `json:"sourceName,omitempty"`
	SourcePeerID string `json:"sourcePeerId,omitempty"`
	// FirstMessage is a truncated preview of the first user message.
	FirstMessage string `json:"firstMessage"`
}

// sessionBackend adapts AgentSessions to the Backend interface.
//
// It starts as a view of the shared AgentSession (the one the TUI uses), so
// web and TUI watch the same conversation. The first time the web console
// changes its session (new or switch), the backend FORKS into its own
// independent AgentSession: from then on the two run side by side — separate
// transcripts, separate working directories — with no cross talk. This keeps
// the shared session (and the process working directory) untouched.
type sessionBackend struct {
	ver  string
	ctrl *controlTable // nil = plain shared backend (no arbitration)
	id   string        // consumer identity ("tui" or a window clientID)

	mu        sync.Mutex
	cur       *coding.AgentSession      // current session's shared active instance
	shared    *coding.AgentSession      // TUI-owned session (protected from deletion), may be cur
	curKey    string                    // current session file path (control key)
	listener  agent.AgentListener       // installed listener (re-wired on session switch)
	unsubFn   func()                    // current subscription unsubscribe
	setKindFn func(coding.ConsumerKind) // current session kind updater
	kind      coding.ConsumerKind       // current interactivity kind (default headless)
	subGen    int                       // subscription generation (funcs aren't comparable)
	ask       permissions.AskHandler
}

// NewBackend wraps a live AgentSession for the web console. tuiActive marks
// whether the TUI is running alongside in the same process: only then is the
// original (shared) session protected from web-side deletion.
func NewBackend(s *coding.AgentSession, version string, tuiActive bool) Backend {
	b := &sessionBackend{cur: s, ver: version, curKey: s.CurrentSession().FilePath}
	if tuiActive {
		b.shared = s
	}
	return b
}

// newWindowBackend builds a per-window session view: it starts watching the
// session's shared active instance (normally the shared AgentSession; if the
// control table already holds a live run for the key, that instance is used so
// a fresh window follows what is actually running) and takes write control of
// that session lazily (the first viewer of an uncontended session becomes its
// owner). Session changes move the view to the target session's shared active
// instance.
func newWindowBackend(ctrl *controlTable, shared *coding.AgentSession, id, version string, tuiActive bool) *sessionBackend {
	b := &sessionBackend{
		ver:    version,
		ctrl:   ctrl,
		id:     id,
		curKey: shared.CurrentSession().FilePath,
	}
	if tuiActive {
		b.shared = shared
	}
	active, _, _ := ctrl.open(b.curKey, id, func() (*coding.AgentSession, error) { return shared, nil })
	b.cur = active
	b.curKey = active.CurrentSession().FilePath
	return b
}

// currentAgent exposes the wrapped AgentSession (used to seed the shared
// session into per-window backends).
func (b *sessionBackend) currentAgent() *coding.AgentSession {
	return b.current()
}

// current returns the active session under lock.
func (b *sessionBackend) current() *coding.AgentSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur
}

// ClaimControl requests write control of the current session (lazy: an
// uncontended session is owned immediately, otherwise the caller joins the
// FIFO wait queue and stays read-only until the owner hands over).
func (b *sessionBackend) ClaimControl() (owned, queued bool) {
	if b.ctrl == nil {
		return true, false
	}
	b.mu.Lock()
	key, id := b.curKey, b.id
	b.mu.Unlock()
	return b.ctrl.acquire(key, id)
}

// TakeoverControl forcibly transfers write control of the current session to
// this window, releasing the previous owner without waiting for it to hand
// off. This is the explicit "强夺" path for a blocked/queued window. Returns
// whether the takeover succeeded and the previous owner's id ("" when
// uncontended).
func (b *sessionBackend) TakeoverControl() (owned bool, prevOwner string) {
	if b.ctrl == nil {
		return true, ""
	}
	b.mu.Lock()
	key, id := b.curKey, b.id
	b.mu.Unlock()
	return b.ctrl.takeover(key, id)
}

// ensureWritable rejects write operations on a session the window does not
// own (arbitration is disabled for plain shared backends).
func (b *sessionBackend) ensureWritable() error {
	if b.ctrl == nil {
		return nil
	}
	b.mu.Lock()
	key, id := b.curKey, b.id
	b.mu.Unlock()
	if !b.ctrl.isOwner(key, id) {
		return errSessionReadOnly
	}
	return nil
}

// Writable reports whether the window currently holds write control of its
// session (used by handlers to reject writes synchronously).
func (b *sessionBackend) Writable() bool {
	return b.ensureWritable() == nil
}

// switchTo adopts a session's active instance as the window's current view,
// rebinding the event listener onto it.
func (b *sessionBackend) switchTo(cur *coding.AgentSession, key string) {
	b.mu.Lock()
	b.cur = cur
	b.curKey = key
	b.mu.Unlock()
	b.resubscribe()
}

// resubscribe moves the window's event listener to the current session's
// active instance. The permission handler is only installed on web-owned
// instances; the shared (TUI) instance keeps whatever the TUI installed.
func (b *sessionBackend) resubscribe() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.unsubFn != nil {
		b.unsubFn()
		b.unsubFn = nil
	}
	if b.listener != nil {
		b.unsubFn, b.setKindFn = b.cur.Subscribe(b.listener, b.kind)
		b.subGen++
	}
	if b.ask != nil && b.cur != b.shared {
		b.cur.SetPermissionAskHandler(b.ask)
	}
}

// resolveSessionKey maps a session id / path / basename to its file path
// (the control-table key).
func (b *sessionBackend) resolveSessionKey(idOrPath string) (string, error) {
	idOrPath = strings.TrimSpace(idOrPath)
	if idOrPath == "" {
		return "", errors.New("empty session id")
	}
	cur := b.current()
	// Fast path: ids are embedded in session file names, so the key resolves
	// with a directory-name walk instead of reading every session file.
	if filepath.IsAbs(idOrPath) || strings.Contains(idOrPath, string(os.PathSeparator)) {
		if _, err := cur.SessionManager.LoadSession(idOrPath); err == nil {
			return idOrPath, nil
		}
	} else if p := cur.SessionManager.FindSessionFile(idOrPath); p != "" {
		return p, nil
	}
	summaries, err := cur.ListSessions()
	if err != nil {
		return "", err
	}
	for _, s := range summaries {
		if s.ID == idOrPath || s.FilePath == idOrPath || filepath.Base(s.FilePath) == idOrPath {
			return s.FilePath, nil
		}
	}
	if filepath.IsAbs(idOrPath) || strings.Contains(idOrPath, string(os.PathSeparator)) {
		if _, err := b.current().SessionManager.LoadSession(idOrPath); err == nil {
			return idOrPath, nil
		}
	}
	return "", fmt.Errorf("session not found: %s", idOrPath)
}

func (b *sessionBackend) Prompt(text string) error {
	if err := b.ensureWritable(); err != nil {
		return err
	}
	return b.current().Prompt(text)
}
func (b *sessionBackend) PromptWithAttachments(text string, atts []coding.Attachment) error {
	if err := b.ensureWritable(); err != nil {
		return err
	}
	return b.current().PromptWithAttachments(text, atts)
}
func (b *sessionBackend) Abort() {
	if b.ensureWritable() != nil {
		return
	}
	b.current().Abort()
}
func (b *sessionBackend) Compact() error {
	if err := b.ensureWritable(); err != nil {
		return err
	}
	return b.current().Compact()
}
func (b *sessionBackend) SwitchModel(spec string) error {
	if err := b.ensureWritable(); err != nil {
		return err
	}
	return b.current().SwitchModel(spec)
}

// SetThinkingLevel 调整推理级别（同一会话只读窗口拒绝）
func (b *sessionBackend) SetThinkingLevel(level string) error {
	if err := b.ensureWritable(); err != nil {
		return err
	}
	return b.current().SetThinkingLevel(level)
}

// NewSession starts a fresh session. The window opens the new session and
// becomes its owner (a fresh session has no other consumers).
func (b *sessionBackend) NewSession() error {
	if b.ctrl == nil {
		return b.current().NewSession()
	}
	return b.newSessionIn("")
}

// NewSessionIn 在指定目录开新会话，空串用当前目录
func (b *sessionBackend) NewSessionIn(cwd string) error {
	if b.ctrl == nil {
		return b.current().NewSessionIn(cwd)
	}
	return b.newSessionIn(cwd)
}

// newSessionIn 多窗口路径：fork 到目标目录的独立会话并切换过去
func (b *sessionBackend) newSessionIn(cwd string) error {
	b.mu.Lock()
	oldKey, oldCur := b.curKey, b.cur
	b.mu.Unlock()
	f, err := oldCur.ForkNewIn(cwd)
	if err != nil {
		return err
	}
	key := f.CurrentSession().FilePath
	active, _, err := b.ctrl.open(key, b.id, func() (*coding.AgentSession, error) { return f, nil })
	if err != nil {
		return err
	}
	b.switchTo(active, key)
	b.ctrl.release(oldKey, b.id)
	return nil
}

// SwitchSession opens an existing session (by id or path). The window joins
// the target session's viewers; it becomes the owner if the session is
// uncontended, otherwise it observes read-only (and may claim control later).
// If the target session currently has a live active instance (a remote-task
// fork running on this slave), the window subscribes to THAT instance so its
// events stream live instead of a detached snapshot.
func (b *sessionBackend) SwitchSession(id string) error {
	if b.ctrl == nil {
		return b.current().SwitchSession(id, false)
	}
	b.mu.Lock()
	oldKey, oldCur := b.curKey, b.cur
	b.mu.Unlock()
	key, err := b.resolveSessionKey(id)
	if err != nil {
		return err
	}
	active, _, err := b.ctrl.open(key, b.id, func() (*coding.AgentSession, error) {
		if act := oldCur.SessionManager.ActiveFor(key); act != nil {
			return act, nil
		}
		return oldCur.ForkSession(id)
	})
	if err != nil {
		return err
	}
	b.switchTo(active, key)
	// 切到的就是当前会话时不能 release 同一个 key：会把刚 open 注册的
	// reader 与 owner 一起抹掉，窗口无端变只读
	if key != oldKey {
		b.ctrl.release(oldKey, b.id)
	}
	return nil
}

// DeleteSession removes a persisted session by id. The current session, the
// TUI's shared session, and any session with live consumers are protected.
func (b *sessionBackend) DeleteSession(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("empty session id")
	}
	cur := b.current()
	summaries, err := cur.SessionManager.ListSessions()
	if err != nil {
		return err
	}
	target := ""
	for _, s := range summaries {
		if s.ID == id {
			target = s.FilePath
			break
		}
	}
	if target == "" {
		return fmt.Errorf("session not found: %s", id)
	}
	if target == cur.CurrentSession().FilePath {
		return errors.New("不能删除当前会话")
	}
	if b.shared != nil && target == b.shared.CurrentSession().FilePath {
		return errors.New("不能删除 TUI 正在使用的会话")
	}
	if b.ctrl != nil && len(b.ctrl.readersOf(target)) > 0 {
		return errors.New("会话正在被其他窗口使用，不能删除")
	}
	return cur.SessionManager.DeleteSession(target)
}

// RewriteTurn rewinds the shared/web agent to the messageSeq-th user
// message, replacing it with the edited text and re-running from there.
func (b *sessionBackend) RewriteTurn(messageSeq int, text string) error {
	if err := b.ensureWritable(); err != nil {
		return err
	}
	cur := b.current()
	if _, err := cur.RewriteTurn(messageSeq, text); err != nil {
		return err
	}
	go func() { _ = cur.Prompt(text) }()
	return nil
}

func (b *sessionBackend) Subscribe(l agent.AgentListener) (func(), func(coding.ConsumerKind)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listener = l
	unsub, setKind := b.cur.Subscribe(l, b.kind)
	b.unsubFn = unsub // this is now the active subscription
	b.setKindFn = setKind
	gen := b.subGen // generation of this subscription
	b.subGen++
	return func() {
			b.mu.Lock()
			if gen == b.subGen-1 {
				b.unsubFn = nil // still active; a later fork re-subscribes itself
			}
			b.mu.Unlock()
			unsub()
		}, func(k coding.ConsumerKind) {
			b.mu.Lock()
			b.kind = k
			if b.setKindFn != nil {
				b.setKindFn(k)
			}
			b.mu.Unlock()
		}
}

func (b *sessionBackend) InstallPermissionHandler(h permissions.AskHandler) {
	b.mu.Lock()
	b.ask = h
	cur := b.cur
	b.mu.Unlock()
	cur.SetPermissionAskHandler(h)
}

// AnswerQuestion writes a human's answer to a pending mesh question on the
// CURRENT session. The question is registered on the session that dispatched
// the task, so only a window viewing that session can answer it — a different
// window must not reach into another session (no session leakage).
func (b *sessionBackend) AnswerQuestion(id, text string) bool {
	return b.current().AnswerQuestion(id, text)
}

func (b *sessionBackend) Status() Status {
	cur := b.current()
	state := cur.Agent.State()
	st := Status{
		SessionID:    cur.CurrentSession().Header.ID,
		Cwd:          cur.GetCwd(),
		Thinking:     state.ThinkingLevel,
		Streaming:    state.IsStreaming,
		PendingTools: []string{},
		ErrorMessage: state.ErrorMessage,
		Version:      b.ver,
		HasAssistant: cur.CurrentSession().HasAssistant,
		Writable:     b.Writable(),
	}
	// 进程启动目录即"全局目录"（doge 不 chdir，会话目录只存在于会话层）
	if wd, err := os.Getwd(); err == nil {
		st.GlobalCwd = wd
	}
	if b.ctrl != nil {
		b.mu.Lock()
		key := b.curKey
		b.mu.Unlock()
		if o, ok := b.ctrl.Owner(key); ok {
			st.Owner = o
		}
	}
	if state.Model != nil {
		st.Model = state.Model.Provider + "/" + state.Model.ID
	}
	for id := range state.PendingToolCalls {
		st.PendingTools = append(st.PendingTools, id)
	}
	sort.Strings(st.PendingTools)
	return st
}

func (b *sessionBackend) Stats() coding.SessionStats {
	cur := b.current()
	sess := cur.CurrentSession()
	msgs := coding.ContextEntriesToMessages(sess.ContextEntries())
	st := coding.ComputeSessionStats(msgs, cur.CostPolicyFor)
	st.SessionID = sess.Header.ID
	st.Cwd = sess.Header.Cwd
	// 上下文占用取最后一条带 Usage 的 assistant 消息，窗口取当前模型
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Role == ai.RoleAssistant && m.Usage != nil {
			st.ContextTokens = m.Usage.Input + m.Usage.CacheRead
			break
		}
	}
	if model := cur.Agent.State().Model; model != nil {
		st.ContextWindow = model.ContextWindow
	}
	return st
}

// ReloadConfig re-reads models.json, auth.json and permission.json.
func (b *sessionBackend) ReloadConfig() error {
	return b.current().ReloadConfig()
}

// McpInfo 当前会话的 MCP 会话级状态
func (b *sessionBackend) McpInfo() McpSessionState {
	cur := b.current()
	desired, mounted, pending := cur.McpState()
	return McpSessionState{
		Available: cur.HasMcp(),
		Kind:      cur.CurrentSession().Header.Kind,
		Enabled:   desired,
		Mounted:   mounted,
		Pending:   pending,
	}
}

// SkillsInfo reports the active session's loaded skills.
func (b *sessionBackend) SkillsInfo() []coding.SkillInfo {
	return b.current().SkillsInfo()
}

// SetMcpEnabled 记录本会话 MCP 启停意图（远端任务会话不可单独配置；落地
// 时机由用户重载决定）
func (b *sessionBackend) SetMcpEnabled(on bool) error {
	cur := b.current()
	if cur.CurrentSession().Header.Kind == "remote" {
		return errRemoteSessionMcp
	}
	return cur.SetMcpEnabled(on)
}

func (b *sessionBackend) ListModels() []ModelInfo {
	cur := b.current()
	seen := map[string]bool{}
	var out []ModelInfo
	// Only models the user can actually run (resolvable key or no-key local
	// endpoint); the active model is always included even if it became
	// unavailable, so the dropdown never shows a blank selection.
	for _, m := range cur.ModelRuntime.Models.AvailableModels() {
		spec := m.Provider + "/" + m.ID
		seen[spec] = true
		out = append(out, modelInfo(m))
	}
	if cur := cur.Agent.State().Model; cur != nil {
		spec := cur.Provider + "/" + cur.ID
		if !seen[spec] {
			out = append(out, modelInfo(cur))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec < out[j].Spec })
	return out
}

func (b *sessionBackend) ListAllModels() []ModelInfo {
	var out []ModelInfo
	for _, m := range b.current().ModelRuntime.Models.AllModels() {
		out = append(out, modelInfo(m))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec < out[j].Spec })
	return out
}

func modelInfo(m *ai.Model) ModelInfo {
	return ModelInfo{
		Spec:          m.Provider + "/" + m.ID,
		Name:          m.Name,
		Provider:      m.Provider,
		ID:            m.ID,
		API:           m.API,
		ContextWindow: m.ContextWindow,
		Reasoning:     m.Reasoning,
		Input:         m.Input,
		MaxTokens:     m.MaxTokens,
	}
}

func (b *sessionBackend) SessionInfo() SessionInfo {
	cur := b.current()
	sess := cur.CurrentSession()
	info := SessionInfo{
		ID:           sess.Header.ID,
		Cwd:          sess.Header.Cwd,
		FilePath:     sess.FilePath,
		SourceName:   sess.Header.SourceName,
		SourcePeerID: sess.Header.SourcePeerID,
		Kind:         sess.Header.Kind,
	}
	// The view may be a detached snapshot (a fork created when the session was
	// opened); later remote-task continuations appended to the same file from
	// NEW fork instances. Reload the committed transcript from disk so the
	// latest content always shows, even without re-switching.
	if loaded, err := cur.SessionManager.LoadSession(sess.FilePath); err == nil {
		sess = loaded
		info.SourceName = sess.Header.SourceName
		info.SourcePeerID = sess.Header.SourcePeerID
		info.Kind = sess.Header.Kind
	}
	info.Messages = coding.ContextEntriesToMessages(sess.ContextEntries())
	return info
}

func (b *sessionBackend) StreamingMessage() *ai.Message {
	return b.current().Agent.State().StreamingMessage
}

func (b *sessionBackend) ListSessions() ([]SessionSummary, error) {
	summaries, err := b.current().SessionManager.ListSessions()
	if err != nil {
		return nil, err
	}
	out := make([]SessionSummary, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, SessionSummary{
			ID:           s.ID,
			FilePath:     s.FilePath,
			Cwd:          s.Cwd,
			Timestamp:    s.Timestamp.UnixMilli(),
			Name:         s.Name,
			MessageCount: s.MessageCount,
			Kind:         s.Kind,
			SourceName:   s.SourceName,
			SourcePeerID: s.SourcePeerID,
			FirstMessage: s.FirstMessage,
		})
	}
	return out, nil
}
