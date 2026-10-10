package ai

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestApplyImpersonationPresets verifies each known client sets its expected
// User-Agent and signature headers.
func TestApplyImpersonationPresets(t *testing.T) {
	cases := []struct {
		tool  string
		ua    string
		extra map[string]string
	}{
		{"claude-code", "claude-cli/2.1.75 (external, cli)", map[string]string{"x-app": "cli"}},
		{"codex", "codex_cli_rs/0.0.1", map[string]string{"originator": "codex_cli_rs"}},
		{"openclaw", "openclaw/2026.7.1", map[string]string{
			"HTTP-Referer":            "https://openclaw.ai",
			"X-OpenRouter-Title":      "OpenClaw",
			"X-OpenRouter-Categories": "cli-agent,cloud-agent,programming-app,creative-writing,writing-assistant,general-chat,personal-agent",
		}},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			h := map[string]string{}
			applyImpersonation(h, &Model{Impersonate: c.tool})
			if h["User-Agent"] != c.ua {
				t.Errorf("User-Agent = %q, want %q", h["User-Agent"], c.ua)
			}
			for k, v := range c.extra {
				if h[k] != v {
					t.Errorf("%s = %q, want %q (got %v)", k, h[k], v, h)
				}
			}
		})
	}
}

// TestApplyImpersonationNoOp verifies empty and unknown values leave headers
// untouched, and the tool name is matched case-insensitively.
func TestApplyImpersonationNoOp(t *testing.T) {
	for _, tool := range []string{"", "unknown-tool", "cursor", "opencode"} {
		h := map[string]string{"X-Keep": "1"}
		applyImpersonation(h, &Model{Impersonate: tool})
		if h["User-Agent"] != "" || len(h) != 1 {
			t.Errorf("impersonate %q changed headers: %v", tool, h)
		}
	}

	h := map[string]string{}
	applyImpersonation(h, &Model{Impersonate: "Codex"})
	if h["User-Agent"] != "codex_cli_rs/0.0.1" {
		t.Errorf("User-Agent = %q, want case-insensitive match", h["User-Agent"])
	}
}

// TestApplyImpersonationExplicitWins verifies explicit model headers override
// the impersonated identity, and a null can suppress its UA.
func TestApplyImpersonationExplicitWins(t *testing.T) {
	// Mirror the adapter flow: impersonation first, explicit headers after.
	merge := func(model *Model) map[string]string {
		h := map[string]string{}
		applyImpersonation(h, model)
		for k, v := range ApplyModelHeaders(model, nil) {
			h[k] = v
		}
		return h
	}

	model := &Model{Impersonate: "codex", Headers: map[string]string{"User-Agent": "codex_cli_rs/0.1.0"}}
	if h := merge(model); h["User-Agent"] != "codex_cli_rs/0.1.0" {
		t.Errorf("User-Agent = %q, want explicit override", h["User-Agent"])
	}

	suppressed := &Model{Impersonate: "codex", Headers: map[string]string{"User-Agent": ""}}
	if h := merge(suppressed); h["User-Agent"] != "" {
		t.Errorf("User-Agent = %q, want suppressed by null", h["User-Agent"])
	}
}

// TestOpenAICompletionsImpersonation verifies the adapter sends the
// impersonated UA on the wire and that an explicit header overrides it.
func TestOpenAICompletionsImpersonation(t *testing.T) {
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

	ctx := &Context{Messages: []*Message{NewUserTextMessage("hi")}}

	// Default impersonation UA reaches the gateway.
	model := &Model{ID: "m", API: APIOpenAICompletions, Provider: "p", BaseURL: srv.URL, Impersonate: "codex"}
	if _, msg, err := collectStream(openAICompletionsStream(model, ctx, &StreamOptions{APIKey: "k"})); err != nil || msg.Text() != "ok" {
		t.Fatalf("stream: msg=%+v err=%v", msg, err)
	}
	if got := captured.Get("User-Agent"); got != "codex_cli_rs/0.0.1" {
		t.Errorf("User-Agent = %q, want impersonated codex", got)
	}

	// Explicit model header overrides the impersonated UA.
	override := &Model{ID: "m", API: APIOpenAICompletions, Provider: "p", BaseURL: srv.URL,
		Impersonate: "codex", Headers: map[string]string{"User-Agent": "codex_cli_rs/0.1.0"}}
	if _, msg, err := collectStream(openAICompletionsStream(override, ctx, &StreamOptions{APIKey: "k"})); err != nil || msg.Text() != "ok" {
		t.Fatalf("stream: msg=%+v err=%v", msg, err)
	}
	if got := captured.Get("User-Agent"); got != "codex_cli_rs/0.1.0" {
		t.Errorf("User-Agent = %q, want explicit override", got)
	}
}
