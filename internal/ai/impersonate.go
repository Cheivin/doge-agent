package ai

import "strings"

// impersonationPreset is a known coding client identity: the User-Agent plus
// a few attribution headers that aggregator gateways key on to restrict or
// price per client tool. Only request headers are rewritten — no auth is
// bypassed; whether a given client identity may consume a gateway is the
// gateway's terms to decide.
type impersonationPreset struct {
	UserAgent string
	Headers   map[string]string
}

// impersonationPresets maps an "impersonate" value to the client identity its
// headers declare, verified against each tool's source:
//
//   - claude-code: anthropics/claude-code sends user-agent "claude-cli/<ver>
//     (external, cli)" plus x-app: cli (the OAuth route also carries the
//     claude-code anthropic-beta headers).
//   - codex: openai/codex sends user-agent "codex_cli_rs/<ver>" plus
//     originator: codex_cli_rs. The backend whitelists codex_cli_rs /
//     codex_vscode / codex_sdk_ts / Codex*; a wrong originator is rejected
//     with 403.
//   - openclaw: openclaw/openclaw sends "openclaw/<ver>" (lowercase, date-based
//     versions) and, on verified native endpoints, OpenRouter attribution
//     headers carrying the full category list.
//
// opencode (sst/opencode) and cursor (closed source) are intentionally absent:
// opencode sets no User-Agent anywhere in its LLM transport, and cursor's
// "cursor-agent/<ver>" UA is closed-source with no publicly verifiable value.
//
// Version strings are representative and drift with upstream releases; if a
// gateway expects a specific version, set it explicitly via the model's
// "headers" (which override these).
var impersonationPresets = map[string]impersonationPreset{
	"claude-code": {
		UserAgent: "claude-cli/2.1.75 (external, cli)",
		Headers:   map[string]string{"x-app": "cli"},
	},
	"codex": {
		UserAgent: "codex_cli_rs/0.0.1",
		Headers:   map[string]string{"originator": "codex_cli_rs"},
	},
	"openclaw": {
		UserAgent: "openclaw/2026.7.1",
		Headers: map[string]string{
			"HTTP-Referer":            "https://openclaw.ai",
			"X-OpenRouter-Title":      "OpenClaw",
			"X-OpenRouter-Categories": "cli-agent,cloud-agent,programming-app,creative-writing,writing-assistant,general-chat,personal-agent",
		},
	},
}

// applyImpersonation injects the impersonated client's identity headers into
// headers. It runs before explicit model/request headers so those still win.
func applyImpersonation(headers map[string]string, model *Model) {
	if model == nil || model.Impersonate == "" {
		return
	}
	preset, ok := impersonationPresets[strings.ToLower(model.Impersonate)]
	if !ok {
		return
	}
	if preset.UserAgent != "" {
		headers["User-Agent"] = preset.UserAgent
	}
	for k, v := range preset.Headers {
		headers[k] = v
	}
}
