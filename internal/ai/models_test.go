package ai

import (
	"strings"
	"testing"
)

type memStore struct {
	keys map[string]string
}

func (s *memStore) GetAPIKey(provider string) (string, bool) {
	k, ok := s.keys[provider]
	return k, ok
}

func TestModelsStreamDispatch(t *testing.T) {
	srv := newResponsesServer(t, nil)
	defer srv.Close()

	m := NewModels(&memStore{keys: map[string]string{"openai": "stored-key"}})
	m.AddModel(&Model{ID: "gpt-5.2", API: APIOpenAIResponses, Provider: "openai", BaseURL: srv.URL})

	model, ok := m.GetModel("openai", "gpt-5.2")
	if !ok {
		t.Fatal("model not found")
	}
	stream := m.Stream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, nil)
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason == "" {
		t.Errorf("expected a normal stream, got %+v", msg)
	}
}

func TestModelsMissingAPIKey(t *testing.T) {
	// No key anywhere: the stream must encode the guidance as an error event.
	m := NewModels(nil)
	m.AddModel(&Model{ID: "gpt-5.2", API: APIOpenAIResponses, Provider: "openai", BaseURL: "http://api.example.com/v1"})
	model, _ := m.GetModel("openai", "gpt-5.2")
	stream := m.Stream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, nil)
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonError {
		t.Errorf("stop reason = %q, want error", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "OPENAI_API_KEY") {
		t.Errorf("guidance missing env var name: %q", msg.ErrorMessage)
	}
}

func TestModelsUnknownAPI(t *testing.T) {
	m := NewModels(nil)
	m.AddModel(&Model{ID: "x", API: "unknown-api", Provider: "local", BaseURL: "http://localhost:1"})
	model, _ := m.GetModel("local", "x")
	stream := m.Stream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, &StreamOptions{APIKey: "k"})
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonError || !strings.Contains(msg.ErrorMessage, "unknown-api") {
		t.Errorf("error not encoded: %+v", msg)
	}
}

func TestModelsLocalNoKeyRequired(t *testing.T) {
	srv := newResponsesServer(t, nil)
	defer srv.Close()

	m := NewModels(nil)
	m.AddProvider(&Provider{ID: "local", Name: "Local", BaseURL: srv.URL, RequiresKey: false})
	m.AddModel(&Model{ID: "llama", API: APIOpenAIResponses, Provider: "local", BaseURL: srv.URL})
	model, _ := m.GetModel("local", "llama")
	stream := m.Stream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}}, nil)
	_, msg, err := collectStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.StopReason != StopReasonToolUse && msg.StopReason != StopReasonStop {
		t.Errorf("expected a normal stream for keyless local provider, got %+v", msg)
	}
}

func TestResolveAPIKeyPriority(t *testing.T) {
	model := &Model{Provider: "openai"}
	store := &memStore{keys: map[string]string{"openai": "stored"}}

	// Explicit option wins.
	if got := ResolveAPIKey(model, &StreamOptions{APIKey: "explicit"}, store); got != "explicit" {
		t.Errorf("explicit = %q", got)
	}
	// Stored credential wins over env.
	t.Setenv("OPENAI_API_KEY", "env-key")
	if got := ResolveAPIKey(model, &StreamOptions{}, store); got != "stored" {
		t.Errorf("stored = %q", got)
	}
	// Env fallback.
	if got := ResolveAPIKey(model, &StreamOptions{}, nil); got != "env-key" {
		t.Errorf("env = %q", got)
	}
	// Unknown provider falls back to the conventional env var name.
	other := &Model{Provider: "my-custom-provider"}
	t.Setenv("MY_CUSTOM_PROVIDER_API_KEY", "custom")
	if got := ResolveAPIKey(other, &StreamOptions{}, nil); got != "custom" {
		t.Errorf("convention env = %q", got)
	}
}

func TestModelsAvailableFilter(t *testing.T) {
	m := NewModels(&memStore{keys: map[string]string{"openai": "stored-key"}})

	// openai/gpt-5.2 has a stored key -> available.
	m.AddModel(&Model{ID: "gpt-5.2", API: APIOpenAIResponses, Provider: "openai", BaseURL: "https://api.openai.com/v1"})
	// anthropic has no key anywhere and is remote -> unavailable, even though
	// its provider does not set RequiresKey.
	m.AddModel(&Model{ID: "claude-opus-4-1", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: "https://api.anthropic.com/v1"})
	// Local endpoint needs no key -> available.
	m.AddModel(&Model{ID: "llama", API: APIOpenAIResponses, Provider: "local", BaseURL: "http://127.0.0.1:8080/v1"})
	// deepseek keyed only via env -> available.
	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	m.AddModel(&Model{ID: "deepseek-chat", API: APIOpenAICompletions, Provider: "deepseek", BaseURL: "https://api.deepseek.com/v1"})
	// Custom remote provider with no key and no registered provider -> unavailable.
	m.AddModel(&Model{ID: "custom", API: APIOpenAICompletions, Provider: "acme", BaseURL: "https://api.acme.example/v1"})

	got := map[string]bool{}
	for _, model := range m.AvailableModels() {
		got[model.Provider+"/"+model.ID] = true
	}
	for _, want := range []string{
		"openai/gpt-5.2",
		"local/llama",
		"deepseek/deepseek-chat",
	} {
		if !got[want] {
			t.Errorf("expected %q in available models", want)
		}
	}
	for _, banned := range []string{
		"anthropic/claude-opus-4-1",
		"acme/custom",
	} {
		if got[banned] {
			t.Errorf("%q should not be selectable without a key", banned)
		}
	}
}
