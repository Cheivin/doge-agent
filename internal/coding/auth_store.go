package coding

import (
	"encoding/json"
	"os"
	"sync"

	"doge/internal/ai"
)

// AuthStore persists API keys per provider in auth.json
// {"provider": {"type": "api_key", "key": "..."}}).
type AuthStore struct {
	path string
	mu   sync.Mutex
	keys map[string]string
}

// LoadAuthStore loads (or lazily creates) the credential store.
func LoadAuthStore(path string) *AuthStore {
	s := &AuthStore{path: path, keys: map[string]string{}}
	if data, err := os.ReadFile(path); err == nil {
		var raw map[string]struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		}
		if json.Unmarshal(data, &raw) == nil {
			for provider, cred := range raw {
				if cred.Key != "" {
					s.keys[provider] = cred.Key
				}
			}
		}
	}
	return s
}

// GetAPIKey implements ai.CredentialStore.
func (s *AuthStore) GetAPIKey(provider string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[provider]
	return k, ok
}

// Reload re-reads auth.json from disk so credential edits take effect without
// a restart. A missing file clears the store; a parse error keeps the current
// keys.
func (s *AuthStore) Reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.keys = map[string]string{}
			s.mu.Unlock()
			return nil
		}
		return err
	}
	var raw map[string]struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	keys := map[string]string{}
	for provider, cred := range raw {
		if cred.Key != "" {
			keys[provider] = cred.Key
		}
	}
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
	return nil
}

// SetAPIKey stores a key and persists the store.
func (s *AuthStore) SetAPIKey(provider, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[provider] = key
	return s.persistLocked()
}

// DeleteAPIKey removes a key.
func (s *AuthStore) DeleteAPIKey(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, provider)
	return s.persistLocked()
}

func (s *AuthStore) persistLocked() error {
	raw := map[string]any{}
	for provider, key := range s.keys {
		raw[provider] = map[string]any{"type": "api_key", "key": key}
	}
	if err := os.MkdirAll(dirOf(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

var _ ai.CredentialStore = (*AuthStore)(nil)
