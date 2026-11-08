package coding

import (
	"fmt"
	"strings"
	"sync"

	"doge/internal/ai"
	"doge/pkg/logx"
)

// ModelRuntime wires the model catalog, credential store, and streaming
// dispatch together.
type ModelRuntime struct {
	Models *ai.Models
	Auth   *AuthStore

	// customFirst 是 models.json 里的第一个自定义模型（用户显式配置的第一个，
	// 默认模型 fallback 链的第二优先级）；AddCustomModels/Reload 时更新。
	// mu 保护 customFirst 与 Reload 的整体过程：HTTP 配置写盘后的热重载扇出
	// 会并发触发多个会话各 Reload 一次共享 runtime（web/config.go
	// reloadAfterConfigWrite），无锁读写 slice 头/指针会撞 race
	mu          sync.Mutex
	customFirst *ai.Model
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
	r.mu.Lock()
	defer r.mu.Unlock()
	custom, err := LoadCustomModels(modelsFile)
	if err != nil {
		return err
	}
	for _, m := range custom {
		r.Models.AddModel(m)
	}
	if len(custom) > 0 && custom[0] != nil {
		r.customFirst = custom[0]
	}
	return nil
}

// Reload re-reads models.json and rebuilds the catalog (built-ins plus custom
// models), so price/billing edits take effect without a restart. A missing
// file keeps just the built-ins; a parse error leaves the catalog untouched.
// 串行化并发 Reload：Reset 与 re-add 交错会让并发 GetModel 瞬时 miss，
// SwitchModel 报 unknown model
func (r *ModelRuntime) Reload(modelsFile string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	r.customFirst = nil
	if len(custom) > 0 && custom[0] != nil {
		r.customFirst = custom[0]
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

// DefaultModel returns the default model with a fallback chain: the first
// custom model from models.json (the user's own configuration), then the
// builtin openai/gpt-5.2, then the first catalog entry. Callers wanting the
// settings default resolve it themselves and fall back here when it no longer
// exists (renamed/deleted models.json entry).
func (r *ModelRuntime) DefaultModel() (*ai.Model, string, error) {
	// 用户配置的第一个自定义模型优先（快照读，避免与 Reload 竞态）
	r.mu.Lock()
	first := r.customFirst
	r.mu.Unlock()
	if first != nil {
		if m, ok := r.Models.GetModel(first.Provider, first.ID); ok {
			return m, "", nil
		}
	}
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

// ResolveDefaultWithFallback 解析 settings 默认模型；配置指向的模型已不存在
// （改名/删除）时降级到 DefaultModel 并记日志，绝不因配置漂移拒绝启动。
func (r *ModelRuntime) ResolveDefaultWithFallback(defaultProvider, defaultModel string) (*ai.Model, string) {
	if defaultProvider != "" && defaultModel != "" {
		if m, _, err := r.ResolveModel(defaultProvider + "/" + defaultModel); err == nil {
			return m, ""
		}
		logx.Warnf("[runtime] settings 默认模型 %s/%s 不存在，回退到可用目录的第一个", defaultProvider, defaultModel)
	}
	m, _, err := r.DefaultModel()
	if err != nil {
		return nil, ""
	}
	return m, ""
}
