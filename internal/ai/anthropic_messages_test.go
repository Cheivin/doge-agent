package ai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newAnthropicServer starts a mock Anthropic Messages API server. The
// default script returns a text block, a thinking block and a tool use.
func newAnthropicServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w,
				sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","usage":{"input_tokens":10,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":2}}}`),
				sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
				sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`),
				sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
				sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`),
				sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"hmm"}}`),
				sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
				sseEvent("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}`),
				sseEvent("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`),
				sseEvent("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`),
				sseEvent("content_block_stop", `{"type":"content_block_stop","index":2}`),
				sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","thinking_signature":"sig_abc"},"usage":{"output_tokens":5}}`),
				sseEvent("message_stop", `{"type":"message_stop"}`),
			)
		}
	}
	return httptest.NewServer(http.HandlerFunc(handler))
}

func TestAnthropicStream(t *testing.T) {
	srv := newAnthropicServer(t, nil)
	defer srv.Close()

	model := &Model{ID: "claude-sonnet-4-5", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: srv.URL}
	stream := anthropicMessagesStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "sk-ant-test"})

	events, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	wantTypes := []string{EventStart,
		EventTextStart, EventTextDelta, EventTextDelta, EventTextEnd,
		EventThinkingStart, EventThinkingDelta, EventThinkingEnd,
		EventToolCallStart, EventToolCallDelta, EventToolCallDelta, EventToolCallEnd,
		EventDone}
	if len(events) != len(wantTypes) {
		t.Fatalf("got events %v, want %v", typesOf(events), wantTypes)
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
	if len(msg.Content) != 3 {
		t.Fatalf("expected 3 blocks, got %+v", msg.Content)
	}
	thinking := msg.Content[1]
	if thinking.Type != ContentTypeThinking || thinking.Thinking != "hmm" {
		t.Errorf("thinking block = %+v", thinking)
	}
	if thinking.ThinkingSignature != "sig_abc" {
		t.Errorf("thinking signature = %q", thinking.ThinkingSignature)
	}
	tc := msg.Content[2]
	if tc.ToolCallID != "toolu_1" || tc.ToolName != "bash" || tc.Arguments["command"] != "ls" {
		t.Errorf("tool call = %+v", tc)
	}
	if msg.Usage == nil || msg.Usage.Input != 12 || msg.Usage.Output != 5 || msg.Usage.CacheRead != 2 {
		t.Errorf("usage = %+v", msg.Usage)
	}
	if msg.ResponseID != "msg_1" {
		t.Errorf("response id = %q", msg.ResponseID)
	}
}

func TestAnthropicRequestShape(t *testing.T) {
	var gotBody map[string]any
	var gotKey, gotVersion string
	srv := newAnthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeSSE(w,
			sseEvent("message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1,"output_tokens":0}}}`),
			sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
			sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`),
			sseEvent("message_stop", `{"type":"message_stop"}`),
		)
	})
	defer srv.Close()

	model := &Model{ID: "claude-sonnet-4-5", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: srv.URL,
		ContextWindow: 200000, MaxTokens: 64000, Reasoning: true,
		ThinkingLevelMap: map[string]any{"medium": 2048.0},
	}
	tool := &Tool{Name: "bash", Description: "Run a command", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}},
	}}
	messages := []*Message{
		NewUserTextMessage("list files"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("ok"), ToolCallBlock("toolu_1", "bash", map[string]any{"command": "ls"})}, StopReason: StopReasonToolUse},
		{Role: RoleToolResult, ToolCallID: "toolu_1", ToolName: "bash", Content: []ContentBlock{TextBlock("a.txt")}},
	}
	stream := anthropicMessagesStream(model, &Context{SystemPrompt: "sys", Messages: messages, Tools: []*Tool{tool}},
		&StreamOptions{APIKey: "sk-ant-test", Reasoning: "medium", MaxTokens: 300})
	_, _, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if gotKey != "sk-ant-test" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q", gotVersion)
	}
	if gotBody["model"] != "claude-sonnet-4-5" || gotBody["system"] != "sys" || gotBody["max_tokens"] != 300.0 {
		t.Errorf("body basics mismatch: %v", gotBody)
	}
	thinking := gotBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != 2048.0 {
		t.Errorf("thinking = %v", gotBody["thinking"])
	}
	msgs := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	// Assistant tool call → tool_use block.
	assistant := msgs[1].(map[string]any)
	ac := assistant["content"].([]any)
	tu := ac[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "toolu_1" || tu["name"] != "bash" {
		t.Errorf("tool_use conversion mismatch: %v", assistant)
	}
	// Tool result → user message with tool_result block.
	tr := msgs[2].(map[string]any)
	trc := tr["content"].([]any)
	trb := trc[0].(map[string]any)
	if trb["type"] != "tool_result" || trb["tool_use_id"] != "toolu_1" || trb["content"] != "a.txt" {
		t.Errorf("tool_result conversion mismatch: %v", tr)
	}
	tools := gotBody["tools"].([]any)
	t0 := tools[0].(map[string]any)
	if t0["name"] != "bash" || t0["input_schema"] == nil {
		t.Errorf("tools conversion mismatch: %v", t0)
	}
}

func TestAnthropicErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sseEvent("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	}))
	defer srv.Close()

	model := &Model{ID: "claude-sonnet-4-5", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: srv.URL}
	stream := anthropicMessagesStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "sk-ant-test"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonError || !strings.Contains(msg.ErrorMessage, "Overloaded") {
		t.Errorf("error not encoded: %+v", msg)
	}
}

func TestAnthropicHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"type":"error","error":{"message":"invalid api key"}}`, 401)
	}))
	defer srv.Close()

	model := &Model{ID: "claude-sonnet-4-5", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: srv.URL}
	stream := anthropicMessagesStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "bad-key"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonError || !strings.Contains(msg.ErrorMessage, "401") {
		t.Errorf("error not encoded: %+v", msg)
	}
}

func TestAnthropicLengthStop(t *testing.T) {
	srv := newAnthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			sseEvent("message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1,"output_tokens":0}}}`),
			sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
			sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
			sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":100}}`),
			sseEvent("message_stop", `{"type":"message_stop"}`),
		)
	})
	defer srv.Close()

	model := &Model{ID: "claude-sonnet-4-5", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: srv.URL}
	stream := anthropicMessagesStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "sk-ant-test"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonLength {
		t.Errorf("stop reason = %q, want length", msg.StopReason)
	}
}
