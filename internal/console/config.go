package console

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"doge/internal/ai"
	"doge/internal/coding"
	"doge/pkg/logx"
)

// ConfigDeps 配置操作的会话上下文：cwd 决定项目层 settings 文件位置；
// ReloadAll 是配置写盘后的会话热重载扇出（web: 控制表活跃会话；desktop:
// Backend + 会话注册表）；ModelCurrent/ModelFallback/ModelSwitch 服务于
// "删除/改名活跃模型"的回退与重指向。
type ConfigDeps struct {
	Cwd           string
	ReloadAll     func() error
	ModelCurrent  func() string
	ModelFallback func() []string // 可用模型 spec（取第一个可用者回退）
	ModelSwitch   func(spec string) error
}

// configMu 串行化配置文件的读-改-写段（settings/auth/models）：并发写都是
// Load→改→Save，无锁并发丢更新
var configMu sync.Mutex

// authEntry auth.json 的单条目（type 恒 api_key）
type authEntry struct {
	Key  string `json:"key"`
	Type string `json:"type"`
}

// readAuthKeys 解析 auth.json 为 provider → key（文件缺失视为空）
func readAuthKeys() (map[string]string, error) {
	data, err := os.ReadFile(coding.AuthFile())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	raw := map[string]authEntry{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	keys := make(map[string]string, len(raw))
	for p, e := range raw {
		if e.Key != "" {
			keys[p] = e.Key
		}
	}
	return keys, nil
}

// maskKey 只保留末 4 位：UI 能提示设置的是哪把密钥，明文永不回传
func maskKey(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return "…" + key[len(key)-4:]
}

// SettingsGet 生效设置（全局 + 项目合并）与两个文件路径
func SettingsGet(cwd string) (map[string]any, *Error) {
	settings, err := coding.LoadSettings(coding.SettingsFile(), coding.ProjectSettingsFile(cwd))
	if err != nil {
		return nil, Wrap(err)
	}
	return map[string]any{
		"settings":    settings.Settings,
		"globalPath":  coding.SettingsFile(),
		"projectPath": coding.ProjectSettingsFile(cwd),
	}, nil
}

// SettingsPut 整体替换全局 settings.json（GET → 编辑 → PUT），启动时生效。
// 原子落盘；项目层持有旧默认模型时同步清掉（project wins 会遮蔽全局新值）。
func SettingsPut(cfg coding.Settings, d ConfigDeps) *Error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Wrap(err)
	}
	configMu.Lock()
	defer configMu.Unlock()
	if err := WriteFileAtomic(coding.SettingsFile(), append(data, '\n'), 0o600); err != nil {
		return Wrap(err)
	}
	logx.Infof("[console] settings.json rewritten")
	clearProjectDefaultIfStale(d.Cwd, cfg.DefaultProvider, cfg.DefaultModel)
	return nil
}

// clearProjectDefaultIfStale 项目 settings 持有默认模型且与请求体的默认不同
// 时清掉项目层旧默认：合并时 project wins，只写全局不生效——UI 改默认显示
// 已保存但实际仍是旧值即来源于此。清场后默认模型的决定权归全局。
func clearProjectDefaultIfStale(cwd, provider, model string) {
	path := coding.ProjectSettingsFile(cwd)
	st, err := coding.ReadSettingsFile(path)
	if err != nil || st.DefaultProvider == "" {
		return
	}
	if st.DefaultProvider == provider && st.DefaultModel == model {
		return
	}
	st.DefaultProvider = ""
	st.DefaultModel = ""
	if err := coding.SaveSettings(path, st); err != nil {
		logx.Warnf("[console] clear project default: %v", err)
		return
	}
	logx.Infof("[console] project-level stale default cleared: %s", path)
}

// AuthList 各 provider 的密钥配置状态（打码，不回传明文）
func AuthList() (map[string]any, *Error) {
	keys, err := readAuthKeys()
	if err != nil {
		return nil, Wrap(err)
	}
	providers := make(map[string]map[string]any, len(keys))
	for p, key := range keys {
		providers[p] = map[string]any{"configured": true, "maskedKey": maskKey(key)}
	}
	return map[string]any{"providers": providers}, nil
}

// AuthSet 写入 provider API 密钥（空 key 等价删除）并热重载
func AuthSet(provider, key string, d ConfigDeps) (map[string]any, *Error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, Invalid("provider required")
	}
	configMu.Lock()
	defer configMu.Unlock()
	store := coding.LoadAuthStore(coding.AuthFile())
	var err error
	if strings.TrimSpace(key) == "" {
		err = store.DeleteAPIKey(provider)
	} else {
		err = store.SetAPIKey(provider, strings.TrimSpace(key))
	}
	if err != nil {
		return nil, Wrap(err)
	}
	logx.Infof("[console] auth.json updated (provider %s)", provider)
	return reloadFor(d), nil
}

// AuthDelete 删除 provider 密钥并热重载
func AuthDelete(provider string, d ConfigDeps) (map[string]any, *Error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, Invalid("provider required")
	}
	configMu.Lock()
	defer configMu.Unlock()
	if err := coding.LoadAuthStore(coding.AuthFile()).DeleteAPIKey(provider); err != nil {
		return nil, Wrap(err)
	}
	logx.Infof("[console] auth.json updated (provider %s removed)", provider)
	return reloadFor(d), nil
}

// reloadFor 配置写盘后的热重载作答；重载失败不影响已写入的文件
func reloadFor(d ConfigDeps) map[string]any {
	resp := map[string]any{"ok": true, "reloaded": true}
	if d.ReloadAll == nil {
		return resp
	}
	if err := d.ReloadAll(); err != nil {
		logx.Warnf("[console] reload after config write: %v", err)
		resp["reloaded"] = false
		resp["error"] = err.Error()
	}
	return resp
}

// providerInfo provider 目录条目（引导页/密钥面板数据源）
type providerInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	BaseURL       string   `json:"baseUrl,omitempty"`
	RequiresKey   bool     `json:"requiresKey"`
	EnvKeys       []string `json:"envKeys,omitempty"`
	KeyConfigured bool     `json:"keyConfigured"`
}

// providerRequiresKey 判定 provider 是否需要密钥：本地端点（local 或
// loopback baseUrl）免密，其余一律需要。内置远端 provider 未置 RequiresKey
// （Stream 靠密钥解析判定），这里按 baseUrl 语义补齐。
func providerRequiresKey(p *ai.Provider) bool {
	if p.ID == "local" {
		return false
	}
	if p.BaseURL == "" {
		return true
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return true
	}
	host := u.Hostname()
	return host != "localhost" && host != "127.0.0.1" && host != "::1"
}

// providerKeyReady 密钥就绪判定：auth.json 或环境变量任一（免密 provider 恒就绪）
func providerKeyReady(p *ai.Provider, keys map[string]string) bool {
	if !providerRequiresKey(p) {
		return true
	}
	if keys[p.ID] != "" {
		return true
	}
	for _, env := range p.EnvKeys {
		if os.Getenv(env) != "" {
			return true
		}
	}
	return false
}

// Providers 内置 provider 目录 + 密钥就绪状态（与 web GET /api/config/providers
// 同构）。models.json 里出现过的自定义 provider 一并补进目录，密钥面板才能
// 管理自定义网关的密钥。
func Providers() (map[string]any, *Error) {
	keys, err := readAuthKeys()
	if err != nil {
		return nil, Wrap(err)
	}
	extra := map[string]bool{}
	if custom, err := coding.LoadCustomModels(coding.ModelsFile()); err == nil {
		for _, m := range custom {
			if m != nil {
				extra[m.Provider] = true
			}
		}
	}
	out := make([]providerInfo, 0, len(coding.BuiltinProviders())+len(extra))
	for _, p := range coding.BuiltinProviders() {
		delete(extra, p.ID)
		out = append(out, providerInfo{
			ID:            p.ID,
			Name:          p.Name,
			BaseURL:       p.BaseURL,
			RequiresKey:   providerRequiresKey(p),
			EnvKeys:       p.EnvKeys,
			KeyConfigured: providerKeyReady(p, keys),
		})
	}
	for id := range extra {
		p := &ai.Provider{ID: id, EnvKeys: ai.EnvKeysFor(id)}
		out = append(out, providerInfo{
			ID:            p.ID,
			Name:          p.ID,
			RequiresKey:   true,
			EnvKeys:       p.EnvKeys,
			KeyConfigured: providerKeyReady(p, keys),
		})
	}
	return map[string]any{"providers": out}, nil
}

// ModelsGet 自定义模型原始定义（models.json，编辑面板预填用）
func ModelsGet() ([]*ai.Model, *Error) {
	custom, err := coding.LoadCustomModels(coding.ModelsFile())
	if err != nil {
		return nil, Wrap(err)
	}
	if custom == nil {
		custom = []*ai.Model{}
	}
	return custom, nil
}

// ModelUpsert 新增/替换一个自定义模型（provider+id 匹配）并热重载。
// previous 非空（"provider/id"）表示改名编辑：替换旧条目而非追加重复，
// 当前会话落在旧 key 上时同步切到新 spec。
func ModelUpsert(m ai.Model, previous string, d ConfigDeps) (map[string]any, *Error) {
	if strings.TrimSpace(m.Provider) == "" || strings.TrimSpace(m.ID) == "" {
		return nil, Invalid("provider and id required")
	}
	prevProvider, prevID := "", ""
	if v := strings.TrimPrefix(strings.TrimSpace(previous), "/"); v != "" && strings.Contains(v, "/") {
		if idx := strings.Index(v, "/"); idx > 0 {
			prevProvider, prevID = v[:idx], v[idx+1:]
		}
	}
	configMu.Lock()
	defer configMu.Unlock()
	path := coding.ModelsFile()
	custom, err := coding.LoadCustomModels(path)
	if err != nil {
		return nil, Wrap(err)
	}
	replaced := false
	for i, c := range custom {
		if c.Provider == m.Provider && c.ID == m.ID {
			// 命中既有条目：扩展字段（headers 等高级配置）回填旧值，编辑不丢配置
			custom[i] = coding.MergeModelEdit(c, &m)
			replaced = true
			break
		}
	}
	renameHit := false
	if !replaced && prevProvider != "" {
		for i, c := range custom {
			if c.Provider == prevProvider && c.ID == prevID {
				custom[i] = coding.MergeModelEdit(c, &m)
				replaced = true
				renameHit = true
				break
			}
		}
	}
	if !replaced {
		custom = append(custom, &m)
	}
	if err := coding.SaveCustomModels(path, custom); err != nil {
		return nil, Wrap(err)
	}
	logx.Infof("[console] models.json updated (%s/%s)", m.Provider, m.ID)
	resp := reloadFor(d)
	renamed := renameHit && (prevProvider != m.Provider || prevID != m.ID)
	if renamed {
		oldSpec, newSpec := prevProvider+"/"+prevID, m.Provider+"/"+m.ID
		if d.ModelCurrent != nil && d.ModelCurrent() == oldSpec && d.ModelSwitch != nil {
			if err := d.ModelSwitch(newSpec); err != nil {
				logx.Warnf("[console] re-point renamed model: %v", err)
			}
		}
		syncSettingsDefault(d.Cwd, oldSpec, newSpec)
	}
	return resp, nil
}

// ModelDelete 删除自定义模型（内置模型拒绝）并热重载；删除的是当前活跃
// 模型时回退切到第一个可用模型
func ModelDelete(provider, id string, d ConfigDeps) (map[string]any, *Error) {
	configMu.Lock()
	defer configMu.Unlock()
	path := coding.ModelsFile()
	custom, err := coding.LoadCustomModels(path)
	if err != nil {
		return nil, Wrap(err)
	}
	var keep []*ai.Model
	found := false
	for _, c := range custom {
		if c.Provider == provider && c.ID == id {
			found = true
			continue
		}
		keep = append(keep, c)
	}
	if !found {
		return nil, Invalid("not a custom model: " + provider + "/" + id)
	}
	if err := coding.SaveCustomModels(path, keep); err != nil {
		return nil, Wrap(err)
	}
	logx.Infof("[console] models.json updated (removed %s/%s)", provider, id)
	resp := reloadFor(d)
	removedSpec := provider + "/" + id
	if d.ModelCurrent != nil && d.ModelCurrent() == removedSpec && d.ModelFallback != nil && d.ModelSwitch != nil {
		for _, spec := range d.ModelFallback() {
			if err := d.ModelSwitch(spec); err != nil {
				logx.Warnf("[console] fallback after model delete: %v", err)
				continue
			}
			break
		}
	}
	syncSettingsDefault(d.Cwd, removedSpec, "")
	return resp, nil
}

// syncSettingsDefault 模型改名/删除后同步默认模型指向。全局与项目两个
// settings 文件逐个检查：哪个文件持有旧指向就改哪个——合并视图判断会把
// 项目层默认误写进全局，项目旧值仍在且 project wins，同步无效。newSpec 为
// 空表示清除，启动 fallback 链接管。
func syncSettingsDefault(cwd, oldSpec, newSpec string) {
	for _, path := range []string{
		coding.SettingsFile(),
		coding.ProjectSettingsFile(cwd),
	} {
		st, err := coding.ReadSettingsFile(path)
		if err != nil {
			logx.Warnf("[console] sync settings default: %v", err)
			continue
		}
		if st.DefaultProvider == "" || st.DefaultProvider+"/"+st.DefaultModel != oldSpec {
			continue
		}
		if newSpec != "" {
			idx := strings.Index(newSpec, "/")
			st.DefaultProvider = newSpec[:idx]
			st.DefaultModel = newSpec[idx+1:]
		} else {
			st.DefaultProvider = ""
			st.DefaultModel = ""
		}
		if err := coding.SaveSettings(path, st); err != nil {
			logx.Warnf("[console] sync settings default: %v", err)
			continue
		}
		logx.Infof("[console] settings default model re-pointed in %s: %s -> %s", path, oldSpec, newSpec)
	}
}

// WriteFileAtomic 原子落盘：写同目录临时文件后 rename，磁盘满/中途失败
// 不留截断的目标文件
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".doge-apply-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
