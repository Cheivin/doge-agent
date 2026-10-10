package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"doge/internal/ai"
)

// AgentOptions 配置 Agent
type AgentOptions struct {
	// 初始状态
	SystemPrompt  string
	Model         *ai.Model
	ThinkingLevel string
	Tools         []*AgentTool
	Messages      []*ai.Message

	StreamFn StreamFn // 流式请求函数，默认使用全局 ai 注册表

	// 钩子函数
	BeforeToolCall  func(ctx BeforeToolCallContext) (*BeforeToolCallResult, error)
	AfterToolCall   func(ctx AfterToolCallContext) (*AfterToolCallResult, error)
	PrepareNextTurn func(ctx *TurnContext) (*TurnUpdate, error)

	SteeringMode QueueMode // 转向队列模式
	FollowUpMode QueueMode // 跟进队列模式

	SessionID string // 会话 ID，转发给缓存感知后端

	ToolExecution ToolExecutionMode // 工具执行模式：parallel/sequential

	GetAPIKey func(provider string) string // 动态获取 API 密钥
}

// activeRun 追踪进行中的 prompt 运行
type activeRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Agent 有状态的 agent 循环包装器
type Agent struct {
	mu sync.Mutex

	state AgentState

	listeners []*listenerEntry
	steeringQ *PendingQueue
	followUpQ *PendingQueue
	streamFn  StreamFn
	opts      AgentOptions
	activeRun *activeRun
	runCtx    context.Context // context of the current run, for listeners
}

// NewAgent 创建 Agent
func NewAgent(options AgentOptions) *Agent {
	if options.StreamFn == nil {
		options.StreamFn = defaultStreamFn()
	}
	if options.SteeringMode == "" {
		options.SteeringMode = QueueOneAtATime
	}
	if options.FollowUpMode == "" {
		options.FollowUpMode = QueueOneAtATime
	}
	if options.ToolExecution == "" {
		options.ToolExecution = ToolExecutionParallel
	}
	a := &Agent{
		state: AgentState{
			SystemPrompt:     options.SystemPrompt,
			Model:            options.Model,
			ThinkingLevel:    options.ThinkingLevel,
			Tools:            append([]*AgentTool{}, options.Tools...),
			Messages:         append([]*ai.Message{}, options.Messages...),
			PendingToolCalls: map[string]bool{},
		},
		steeringQ: newPendingQueue(options.SteeringMode),
		followUpQ: newPendingQueue(options.FollowUpMode),
		streamFn:  options.StreamFn,
		opts:      options,
	}
	return a
}

// State 返回 agent 状态的副本
func (a *Agent) State() AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.state
	s.Tools = append([]*AgentTool{}, a.state.Tools...)
	s.Messages = append([]*ai.Message{}, a.state.Messages...)
	s.PendingToolCalls = cloneBoolMap(a.state.PendingToolCalls)
	return s
}

func cloneBoolMap(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SetModel 替换后续轮次使用的模型
func (a *Agent) SetModel(model *ai.Model) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Model = model
}

// SetSystemPrompt 替换系统提示词
func (a *Agent) SetSystemPrompt(prompt string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.SystemPrompt = prompt
}

// SetThinkingLevel 设置推理级别
func (a *Agent) SetThinkingLevel(level string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.ThinkingLevel = level
}

// SetTools 替换可用工具
func (a *Agent) SetTools(tools []*AgentTool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Tools = append([]*AgentTool{}, tools...)
}

// SetMessages 替换对话记录（用于压缩重建上下文）
func (a *Agent) SetMessages(messages []*ai.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Messages = append([]*ai.Message{}, messages...)
}

// ResetQueues 清空所有队列消息
func (a *Agent) ResetQueues() {
	a.steeringQ.Clear()
	a.followUpQ.Clear()
}

// listenerEntry 订阅监听器与稳定标识的配对
type listenerEntry struct {
	fn AgentListener
}

// Subscribe 注册事件监听器，返回取消订阅函数
func (a *Agent) Subscribe(listener AgentListener) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry := &listenerEntry{fn: listener}
	a.listeners = append(a.listeners, entry)
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			for i, e := range a.listeners {
				if e == entry {
					a.listeners = append(a.listeners[:i], a.listeners[i+1:]...)
					return
				}
			}
		})
	}
}

// Steer 排队消息在当前助手轮次后注入
func (a *Agent) Steer(m *ai.Message) {
	a.steeringQ.Enqueue(m)
}

// FollowUp 排队消息在 agent 即将停止时运行
func (a *Agent) FollowUp(m *ai.Message) {
	a.followUpQ.Enqueue(m)
}

// ClearQueues 清空所有队列
func (a *Agent) ClearQueues() {
	a.steeringQ.Clear()
	a.followUpQ.Clear()
}

// HasQueuedMessages 报告是否有队列消息
func (a *Agent) HasQueuedMessages() bool {
	return a.steeringQ.HasItems() || a.followUpQ.HasItems()
}

// Abort cancels the current run, if any.
func (a *Agent) Abort() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeRun != nil {
		a.activeRun.cancel()
	}
}

// WaitForIdle 阻塞直到当前运行完成
func (a *Agent) WaitForIdle() {
	a.mu.Lock()
	run := a.activeRun
	a.mu.Unlock()
	if run != nil {
		<-run.done
	}
}

// Reset 清空对话状态和队列消息
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Messages = nil
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]bool{}
	a.state.ErrorMessage = ""
	a.steeringQ.Clear()
	a.followUpQ.Clear()
}

// Prompt 从文本或消息启动新的 prompt
func (a *Agent) Prompt(input any) error {
	if a.isRunning() {
		return errors.New("agent is already processing a prompt; use Steer or FollowUp to queue messages")
	}
	var messages []*ai.Message
	switch v := input.(type) {
	case string:
		messages = []*ai.Message{ai.NewUserTextMessage(v)}
	case *ai.Message:
		messages = []*ai.Message{v}
	case []*ai.Message:
		messages = v
	default:
		return errors.New("invalid prompt input")
	}
	return a.runWithLifecycle(func(ctx context.Context, emit func(AgentEvent) error) error {
		return runLoop(messages, a.snapshot(), a.loopConfig(), emit, ctx)
	})
}

// Continue 从当前对话继续，最后一条消息必须是用户或工具结果
func (a *Agent) Continue() error {
	if a.isRunning() {
		return errors.New("agent is already processing")
	}
	a.mu.Lock()
	msgs := a.state.Messages
	a.mu.Unlock()
	if len(msgs) == 0 {
		return errors.New("no messages to continue from")
	}
	last := msgs[len(msgs)-1]
	if last.Role == ai.RoleAssistant {
		// Drain steering or follow-up queues first.
		if steered := a.steeringQ.Drain(); len(steered) > 0 {
			return a.runWithLifecycle(func(ctx context.Context, emit func(AgentEvent) error) error {
				return runLoop(steered, a.snapshot(), a.loopConfig(), emit, ctx)
			})
		}
		if followUps := a.followUpQ.Drain(); len(followUps) > 0 {
			return a.runWithLifecycle(func(ctx context.Context, emit func(AgentEvent) error) error {
				return runLoop(followUps, a.snapshot(), a.loopConfig(), emit, ctx)
			})
		}
		return errors.New("cannot continue from message role: assistant")
	}
	return a.runWithLifecycle(func(ctx context.Context, emit func(AgentEvent) error) error {
		return runLoopContinue(a.snapshot(), a.loopConfig(), emit, ctx)
	})
}

func (a *Agent) isRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.activeRun != nil
}

// snapshot 复制当前状态用于运行
func (a *Agent) snapshot() *AgentContext {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &AgentContext{
		SystemPrompt: a.state.SystemPrompt,
		Messages:     append([]*ai.Message{}, a.state.Messages...),
		Tools:        append([]*AgentTool{}, a.state.Tools...),
	}
}

// loopConfig 构建每次运行的循环配置。模型与思考级别取当前状态而非构造
// 参数，保证 SetModel / SetThinkingLevel 的切换对下一次提问生效。
func (a *Agent) loopConfig() *loopConfig {
	a.mu.Lock()
	model := a.state.Model
	thinkingLevel := a.state.ThinkingLevel
	a.mu.Unlock()
	return &loopConfig{
		model:               model,
		thinkingLevel:       thinkingLevel,
		sessionID:           a.opts.SessionID,
		streamFn:            a.streamFn,
		toolExecution:       a.opts.ToolExecution,
		beforeToolCall:      a.opts.BeforeToolCall,
		afterToolCall:       a.opts.AfterToolCall,
		prepareNextTurn:     a.opts.PrepareNextTurn,
		getAPIKey:           a.opts.GetAPIKey,
		getSteeringMessages: func() []*ai.Message { return a.steeringQ.Drain() },
		getFollowUpMessages: func() []*ai.Message { return a.followUpQ.Drain() },
		steeringAvailable:   func() bool { return a.steeringQ.HasItems() },
	}
}

// runWithLifecycle 执行运行：设置中止上下文、发射事件、记录失败/中止
func (a *Agent) runWithLifecycle(executor func(ctx context.Context, emit func(AgentEvent) error) error) error {
	a.mu.Lock()
	if a.activeRun != nil {
		a.mu.Unlock()
		return errors.New("agent is already processing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &activeRun{cancel: cancel, done: make(chan struct{})}
	a.activeRun = run
	a.state.IsStreaming = true
	a.state.StreamingMessage = nil
	a.state.ErrorMessage = ""
	listeners := make([]AgentListener, 0, len(a.listeners))
	for _, e := range a.listeners {
		listeners = append(listeners, e.fn)
	}
	a.mu.Unlock()

	emit := func(ev AgentEvent) error {
		// Reduce internal state, then await listeners in subscription order.
		if err := a.applyEvent(ev); err != nil {
			return err
		}
		for _, l := range listeners {
			if err := l(ev); err != nil {
				return err
			}
		}
		return nil
	}

	var runErr error
	func() {
		defer close(run.done)
		defer func() {
			if r := recover(); r != nil {
				runErr = errors.New("agent run panic: " + toString(r))
			}
		}()
		runErr = executor(ctx, emit)
	}()

	a.mu.Lock()
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]bool{}
	a.activeRun = nil
	a.mu.Unlock()
	return runErr
}

// applyEvent 根据循环事件更新内部状态
func (a *Agent) applyEvent(ev AgentEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch ev.Type {
	case EventMessageStart, EventMessageUpdate:
		a.state.StreamingMessage = ev.Message
	case EventMessageEnd:
		a.state.StreamingMessage = nil
		a.state.Messages = append(a.state.Messages, ev.Message)
	case EventToolExecutionStart:
		a.state.PendingToolCalls[ev.ToolCallID] = true
	case EventToolExecutionEnd:
		delete(a.state.PendingToolCalls, ev.ToolCallID)
	case EventTurnEnd:
		if ev.Message != nil && ev.Message.Role == ai.RoleAssistant && ev.Message.ErrorMessage != "" {
			a.state.ErrorMessage = ev.Message.ErrorMessage
		}
	case EventAgentEnd:
		a.state.StreamingMessage = nil
	}
	return nil
}

// toString 渲染 recover 的 panic 值
func toString(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	return fmt.Sprint(v)
}
