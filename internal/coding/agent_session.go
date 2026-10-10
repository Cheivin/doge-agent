// Package coding 编码 agent 会话层：装配 agent/ai/tools/permissions，
// 管理会话、模型目录、配置、技能与提示词模板，是 TUI/Web/print 三种
// 运行模式共用的核心层。
package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding/permissions"
	"doge/internal/coding/tools"
	"doge/pkg/logx"
)

// AgentSessionOptions configures an AgentSession.
type AgentSessionOptions struct {
	Cwd           string
	Model         *ai.Model
	ThinkingLevel string
	CustomPrompt  string
	APIKey        string

	SessionManager *SessionManager
	Session        *Session // existing session to resume, or nil
	Settings       *SettingsManager
	ModelRuntime   *ModelRuntime

	Skills          []Skill
	PromptTemplates []PromptTemplate

	// SkillsDirs 是技能发现目录（--skill 自定义时非默认目录），ReloadConfig
	// 热刷新技能时重扫这里；空则回退默认目录（全局 + 项目）。
	SkillsDirs []string

	// Tools overrides the default built-in tool set.
	Tools []*agent.AgentTool

	// ExtraTools are appended after the default (or overridden) tool set.
	// Used to inject mesh tools (mesh_forward) without re-implementing the
	// default tool construction.
	ExtraTools []*agent.AgentTool

	// PermissionAskHandler resolves "ask" permission decisions (the TUI
	// installs one once running; headless modes leave it nil to deny).
	PermissionAskHandler permissions.AskHandler
}

// AgentSession orchestrates the agent runtime with session persistence,
// compaction, and skills. It is the shared core of print/json/interactive
// modes (following the AgentSession).
type AgentSession struct {
	Cwd     string
	Agent   *agent.Agent
	Session *Session

	Settings       *SettingsManager
	ModelRuntime   *ModelRuntime
	SessionManager *SessionManager

	skills          []Skill
	skillsDirs      []string
	promptTemplates []PromptTemplate
	compaction      CompactionSettings
	customPrompt    string
	apiKey          string
	// Tool configuration inherited by forked sessions (ForkNew/ForkSession):
	// customTools is the Tools override, extraTools the injected extras (mesh
	// tools). Without this, a web window that forks its own session would lose
	// mesh_forward and only get the default tool set.
	customTools []*agent.AgentTool
	extraTools  []*agent.AgentTool
	// extraToolsBuilder builds PER-SESSION injected tools (e.g. the mesh
	// remote_agent tool) bound to the session being constructed. The shared
	// session installs it via SetExtraToolsBuilder; every fork inherits it and
	// builds its own instance bound to itself, so async results route back to
	// the session that dispatched instead of the shared one.
	extraToolsBuilder func(*AgentSession) []*agent.AgentTool
	// mcp 工具组（mcp 代理 + direct 提升），main 启动时注入一次。会话级启停
	// 记录在 mcpDesired，由 ReloadConfig 落地（见 agent_session_mcp.go）。
	mcpTools   []*agent.AgentTool
	mcpMounted bool
	mcpDesired *bool
	// cwdCell binds the tool operations to this session's working directory
	// (never the process cwd), so several agents can work side by side in
	// different directories.
	cwdCell *tools.CwdCell

	mu                        sync.Mutex
	compactInProgress         bool
	overflowRecoveryAttempted bool

	// ops is the per-session tool operations carrying session identity and
	// the code-tool caller. Swapping sessions updates these fields in place
	// (like cwdCell.Set), so concurrent sessions never share them.
	ops *tools.Operations

	// consumers are external event subscribers (TUI/Web/mesh). kind marks
	// whether a human interacts on the other end; see InteractiveCount.
	consumers []*consumerEntry

	// pendingQuestions maps question IDs to the answer channel a caller
	// (mesh bridge) is blocking on. Answers arrive out-of-band from a UI via
	// AnswerQuestion, bypassing the agent loop.
	pendingQuestions map[string]chan string

	// permissions is the tool-call permission decider (nil when disabled).
	permissions *permissions.Decider
}

// ConsumerKind classifies an event consumer: does a human sit on the other
// end, or is it an unattended headless listener (web hub with no browser,
// mesh forwarder, recorder)? Question routing and availability checks use
// InteractiveCount, which only counts ConsumerInteractive.
type ConsumerKind int

const (
	// ConsumerHeadless is an unattended listener (no human present).
	ConsumerHeadless ConsumerKind = iota
	// ConsumerInteractive has a human available to answer prompts.
	ConsumerInteractive
)

// consumerEntry pairs a subscribed listener with its interactivity kind.
type consumerEntry struct {
	fn   agent.AgentListener
	kind ConsumerKind
}

// NewAgentSession builds a session: tools, agent, system prompt, session
// storage.
func NewAgentSession(opts AgentSessionOptions) (*AgentSession, error) {
	// 全局禁用的技能（settings.disabledSkills）加载前剔除，模型不可见也不可调用
	skills := filterDisabledSkills(opts.Skills, DisabledSkillSet(opts.Cwd))
	s := &AgentSession{
		Cwd:             opts.Cwd,
		Settings:        opts.Settings,
		ModelRuntime:    opts.ModelRuntime,
		SessionManager:  opts.SessionManager,
		skills:          skills,
		skillsDirs:      opts.SkillsDirs,
		promptTemplates: opts.PromptTemplates,
		customPrompt:    opts.CustomPrompt,
		apiKey:          opts.APIKey,
		customTools:     opts.Tools,
		extraTools:      opts.ExtraTools,
		cwdCell:         tools.NewCwdCell(opts.Cwd),
	}
	if s.Settings != nil {
		s.compaction = s.Settings.Compaction()
	} else {
		s.compaction = DefaultCompactionSettings()
	}

	// Code tool bridge and session identity are per-session: they ride on the
	// session's Operations below, never on package globals.

	if opts.Session != nil {
		s.Session = opts.Session
	} else {
		sess, err := opts.SessionManager.CreateSession(opts.Cwd)
		if err != nil {
			return nil, err
		}
		s.Session = sess
	}

	// Built-in tools, bound to the session's working directory via the cwd
	// cell (relative paths and commands resolve there, not in the process
	// cwd). Session identity and the code-tool caller are stamped onto the
	// per-session Operations, so parallel sessions never share them.
	toolSet := opts.Tools
	if toolSet == nil {
		ops := tools.WithCwd(tools.LocalOperations(), s.cwdCell)
		ops.SessionID = s.Session.Header.ID
		ops.SessionFile = s.Session.FilePath
		ops.CodeCaller = s.codeToolCaller
		s.ops = ops
		toolSet = tools.AllTools(ops)
	}
	toolSet = append(toolSet, opts.ExtraTools...)
	if len(s.skills) > 0 {
		toolSet = append(toolSet, SkillTool(s.skills))
	}

	// Restore messages from the session context (compaction-aware).
	contextEntries := s.Session.ContextEntries()
	messages := ContextEntriesToMessages(contextEntries)

	// Permission policy: global + project files; built-in high-risk
	// command denial applies even without a config file.
	if policy, err := permissions.LoadPolicy(permissions.DefaultPaths(AgentDir(), opts.Cwd)...); err != nil {
		return nil, err
	} else {
		decider := permissions.NewDecider(policy, opts.Cwd, opts.PermissionAskHandler)
		if policy.LLMApproval.Enabled {
			decider.SetApprover(&LLMApprover{
				Runtime:   opts.ModelRuntime,
				Model:     opts.Model,
				ModelSpec: policy.LLMApproval.Model,
			})
		}
		s.permissions = decider
	}

	agentOpts := agent.AgentOptions{
		BeforeToolCall:  s.checkToolPermission,
		PrepareNextTurn: s.prepareNextTurn,
		SystemPrompt:    buildSystemPromptWithTools(opts.CustomPrompt, toolSet, opts.Cwd, skills),
		Model:           opts.Model,
		ThinkingLevel:   opts.ThinkingLevel,
		Tools:           toolSet,
		Messages:        messages,
		StreamFn: func(model *ai.Model, ctx *ai.Context, options *ai.StreamOptions) *ai.EventStream {
			if opts.APIKey != "" {
				options.APIKey = opts.APIKey
			}
			return opts.ModelRuntime.Models.Stream(model, ctx, options)
		},
		SessionID: s.Session.Header.ID,
	}
	s.Agent = agent.NewAgent(agentOpts)
	s.Agent.Subscribe(s.handleAgentEvent)
	return s, nil
}

// checkToolPermission is the beforeToolCall hook: it blocks tool calls
// that the permission policy denies, with a visible reason.
func (s *AgentSession) checkToolPermission(ctx agent.BeforeToolCallContext) (*agent.BeforeToolCallResult, error) {
	if s.permissions == nil {
		return nil, nil
	}
	allowed, reason := s.permissions.Check(permissions.CheckContext{
		ToolName: ctx.ToolCall.ToolName,
		Args:     ctx.Args,
		Cwd:      s.Cwd,
	})
	if !allowed {
		return &agent.BeforeToolCallResult{
			Block:  true,
			Reason: "Permission denied: " + reason,
		}, nil
	}
	return nil, nil
}

// codeToolCaller implements tools.CodeCaller: every tool call made from
// inside a code script is checked against the permission policy, then run
// through the session's real tool implementation.
func (s *AgentSession) codeToolCaller(name string, args map[string]any) (string, bool, error) {
	if s.permissions != nil {
		allowed, reason := s.permissions.Check(permissions.CheckContext{
			ToolName: name,
			Args:     args,
			Cwd:      s.Cwd,
		})
		if !allowed {
			return "Permission denied: " + reason, true, nil
		}
	}
	if s.Agent == nil {
		return "agent not initialized", true, nil
	}
	var tool *agent.AgentTool
	for _, t := range s.Agent.State().Tools {
		if t.Name == name {
			tool = t
			break
		}
	}
	if tool == nil {
		return "tool not found: " + name, true, nil
	}
	result, err := tool.Execute("code-call", args, context.Background(), nil)
	if err != nil {
		return "tool failed: " + err.Error(), true, nil
	}
	if result == nil {
		return "tool returned no result", true, nil
	}
	var sb strings.Builder
	for _, b := range result.Content {
		if b.Type == ai.ContentTypeText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String(), false, nil
}

// SetPermissionAskHandler installs the ask handler (e.g. once the TUI
// program is running). Headless decisions stay denied without it.
//
// Remote-task forks refuse ask handlers: they run unattended, and their
// permission contract is the dispatch level (readonly/write/full) plus
// permission.json. An observer that opens the fork (e.g. the slave's web
// console viewing a running remote task) must not turn asks into a block on
// its prompt modal — that stalls the whole remote run for the ask timeout.
func (s *AgentSession) SetPermissionAskHandler(h permissions.AskHandler) {
	if s.Session != nil && s.Session.Header.Kind == SessionKindRemote {
		return
	}
	if s.permissions != nil {
		s.permissions.SetAskHandler(h)
	}
}

// buildSystemPromptWithTools assembles the prompt without needing the agent.
// 全局（~/.doge/agent/AGENTS.md）与项目（cwd 的 AGENTS.md/CLAUDE.md）上下文
// 都在组装时现读，重载提示词即重读文件。
func buildSystemPromptWithTools(customPrompt string, toolSet []*agent.AgentTool, cwd string, skills []Skill) string {
	return BuildSystemPrompt(SystemPromptOptions{
		CustomPrompt:   customPrompt,
		Tools:          toolSet,
		GlobalContext:  LoadGlobalContext(),
		ProjectContext: LoadProjectContext(cwd),
		Skills:         skills,
		Cwd:            cwd,
	})
}

// buildSystemPrompt rebuilds the prompt with the current tool set (used
// after tool changes).
func (s *AgentSession) buildSystemPrompt(customPrompt string) string {
	state := s.Agent.State()
	return buildSystemPromptWithTools(customPrompt, state.Tools, s.Cwd, s.loadedSkills())
}

// loadedSkills 返回当前技能集快照（reloadSkills 可从 HTTP goroutine 写入，
// 读方一律走这里）
func (s *AgentSession) loadedSkills() []Skill {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skills
}

// AppendExtraTools attaches additional tools to the session's extra tool set
// and the live agent, and rebuilds the system prompt so the new tools are
// announced. Forks (ForkNew/ForkSession/ForkRemote) inherit them through the
// extraTools field — this is how the mesh mesh_forward/mesh_peers/status tools,
// which are session-agnostic, reach current and forked sessions.
func (s *AgentSession) AppendExtraTools(tools []*agent.AgentTool) {
	if len(tools) == 0 {
		return
	}
	s.mu.Lock()
	s.extraTools = append(s.extraTools, tools...)
	s.mu.Unlock()
	s.addSessionTools(tools)
}

// addSessionTools adds tools to the live agent and system prompt WITHOUT
// putting them in extraTools (so forks never inherit a shared instance — a
// per-session tool like remote_agent must be rebuilt per fork). The lock must
// not be held by the caller.
func (s *AgentSession) addSessionTools(tools []*agent.AgentTool) {
	if len(tools) == 0 {
		return
	}
	s.Agent.SetTools(append(s.Agent.State().Tools, tools...))
	s.Agent.SetSystemPrompt(s.buildSystemPrompt(s.customPrompt))
}

// SetExtraToolsBuilder registers a per-session tool factory and applies it to
// this session immediately. The built tools are added to the live agent and
// system prompt but NOT to extraTools; every fork (ForkNew/ForkSession)
// rebuilds its own instance via the inherited builder. The mesh layer uses it
// so remote_agent dispatches from a web-forked session route async results
// back to that same session instead of the shared one.
func (s *AgentSession) SetExtraToolsBuilder(fn func(*AgentSession) []*agent.AgentTool) {
	s.mu.Lock()
	s.extraToolsBuilder = fn
	s.mu.Unlock()
	if fn != nil {
		s.addSessionTools(fn(s))
	}
}

// handleAgentEvent persists messages and triggers compaction checks.
func (s *AgentSession) handleAgentEvent(ev agent.AgentEvent) error {
	var persistErr error
	switch ev.Type {
	case agent.EventMessageEnd:
		msg := ev.Message
		if msg == nil {
			return nil
		}
		switch msg.Role {
		case ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult:
			persistErr = s.Session.AppendMessage(msg)
			if persistErr != nil {
				// A failed append aborts the run (the error propagates
				// through the emit chain); log it — this is the one failure
				// that otherwise ends a run silently.
				logx.Errorf("[session] %s persist %s failed (run will stop): %v",
					s.Session.Header.ID, msg.Role, persistErr)
			}
		}
	case agent.EventAgentEnd:
		// Compaction check after the run finishes.
		s.checkCompaction()
	}
	// The event is always broadcast to consumers: a persistence failure must
	// not hide the event from observers (TUI/Web/mesh). The error still
	// propagates so the run aborts — an unwritable session cannot continue.
	s.fanOut(ev)
	return persistErr
}

// fanOut delivers an event to every registered consumer. Consumers are
// snapshotted under lock but invoked without it, so a slow consumer never
// stalls subscription bookkeeping. A consumer error is observational only:
// it is debug-logged and never allowed to abort the run.
func (s *AgentSession) fanOut(ev agent.AgentEvent) {
	s.mu.Lock()
	consumers := make([]*consumerEntry, len(s.consumers))
	copy(consumers, s.consumers)
	s.mu.Unlock()
	for _, c := range consumers {
		if err := c.fn(ev); err != nil {
			logx.Debugf("agent session: consumer error: %v", err)
		}
	}
}

// Prompt sends a user message, expanding skills and prompt templates, and
// checks compaction before sending. When the agent is mid-run (streaming), the
// message is queued as a steering message instead of being rejected, so it
// interrupts the current tool batch and is injected into the next turn.
func (s *AgentSession) Prompt(text string) error {
	if s.Agent.State().IsStreaming {
		s.Agent.Steer(ai.NewUserTextMessage(s.expandCommands(text)))
		return nil
	}
	text = s.expandCommands(text)
	// Check compaction before sending.
	s.checkCompaction()
	return s.Agent.Prompt(text)
}

// expandCommands expands /skill:name and prompt template invocations.
func (s *AgentSession) expandCommands(text string) string {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "/skill:") {
		return ExpandSkillCommand(s.loadedSkills(), trimmed)
	}
	if strings.HasPrefix(trimmed, "/") && !strings.HasPrefix(trimmed, "//") {
		name := trimmed[1:]
		if idx := strings.IndexAny(name, " \t"); idx >= 0 {
			name = name[:idx]
		}
		for _, t := range s.promptTemplates {
			if t.Name == name {
				args := ""
				if idx := strings.IndexAny(trimmed, " \t"); idx >= 0 {
					args = trimmed[idx+1:]
				}
				return ExpandPromptTemplate(t, args)
			}
		}
	}
	return text
}

// AgentRuntime returns the underlying agent runtime.
func (s *AgentSession) AgentRuntime() *agent.Agent {
	return s.Agent
}

// prepareNextTurn is the per-turn hook. When the assistant emits a
// <skill name location/> block (the model-invocation syntax), the skill's
// content is loaded and delivered as a follow-up user message, so the next
// turn can act on it. Blocks resolve only against the session's loaded
// skills — never against arbitrary paths the model might write into the
// block. The other invocation paths (the "skill" tool, and /skill: inline
// content) do not produce blocks, so they never land here.
func (s *AgentSession) prepareNextTurn(ctx *agent.TurnContext) (*agent.TurnUpdate, error) {
	skills := s.loadedSkills()
	if len(skills) == 0 || ctx.Message == nil {
		return nil, nil
	}
	var loaded []string
	for _, b := range ctx.Message.Content {
		if b.Type != ai.ContentTypeText {
			continue
		}
		loaded = append(loaded, ResolveSkillBlocks(b.Text, skills)...)
	}
	for _, c := range loaded {
		s.Agent.FollowUp(ai.NewUserTextMessage(c))
	}
	return nil, nil
}

// Subscribe registers an event consumer (TUI, web console, mesh forwarder).
// kind marks whether a human interacts on the other end; a web hub that has
// no browser connected may downgrade itself to ConsumerHeadless via setKind.
// The returned unsubscribe is idempotent and removes by identity.
func (s *AgentSession) Subscribe(l agent.AgentListener, kind ConsumerKind) (unsubscribe func(), setKind func(ConsumerKind)) {
	s.mu.Lock()
	entry := &consumerEntry{fn: l, kind: kind}
	s.consumers = append(s.consumers, entry)
	s.mu.Unlock()

	var once sync.Once
	unsubscribe = func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			for i, e := range s.consumers {
				if e == entry {
					s.consumers = append(s.consumers[:i], s.consumers[i+1:]...)
					return
				}
			}
		})
	}
	setKind = func(k ConsumerKind) {
		s.mu.Lock()
		entry.kind = k
		s.mu.Unlock()
	}
	return unsubscribe, setKind
}

// ConsumerCount returns the number of registered event consumers.
func (s *AgentSession) ConsumerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.consumers)
}

// InteractiveCount returns how many consumers have a human on the other end.
// Question routing (mesh) and availability checks use this, not ConsumerCount:
// a headless listener (web hub with no browser) must not count as "someone
// will answer".
func (s *AgentSession) InteractiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.consumers {
		if c.kind == ConsumerInteractive {
			n++
		}
	}
	return n
}

// AskQuestion surfaces a decision request to the human consumers (TUI/Web)
// via the session pub-sub and returns a channel that receives the answer.
// The answer bypasses the agent loop entirely (see AnswerQuestion), so a
// blocking questioner never deadlocks a running agent. It returns an error
// when no interactive consumer is present — callers fall back to their own
// resolution (e.g. an unattended LLM self-answer).
func (s *AgentSession) AskQuestion(q agent.Question) (<-chan string, error) {
	if q.ID == "" {
		q.ID = newQuestionID()
	}
	if s.InteractiveCount() == 0 {
		return nil, errors.New("no interactive consumer to answer the question")
	}
	reply := make(chan string, 1)
	s.mu.Lock()
	if s.pendingQuestions == nil {
		s.pendingQuestions = map[string]chan string{}
	}
	s.pendingQuestions[q.ID] = reply
	s.mu.Unlock()
	s.emitQuestion(q)
	return reply, nil
}

// AnswerQuestion writes a human's answer to a pending question without
// touching the conversation transcript or the agent loop. It reports whether
// the question id was pending.
func (s *AgentSession) AnswerQuestion(id, text string) bool {
	s.mu.Lock()
	reply := s.pendingQuestions[id]
	if reply != nil {
		delete(s.pendingQuestions, id)
	}
	s.mu.Unlock()
	if reply == nil {
		return false
	}
	select {
	case reply <- text:
	default:
	}
	return true
}

// emitQuestion fans a question event out to the session consumers.
func (s *AgentSession) emitQuestion(q agent.Question) {
	s.fanOut(agent.AgentEvent{Type: agent.EventQuestion, Question: &q})
}

// EmitMeshProgress broadcasts a remote task's status update to the session's
// UI consumers (TUI/Web) without entering the conversation transcript.
func (s *AgentSession) EmitMeshProgress(p agent.MeshProgress) {
	s.fanOut(agent.AgentEvent{Type: agent.EventMeshProgress, MeshProgress: &p})
}

// newQuestionID returns a short random id for a question.
func newQuestionID() string {
	return newSessionID()
}

// SwitchModel resolves and switches the active model (slash /model command).
func (s *AgentSession) SwitchModel(spec string) error {
	model, thinking, err := s.ModelRuntime.ResolveModel(spec)
	if err != nil {
		return err
	}
	s.Agent.SetModel(model)
	if thinking != "" {
		s.Agent.SetThinkingLevel(thinking)
	}
	return nil
}

// SetThinkingLevel 调整推理级别并落 thinking_level_change entry（TUI /model、Web /api/thinking 共用）
func (s *AgentSession) SetThinkingLevel(level string) error {
	s.Agent.SetThinkingLevel(level)
	return s.CurrentSession().AppendCustomEntry(&SessionEntry{
		Type:          EntryThinkingLevelChange,
		ThinkingLevel: level,
	})
}

// ReloadConfig re-reads auth.json, models.json and permission.json from disk
// so edits take effect without a restart. Credentials load first so the
// rebuilt catalog can resolve keys; the current model is re-pointed at its
// fresh catalog entry so new requests bill at the updated prices (its
// identity is unchanged).
func (s *AgentSession) ReloadConfig() error {
	if s.ModelRuntime != nil && s.ModelRuntime.Auth != nil {
		if err := s.ModelRuntime.Auth.Reload(); err != nil {
			return err
		}
	}
	if s.ModelRuntime != nil {
		if err := s.ModelRuntime.Reload(ModelsFile()); err != nil {
			return err
		}
		s.refreshCurrentModel()
	}
	// 会话级 MCP 启停在这里落地（用户手动重载才生效）
	s.applyMcp()
	// 技能目录热刷新（skills-zip apply / 手动装技能后生效入口）
	s.reloadSkills()
	// 上下文文件（全局/项目 AGENTS.md）热重读：改完 /reload 即生效
	s.Agent.SetSystemPrompt(s.buildSystemPrompt(s.customPrompt))
	return s.reloadPermissions()
}

// reloadSkills 重扫技能目录：技能集变化时替换 skill 工具并重建系统提示，
// 让新装技能无需新建会话即生效。进行中的 run 不受影响（工具清单下一轮生效）。
func (s *AgentSession) reloadSkills() {
	dirs := s.skillsDirs
	if len(dirs) == 0 {
		dirs = SkillsDirs(s.Cwd)
	}
	fresh := LoadSkills(dirs...)
	fresh = filterDisabledSkills(fresh, DisabledSkillSet(s.Cwd))
	// skills 与 agent loop（prepareNextTurn/expandCommands）并发读，写必须持锁
	s.mu.Lock()
	unchanged := slices.Equal(s.skills, fresh)
	if !unchanged {
		s.skills = fresh
	}
	s.mu.Unlock()
	if unchanged {
		return
	}
	// 重建工具清单：摘掉旧 skill 工具，按需补上新的
	state := s.Agent.State()
	toolSet := make([]*agent.AgentTool, 0, len(state.Tools)+1)
	for _, t := range state.Tools {
		if t.Name != "skill" {
			toolSet = append(toolSet, t)
		}
	}
	if len(fresh) > 0 {
		toolSet = append(toolSet, SkillTool(fresh))
	}
	s.Agent.SetTools(toolSet)
	s.Agent.SetSystemPrompt(s.buildSystemPrompt(s.customPrompt))
}

// refreshCurrentModel re-points the agent at the freshly loaded catalog entry
// for the current model, so updated prices apply to new requests. The model
// identity (provider/id) is unchanged; a model that disappeared from
// models.json keeps running on its previous definition.
func (s *AgentSession) refreshCurrentModel() {
	cur := s.Agent.State().Model
	if cur == nil || s.ModelRuntime == nil {
		return
	}
	if fresh, ok := s.ModelRuntime.Models.GetModel(cur.Provider, cur.ID); ok {
		s.Agent.SetModel(fresh)
	}
}

// reloadPermissions reloads the permission policy and re-wires the LLM
// approver to match the new policy.
func (s *AgentSession) reloadPermissions() error {
	policy, err := permissions.LoadPolicy(permissions.DefaultPaths(AgentDir(), s.Cwd)...)
	if err != nil {
		return err
	}
	if s.permissions == nil {
		s.permissions = permissions.NewDecider(policy, s.Cwd, nil)
	} else {
		s.permissions.SetPolicy(policy)
	}
	if policy.LLMApproval.Enabled {
		s.permissions.SetApprover(&LLMApprover{
			Runtime:   s.ModelRuntime,
			Model:     s.Agent.State().Model,
			ModelSpec: policy.LLMApproval.Model,
		})
	} else {
		s.permissions.SetApprover(nil)
	}
	return nil
}

// Abort cancels the current run.
func (s *AgentSession) Abort() {
	s.Agent.Abort()
}

// WaitForIdle blocks until the current run completes.
func (s *AgentSession) WaitForIdle() {
	s.Agent.WaitForIdle()
}

// CurrentMessages returns the transcript.
func (s *AgentSession) CurrentMessages() []*ai.Message {
	return s.Agent.State().Messages
}

// currentSession returns the active session, guarded so a concurrent
// /new or session switch never tears a reader.
func (s *AgentSession) CurrentSession() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Session
}

// NewSession starts a fresh, empty session in the current working directory,
// resetting the agent transcript, system prompt, and tool session context.
// It is an error while a run is in flight.
func (s *AgentSession) NewSession() error {
	if s.Agent.State().IsStreaming {
		return errors.New("agent is busy")
	}
	sess, err := s.SessionManager.CreateSession(s.Cwd)
	if err != nil {
		return err
	}
	return s.activateSession(sess)
}

// NewSessionIn 在指定工作目录开新会话（web 控制台的项目切换入口），空 cwd
// 等价于 NewSession。目录必须存在，切换链路（cwdCell/权限/系统提示词）由
// activateSession 统一处理。
func (s *AgentSession) NewSessionIn(cwd string) error {
	if s.Agent.State().IsStreaming {
		return errors.New("agent is busy")
	}
	sess, err := s.createSessionIn(cwd)
	if err != nil {
		return err
	}
	return s.activateSession(sess)
}

// ForkNewIn 同 ForkNew，但在指定工作目录开新会话（多窗口模式的路径）。
func (s *AgentSession) ForkNewIn(cwd string) (*AgentSession, error) {
	sess, err := s.createSessionIn(cwd)
	if err != nil {
		return nil, err
	}
	return s.forkTo(sess)
}

// createSessionIn 校验并准备目标目录，空 cwd 用当前会话目录
func (s *AgentSession) createSessionIn(cwd string) (*Session, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return s.SessionManager.CreateSession(s.Cwd)
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return nil, errors.New("not a directory: " + abs)
	}
	return s.SessionManager.CreateSession(abs)
}

// SwitchSession loads an existing session (by id or file path) and makes it
// active. With strict=false the process working directory moves to the
// session's directory, so sessions recorded elsewhere run their tool calls in
// the right place (used by the web console). With strict=true switches are
// limited to sessions recorded in the current directory (used by the TUI).
func (s *AgentSession) SwitchSession(idOrPath string, strict bool) error {
	if s.Agent.State().IsStreaming {
		return errors.New("agent is busy")
	}
	idOrPath = strings.TrimSpace(idOrPath)
	if idOrPath == "" {
		return errors.New("empty session id")
	}
	target, err := s.resolveSession(idOrPath)
	if err != nil {
		return err
	}
	if strict && target.Header.Cwd != s.Cwd {
		return fmt.Errorf("session %s 的工作目录(%s)与当前目录(%s)不一致，TUI 仅支持同目录会话",
			target.Header.ID, target.Header.Cwd, s.Cwd)
	}
	return s.activateSession(target)
}

// OpenSession switches to an existing session in the current working
// directory only (TUI use).
func (s *AgentSession) OpenSession(idOrPath string) error {
	return s.SwitchSession(idOrPath, true)
}

// ListSessions returns the persisted session summaries (newest first).
func (s *AgentSession) ListSessions() ([]SessionSummary, error) {
	return s.SessionManager.ListSessions()
}

// DeleteSession removes a persisted session by id or file path. The
// currently active session is protected.
func (s *AgentSession) DeleteSession(idOrPath string) error {
	idOrPath = strings.TrimSpace(idOrPath)
	if idOrPath == "" {
		return errors.New("empty session id")
	}
	target, err := s.resolveSession(idOrPath)
	if err != nil {
		return err
	}
	if target.Header.ID == s.CurrentSession().Header.ID {
		return errors.New("不能删除当前会话")
	}
	return s.SessionManager.DeleteSession(target.FilePath)
}

// RewriteTurn rewinds the conversation to the messageSeq-th user message
// (0-based, counting the compaction-aware message view) and replaces it with
// newText: everything from that message onward is dropped, the session leaf
// is rebased so the rewritten turn branches from the kept history, and the
// agent transcript is reset to the retained messages. The caller then sends
// the edited text (e.g. via Prompt) to re-run from there. Returns the kept
// transcript.
func (s *AgentSession) RewriteTurn(messageSeq int, newText string) ([]*ai.Message, error) {
	if s.Agent.State().IsStreaming {
		return nil, errors.New("agent is busy")
	}
	newText = strings.TrimSpace(newText)
	if newText == "" {
		return nil, errors.New("empty message")
	}
	keep, keepEnd, err := rewritePlan(s.Session.ContextEntries(), messageSeq)
	if err != nil {
		return nil, err
	}
	entries := s.Session.ContextEntries()
	leafID := ""
	if keepEnd > 0 && keepEnd <= len(entries) {
		leafID = entries[keepEnd-1].ID
	}
	s.Session.Branch(leafID)
	s.Agent.SetMessages(keep)
	s.Agent.ResetQueues()

	s.mu.Lock()
	s.overflowRecoveryAttempted = false
	s.compactInProgress = false
	s.mu.Unlock()
	return keep, nil
}

// rewritePlan maps a user-message ordinal over the compaction-aware message
// view back to the entry boundary: it returns the messages to keep (before
// the target) and the number of leading entries they correspond to. Compaction
// and branch summaries count as user messages in the view, matching what the
// UIs display.
func rewritePlan(entries []*SessionEntry, messageSeq int) ([]*ai.Message, int, error) {
	msgs, ends := contextEntriesToMessagesWithEnd(entries)
	target := -1
	seen := 0
	for i, m := range msgs {
		if m.Role == ai.RoleUser {
			if seen == messageSeq {
				target = i
				break
			}
			seen++
		}
	}
	if target < 0 {
		return nil, 0, fmt.Errorf("用户消息位置 %d 无效", messageSeq)
	}
	keepEnd := 0
	if target > 0 {
		keepEnd = ends[target-1]
	}
	return msgs[:target], keepEnd, nil
}

// contextEntriesToMessagesWithEnd converts entries to messages and records,
// for each message, the entry index one past the last entry that produced it
// (so entries[:end] yield messages[:i+1]).
func contextEntriesToMessagesWithEnd(entries []*SessionEntry) ([]*ai.Message, []int) {
	var msgs []*ai.Message
	var ends []int
	for j, e := range entries {
		switch e.Type {
		case EntryMessage:
			if e.Message != nil {
				msgs = append(msgs, e.Message)
				ends = append(ends, j+1)
			}
		case EntryCompaction, EntryBranchSummary, EntryCustomMessage:
			msg := &ai.Message{Role: ai.RoleUser, Timestamp: e.Timestamp}
			switch e.Type {
			case EntryCompaction:
				msg.Content = []ai.ContentBlock{ai.TextBlock("<compaction-summary>\n" + e.Summary + "\n</compaction-summary>")}
			case EntryBranchSummary:
				msg.Content = []ai.ContentBlock{ai.TextBlock("<branch-summary>\n" + e.Summary + "\n</branch-summary>")}
			default:
				msg.Content = e.Content
				msg.Details = e.Data
			}
			msgs = append(msgs, msg)
			ends = append(ends, j+1)
		}
	}
	return msgs, ends
}

// CurrentSessionID returns the active session id.
func (s *AgentSession) CurrentSessionID() string {
	return s.CurrentSession().Header.ID
}

// GetCwd returns the session working directory.
func (s *AgentSession) GetCwd() string { return s.Cwd }

// resolveSession finds a session by id or file path.
func (s *AgentSession) resolveSession(idOrPath string) (*Session, error) {
	if filepath.IsAbs(idOrPath) {
		return s.SessionManager.LoadSession(idOrPath)
	}
	// Fast path: session ids are embedded in the file name, so the target can
	// be located without scanning (and fully reading) every session file.
	if p := s.SessionManager.FindSessionFile(idOrPath); p != "" {
		return s.SessionManager.LoadSession(p)
	}
	summaries, err := s.SessionManager.ListSessions()
	if err != nil {
		return nil, err
	}
	for _, sm := range summaries {
		if sm.ID == idOrPath || sm.FilePath == idOrPath || filepath.Base(sm.FilePath) == idOrPath {
			return s.SessionManager.LoadSession(sm.FilePath)
		}
	}
	if s, err := s.SessionManager.LoadSession(filepath.Join(s.SessionManager.Dir(), idOrPath)); err == nil {
		return s, nil
	}
	return nil, fmt.Errorf("session not found: %s", idOrPath)
}

// activateSession swaps the active session: the tool working directory is
// rebound to the session's directory (per-agent, never the process cwd), then
// the transcript, system prompt, and tool session environment are rebuilt.
func (s *AgentSession) activateSession(sess *Session) error {
	if sess.Header.Cwd != s.Cwd {
		s.Cwd = sess.Header.Cwd
		s.cwdCell.Set(sess.Header.Cwd)
	}
	if s.permissions != nil {
		s.permissions.SetCwd(s.Cwd)
	}

	msgs := ContextEntriesToMessages(sess.ContextEntries())
	if s.ops != nil {
		s.ops.SessionID = sess.Header.ID
		s.ops.SessionFile = sess.FilePath
	}
	s.Agent.Reset()
	s.Agent.SetMessages(msgs)
	s.Agent.SetSystemPrompt(buildSystemPromptWithTools(s.customPrompt, s.Agent.State().Tools, s.Cwd, s.loadedSkills()))

	s.mu.Lock()
	s.Session = sess
	s.overflowRecoveryAttempted = false
	s.compactInProgress = false
	s.mu.Unlock()
	return nil
}

// ForkNew creates a fresh empty session in this session's working directory
// and returns an INDEPENDENT AgentSession for it. The receiver is untouched,
// which lets a second UI (the web console) run its own conversation without
// disturbing the shared one. Forking copies static configuration only and
// never inspects the source agent's running state, so it is allowed while the
// source is mid-run — the web console opens another conversation while the
// TUI is busy, and the mesh slave forks a session per remote task while the
// local session is active (P0 requirement).
func (s *AgentSession) ForkNew() (*AgentSession, error) {
	sess, err := s.SessionManager.CreateSession(s.Cwd)
	if err != nil {
		return nil, err
	}
	return s.forkTo(sess)
}

// ForkSession resolves an existing session (by id or file path, any working
// directory) and returns an INDEPENDENT AgentSession bound to it. The
// receiver is untouched. Like ForkNew it only reads static configuration, so
// it is also valid while the source is streaming.
func (s *AgentSession) ForkSession(idOrPath string) (*AgentSession, error) {
	idOrPath = strings.TrimSpace(idOrPath)
	if idOrPath == "" {
		return nil, errors.New("empty session id")
	}
	target, err := s.resolveSession(idOrPath)
	if err != nil {
		return nil, err
	}
	return s.forkTo(target)
}

// forkTo builds a new AgentSession for the target session, cloning this
// session's configuration (model, thinking level, prompts, skills, API key).
func (s *AgentSession) forkTo(sess *Session) (*AgentSession, error) {
	state := s.Agent.State()
	cwd := sess.Header.Cwd
	fork, err := NewAgentSession(AgentSessionOptions{
		Cwd:             cwd,
		Model:           state.Model,
		ThinkingLevel:   state.ThinkingLevel,
		CustomPrompt:    s.customPrompt,
		APIKey:          s.apiKey,
		SessionManager:  s.SessionManager,
		Session:         sess,
		Settings:        s.Settings,
		ModelRuntime:    s.ModelRuntime,
		Skills:          s.loadedSkills(),
		SkillsDirs:      s.skillsDirs,
		PromptTemplates: s.promptTemplates,
		Tools:           s.customTools,
		ExtraTools:      s.extraTools,
	})
	if err != nil {
		return nil, err
	}
	// Per-session tools (e.g. remote_agent) are rebuilt for the fork, bound to
	// the fork itself, so async results return to this session. The builder is
	// also inherited, so a fork-of-a-fork keeps building its own instance
	// instead of silently losing the tool.
	if s.extraToolsBuilder != nil {
		fork.extraToolsBuilder = s.extraToolsBuilder
		fork.addSessionTools(s.extraToolsBuilder(fork))
	}
	// mcp 工具组与挂载状态随 fork 继承：extraTools 已按挂载态复制，这里补上
	// 组引用与状态，fork 内的启停同样走 pending → Reload 生效。
	s.mu.Lock()
	fork.mu.Lock()
	fork.mcpTools = s.mcpTools
	fork.mcpMounted = s.mcpMounted
	fork.mu.Unlock()
	s.mu.Unlock()
	return fork, nil
}

// RemoteToolLevel is the permission tier for a remote (mesh) task fork:
// which built-in tools the unattended worker may call.
type RemoteToolLevel string

const (
	// RemoteReadOnly mounts only read/grep/find/ls (no writes, no bash).
	RemoteReadOnly RemoteToolLevel = "readonly"
	// RemoteWrite adds write/edit/code (still no bash).
	RemoteWrite RemoteToolLevel = "write"
	// RemoteFull mounts every built-in tool (including bash); the local
	// permission.json remains the final boundary (unattended ask defaults to
	// deny).
	RemoteFull RemoteToolLevel = "full"
)

// ForkRemote creates an INDEPENDENT AgentSession for a remote task: a fresh
// session bound to cwd (marked SessionKindRemote so the UIs can filter it out
// of the user's own session list), using the tool set of the given permission
// level plus any extra tools (e.g. the slave's contact_supervisor). The
// receiver is untouched and may be mid-run. The fork shares this session's
// configuration (settings, skills, prompts, API key, model runtime) but never
// its transcript, so concurrent remote tasks are fully isolated (P3).
func (s *AgentSession) ForkRemote(cwd string, level RemoteToolLevel, model *ai.Model, extra []*agent.AgentTool) (*AgentSession, error) {
	sess, err := s.SessionManager.CreateSession(cwd)
	if err != nil {
		return nil, err
	}
	sess.Header.Kind = SessionKindRemote
	return s.ForkRemoteTo(sess, level, model, extra)
}

// ForkRemoteTo builds an INDEPENDENT AgentSession bound to an EXISTING
// session — a prior remote task's session, resumed for task continuity (the
// transcript is restored and the new task is appended to it). Tool set,
// model and isolation semantics are identical to ForkRemote. The receiver is
// untouched. The caller must ensure the target session is a remote-task
// session (Header.Kind == SessionKindRemote) before continuing it.
func (s *AgentSession) ForkRemoteTo(sess *Session, level RemoteToolLevel, model *ai.Model, extra []*agent.AgentTool) (*AgentSession, error) {
	state := s.Agent.State()
	if model == nil {
		model = state.Model
	}
	cwd := sess.Header.Cwd

	// Tool operations are bound to the session's cwd and carry the session
	// identity (never the receiver's), so concurrent remote tasks in different
	// directories never interfere.
	ops := tools.WithCwd(tools.LocalOperations(), tools.NewCwdCell(cwd))
	ops.SessionID = sess.Header.ID
	ops.SessionFile = sess.FilePath

	// The code tool's script-side calls route through the fork's own
	// permission policy and tools; resolved after construction below.
	var fork *AgentSession
	ops.CodeCaller = func(name string, args map[string]any) (string, bool, error) {
		if fork == nil {
			return "agent not initialized", true, nil
		}
		return fork.codeToolCaller(name, args)
	}

	built := remoteToolSet(ops, level)
	built = append(built, extra...)

	fork, err := NewAgentSession(AgentSessionOptions{
		Cwd:             cwd,
		Model:           model,
		ThinkingLevel:   state.ThinkingLevel,
		CustomPrompt:    s.customPrompt,
		APIKey:          s.apiKey,
		SessionManager:  s.SessionManager,
		Session:         sess,
		Settings:        s.Settings,
		ModelRuntime:    s.ModelRuntime,
		Skills:          s.loadedSkills(),
		SkillsDirs:      s.skillsDirs,
		PromptTemplates: s.promptTemplates,
		Tools:           built,
	})
	if err != nil {
		return nil, err
	}
	return fork, nil
}

// ResolveSession loads a persisted session by id, file path, or basename
// (used by the mesh slave to continue a previous remote-task session).
func (s *AgentSession) ResolveSession(idOrPath string) (*Session, error) {
	return s.resolveSession(idOrPath)
}

// remoteToolSet builds the built-in tool set for a remote task permission
// level, bound to ops.
func remoteToolSet(ops *tools.Operations, level RemoteToolLevel) []*agent.AgentTool {
	switch level {
	case RemoteWrite:
		return []*agent.AgentTool{
			tools.ReadTool(ops), tools.GrepTool(ops), tools.FindTool(ops), tools.LSTool(ops),
			tools.WriteTool(ops), tools.EditTool(ops), tools.CodeTool(ops),
		}
	case RemoteFull:
		return tools.AllTools(ops)
	default:
		return tools.ReadOnlyTools(ops)
	}
}

// SummarizeContext produces a context summary of the current session without
// modifying it (unlike Compact). The remote_agent tool uses it to prepare a
// "summary" snapshot for a remote task; the summary never enters the session
// transcript.
func (s *AgentSession) SummarizeContext() (string, error) {
	entries := s.Session.ContextEntries()
	return s.summarize(entries)
}

// RecentMessages returns the recent transcript messages, bounded by the
// compaction keep-recent token budget (the same bound used for fork
// snapshots). The remote_agent tool uses it to prepare a "fork" snapshot.
func (s *AgentSession) RecentMessages() []*ai.Message {
	entries := s.Session.ContextEntries()
	if cut, ok := FindCutPoint(entries, s.compaction.KeepRecentTokens); ok && cut > 0 {
		entries = entries[cut:]
	}
	return ContextEntriesToMessages(entries)
}

// checkCompaction evaluates the compaction triggers after a turn:
//  1. overflow: the assistant usage exceeded the context window → compact
//     and retry once (continue).
//  2. threshold: estimated context tokens exceed window - reserve → compact.
func (s *AgentSession) checkCompaction() {
	if !s.compaction.Enabled {
		return
	}
	s.mu.Lock()
	if s.compactInProgress {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	state := s.Agent.State()
	model := state.Model
	if model == nil || model.ContextWindow <= 0 {
		return
	}
	last := lastAssistantMessage(state.Messages)
	if last == nil || last.Usage == nil {
		return
	}

	// Overflow: usage exceeds the window.
	if last.Usage.TotalTokens > model.ContextWindow && !s.overflowRecoveryAttempted {
		s.overflowRecoveryAttempted = true
		// Remove the overflow error message so the retry has clean context.
		s.removeLastMessage()
		if err := s.Compact(); err != nil {
			return
		}
		go func() {
			_ = s.Agent.Continue()
		}()
		return
	}

	// Threshold: estimated tokens exceed window - reserve.
	if !s.compactInProgress {
		if last.Usage.TotalTokens > 0 {
			ctxTokens := last.Usage.TotalTokens
			if ShouldCompact(ctxTokens, model.ContextWindow, s.compaction.ReserveTokens) {
				_ = s.Compact()
			}
		}
	}
}

func (s *AgentSession) removeLastMessage() {
	msgs := s.Agent.State().Messages
	if len(msgs) > 0 {
		s.Agent.SetMessages(msgs[:len(msgs)-1])
	}
}

func lastAssistantMessage(messages []*ai.Message) *ai.Message {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == ai.RoleAssistant {
			return messages[i]
		}
	}
	return nil
}

// Compact summarizes the summarized portion of the context and rebuilds the
// agent transcript with the compaction entry.
func (s *AgentSession) Compact() error {
	s.mu.Lock()
	if s.compactInProgress {
		s.mu.Unlock()
		return nil
	}
	s.compactInProgress = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.compactInProgress = false
		s.mu.Unlock()
	}()

	entries := s.Session.ContextEntries()
	cut, ok := FindCutPoint(entries, s.compaction.KeepRecentTokens)
	if !ok || cut < 1 {
		return nil // nothing to compact
	}
	toSummarize := entries[:cut]
	kept := entries[cut:]

	// Build the summary with an independent LLM call.
	summary, err := s.summarize(toSummarize)
	if err != nil {
		return fmt.Errorf("compaction summarization failed: %w", err)
	}

	firstKept := ""
	if len(kept) > 0 {
		firstKept = kept[0].ID
	}
	compactionEntry := &SessionEntry{
		Type:             EntryCompaction,
		Summary:          summary,
		FirstKeptEntryID: firstKept,
		TokensBefore:     sumTokens(entries),
	}
	if err := s.Session.AppendCustomEntry(compactionEntry); err != nil {
		return err
	}

	// Rebuild the agent transcript from the compaction-aware context.
	messages := ContextEntriesToMessages(s.Session.ContextEntries())
	s.Agent.SetMessages(messages)
	s.Agent.ResetQueues()
	return nil
}

// summarize calls the LLM with the summarization prompt. The call does not
// enter the session transcript.
func (s *AgentSession) summarize(entries []*SessionEntry) (string, error) {
	state := s.Agent.State()
	model := state.Model
	if model == nil {
		return "", errors.New("no model selected")
	}
	previous := previousSummary(entries)
	prompt := BuildSummarizationPrompt(entries, previous)

	ctx := &ai.Context{
		SystemPrompt: SummarizationSystemPrompt,
		Messages:     []*ai.Message{ai.NewUserTextMessage(prompt)},
	}
	stream := s.ModelRuntime.Models.Stream(model, ctx, &ai.StreamOptions{
		Reasoning: "off",
		MaxTokens: 12000,
	})
	var text strings.Builder
	err := stream.Range(func(ev ai.AssistantMessageEvent) error {
		if ev.Type == ai.EventTextDelta && ev.Partial != nil {
			for _, b := range ev.Partial.Content {
				if b.Type == ai.ContentTypeText {
					text.WriteString(b.Text)
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	msg, err := stream.Result()
	if err != nil {
		return "", err
	}
	if msg.StopReason == ai.StopReasonError || msg.StopReason == ai.StopReasonAborted {
		return "", errors.New(msg.ErrorMessage)
	}
	if text.Len() == 0 {
		text.WriteString(msg.Text())
	}
	return strings.TrimSpace(text.String()), nil
}

func previousSummary(entries []*SessionEntry) string {
	for _, e := range entries {
		if e.Type == EntryCompaction && e.Summary != "" {
			return e.Summary
		}
	}
	return ""
}

func sumTokens(entries []*SessionEntry) int {
	total := 0
	for _, e := range entries {
		total += estimateEntryTokens(e)
	}
	return total
}
