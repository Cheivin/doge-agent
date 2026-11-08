package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// TestAgentListenerErrorUnwedgesStream：监听器在首个文本增量报错时，Prompt
// 必须在有限时间内返回该错误。回归：小缓冲下生产者卡死在 Push 上，Prompt
// 永久阻塞、HTTP 流持续烧 token。
func TestAgentListenerErrorUnwedgesStream(t *testing.T) {
	// 小缓冲 + 大量增量：监听器停止消费后生产者必然卡在 Push 上。
	streamFn := func(model *ai.Model, ctx *ai.Context, opts *ai.StreamOptions) *ai.EventStream {
		s := ai.NewEventStream(2)
		go func() {
			partial := ai.NewAssistantMessage(model.API, model.Provider, model.ID)
			s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: partial.Clone()})
			for i := 0; i < 64; i++ {
				partial.Content = append(partial.Content, ai.ContentBlock{Type: ai.ContentTypeText, Text: "x"})
				s.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, Partial: partial.Clone(), Delta: "x"})
			}
			final := partial.Clone()
			final.StopReason = ai.StopReasonStop
			s.End(final)
		}()
		return s
	}
	a := NewAgent(AgentOptions{Model: testModel(), StreamFn: streamFn})
	a.Subscribe(func(ev AgentEvent) error {
		if ev.Type == EventMessageUpdate && ev.AssistantMessageEvent != nil && ev.AssistantMessageEvent.Type == ai.EventTextDelta {
			return errors.New("listener boom")
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- a.Prompt("go") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "listener boom") {
			t.Fatalf("prompt err = %v, want the listener error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt wedged: listener error never surfaced (producer stuck in Push)")
	}
}

// TestAgentToolBatchFailureSurfacesError：工具批次内事件发射失败必须以错误
// 结束运行，而不是发出干净的 turn_end/agent_end 让 Prompt 返回 nil。
func TestAgentToolBatchFailureSurfacesError(t *testing.T) {
	for _, mode := range []ToolExecutionMode{ToolExecutionParallel, ToolExecutionSequential} {
		t.Run(string(mode), func(t *testing.T) {
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
				Model:         testModel(),
				Tools:         []*AgentTool{tool},
				ToolExecution: mode,
				StreamFn: fakeStream(
					func(call int, ctx *ai.Context) *ai.Message {
						return toolCallStop("c1", "bash", map[string]any{})
					},
					func(call int, ctx *ai.Context) *ai.Message {
						t.Error("a second LLM turn must not run after a batch failure")
						return textStop("unreachable")
					},
				),
			})
			rec := collectEvents(a)
			a.Subscribe(func(ev AgentEvent) error {
				if ev.Type == EventToolExecutionStart {
					return errors.New("persistence boom")
				}
				return nil
			})
			done := make(chan error, 1)
			go func() { done <- a.Prompt("go") }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "persistence boom") {
					t.Fatalf("prompt err = %v, want the batch failure", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("prompt did not surface the tool batch failure")
			}
			if executed != 0 {
				t.Errorf("tool executed %d times despite the batch failure", executed)
			}
			// 失败的轮次仍以 turn_end 收口，事件流可见
			if len(rec.events) == 0 {
				t.Fatal("no events recorded")
			}
			if last := rec.events[len(rec.events)-1]; last.Type != EventTurnEnd {
				t.Errorf("last event = %s, want turn_end", last.Type)
			}
		})
	}
}

// TestAgentInvalidRawArgsNotExecuted：适配器标记的非法参数调用（RawArgs）
// 不执行，产出命名原因的错误结果，轮次照常继续。
func TestAgentInvalidRawArgsNotExecuted(t *testing.T) {
	for _, mode := range []ToolExecutionMode{ToolExecutionParallel, ToolExecutionSequential} {
		t.Run(string(mode), func(t *testing.T) {
			executed := 0
			tool := &AgentTool{
				Name: "bash", Description: "x",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string"}},
				},
				Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
					executed++
					return &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("ran")}}, nil
				},
			}
			bad := ai.ToolCallBlock("c_bad", "bash", map[string]any{})
			bad.RawArgs = `{"command": "ls"` // 非法 JSON
			a := NewAgent(AgentOptions{
				Model:         testModel(),
				Tools:         []*AgentTool{tool},
				ToolExecution: mode,
				StreamFn: fakeStream(
					func(call int, ctx *ai.Context) *ai.Message {
						m := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonToolUse, Timestamp: time.Now().UnixMilli()}
						m.Content = []ai.ContentBlock{bad}
						return m
					},
					func(call int, ctx *ai.Context) *ai.Message {
						last := ctx.Messages[len(ctx.Messages)-1]
						if last.Role != ai.RoleToolResult || !last.IsError || !strings.Contains(last.Text(), "arguments json invalid") {
							t.Errorf("second turn did not receive the invalid-arguments error result: %+v", last)
						}
						return textStop("recovered")
					},
				),
			})
			if err := a.Prompt("go"); err != nil {
				t.Fatalf("prompt: %v", err)
			}
			if executed != 0 {
				t.Errorf("tool executed %d times with invalid arguments", executed)
			}
		})
	}
}

// TestAgentAdapterInvalidToolCallJSONEndToEnd：三个适配器真实流式拼出非法
// 参数 JSON 时，工具不得执行且产出命名原因的错误结果。
func TestAgentAdapterInvalidToolCallJSONEndToEnd(t *testing.T) {
	for _, api := range []string{ai.APIOpenAIResponses, ai.APIOpenAICompletions, ai.APIAnthropicMessages} {
		t.Run(api, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				n := calls
				calls++
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				switch api {
				case ai.APIOpenAIResponses:
					if n == 0 {
						writeAgentSSE(w,
							"response.created", `{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
							"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"bash","arguments":"","call_id":"call_1"}}`,
							"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"command\":"}`,
							"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"ls\""}`,
							"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"command\":\"ls\""}`,
							"response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
						)
						return
					}
					writeAgentSSE(w,
						"response.created", `{"type":"response.created","response":{"id":"resp_2","status":"in_progress"}}`,
						"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_2","type":"message","role":"assistant","content":[]}}`,
						"response.content_part.added", `{"type":"response.content_part.added","item_id":"msg_2","part":{"type":"output_text"}}`,
						"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_2","delta":"recovered"}`,
						"response.completed", `{"type":"response.completed","response":{"id":"resp_2","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
					)
				case ai.APIOpenAICompletions:
					if n == 0 {
						writeAgentSSEData(w,
							`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":""}}]}}]}`,
							`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}`,
							`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\""}}]}}]}`,
							`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
							`[DONE]`,
						)
						return
					}
					writeAgentSSEData(w,
						`{"choices":[{"delta":{"content":"recovered"}}]}`,
						`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
						`[DONE]`,
					)
				case ai.APIAnthropicMessages:
					if n == 0 {
						writeAgentSSE(w,
							"message_start", `{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":1,"output_tokens":0}}}`,
							"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}`,
							"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
							"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ls\""}}`,
							"content_block_stop", `{"type":"content_block_stop","index":0}`,
							"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
							"message_stop", `{"type":"message_stop"}`,
						)
						return
					}
					writeAgentSSE(w,
						"message_start", `{"type":"message_start","message":{"id":"m2","usage":{"input_tokens":1,"output_tokens":0}}}`,
						"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
						"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"recovered"}}`,
						"content_block_stop", `{"type":"content_block_stop","index":0}`,
						"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
						"message_stop", `{"type":"message_stop"}`,
					)
				}
			}))
			defer srv.Close()

			executed := 0
			tool := &AgentTool{
				Name: "bash", Description: "x",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string"}},
				},
				Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate AgentToolUpdateCallback) (*AgentToolResult, error) {
					executed++
					return &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("ran")}}, nil
				},
			}
			model := &ai.Model{ID: "test-model", API: api, Provider: "test", BaseURL: srv.URL}
			a := NewAgent(AgentOptions{Model: model, Tools: []*AgentTool{tool}, StreamFn: ai.DefaultStream})
			if err := a.Prompt("go"); err != nil {
				t.Fatalf("prompt: %v", err)
			}
			if executed != 0 {
				t.Errorf("tool executed %d times with malformed arguments", executed)
			}
			var errResult *ai.Message
			for _, m := range a.State().Messages {
				if m.Role == ai.RoleToolResult && m.IsError {
					errResult = m
				}
			}
			if errResult == nil {
				t.Fatal("no error tool result for the malformed call")
			}
			if !strings.Contains(errResult.Text(), "arguments json invalid") {
				t.Errorf("error result %q does not name the invalid-arguments reason", errResult.Text())
			}
		})
	}
}

// writeAgentSSE 写具名 SSE 事件流
func writeAgentSSE(w http.ResponseWriter, pairs ...string) {
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", pairs[i], pairs[i+1])
	}
}

// writeAgentSSEData 写仅 data 行的 SSE 事件流
func writeAgentSSEData(w http.ResponseWriter, datas ...string) {
	for _, d := range datas {
		fmt.Fprintf(w, "data: %s\n\n", d)
	}
}
