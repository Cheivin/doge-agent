package ai

import (
	"fmt"
	"os"
	"strings"
)

// CredentialStore is a persistent credential backend. The in-memory and
// file-based stores are provided by the coding layer.
type CredentialStore interface {
	// GetAPIKey returns the stored API key for a provider, if any.
	GetAPIKey(provider string) (string, bool)
}

// defaultEnvKeys maps provider ids to environment variables that may hold an
// API key environment variables for the supported providers.
var defaultEnvKeys = map[string][]string{
	"openai":     {"OPENAI_API_KEY"},
	"anthropic":  {"ANTHROPIC_API_KEY"},
	"deepseek":   {"DEEPSEEK_API_KEY"},
	"qwen":       {"QWEN_API_KEY", "DASHSCOPE_API_KEY"},
	"moonshot":   {"MOONSHOT_API_KEY"},
	"zhipu":      {"ZHIPU_API_KEY", "BIGMODEL_API_KEY"},
	"zai":        {"ZAI_API_KEY"},
	"mistral":    {"MISTRAL_API_KEY"},
	"groq":       {"GROQ_API_KEY"},
	"openrouter": {"OPENROUTER_API_KEY"},
	"xai":        {"XAI_API_KEY"},
	"cerebras":   {"CEREBRAS_API_KEY"},
	"fireworks":  {"FIREWORKS_API_KEY"},
	"together":   {"TOGETHER_API_KEY"},
	"nvidia":     {"NVIDIA_API_KEY"},
	"local":      {},
}

// envKeysFor returns the environment variables that may hold a key for the
// provider, falling back to the conventional <PROVIDER>_API_KEY name.
func envKeysFor(provider string) []string {
	if keys, ok := defaultEnvKeys[provider]; ok {
		return keys
	}
	name := strings.ToUpper(strings.ReplaceAll(provider, "-", "_"))
	if provider != "" {
		return []string{name + "_API_KEY"}
	}
	return nil
}

// ResolveAPIKey resolves the API key for a request with priority:
// explicit request key, then stored credential, then environment variables.
func ResolveAPIKey(model *Model, opts *StreamOptions, store CredentialStore) string {
	if opts != nil && opts.APIKey != "" {
		return opts.APIKey
	}
	if store != nil {
		if key, ok := store.GetAPIKey(model.Provider); ok && key != "" {
			return key
		}
	}
	for _, env := range envKeysFor(model.Provider) {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return ""
}

// MissingAPIKeyError builds the guidance message shown when no key is found.
func MissingAPIKeyError(model *Model) error {
	envs := envKeysFor(model.Provider)
	if len(envs) == 0 {
		return fmt.Errorf("no API key found for provider %q", model.Provider)
	}
	return fmt.Errorf(
		"no API key found for provider %q. Set the %s environment variable or pass an API key with --api-key.",
		model.Provider, strings.Join(envs, " or "),
	)
}
