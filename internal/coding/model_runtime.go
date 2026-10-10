package coding

import (
	"fmt"
	"strings"

	"doge/internal/ai"
)

// ModelRuntime wires the model catalog, credential store, and streaming
// dispatch together.
type ModelRuntime struct {
	Models *ai.Models
	Auth   *AuthStore
}

// NewModelRuntime builds the runtime from the catalog and auth store.
func NewModelRuntime(auth *AuthStore) *ModelRuntime {
	models := ai.NewModels(auth)
	RegisterBuiltinProviders(models)
	for _, m := range builtinModels {
		models.AddModel(m)
	}
	return &ModelRuntime{Models: models, Auth: auth}
}

// AddCustomModels registers user-defined models from models.json.
func (r *ModelRuntime) AddCustomModels(modelsFile string) error {
	custom, err := LoadCustomModels(modelsFile)
	if err != nil {
		return err
	}
	for _, m := range custom {
		r.Models.AddModel(m)
	}
	return nil
}

// Reload re-reads models.json and rebuilds the catalog (built-ins plus custom
// models), so price/billing edits take effect without a restart. A missing
// file keeps just the built-ins; a parse error leaves the catalog untouched.
func (r *ModelRuntime) Reload(modelsFile string) error {
	custom, err := LoadCustomModels(modelsFile)
	if err != nil {
		return err
	}
	r.Models.Reset()
	RegisterBuiltinProviders(r.Models)
	for _, m := range builtinModels {
		r.Models.AddModel(m)
	}
	for _, m := range custom {
		r.Models.AddModel(m)
	}
	return nil
}

// ResolveModel parses "provider/model[:thinking]" and returns the model.
// A bare model id is searched across providers.
func (r *ModelRuntime) ResolveModel(spec string) (*ai.Model, string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return r.DefaultModel()
	}
	thinking := ""
	if idx := strings.LastIndex(spec, ":"); idx >= 0 {
		thinking = spec[idx+1:]
		spec = spec[:idx]
	}
	if strings.Contains(spec, "/") {
		parts := strings.SplitN(spec, "/", 2)
		model, ok := r.Models.GetModel(parts[0], parts[1])
		if !ok {
			return nil, "", fmt.Errorf("unknown model %q (provider %q)", parts[1], parts[0])
		}
		return model, thinking, nil
	}
	// Bare id: search all providers.
	for _, m := range r.Models.AllModels() {
		if m.ID == spec {
			return m, thinking, nil
		}
	}
	return nil, "", fmt.Errorf("unknown model %q. Use provider/model syntax (e.g. openai/gpt-5.2)", spec)
}

// DefaultModel returns the settings default or the first catalog model.
func (r *ModelRuntime) DefaultModel() (*ai.Model, string, error) {
	// Fall back to openai/gpt-5.2 (a sensible default).
	model, ok := r.Models.GetModel("openai", "gpt-5.2")
	if !ok {
		models := r.Models.AllModels()
		if len(models) == 0 {
			return nil, "", fmt.Errorf("no models available")
		}
		return models[0], "", nil
	}
	return model, "", nil
}
