package ai

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// TestDefaultUserAgent verifies the UA is stamped with the (settable) version
// and the current platform, e.g. "doge/0.2.0 (darwin arm64)".
func TestDefaultUserAgent(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	Version = "0.3.0-test"

	ua := defaultUserAgent()
	if !strings.HasPrefix(ua, "doge/0.3.0-test (") {
		t.Errorf("user agent = %q, want doge/<version> (goos goarch) prefix", ua)
	}
	if !strings.Contains(ua, runtime.GOOS+" "+runtime.GOARCH+")") {
		t.Errorf("user agent = %q, missing platform %s %s", ua, runtime.GOOS, runtime.GOARCH)
	}
}

// TestApplyModelHeadersMergeAndSuppress covers canonical-key merging, request
// precedence, and null (empty-value) suppression.
func TestApplyModelHeadersMergeAndSuppress(t *testing.T) {
	model := &Model{Headers: map[string]string{
		"user-agent":    "pi (custom)", // lowercase key must canonicalize
		"Authorization": "Bearer model",
		"X-Extra":       "keep",
	}}
	opts := &StreamOptions{Headers: map[string]string{
		"X-Extra":       "request-wins",
		"Authorization": "", // null → suppress the model/default header
	}}

	got := ApplyModelHeaders(model, opts)

	if got["User-Agent"] != "pi (custom)" {
		t.Errorf("User-Agent = %q, want canonicalized model header", got["User-Agent"])
	}
	if got["X-Extra"] != "request-wins" {
		t.Errorf("X-Extra = %q, want request-level override", got["X-Extra"])
	}
	if _, ok := got["Authorization"]; !ok {
		t.Fatal("Authorization missing from merged map")
	}
	if got["Authorization"] != "" {
		t.Errorf("Authorization = %q, want empty (suppressed)", got["Authorization"])
	}
}

// TestApplySessionAffinityHeaders covers the openai / openrouter formats and
// the disabled cases.
func TestApplySessionAffinityHeaders(t *testing.T) {

	t.Run("disabled by default", func(t *testing.T) {
		h := map[string]string{}
		applySessionAffinityHeaders(h, &Model{}, &StreamOptions{SessionID: "s1"})
		if len(h) != 0 {
			t.Errorf("unexpected headers: %v", h)
		}
	})

	t.Run("no session id", func(t *testing.T) {
		h := map[string]string{}
		applySessionAffinityHeaders(h, &Model{SessionAffinity: "openai"}, &StreamOptions{})
		if len(h) != 0 {
			t.Errorf("unexpected headers: %v", h)
		}
	})

	t.Run("openai format", func(t *testing.T) {
		h := map[string]string{}
		applySessionAffinityHeaders(h, &Model{SessionAffinity: "openai"}, &StreamOptions{SessionID: "s1"})
		want := map[string]string{
			"session_id":          "s1",
			"x-client-request-id": "s1",
			"x-session-affinity":  "s1",
		}
		for k, v := range want {
			if h[k] != v {
				t.Errorf("%s = %q, want %q (got %v)", k, h[k], v, h)
			}
		}
	})

	t.Run("openrouter format", func(t *testing.T) {
		h := map[string]string{}
		applySessionAffinityHeaders(h, &Model{SessionAffinity: "openrouter"}, &StreamOptions{SessionID: "s1"})
		if h["x-session-id"] != "s1" {
			t.Errorf("x-session-id = %q, want s1", h["x-session-id"])
		}
		if _, ok := h["x-session-affinity"]; ok {
			t.Errorf("openrouter format must not send x-session-affinity: %v", h)
		}
	})

	t.Run("custom format", func(t *testing.T) {
		h := map[string]string{}
		applySessionAffinityHeaders(h, &Model{SessionAffinity: "custom"}, &StreamOptions{SessionID: "s1"})
		if h["x-client-request-id"] != "s1" || h["x-session-affinity"] != "s1" {
			t.Errorf("unexpected headers: %v", h)
		}
	})
}

// TestDoStreamRequestHeaderDefaultsAndSuppression verifies the request layer
// sends the default UA/Accept/Content-Type, honors custom overrides, and drops
// headers suppressed with an empty value.
func TestDoStreamRequestHeaderDefaultsAndSuppression(t *testing.T) {
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		writeSSE(w,
			`data: {"choices":[{"delta":{"content":"ok"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	t.Run("defaults present", func(t *testing.T) {
		resp, err := doStreamRequest(t.Context(), srv.URL, nil, []byte(`{}`))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if got := captured.Get("User-Agent"); got != defaultUserAgent() {
			t.Errorf("User-Agent = %q, want %q", got, defaultUserAgent())
		}
		if captured.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q", captured.Get("Accept"))
		}
		if captured.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", captured.Get("Content-Type"))
		}
	})

	t.Run("custom override wins", func(t *testing.T) {
		headers := map[string]string{"User-Agent": "pi (custom)", "X-Extra": "1"}
		resp, err := doStreamRequest(t.Context(), srv.URL, headers, []byte(`{}`))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if got := captured.Get("User-Agent"); got != "pi (custom)" {
			t.Errorf("User-Agent = %q, want pi (custom)", got)
		}
		if captured.Get("X-Extra") != "1" {
			t.Errorf("X-Extra = %q", captured.Get("X-Extra"))
		}
	})

	t.Run("empty value suppresses default", func(t *testing.T) {
		headers := map[string]string{"User-Agent": "", "Accept": ""}
		resp, err := doStreamRequest(t.Context(), srv.URL, headers, []byte(`{}`))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if got := captured.Get("User-Agent"); got != "" {
			t.Errorf("User-Agent = %q, want suppressed", got)
		}
		if got := captured.Get("Accept"); got != "" {
			t.Errorf("Accept = %q, want suppressed", got)
		}
		if captured.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want kept", captured.Get("Content-Type"))
		}
	})
}

// TestOpenAICompletionsHeaderWiring verifies the adapter sends the session
// affinity header end to end and that a null model header suppresses the
// default Authorization.
func TestOpenAICompletionsHeaderWiring(t *testing.T) {
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		writeSSE(w,
			`data: {"choices":[{"delta":{"content":"ok"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	model := &Model{ID: "m", API: APIOpenAICompletions, Provider: "p", BaseURL: srv.URL, SessionAffinity: "openrouter"}
	ctx := &Context{Messages: []*Message{NewUserTextMessage("hi")}}
	opts := &StreamOptions{APIKey: "k", SessionID: "sess-1"}
	_, msg, err := collectStream(openAICompletionsStream(model, ctx, opts))
	if err != nil || msg.Text() != "ok" {
		t.Fatalf("stream: msg=%+v err=%v", msg, err)
	}
	if captured.Get("x-session-id") != "sess-1" {
		t.Errorf("x-session-id = %q, want sess-1", captured.Get("x-session-id"))
	}
	if captured.Get("Authorization") != "Bearer k" {
		t.Errorf("Authorization = %q", captured.Get("Authorization"))
	}

	// Null model header suppresses the default Authorization.
	suppressed := &Model{ID: "m", API: APIOpenAICompletions, Provider: "p", BaseURL: srv.URL,
		Headers: map[string]string{"Authorization": ""}}
	_, _, err = collectStream(openAICompletionsStream(suppressed, ctx, &StreamOptions{APIKey: "k"}))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := captured.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want suppressed", got)
	}
}
