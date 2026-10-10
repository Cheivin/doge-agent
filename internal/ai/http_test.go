package ai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRetryAbsorbsTransient500 verifies the adapter retries gateway 5xx
// responses (e.g. aggregator "Router.Unavailable") before failing.
func TestRetryAbsorbsTransient500(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			http.Error(w, `{"type":"Router.Unavailable"}`, 500)
			return
		}
		writeSSE(w,
			`data: {"choices":[{"delta":{"content":"ok"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	model := &Model{ID: "deepseek-v4-flash", API: APIOpenAICompletions, Provider: "model", BaseURL: srv.URL, Reasoning: true}
	stream := openAICompletionsStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "k"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonStop || msg.Text() != "ok" {
		t.Fatalf("msg = %+v", msg)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (two retries after 500s)", attempts)
	}
}

// TestRetryGivesUpAfterMaxAttempts: a persistent 5xx surfaces the final
// gateway error message after the bounded retries.
func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, `{"error":"down"}`, 503)
	}))
	defer srv.Close()

	model := &Model{ID: "m", API: APIOpenAICompletions, Provider: "p", BaseURL: srv.URL}
	stream := openAICompletionsStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "k"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want error", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "503") || !strings.Contains(msg.ErrorMessage, "down") {
		t.Errorf("error message = %q", msg.ErrorMessage)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

// TestRetryUpstream400: a 400 from the provider's internals
// ("InternalError.Algo") is transient and gets retried.
func TestRetryUpstream400(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			http.Error(w, `<400> InternalError.Algo.InvalidParameter: The content field is a required field.`, 400)
			return
		}
		writeSSE(w,
			`data: {"choices":[{"delta":{"content":"ok"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	model := &Model{ID: "m", API: APIOpenAICompletions, Provider: "p", BaseURL: srv.URL}
	stream := openAICompletionsStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "k"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.Text() != "ok" {
		t.Fatalf("msg = %+v", msg)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

// TestNoRetryPlain400: an ordinary 400 (client error) is not retried and
// its body is preserved for the error message.
func TestNoRetryPlain400(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, `{"error":{"message":"bad request"}}`, 400)
	}))
	defer srv.Close()

	model := &Model{ID: "m", API: APIOpenAICompletions, Provider: "p", BaseURL: srv.URL}
	stream := openAICompletionsStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "k"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonError || !strings.Contains(msg.ErrorMessage, "bad request") {
		t.Fatalf("msg = %+v", msg)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry for plain 400)", attempts)
	}
}
