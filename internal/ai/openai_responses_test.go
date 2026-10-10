package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseEvent renders one SSE event: "event: <name>\ndata: <data>".
func sseEvent(name, data string) string {
	if name == "" {
		return "data: " + data
	}
	return "event: " + name + "\ndata: " + data
}

// collectStream drains a stream, returning the event types and the final
// message.
func collectStream(s *EventStream) ([]AssistantMessageEvent, *Message, error) {
	var events []AssistantMessageEvent
	_ = s.Range(func(ev AssistantMessageEvent) error {
		events = append(events, ev)
		return nil
	})
	msg, err := s.Result()
	return events, msg, err
}

// newResponsesServer starts a mock OpenAI Responses API server. The handler
// receives the request; the default script returns a text + tool call turn.
func newResponsesServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w,
				sseEvent("response.created", `{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`),
				sseEvent("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`),
				sseEvent("response.content_part.added", `{"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`),
				sseEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}`),
				sseEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" world"}`),
				sseEvent("response.output_text.done", `{"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"Hello world","text_signature":"sig_1"}`),
				sseEvent("response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","status":"in_progress","name":"bash","arguments":"","call_id":"call_1"}}`),
				sseEvent("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"command\":"}`),
				sseEvent("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"\"ls\"}"}`),
				sseEvent("response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":1,"arguments":"{\"command\":\"ls\"}"}`),
				sseEvent("response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","incomplete_details":null,"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":2},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":15}}}`),
			)
		}
	}
	return httptest.NewServer(http.HandlerFunc(handler))
}

func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprintf(w, "%s\n\n", e)
	}
}

func TestOpenAIResponsesTextAndToolCalls(t *testing.T) {
	srv := newResponsesServer(t, nil)
	defer srv.Close()

	model := &Model{
		ID: "gpt-5.2", Name: "GPT-5.2", API: APIOpenAIResponses, Provider: "openai",
		BaseURL: srv.URL, ContextWindow: 200000, MaxTokens: 16384,
		Input: []string{"text"},
		Cost:  ModelCost{Input: 1.25, Output: 10},
	}
	ctx := &Context{SystemPrompt: "be concise", Messages: []*Message{NewUserTextMessage("hi")}}
	stream := openAIResponsesStream(model, ctx, &StreamOptions{APIKey: "sk-test"})

	events, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	// Expected event types: start, text_start, text_delta×2, text_end,
	// toolcall_start, toolcall_delta×2, toolcall_end, done.
	wantTypes := []string{EventStart, EventTextStart, EventTextDelta, EventTextDelta, EventTextEnd,
		EventToolCallStart, EventToolCallDelta, EventToolCallDelta, EventToolCallEnd, EventDone}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events %v, want %v", len(events), typesOf(events), wantTypes)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d: type = %q, want %q", i, events[i].Type, want)
		}
	}

	if msg.StopReason != StopReasonToolUse {
		t.Errorf("stop reason = %q, want %q", msg.StopReason, StopReasonToolUse)
	}
	if msg.Text() != "Hello world" {
		t.Errorf("text = %q", msg.Text())
	}
	if len(msg.Content) != 2 || msg.Content[1].Type != ContentTypeToolCall {
		t.Fatalf("expected text + tool call blocks, got %+v", msg.Content)
	}
	tc := msg.Content[1]
	if tc.ToolCallID != "call_1" && tc.ToolCallID != "fc_1" {
		t.Errorf("tool call id = %q", tc.ToolCallID)
	}
	if tc.ToolName != "bash" {
		t.Errorf("tool name = %q", tc.ToolName)
	}
	if tc.Arguments["command"] != "ls" {
		t.Errorf("tool args = %v", tc.Arguments)
	}
	if msg.Usage == nil || msg.Usage.Input != 10 || msg.Usage.Output != 5 || msg.Usage.CacheRead != 2 || msg.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", msg.Usage)
	}
	if msg.ResponseID != "resp_1" {
		t.Errorf("response id = %q", msg.ResponseID)
	}
	// The text signature is preserved for multi-turn continuity.
	if msg.Content[0].TextSignature != "sig_1" {
		t.Errorf("text signature = %q", msg.Content[0].TextSignature)
	}
}

func typesOf(events []AssistantMessageEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func TestOpenAIResponsesRequestShape(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := newResponsesServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		gotBody = body
		writeSSE(w, sseEvent("response.created", `{"type":"response.created","response":{"id":"r"}}`),
			sseEvent("response.completed", `{"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
	})
	defer srv.Close()

	model := &Model{
		ID: "gpt-5.2", Name: "GPT-5.2", API: APIOpenAIResponses, Provider: "openai",
		BaseURL: srv.URL, ContextWindow: 200000, MaxTokens: 16384, Input: []string{"text"},
	}
	tool := &Tool{Name: "bash", Description: "Run a command", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}},
		"required": []any{"command"},
	}}
	messages := []*Message{
		NewUserTextMessage("list files"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("ok"), ToolCallBlock("call_1", "bash", map[string]any{"command": "ls"})}, StopReason: StopReasonToolUse},
		{Role: RoleToolResult, ToolCallID: "call_1", ToolName: "bash", Content: []ContentBlock{TextBlock("a.txt")}},
	}
	ctx := &Context{SystemPrompt: "sys", Messages: messages, Tools: []*Tool{tool}}
	stream := openAIResponsesStream(model, ctx, &StreamOptions{APIKey: "sk-test", Reasoning: "medium", MaxTokens: 500})
	_, _, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotBody["model"] != "gpt-5.2" || gotBody["instructions"] != "sys" || gotBody["stream"] != true {
		t.Errorf("body basics mismatch: %v", gotBody)
	}
	if gotBody["max_output_tokens"] != 500.0 {
		t.Errorf("max_output_tokens = %v", gotBody["max_output_tokens"])
	}
	reasoning, ok := gotBody["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "medium" {
		t.Errorf("reasoning = %v", gotBody["reasoning"])
	}
	input, ok := gotBody["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("input = %v", gotBody["input"])
	}
	// Tool result converts to a user message with function_call_output.
	last := input[2].(map[string]any)
	content := last["content"].([]any)
	fco := content[0].(map[string]any)
	if fco["type"] != "function_call_output" || fco["call_id"] != "call_1" || fco["output"] != "a.txt" {
		t.Errorf("tool result conversion mismatch: %v", last)
	}
	// Assistant tool call converts to a function_call block.
	assistant := input[1].(map[string]any)
	ac := assistant["content"].([]any)
	fc := ac[1].(map[string]any)
	if fc["type"] != "function_call" || fc["name"] != "bash" || fc["arguments"] != `{"command":"ls"}` {
		t.Errorf("assistant tool call conversion mismatch: %v", assistant)
	}
	tools := gotBody["tools"].([]any)
	t0 := tools[0].(map[string]any)
	if t0["type"] != "function" || t0["name"] != "bash" {
		t.Errorf("tools conversion mismatch: %v", t0)
	}
}

func TestOpenAIResponsesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"rate limited"}}`, 429)
	}))
	defer srv.Close()

	model := &Model{ID: "gpt-5.2", API: APIOpenAIResponses, Provider: "openai", BaseURL: srv.URL}
	stream := openAIResponsesStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "sk-test"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonError {
		t.Errorf("stop reason = %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "429") {
		t.Errorf("error message = %q", msg.ErrorMessage)
	}
}

func TestOpenAIResponsesAbort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sseEvent("response.created", `{"type":"response.created","response":{"id":"r"}}`))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	model := &Model{ID: "gpt-5.2", API: APIOpenAIResponses, Provider: "openai", BaseURL: srv.URL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := openAIResponsesStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}},
		&StreamOptions{APIKey: "sk-test", Context: ctx})
	var events []AssistantMessageEvent
	_ = stream.Range(func(ev AssistantMessageEvent) error {
		events = append(events, ev)
		if ev.Type == EventStart {
			cancel()
		}
		return nil
	})
	msg, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if msg.StopReason != StopReasonAborted {
		t.Errorf("stop reason = %q, want aborted", msg.StopReason)
	}
}

func TestOpenAIResponsesLengthStop(t *testing.T) {
	srv := newResponsesServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			sseEvent("response.created", `{"type":"response.created","response":{"id":"r"}}`),
			sseEvent("response.incomplete", `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`),
			sseEvent("response.completed", `{"type":"response.completed","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`),
		)
	})
	defer srv.Close()

	model := &Model{ID: "gpt-5.2", API: APIOpenAIResponses, Provider: "openai", BaseURL: srv.URL}
	stream := openAIResponsesStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "sk-test"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonLength {
		t.Errorf("stop reason = %q, want length", msg.StopReason)
	}
}

func TestOpenAIResponsesErrorEvent(t *testing.T) {
	srv := newResponsesServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sseEvent("error", `{"type":"error","error":{"code":"server_error","message":"boom"}}`))
	})
	defer srv.Close()

	model := &Model{ID: "gpt-5.2", API: APIOpenAIResponses, Provider: "openai", BaseURL: srv.URL}
	stream := openAIResponsesStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "sk-test"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonError || !strings.Contains(msg.ErrorMessage, "boom") {
		t.Errorf("error not encoded: %+v", msg)
	}
}

func TestOpenAIResponsesTimeout(t *testing.T) {
	// A server that never responds; the context deadline must abort the
	// stream with an aborted message instead of hanging.
	//
	// Note: net/http does not reliably close the connection when a request
	// with a body is canceled by its context (the transport keeps it open
	// waiting for a response), so the handler must not wait on
	// r.Context().Done() to return; a fallback timer ends it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	model := &Model{ID: "gpt-5.2", API: APIOpenAIResponses, Provider: "openai", BaseURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	stream := openAIResponsesStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}},
		&StreamOptions{APIKey: "sk-test", Context: ctx})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = stream.Range(func(AssistantMessageEvent) error { return nil })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not terminate after context timeout")
	}
	msg, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if msg.StopReason != StopReasonAborted {
		t.Errorf("stop reason = %q, want aborted", msg.StopReason)
	}
}

func TestOpenAIResponsesReasoningDisplay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			sseEvent("response.created", `{"type":"response.created","response":{"id":"resp_r","status":"in_progress"}}`),
			sseEvent("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","summary":[{"type":"summary_text","text":""}],"content":[{"type":"reasoning_text","text":""}]}}`),
			sseEvent("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"思考"}`),
			sseEvent("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"中..."}`),
			sseEvent("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"思考中..."}],"content":[{"type":"reasoning_text","text":"深入思考"}]}}`),
			sseEvent("response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`),
			sseEvent("response.content_part.added", `{"type":"response.content_part.added","item_id":"msg_1","output_index":1,"content_index":0,"part":{"type":"output_text","text":""}}`),
			sseEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"答案"}`),
			sseEvent("response.output_text.done", `{"type":"response.output_text.done","item_id":"msg_1","output_index":1,"content_index":0,"text":"答案"}`),
			sseEvent("response.completed", `{"type":"response.completed","response":{"id":"resp_r","status":"completed","usage":{"input_tokens":5,"output_tokens":5,"total_tokens":10}}}`),
		)
	}))
	defer srv.Close()

	model := &Model{ID: "thinking-model", API: APIOpenAIResponses, Provider: "model", BaseURL: srv.URL,
		Reasoning: true, ThinkingLevelMap: map[string]any{"high": "high"}}
	stream := openAIResponsesStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}},
		&StreamOptions{APIKey: "k", Reasoning: "high"})

	events, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	// Assert thinking events were emitted.
	var sawStart, sawDelta, sawEnd bool
	for _, ev := range events {
		switch ev.Type {
		case EventThinkingStart:
			sawStart = true
		case EventThinkingDelta:
			sawDelta = true
		case EventThinkingEnd:
			sawEnd = true
		}
	}
	if !sawStart || !sawDelta || !sawEnd {
		t.Errorf("thinking events missing: start=%v delta=%v end=%v (%v)", sawStart, sawDelta, sawEnd, typesOf(events))
	}
	// The final message carries a thinking block. The done event's content
	// replaces the streamed delta text (no duplication).
	if len(msg.Content) != 2 || msg.Content[0].Type != ContentTypeThinking {
		t.Fatalf("expected thinking + text blocks, got %+v", msg.Content)
	}
	if msg.Content[0].Thinking != "深入思考" {
		t.Errorf("thinking text = %q", msg.Content[0].Thinking)
	}
	if msg.Content[1].Text != "答案" {
		t.Errorf("answer text = %q", msg.Content[1].Text)
	}
	if msg.StopReason != StopReasonStop {
		t.Errorf("stop reason = %q", msg.StopReason)
	}
}
