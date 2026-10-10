// Package agent implements the agent runtime: the stateful Agent wrapper,
// the agent loop, and the event bus.
package agent

import (
	"context"
	"sync"

	"doge/internal/ai"
)

// QueueMode controls how many queued messages are injected at a drain point.
type QueueMode string

const (
	// QueueAll drains and injects every queued message.
	QueueAll QueueMode = "all"
	// QueueOneAtATime drains and injects only the oldest queued message.
	QueueOneAtATime QueueMode = "one-at-a-time"
)

// ToolExecutionMode controls how tool calls from one assistant message run.
type ToolExecutionMode string

const (
	// ToolExecutionSequential executes tool calls one after another.
	ToolExecutionSequential ToolExecutionMode = "sequential"
	// ToolExecutionParallel prepares tool calls sequentially, then executes
	// them concurrently; results are reported in assistant source order.
	ToolExecutionParallel ToolExecutionMode = "parallel"
)

// Thinking levels supported by models with reasoning.
const (
	ThinkingOff     = "off"
	ThinkingMinimal = "minimal"
	ThinkingLow     = "low"
	ThinkingMedium  = "medium"
	ThinkingHigh    = "high"
	ThinkingXHigh   = "xhigh"
	ThinkingMax     = "max"
)

// AgentToolResult is the result of a tool execution.
type AgentToolResult struct {
	// Content is returned to the model.
	Content []ai.ContentBlock
	// Details carries structured data for logs or UI rendering.
	Details any
	// Usage from the tool execution itself, if available.
	Usage *ai.Usage
	// AddedToolNames names tools introduced by this result.
	AddedToolNames []string
	// Terminate hints that the agent should stop after this tool batch.
	// Early termination only happens when every result in the batch sets it.
	Terminate bool
}

// AgentToolUpdateCallback streams partial execution updates. Calls made after
// the execute promise settles are ignored.
type AgentToolUpdateCallback func(partial AgentToolResult)

// AgentTool is a tool definition executed by the agent runtime.
type AgentTool struct {
	Name        string
	Label       string
	Description string

	// Parameters is a JSON Schema document (draft 2020-12).
	Parameters map[string]any

	// PrepareArguments optionally normalizes raw tool-call arguments before
	// schema validation. It must return an object matching Parameters.
	PrepareArguments func(args any) (map[string]any, error)

	// Execute runs the tool. Returning an error encodes it as an error tool
	// result; the error message becomes the result content.
	Execute func(toolCallID string, params map[string]any, ctx context.Context,
		onUpdate AgentToolUpdateCallback) (*AgentToolResult, error)

	// ExecutionMode overrides the default tool execution mode for this tool.
	ExecutionMode ToolExecutionMode
}

// AgentEvent is a lifecycle event emitted by the Agent for UI and
// persistence. Event emission order: agent_start → turn_start →
// message_* → tool_execution_* → turn_end → … → agent_end.
type AgentEvent struct {
	Type string

	// agent_end
	Messages []*ai.Message
	// turn_end
	ToolResults []*ai.Message

	// message_start / message_update / message_end / turn_end
	Message *ai.Message
	// message_update carries the underlying streaming event.
	AssistantMessageEvent *ai.AssistantMessageEvent

	// tool_execution_start / tool_execution_update / tool_execution_end
	ToolCallID    string
	ToolName      string
	Args          any
	PartialResult any
	Result        *AgentToolResult
	IsError       bool

	// question
	Question *Question

	// mesh_progress
	MeshProgress *MeshProgress
}

// Agent event types.
const (
	EventAgentStart = "agent_start"
	EventAgentEnd   = "agent_end"
	EventTurnStart  = "turn_start"
	EventTurnEnd    = "turn_end"

	EventMessageStart  = "message_start"
	EventMessageUpdate = "message_update"
	EventMessageEnd    = "message_end"

	EventToolExecutionStart  = "tool_execution_start"
	EventToolExecutionUpdate = "tool_execution_update"
	EventToolExecutionEnd    = "tool_execution_end"

	// EventQuestion is emitted by an AgentSession when an external actor (a
	// remote mesh task) asks for a decision. It is NOT part of the normal loop
	// event order: it can arrive at any time (even mid-run) and is answered
	// out-of-band via AgentSession.AnswerQuestion — never through the agent
	// loop, which would deadlock a blocking questioner.
	EventQuestion = "question"

	// EventMeshProgress carries a remote task's status update to UI consumers
	// (TUI/Web). Like EventQuestion it is out-of-band: it never enters the
	// agent conversation, only the pub-sub stream.
	EventMeshProgress = "mesh_progress"
)

// Question is a decision request surfaced to a human consumer (TUI/Web)
// outside the agent loop — e.g. a remote mesh task asking the master for
// guidance. The UI presents it and writes the answer back via
// AgentSession.AnswerQuestion, which never enters the conversation stream.
type Question struct {
	// ID routes the answer back to the waiting caller.
	ID string `json:"id"`
	// Prompt is the question text shown to the human.
	Prompt string `json:"prompt"`
	// Kind classifies the question ("mesh" for remote-task questions).
	Kind string `json:"kind,omitempty"`
	// From is the source (a mesh PeerID for remote tasks).
	From string `json:"from,omitempty"`
	// Task is the remote task id this question belongs to.
	Task string `json:"task,omitempty"`
}

// MeshProgress is a remote task's status update broadcast to UI consumers
// through the session pub-sub. It never enters the conversation transcript.
type MeshProgress struct {
	TaskID  string `json:"taskId"`
	Peer    string `json:"peer,omitempty"`
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

// BeforeToolCallContext is passed to the beforeToolCall hook.
type BeforeToolCallContext struct {
	AssistantMessage *ai.Message
	ToolCall         ai.ContentBlock
	Args             map[string]any
	Context          *AgentContext
}

// BeforeToolCallResult may block a tool call.
type BeforeToolCallResult struct {
	Block  bool
	Reason string
}

// AfterToolCallContext is passed to the afterToolCall hook.
type AfterToolCallContext struct {
	AssistantMessage *ai.Message
	ToolCall         ai.ContentBlock
	Args             map[string]any
	Result           *AgentToolResult
	IsError          bool
	Context          *AgentContext
}

// AfterToolCallResult overrides parts of the executed tool result.
type AfterToolCallResult struct {
	Content   []ai.ContentBlock
	Details   any
	IsError   *bool
	Terminate *bool
}

// AgentContext is a snapshot of the transcript state passed to the loop.
type AgentContext struct {
	SystemPrompt string
	Messages     []*ai.Message
	Tools        []*AgentTool
}

// AgentState is the public state of an Agent.
type AgentState struct {
	SystemPrompt     string
	Model            *ai.Model
	ThinkingLevel    string
	Tools            []*AgentTool
	Messages         []*ai.Message
	IsStreaming      bool
	StreamingMessage *ai.Message
	PendingToolCalls map[string]bool
	ErrorMessage     string
}

// StreamFn streams a single model request. It must never return a Go error:
// failures are encoded as error events in the returned stream.
type StreamFn func(model *ai.Model, ctx *ai.Context, opts *ai.StreamOptions) *ai.EventStream

// defaultStreamFn dispatches through the global ai registry (env keys and
// explicit request keys only; no credential store).
func defaultStreamFn() StreamFn {
	return ai.DefaultStream
}

// StreamOptions are per-request options forwarded to the stream function.
type StreamOptions struct {
	APIKey    string
	Headers   map[string]string
	MaxTokens int
	Reasoning string
	SessionID string
	Context   context.Context
}

// TurnContext describes a completed turn for the prepareNextTurn hook.
type TurnContext struct {
	Message     *ai.Message
	ToolResults []*ai.Message
	Context     *AgentContext
	NewMessages []*ai.Message
}

// TurnUpdate replaces runtime state before the next provider request.
type TurnUpdate struct {
	Context       *AgentContext
	Model         *ai.Model
	ThinkingLevel *string
}

// AgentListener receives agent events in subscription order. Returning an
// error aborts the current run.
type AgentListener func(ev AgentEvent) error

// PendingQueue is the steering/follow-up message queue. It is safe for
// concurrent use: the agent loop drains it while the caller (UI input,
// web backend) enqueues steering/follow-up messages from other goroutines.
type PendingQueue struct {
	mu       sync.Mutex
	messages []*ai.Message
	mode     QueueMode
}

func newPendingQueue(mode QueueMode) *PendingQueue {
	return &PendingQueue{mode: mode}
}

func (q *PendingQueue) Enqueue(m *ai.Message) {
	q.mu.Lock()
	q.messages = append(q.messages, m)
	q.mu.Unlock()
}

func (q *PendingQueue) HasItems() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.messages) > 0
}

// Drain returns the messages to inject at this drain point.
func (q *PendingQueue) Drain() []*ai.Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.messages) == 0 {
		return nil
	}
	if q.mode == QueueAll {
		out := q.messages
		q.messages = nil
		return out
	}
	out := q.messages[:1]
	q.messages = q.messages[1:]
	return out
}

func (q *PendingQueue) Clear() {
	q.mu.Lock()
	q.messages = nil
	q.mu.Unlock()
}

func (q *PendingQueue) SetMode(mode QueueMode) {
	q.mu.Lock()
	q.mode = mode
	q.mu.Unlock()
}
