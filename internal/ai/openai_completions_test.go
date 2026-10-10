package ai

import (
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompletionsStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			`data: {"choices":[{"delta":{"role":"assistant","content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":" world"}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":""}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	model := &Model{ID: "deepseek-chat", API: APIOpenAICompletions, Provider: "deepseek", BaseURL: srv.URL}
	stream := openAICompletionsStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "sk-test"})

	events, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	wantTypes := []string{EventStart, EventTextStart, EventTextDelta, EventTextDelta, EventToolCallStart,
		EventToolCallDelta, EventToolCallDelta, EventToolCallEnd, EventDone}
	if len(events) != len(wantTypes) {
		t.Fatalf("got events %v, want %v", typesOf(events), wantTypes)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d: type = %q, want %q", i, events[i].Type, want)
		}
	}
	if msg.Text() != "Hello world" {
		t.Errorf("text = %q", msg.Text())
	}
	if msg.StopReason != StopReasonToolUse {
		t.Errorf("stop reason = %q, want %q", msg.StopReason, StopReasonToolUse)
	}
	if len(msg.Content) != 2 || msg.Content[1].Type != ContentTypeToolCall {
		t.Fatalf("expected text + tool call, got %+v", msg.Content)
	}
	tc := msg.Content[1]
	if tc.ToolCallID != "call_1" || tc.ToolName != "bash" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.Arguments["command"] != "ls" {
		t.Errorf("tool args = %v", tc.Arguments)
	}
	if msg.Usage == nil || msg.Usage.Input != 10 || msg.Usage.Output != 5 || msg.Usage.CacheRead != 3 || msg.Usage.Reasoning != 2 || msg.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", msg.Usage)
	}
}

func TestOpenAICompletionsRequestShape(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeSSE(w,
			`data: {"choices":[{"delta":{"content":"ok"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	model := &Model{ID: "deepseek-chat", API: APIOpenAICompletions, Provider: "deepseek", BaseURL: srv.URL}
	tool := &Tool{Name: "bash", Description: "Run a command", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}},
	}}
	messages := []*Message{
		NewUserTextMessage("list files"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("ok"), ToolCallBlock("call_1", "bash", map[string]any{"command": "ls"})}, StopReason: StopReasonToolUse},
		{Role: RoleToolResult, ToolCallID: "call_1", ToolName: "bash", Content: []ContentBlock{TextBlock("a.txt")}},
	}
	stream := openAICompletionsStream(model, &Context{SystemPrompt: "sys", Messages: messages, Tools: []*Tool{tool}}, &StreamOptions{APIKey: "sk-test", MaxTokens: 400})
	_, _, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody["model"] != "deepseek-chat" {
		t.Errorf("model = %v", gotBody["model"])
	}
	if gotBody["max_tokens"] != 400.0 {
		t.Errorf("max_tokens = %v", gotBody["max_tokens"])
	}
	msgs := gotBody["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("system message missing: %v", msgs[0])
	}
	// Assistant tool calls → tool_calls.
	assistant := msgs[2].(map[string]any)
	if assistant["content"] != "ok" {
		t.Errorf("assistant content = %v, want text kept", assistant["content"])
	}
	tcs := assistant["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["arguments"] != `{"command":"ls"}` || tc["id"] != "call_1" {
		t.Errorf("tool_calls conversion mismatch: %v", assistant)
	}
	// Tool result → role "tool".
	tr := msgs[3].(map[string]any)
	if tr["role"] != "tool" || tr["tool_call_id"] != "call_1" || tr["content"] != "a.txt" {
		t.Errorf("tool result conversion mismatch: %v", tr)
	}
	tools := gotBody["tools"].([]any)
	t0 := tools[0].(map[string]any)
	if t0["type"] != "function" {
		t.Errorf("tools = %v", t0)
	}
}

func TestOpenAICompletionsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"quota exceeded"}}`, 429)
	}))
	defer srv.Close()

	model := &Model{ID: "deepseek-chat", API: APIOpenAICompletions, Provider: "deepseek", BaseURL: srv.URL}
	stream := openAICompletionsStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "sk-test"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonError || !strings.Contains(msg.ErrorMessage, "429") {
		t.Errorf("error not encoded: %+v", msg)
	}
}

func TestOpenAICompletionsLengthStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			`data: {"choices":[{"delta":{"content":"partial"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":100,"total_tokens":101}}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	model := &Model{ID: "deepseek-chat", API: APIOpenAICompletions, Provider: "deepseek", BaseURL: srv.URL}
	stream := openAICompletionsStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "sk-test"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonLength {
		t.Errorf("stop reason = %q, want length", msg.StopReason)
	}
}

func TestOpenAICompletionsNoTools(t *testing.T) {
	// Models without tools defined: server responds without any tool calls.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, hasTools := body["tools"]; hasTools {
			t.Errorf("tools should be omitted when context has no tools: %v", body)
		}
		writeSSE(w,
			`data: {"choices":[{"delta":{"content":"done"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	model := &Model{ID: "deepseek-chat", API: APIOpenAICompletions, Provider: "deepseek", BaseURL: srv.URL}
	stream := openAICompletionsStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "sk-test"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonStop {
		t.Errorf("stop reason = %q", msg.StopReason)
	}
}

func TestOpenAICompletionsReasoningDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			`data: {"choices":[{"delta":{"role":"assistant","reasoning_content":""}}]}`,
			`data: {"choices":[{"delta":{"reasoning_content":"Let me"}}]}`,
			`data: {"choices":[{"delta":{"reasoning_content":" think"}}]}`,
			`data: {"choices":[{"delta":{"content":"Answer"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8,"completion_tokens_details":{"reasoning_tokens":2}}}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	model := &Model{ID: "deepseek-chat", API: APIOpenAICompletions, Provider: "deepseek", BaseURL: srv.URL, Reasoning: true}
	stream := openAICompletionsStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "sk-test"})

	events, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	wantTypes := []string{EventStart, EventThinkingStart, EventThinkingDelta, EventThinkingDelta, EventThinkingEnd,
		EventTextStart, EventTextDelta, EventDone}
	if len(events) != len(wantTypes) {
		t.Fatalf("got events %v, want %v", typesOf(events), wantTypes)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d: type = %q, want %q", i, events[i].Type, want)
		}
	}
	if len(msg.Content) != 2 || msg.Content[0].Type != ContentTypeThinking {
		t.Fatalf("expected thinking + text blocks, got %+v", msg.Content)
	}
	if msg.Content[0].Thinking != "Let me think" {
		t.Errorf("thinking = %q", msg.Content[0].Thinking)
	}
	if msg.Content[1].Text != "Answer" {
		t.Errorf("text = %q", msg.Content[1].Text)
	}
	if msg.Usage == nil || msg.Usage.Reasoning != 2 {
		t.Errorf("usage = %+v", msg.Usage)
	}
}

func TestOpenAICompletionsReasoningRoundTrip(t *testing.T) {
	for _, reasoning := range []bool{true, false} {
		var gotBody map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewDecoder(r.Body).Decode(&gotBody)
			writeSSE(w,
				`data: {"choices":[{"delta":{"content":"ok"}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			)
		}))
		model := &Model{ID: "deepseek-chat", API: APIOpenAICompletions, Provider: "deepseek", BaseURL: srv.URL, Reasoning: reasoning}
		messages := []*Message{
			NewUserTextMessage("hi"),
			{Role: RoleAssistant, Content: []ContentBlock{
				ThinkingBlock("deep thought"),
				TextBlock("ok"),
			}, StopReason: StopReasonStop},
		}
		stream := openAICompletionsStream(model, &Context{Messages: messages}, &StreamOptions{APIKey: "sk-test"})
		if _, _, err := collectStream(stream); err != nil {
			srv.Close()
			t.Fatalf("stream: %v", err)
		}
		msgs := gotBody["messages"].([]any)
		assistant := msgs[1].(map[string]any)
		rc, hasRC := assistant["reasoning_content"]
		if reasoning {
			if !hasRC || rc != "deep thought" {
				t.Errorf("reasoning model: reasoning_content = %v (present=%v), want round-trip", rc, hasRC)
			}
		} else if hasRC {
			t.Errorf("non-reasoning model: reasoning_content present = %v", rc)
		}
		srv.Close()
	}
}

func TestOpenAICompletionsNullContent(t *testing.T) {
	// Assistant messages without text must serialize content as null (not
	// an empty string): some gateways reject "" with a 500.
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		writeSSE(w,
			`data: {"choices":[{"delta":{"content":"ok"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	model := &Model{ID: "deepseek-v4-flash", API: APIOpenAICompletions, Provider: "model", BaseURL: srv.URL, Reasoning: true}
	messages := []*Message{
		NewUserTextMessage("run ls"),
		{Role: RoleAssistant, Content: []ContentBlock{
			ThinkingBlock("check the dir"),
			ToolCallBlock("call_1", "bash", map[string]any{"command": "ls"}),
		}, StopReason: StopReasonToolUse},
		{Role: RoleToolResult, ToolCallID: "call_1", ToolName: "bash", Content: []ContentBlock{TextBlock("cmd")}},
	}
	stream := openAICompletionsStream(model, &Context{Messages: messages}, &StreamOptions{APIKey: "sk-test"})
	if _, _, err := collectStream(stream); err != nil {
		t.Fatalf("stream: %v", err)
	}
	msgs := gotBody["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	c, ok := assistant["content"]
	if !ok || c != nil {
		t.Errorf("assistant content = %v (present=%v), want null", c, ok)
	}
	if assistant["reasoning_content"] != "check the dir" {
		t.Errorf("reasoning_content = %v", assistant["reasoning_content"])
	}
}
