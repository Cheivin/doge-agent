package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
)

// StreamOptions are the per-request options forwarded to API adapters,

type StreamOptions struct {
	// APIKey overrides any stored or ambient credential for this request.
	APIKey string

	// Headers are merged over the model's headers for this request.
	Headers map[string]string

	// MaxTokens caps the output token budget. Zero lets the adapter derive a
	// sensible default from the model's context window.
	MaxTokens int

	// Reasoning is the thinking level: "" or "off" disables it, otherwise
	// one of "minimal"|"low"|"medium"|"high"|"xhigh"|"max".
	Reasoning string

	// SessionID is forwarded to cache-aware backends for prompt caching.
	SessionID string

	// Context cancels the request. A canceled context produces a terminal
	// error event with StopReason "aborted".
	Context context.Context
}

// APIStreamFunc streams a single model request, dispatching to the API
// adapter registered for the model's API. It must never return an error:
// failures are encoded as error events in the returned stream.
type APIStreamFunc func(model *Model, ctx *Context, opts *StreamOptions) *EventStream

var (
	apiStreamsMu sync.RWMutex
	apiStreams   = map[string]APIStreamFunc{}
)

// RegisterAPIStream registers the stream implementation for an API adapter
// identifier (see the API* constants). Adapters register themselves in init.
func RegisterAPIStream(api string, fn APIStreamFunc) {
	apiStreamsMu.Lock()
	defer apiStreamsMu.Unlock()
	if _, exists := apiStreams[api]; exists {
		panic("duplicate API stream registration for " + api)
	}
	apiStreams[api] = fn
}

func getAPIStream(api string) (APIStreamFunc, bool) {
	apiStreamsMu.RLock()
	defer apiStreamsMu.RUnlock()
	fn, ok := apiStreams[api]
	return fn, ok
}

// Provider describes a service endpoint that hosts models.
type Provider struct {
	ID      string
	Name    string
	BaseURL string

	// EnvKeys are the environment variables that may hold an API key,
	// overriding the defaults in auth.go.
	EnvKeys []string

	// RequiresKey controls whether requests fail when no API key resolves.
	// Local endpoints (e.g. llama.cpp) set this to false.
	RequiresKey bool
}

// Models is the model catalog registry with request dispatch.
type Models struct {
	mu        sync.RWMutex
	models    map[string]*Model
	providers map[string]*Provider
	store     CredentialStore
}

// NewModels creates an empty registry. store may be nil.
func NewModels(store CredentialStore) *Models {
	return &Models{
		models:    map[string]*Model{},
		providers: map[string]*Provider{},
		store:     store,
	}
}

// AddProvider registers a provider endpoint.
func (m *Models) AddProvider(p *Provider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p != nil {
		m.providers[p.ID] = p
	}
}

// Reset clears all registered models and providers (used when reloading
// models.json).
func (m *Models) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models = map[string]*Model{}
	m.providers = map[string]*Provider{}
}

// AddModel registers a model under its "provider/id" key.
func (m *Models) AddModel(model *Model) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models[model.Provider+"/"+model.ID] = model
}

// GetModel looks up a model by provider and id.
func (m *Models) GetModel(provider, id string) (*Model, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	model, ok := m.models[provider+"/"+id]
	return model, ok
}

// AllModels returns all registered models.
func (m *Models) AllModels() []*Model {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Model, 0, len(m.models))
	for _, model := range m.models {
		out = append(out, model)
	}
	return out
}

// AvailableModels returns the models that can actually be streamed: they
// either resolve an API key (stored credential or environment variable), or
// belong to a provider that needs no key (e.g. a local endpoint). This
// mirrors the missing-key rejection in Stream, so a selectable model will not
// fail at request time for want of credentials.
func (m *Models) AvailableModels() []*Model {
	var out []*Model
	for _, model := range m.AllModels() {
		if m.usable(model) {
			out = append(out, model)
		}
	}
	return out
}

// usable reports whether a model can stream without a missing-key error:
// either a key resolves (stored credential or environment variable) or the
// endpoint is a local one that needs no key. Built-in remote providers leave
// RequiresKey unset, so availability is decided purely by credential
// resolution and the local-endpoint rule — exactly what Stream will accept.
func (m *Models) usable(model *Model) bool {
	if model == nil {
		return false
	}
	return ResolveAPIKey(model, nil, m.store) != "" || isLocalBaseURL(model.BaseURL)
}

// Providers returns all registered providers.
func (m *Models) Providers() []*Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Provider, 0, len(m.providers))
	for _, p := range m.providers {
		out = append(out, p)
	}
	return out
}

func (m *Models) provider(id string) *Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.providers[id]
}

// Stream dispatches a request to the adapter registered for the model's API.
// Auth resolution happens asynchronously inside a lazy stream: a missing key
// produces an error event with guidance, never a Go error.
func (m *Models) Stream(model *Model, ctx *Context, opts *StreamOptions) *EventStream {
	o := &StreamOptions{}
	if opts != nil {
		*o = *opts
	}
	return LazyStream(func() (*EventStream, error) {
		if model == nil {
			return nil, errors.New("no model selected")
		}
		fn, ok := getAPIStream(model.API)
		if !ok {
			return nil, fmt.Errorf("no stream implementation for API %q (model %s)", model.API, model.ID)
		}
		o.APIKey = ResolveAPIKey(model, o, m.store)
		p := m.provider(model.Provider)
		if o.APIKey == "" && (p == nil || p.RequiresKey) && !isLocalBaseURL(model.BaseURL) {
			return nil, MissingAPIKeyError(model)
		}
		return fn(model, ctx, o), nil
	})
}

// DefaultStream dispatches a request through the global API registry without
// a credential store: explicit keys and environment variables only.
func DefaultStream(model *Model, ctx *Context, opts *StreamOptions) *EventStream {
	return NewModels(nil).Stream(model, ctx, opts)
}

// isLocalBaseURL reports whether the endpoint is a local host (localhost,
// loopback), which typically needs no API key.
func isLocalBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// ApplyModelHeaders merges model-level headers with request-level headers,
// request-level winning per header. Keys are canonicalized so a lowercased
// "user-agent" overrides "User-Agent" deterministically. An empty value marks
// the header for suppression — a null in models.json or a request-level
// header set to "" removes the header and any default for it.
func ApplyModelHeaders(model *Model, opts *StreamOptions) map[string]string {
	merged := map[string]string{}
	merge := func(src map[string]string) {
		for k, v := range src {
			if k == "" {
				continue
			}
			merged[http.CanonicalHeaderKey(k)] = v
		}
	}
	if model != nil {
		merge(model.Headers)
	}
	if opts != nil {
		merge(opts.Headers)
	}
	return merged
}

// applySessionAffinityHeaders adds cache-affinity headers when the model opts
// in (SessionAffinity non-empty) and a session ID is present. Mirrors Pi's
// compat.sendSessionAffinityHeaders / sessionAffinityFormat:
//
//	"openrouter": x-session-id
//	"openai":     session_id, x-client-request-id, x-session-affinity
//	other:        x-client-request-id, x-session-affinity
func applySessionAffinityHeaders(headers map[string]string, model *Model, opts *StreamOptions) {
	if model == nil || opts == nil || model.SessionAffinity == "" || opts.SessionID == "" {
		return
	}
	switch model.SessionAffinity {
	case "openrouter":
		headers["x-session-id"] = opts.SessionID
	case "openai":
		headers["session_id"] = opts.SessionID
		headers["x-client-request-id"] = opts.SessionID
		headers["x-session-affinity"] = opts.SessionID
	default:
		headers["x-client-request-id"] = opts.SessionID
		headers["x-session-affinity"] = opts.SessionID
	}
}
