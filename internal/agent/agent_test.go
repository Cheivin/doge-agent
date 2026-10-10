package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"doge/internal/ai"
)

// fakeStream builds a StreamFn whose scripts answer each LLM call in order.
// Each script receives the call index and the request context (for
// assertions) and returns the final assistant message.
func fakeStream(scripts ...func(call int, ctx *ai.Context) *ai.Message) StreamFn {
	var mu sync.Mutex
	n := 0
	return func(model *ai.Model, ctx *ai.Context, opts *ai.StreamOptions) *ai.EventStream {
		mu.Lock()
		call := n
		n++
		mu.Unlock()
		if call >= len(scripts) {
			panic("fakeStream: not enough scripts")
		}
		final := scripts[call](call, ctx)
		s := ai.NewEventStream(16)
		go func() {
			partial := ai.NewAssistantMessage(model.API, model.Provider, model.ID)
			partial.Content = append([]ai.ContentBlock{}, final.Content...)
			partial.StopReason = final.StopReason
			s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: partial})
			for i := range final.Content {
				switch final.Content[i].Type {
				case ai.ContentTypeText:
					s.Push(ai.AssistantMessageEvent{Type: ai.EventTextStart, Partial: partial, ContentIndex: i})
					s.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, Partial: partial, ContentIndex: i, Delta: final.Content[i].Text})
					s.Push(ai.AssistantMessageEvent{Type: ai.EventTextEnd, Partial: partial, ContentIndex: i})
				case ai.ContentTypeToolCall:
					s.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallStart, Partial: partial, ContentIndex: i})
					call := final.Content[i]
					s.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, Partial: partial, ContentIndex: i, ToolCall: &call})
				}
			}
			s.Push(ai.DoneEvent(final))
			s.End(final)
		}()
		return s
	}
}

func textStop(text string) *ai.Message {
	return &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{ai.TextBlock(text)}, Timestamp: time.Now().UnixMilli()}
}

func toolCallStop(id, name string, args map[string]any) *ai.Message {
	return &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonToolUse,
		Content: []ai.ContentBlock{ai.ToolCallBlock(id, name, args)}, Timestamp: time.Now().UnixMilli()}
}

// eventRecorder records agent events for assertions.
type eventRecorder struct {
	events []AgentEvent
}

func (r *eventRecorder) record(ev AgentEvent) error {
	r.events = append(r.events, ev)
	return nil
}

// collectEvents subscribes a recording listener and returns the recorder.
func collectEvents(a *Agent) *eventRecorder {
	r := &eventRecorder{}
	a.Subscribe(r.record)
	return r
}

func eventTypes(events []AgentEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func testModel() *ai.Model {
	return &ai.Model{ID: "test", API: "test-api", Provider: "test", ContextWindow: 100000}
}

func TestAgentSimpleTurn(t *testing.T) {
	a := NewAgent(AgentOptions{
		SystemPrompt: "be brief",
		Model:        testModel(),
		StreamFn: fakeStream(func(call int, ctx *ai.Context) *ai.Message {
			if ctx.SystemPrompt != "be brief" {
				t.Errorf("system prompt = %q", ctx.SystemPrompt)
			}
			if len(ctx.Messages) != 1 || ctx.Messages[0].Text() != "hello" {
				t.Errorf("request messages = %v", ctx.Messages)
			}
			return textStop("hi there")
		}),
	})

	rec := collectEvents(a)
	if err := a.Prompt("hello"); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	want := []string{EventAgentStart, EventTurnStart, EventMessageStart, EventMessageEnd,
		EventMessageStart, EventMessageUpdate, EventMessageUpdate, EventMessageUpdate,
		EventMessageEnd, EventTurnEnd, EventAgentEnd}
	if got := eventTypes(rec.events); len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if rec.events[i].Type != want[i] {
			t.Errorf("event %d = %q, want %q", i, rec.events[i].Type, want[i])
		}
	}
	state := a.State()
	if len(state.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(state.Messages))
	}
	if state.Messages[1].Text() != "hi there" {
		t.Errorf("assistant text = %q", state.Messages[1].Text())
	}
	// agent_end carries the new messages.
	last := rec.events[len(rec.events)-1]
	if len(last.Messages) != 2 {
		t.Errorf("agent_end messages = %d", len(last.Messages))
	}
}

func TestAgentToolUseLoop(t *testing.T) {
	var toolCalls []string
	bashTool := &AgentTool{
		Name: "bash", Label: "Bash", Description: "Run a shell command",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []any{"command"},
		},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
			toolCalls = append(toolCalls, params["command"].(string))
			return &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("ran: " + params["command"].(string))}}, nil
		},
	}

	var secondTurnHasToolResult bool
	a := NewAgent(AgentOptions{
		Model: testModel(),
		Tools: []*AgentTool{bashTool},
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				return toolCallStop("call_1", "bash", map[string]any{"command": "ls"})
			},
			func(call int, ctx *ai.Context) *ai.Message {
				last := ctx.Messages[len(ctx.Messages)-1]
				secondTurnHasToolResult = last.Role == ai.RoleToolResult && last.ToolName == "bash" && last.Text() == "ran: ls"
				return textStop("done")
			},
		),
	})

	rec := collectEvents(a)
	if err := a.Prompt("list files"); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	if len(toolCalls) != 1 || toolCalls[0] != "ls" {
		t.Errorf("tool calls = %v", toolCalls)
	}
	if !secondTurnHasToolResult {
		t.Error("second LLM call did not receive the tool result")
	}

	// Two turns, tool lifecycle events emitted.
	var toolStart, toolEnd bool
	for _, ev := range rec.events {
		if ev.Type == EventToolExecutionStart && ev.ToolName == "bash" {
			toolStart = true
		}
		if ev.Type == EventToolExecutionEnd && ev.ToolName == "bash" && !ev.IsError {
			toolEnd = true
		}
	}
	if !toolStart || !toolEnd {
		t.Errorf("tool lifecycle events missing: start=%v end=%v", toolStart, toolEnd)
	}
	// turn_end carries the tool results.
	for _, ev := range rec.events {
		if ev.Type == EventTurnEnd && len(ev.ToolResults) > 0 {
			if ev.ToolResults[0].ToolName != "bash" {
				t.Errorf("turn_end tool result = %+v", ev.ToolResults[0])
			}
		}
	}
	state := a.State()
	if len(state.Messages) != 4 {
		t.Errorf("final messages = %d, want 4 (user, assistant, toolResult, assistant)", len(state.Messages))
	}
}

func TestAgentParallelToolCalls(t *testing.T) {
	var mu sync.Mutex
	active := 0
	maxActive := 0
	tool := &AgentTool{
		Name: "slow", Description: "slow tool",
		Parameters: map[string]any{"type": "object"},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			return &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("ok " + toolCallID)}}, nil
		},
	}

	a := NewAgent(AgentOptions{
		Model: testModel(),
		Tools: []*AgentTool{tool},
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				m := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonToolUse, Timestamp: time.Now().UnixMilli()}
				m.Content = []ai.ContentBlock{
					ai.ToolCallBlock("c1", "slow", map[string]any{}),
					ai.ToolCallBlock("c2", "slow", map[string]any{}),
				}
				return m
			},
			func(call int, ctx *ai.Context) *ai.Message { return textStop("done") },
		),
	})
	if err := a.Prompt("go"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if maxActive != 2 {
		t.Errorf("max concurrent tool executions = %d, want 2 (parallel)", maxActive)
	}
	// Tool results arrive in source order.
	state := a.State()
	if len(state.Messages) != 5 {
		t.Fatalf("messages = %d, want 5 (user, assistant, toolResult×2, assistant)", len(state.Messages))
	}
	if state.Messages[2].ToolCallID != "c1" || state.Messages[3].ToolCallID != "c2" {
		t.Errorf("tool results out of source order: %v, %v", state.Messages[2], state.Messages[3])
	}
}

func TestAgentSequentialToolExecution(t *testing.T) {
	var mu sync.Mutex
	active := 0
	maxActive := 0
	tool := &AgentTool{
		Name: "serial", Description: "must run serially",
		Parameters:    map[string]any{"type": "object"},
		ExecutionMode: ToolExecutionSequential,
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			return &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("ok")}}, nil
		},
	}
	a := NewAgent(AgentOptions{
		Model: testModel(),
		Tools: []*AgentTool{tool},
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				m := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonToolUse, Timestamp: time.Now().UnixMilli()}
				m.Content = []ai.ContentBlock{
					ai.ToolCallBlock("c1", "serial", map[string]any{}),
					ai.ToolCallBlock("c2", "serial", map[string]any{}),
				}
				return m
			},
			func(call int, ctx *ai.Context) *ai.Message { return textStop("done") },
		),
	})
	if err := a.Prompt("go"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if maxActive != 1 {
		t.Errorf("max concurrent = %d, want 1 (sequential)", maxActive)
	}
}

func TestAgentLengthStopFailsToolCalls(t *testing.T) {
	executed := 0
	tool := &AgentTool{
		Name: "bash", Description: "x",
		Parameters: map[string]any{"type": "object"},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
			executed++
			return &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("ran")}}, nil
		},
	}
	a := NewAgent(AgentOptions{
		Model: testModel(),
		Tools: []*AgentTool{tool},
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				m := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonLength, Timestamp: time.Now().UnixMilli()}
				m.Content = []ai.ContentBlock{ai.ToolCallBlock("c1", "bash", map[string]any{"command": "rm -rf /"})}
				return m
			},
			func(call int, ctx *ai.Context) *ai.Message { return textStop("recovered") },
		),
	})
	if err := a.Prompt("go"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if executed != 0 {
		t.Errorf("truncated tool call was executed %d times", executed)
	}
	// The tool result must be an error so the model re-issues the call.
	state := a.State()
	found := false
	for _, m := range state.Messages {
		if m.Role == ai.RoleToolResult && m.IsError && len(m.Content) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("expected an error tool result for the truncated call")
	}
}

func TestAgentUnknownTool(t *testing.T) {
	a := NewAgent(AgentOptions{
		Model: testModel(),
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				return toolCallStop("c1", "nonexistent", map[string]any{})
			},
			func(call int, ctx *ai.Context) *ai.Message { return textStop("ok") },
		),
	})
	if err := a.Prompt("go"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	state := a.State()
	for _, m := range state.Messages {
		if m.Role == ai.RoleToolResult {
			if !m.IsError || !containsStr(m.Text(), "not found") {
				t.Errorf("unknown tool result = %+v", m)
			}
		}
	}
}

func TestAgentInvalidToolArguments(t *testing.T) {
	tool := &AgentTool{
		Name: "bash", Description: "x",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []any{"command"},
		},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
			t.Error("tool executed with invalid arguments")
			return &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("ran")}}, nil
		},
	}
	a := NewAgent(AgentOptions{
		Model: testModel(),
		Tools: []*AgentTool{tool},
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				return toolCallStop("c1", "bash", map[string]any{"command": 42})
			},
			func(call int, ctx *ai.Context) *ai.Message { return textStop("ok") },
		),
	})
	if err := a.Prompt("go"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	state := a.State()
	for _, m := range state.Messages {
		if m.Role == ai.RoleToolResult {
			if !m.IsError {
				t.Errorf("invalid args should be an error result: %+v", m)
			}
		}
	}
}

func TestAgentBeforeToolCallBlock(t *testing.T) {
	executed := 0
	tool := &AgentTool{
		Name: "bash", Description: "x",
		Parameters: map[string]any{"type": "object"},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
			executed++
			return &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("ran")}}, nil
		},
	}
	a := NewAgent(AgentOptions{
		Model: testModel(),
		Tools: []*AgentTool{tool},
		BeforeToolCall: func(ctx BeforeToolCallContext) (*BeforeToolCallResult, error) {
			return &BeforeToolCallResult{Block: true, Reason: "denied by policy"}, nil
		},
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				return toolCallStop("c1", "bash", map[string]any{})
			},
			func(call int, ctx *ai.Context) *ai.Message { return textStop("ok") },
		),
	})
	if err := a.Prompt("go"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if executed != 0 {
		t.Error("blocked tool executed")
	}
	state := a.State()
	for _, m := range state.Messages {
		if m.Role == ai.RoleToolResult && m.IsError && containsStr(m.Text(), "denied by policy") {
			return
		}
	}
	t.Error("blocked tool result with reason not found")
}

func TestAgentSteeringAndFollowUp(t *testing.T) {
	var turns []string
	a := NewAgent(AgentOptions{
		Model: testModel(),
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				turns = append(turns, "first")
				return textStop("first answer")
			},
			func(call int, ctx *ai.Context) *ai.Message {
				turns = append(turns, "steered")
				last := ctx.Messages[len(ctx.Messages)-1]
				if last.Text() != "steering message" {
					t.Errorf("steering message not injected: %q", last.Text())
				}
				return textStop("second answer")
			},
			func(call int, ctx *ai.Context) *ai.Message {
				turns = append(turns, "followup")
				last := ctx.Messages[len(ctx.Messages)-1]
				if last.Text() != "follow-up message" {
					t.Errorf("follow-up message not injected: %q", last.Text())
				}
				return textStop("third answer")
			},
		),
	})

	if err := a.Prompt("first prompt"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	// Queue a follow-up before the second prompt: it runs after the agent
	// would stop at the end of the second prompt.
	a.FollowUp(ai.NewUserTextMessage("follow-up message"))
	if err := a.Prompt("steering message"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("turns = %v", turns)
	}
	state := a.State()
	texts := []string{}
	for _, m := range state.Messages {
		if m.Role == ai.RoleUser {
			texts = append(texts, m.Text())
		}
	}
	want := []string{"first prompt", "steering message", "follow-up message"}
	if len(texts) != len(want) {
		t.Fatalf("user messages = %v, want %v", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Errorf("user message %d = %q, want %q", i, texts[i], want[i])
		}
	}
}

func TestAgentPrepareNextTurn(t *testing.T) {
	var mu sync.Mutex
	var modelsSeen []string
	newModel := &ai.Model{ID: "new-model", API: "test-api", Provider: "test"}
	noopTool := &AgentTool{
		Name: "noop", Description: "no-op",
		Parameters: map[string]any{"type": "object"},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
			return &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("ok")}}, nil
		},
	}
	a := NewAgent(AgentOptions{
		Model: testModel(),
		Tools: []*AgentTool{noopTool},
		PrepareNextTurn: func(ctx *TurnContext) (*TurnUpdate, error) {
			return &TurnUpdate{Model: newModel}, nil
		},
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				return toolCallStop("c1", "noop", map[string]any{})
			},
			func(call int, ctx *ai.Context) *ai.Message { return textStop("done") },
		),
	})
	// Record the model each LLM call used.
	original := a.streamFn
	a.streamFn = func(model *ai.Model, ctx *ai.Context, opts *ai.StreamOptions) *ai.EventStream {
		mu.Lock()
		modelsSeen = append(modelsSeen, model.ID)
		mu.Unlock()
		return original(model, ctx, opts)
	}
	if err := a.Prompt("first"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(modelsSeen) != 2 || modelsSeen[0] != "test" || modelsSeen[1] != "new-model" {
		t.Errorf("models seen = %v, want [test new-model]", modelsSeen)
	}
}

func TestAgentAbortCancelsTool(t *testing.T) {
	toolCtxCanceled := make(chan struct{})
	tool := &AgentTool{
		Name: "slow", Description: "x",
		Parameters: map[string]any{"type": "object"},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
			<-ctx.Done()
			close(toolCtxCanceled)
			return nil, ctx.Err()
		},
	}
	a := NewAgent(AgentOptions{
		Model: testModel(),
		Tools: []*AgentTool{tool},
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				return toolCallStop("c1", "slow", map[string]any{})
			},
			func(call int, ctx *ai.Context) *ai.Message { return textStop("done") },
		),
	})

	done := make(chan error, 1)
	go func() {
		done <- a.Prompt("go")
	}()
	time.Sleep(50 * time.Millisecond)
	a.Abort()

	select {
	case <-toolCtxCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("tool context was not canceled by abort")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prompt: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not finish after abort")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestAgentSubscribeIdempotentUnsubscribe guards against a double-unsubscribe
// (e.g. a fork tearing down a listener and shutting down again) corrupting
// the listener slice.
func TestAgentSubscribeIdempotentUnsubscribe(t *testing.T) {
	a := NewAgent(AgentOptions{})
	seen1, seen2 := 0, 0
	unsub1 := a.Subscribe(AgentListener(func(AgentEvent) error { seen1++; return nil }))
	unsub2 := a.Subscribe(AgentListener(func(AgentEvent) error { seen2++; return nil }))
	if len(a.listeners) != 2 {
		t.Fatalf("listeners = %d", len(a.listeners))
	}

	// Test the actual unsubscibers directly (the internal listeners slice is
	// the only thing that should shrink; calling the unsub twice must not
	// panic or remove a second element).
	unsub1()
	unsub1() // second call: must be a no-op
	if len(a.listeners) != 1 {
		t.Fatalf("listeners after double unsub = %d (want 1)", len(a.listeners))
	}
	unsub2()
	unsub2()
	if len(a.listeners) != 0 {
		t.Fatalf("listeners after all unsubs = %d (want 0)", len(a.listeners))
	}
}

// TestAgentSteeringInterruptsToolBatch verifies the mid-batch steering
// interrupt: a fast tool completes, a slow tool blocks until its context is
// canceled. Steering while the batch runs must cancel the slow tool (reported
// as interrupted, keeping the fast tool's real result) and then run a new
// turn with the steering message injected.
func TestAgentSteeringInterruptsToolBatch(t *testing.T) {
	fastDone := make(chan struct{})
	slowStarted := make(chan struct{})
	fastTool := &AgentTool{
		Name: "fast", Description: "x",
		Parameters: map[string]any{"type": "object"},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
			close(fastDone)
			return &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("fast ok")}}, nil
		},
	}
	slowTool := &AgentTool{
		Name: "slow", Description: "x",
		Parameters: map[string]any{"type": "object"},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
			close(slowStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	a := NewAgent(AgentOptions{
		Model: testModel(),
		Tools: []*AgentTool{fastTool, slowTool},
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				m := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonToolUse, Timestamp: time.Now().UnixMilli()}
				m.Content = []ai.ContentBlock{
					ai.ToolCallBlock("c1", "fast", map[string]any{}),
					ai.ToolCallBlock("c2", "slow", map[string]any{}),
				}
				return m
			},
			func(call int, ctx *ai.Context) *ai.Message {
				last := ctx.Messages[len(ctx.Messages)-1]
				if last.Role != ai.RoleUser || last.Text() != "stop now" {
					t.Errorf("steering message not injected before the resumed turn: %q (role %s)", last.Text(), last.Role)
				}
				return textStop("after")
			},
		),
	})

	done := make(chan error, 1)
	go func() { done <- a.Prompt("go") }()

	// Wait until the fast tool finished and the slow tool is blocked, then
	// steer: the batch must cancel the slow tool instead of waiting for it.
	<-fastDone
	<-slowStarted
	time.Sleep(10 * time.Millisecond)
	a.Steer(ai.NewUserTextMessage("stop now"))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prompt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not finish after steering interruption")
	}

	state := a.State()
	var fastResult, slowResult *ai.Message
	for _, m := range state.Messages {
		if m.Role == ai.RoleToolResult && m.ToolCallID == "c1" {
			fastResult = m
		}
		if m.Role == ai.RoleToolResult && m.ToolCallID == "c2" {
			slowResult = m
		}
	}
	if fastResult == nil || fastResult.IsError {
		t.Errorf("fast tool result missing or error: %+v", fastResult)
	}
	if slowResult == nil || !slowResult.IsError {
		t.Fatalf("slow tool result missing or not an error: %+v", slowResult)
	}
	if text := slowResult.Text(); !strings.Contains(text, "interrupted") || !strings.Contains(text, "a new user message arrived") {
		t.Errorf("slow tool result should report interruption, got: %q", text)
	}
}

// TestAgentAbortInterruptsToolBatch verifies a global abort (Abort) also
// cancels an in-flight parallel batch, reporting the aborted tool rather than
// waiting for it to drain.
func TestAgentAbortInterruptsToolBatch(t *testing.T) {
	toolStarted := make(chan struct{})
	tool := &AgentTool{
		Name: "slow", Description: "x",
		Parameters: map[string]any{"type": "object"},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
			close(toolStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	a := NewAgent(AgentOptions{
		Model: testModel(),
		Tools: []*AgentTool{tool},
		StreamFn: fakeStream(
			func(call int, ctx *ai.Context) *ai.Message {
				return toolCallStop("c1", "slow", map[string]any{})
			},
			func(call int, ctx *ai.Context) *ai.Message { return textStop("done") },
		),
	})

	done := make(chan error, 1)
	go func() { done <- a.Prompt("go") }()
	<-toolStarted
	time.Sleep(10 * time.Millisecond)
	a.Abort()

	// Abort cancels the batch context; the run must terminate promptly (the
	// loop records an aborted message internally, not a Go error).
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prompt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not finish after abort")
	}
}

// 切换模型必须影响下一次运行的配置（回归：loopConfig 误读构造参数导致切换不生效）
func TestSetModelAffectsNextRun(t *testing.T) {
	modelA := &ai.Model{Provider: "p", ID: "a"}
	modelB := &ai.Model{Provider: "p", ID: "b"}
	a := NewAgent(AgentOptions{Model: modelA})
	a.SetModel(modelB)
	cfg := a.loopConfig()
	if cfg.model != modelB {
		t.Fatalf("want model b, got %v", cfg.model)
	}
	a.SetThinkingLevel("high")
	if cfg2 := a.loopConfig(); cfg2.thinkingLevel != "high" {
		t.Fatalf("want thinking high, got %v", cfg2.thinkingLevel)
	}
}
