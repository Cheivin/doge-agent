package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/console"
	"doge/internal/coding/permissions"
	"doge/internal/web"
	"doge/pkg/logx"
)


type (
	// ConfigService 配置与诊断（对应 /api/config*、/api/reload、/api/logs）
	ConfigService struct {
		d Deps
	}

	// authEntry auth.json 单条记录（{"type":"api_key","key":"..."}）
	authEntry struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
)

// Overview 配置总览：文件位置 + 生效设置 + mcp/skills 概况
func (s *ConfigService) Overview() (map[string]any, error) {
	cwd := s.d.Backend.Status().Cwd
	settings, err := coding.LoadSettings(coding.SettingsFile(), coding.ProjectSettingsFile(cwd))
	if err != nil {
		return nil, errInternal(err)
	}
	mcpOverview := map[string]any{"enabled": false}
	if s.d.Mcp != nil {
		mcpOverview = map[string]any{"enabled": s.d.Mcp.Enabled(), "servers": s.d.Mcp.Servers()}
	}
	skills := coding.LoadSkills(coding.SkillsDirs(cwd)...)
	names := make([]string, 0, len(skills))
	for _, sk := range skills {
		names = append(names, sk.Name)
	}
	return map[string]any{
		"version":     "",
		"agentDir":    coding.AgentDir(),
		"cwd":         cwd,
		"meshEnabled": s.d.Mesh != nil,
		"paths": map[string]string{
			"settings":        coding.SettingsFile(),
			"projectSettings": coding.ProjectSettingsFile(cwd),
			"models":          coding.ModelsFile(),
			"auth":            coding.AuthFile(),
			"sessions":        coding.SessionsDir(),
			"logs":            coding.LogFile(),
			"meshConfig":      filepath.Join(coding.AgentDir(), "mesh.json"),
			"uploads":         coding.UploadsDir(),
			"mcpConfig":       filepath.Join(coding.AgentDir(), "mcp.json"),
			"mcpProject":      filepath.Join(cwd, ".doge", "mcp.json"),
			"mcpShared":       filepath.Join(cwd, ".mcp.json"),
		},
		"settings": settings.Settings,
		"setup":    web.SetupOverview(s.d.Backend, settings.Settings),
		"mcp":      mcpOverview,
		"skills": map[string]any{
			"dirs":  coding.SkillsDirs(cwd),
			"count": len(skills),
			"names": names,
		},
	}, nil
}

// SettingsGet 生效设置（全局 + 项目合并）与两个文件路径
func (s *ConfigService) SettingsGet() (map[string]any, error) {
	cwd := s.d.Backend.Status().Cwd
	settings, err := coding.LoadSettings(coding.SettingsFile(), coding.ProjectSettingsFile(cwd))
	if err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{
		"settings":    settings.Settings,
		"globalPath":  coding.SettingsFile(),
		"projectPath": coding.ProjectSettingsFile(cwd),
	}, nil
}

// SettingsPut 整体替换全局 settings.json（GET → 编辑 → PUT），启动时生效。
// 项目层持有旧默认时同步清掉（project wins 会遮蔽全局新值），默认决定权归全局。
func (s *ConfigService) SettingsPut(cfg coding.Settings) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return errInternal(err)
	}
	if err := os.WriteFile(coding.SettingsFile(), append(data, '\n'), 0o600); err != nil {
		return errInternal(err)
	}
	logx.Infof("[desktop] settings.json rewritten via service")
	s.clearProjectDefaultIfStale(cfg.DefaultProvider, cfg.DefaultModel)
	return nil
}

// clearProjectDefaultIfStale 项目 settings 持有默认模型且与请求体的默认不同
// 时清掉项目层旧默认（与 Web 同构）：合并时 project wins，只写全局不生效。
func (s *ConfigService) clearProjectDefaultIfStale(provider, model string) {
	path := coding.ProjectSettingsFile(s.d.Backend.Status().Cwd)
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
		logx.Warnf("[desktop] clear project default: %v", err)
		return
	}
	logx.Infof("[desktop] project-level stale default cleared: %s", path)
}

// AuthGet 各 provider 的密钥配置状态（打码，不回传明文）
func (s *ConfigService) AuthGet() (map[string]any, error) {
	keys, err := readAuthKeys()
	if err != nil {
		return nil, errInternal(err)
	}
	providers := make(map[string]map[string]any, len(keys))
	for p, key := range keys {
		providers[p] = map[string]any{"configured": true, "maskedKey": maskKey(key)}
	}
	return map[string]any{"providers": providers}, nil
}

// Providers 内置 provider 目录 + 密钥就绪状态（引导页/密钥面板数据源，与
// Web GET /api/config/providers 同构）。models.json 里出现过的自定义 provider
// 一并补进目录，密钥面板才能管理自定义网关的密钥。
func (s *ConfigService) Providers() (map[string]any, error) {
	keys, err := readAuthKeys()
	if err != nil {
		return nil, errInternal(err)
	}
	type info struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		BaseURL       string   `json:"baseUrl,omitempty"`
		RequiresKey   bool     `json:"requiresKey"`
		EnvKeys       []string `json:"envKeys,omitempty"`
		KeyConfigured bool     `json:"keyConfigured"`
	}
	// 自定义模型文件里出现过的 provider 一并补进目录（密钥面板可管理）
	extra := map[string]bool{}
	if custom, err := coding.LoadCustomModels(coding.ModelsFile()); err == nil {
		for _, m := range custom {
			if m != nil {
				extra[m.Provider] = true
			}
		}
	}
	out := make([]info, 0, 16)
	for _, p := range coding.BuiltinProviders() {
		delete(extra, p.ID)
		out = append(out, info{
			ID: p.ID, Name: p.Name, BaseURL: p.BaseURL,
			RequiresKey: p.RequiresKey, EnvKeys: p.EnvKeys,
			KeyConfigured: keyReady(p.ID, !p.RequiresKey, p.EnvKeys, keys),
		})
	}
	for id := range extra {
		envs := ai.EnvKeysFor(id)
		out = append(out, info{
			ID: id, Name: id, RequiresKey: true, EnvKeys: envs,
			KeyConfigured: keyReady(id, false, envs, keys),
		})
	}
	return map[string]any{"providers": out}, nil
}

// keyReady 密钥就绪判定：auth.json 或环境变量任一
func keyReady(id string, noKey bool, envs []string, keys map[string]string) bool {
	if noKey {
		return true
	}
	if keys[id] != "" {
		return true
	}
	for _, env := range envs {
		if os.Getenv(env) != "" {
			return true
		}
	}
	return false
}

// AuthSet 写入 provider API 密钥（空 key 等价删除）并热重载
func (s *ConfigService) AuthSet(provider, key string) map[string]any {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return map[string]any{"ok": false, "reloaded": false, "error": "provider required"}
	}
	store := coding.LoadAuthStore(coding.AuthFile())
	var err error
	if strings.TrimSpace(key) == "" {
		err = store.DeleteAPIKey(provider)
	} else {
		err = store.SetAPIKey(provider, strings.TrimSpace(key))
	}
	if err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
	}
	logx.Infof("[desktop] auth.json updated via service (provider %s)", provider)
	return reloadAnswer(s.d)
}

// AuthDelete 删除 provider 密钥并热重载
func (s *ConfigService) AuthDelete(provider string) map[string]any {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return map[string]any{"ok": false, "reloaded": false, "error": "provider required"}
	}
	if err := coding.LoadAuthStore(coding.AuthFile()).DeleteAPIKey(provider); err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
	}
	logx.Infof("[desktop] auth.json updated via service (provider %s removed)", provider)
	return reloadAnswer(s.d)
}

// ModelsGet 自定义模型原始定义（models.json，编辑面板预填用）
func (s *ConfigService) ModelsGet() ([]*ai.Model, error) {
	custom, err := coding.LoadCustomModels(coding.ModelsFile())
	if err != nil {
		return nil, errInternal(err)
	}
	if custom == nil {
		custom = []*ai.Model{}
	}
	return custom, nil
}

// ModelsUpsert 新增/替换一个自定义模型（provider+id 匹配）并热重载。
// previous 非空（"provider/id"）表示改名编辑：替换旧条目而非追加重复，
// 当前会话落在旧 key 上时同步切到新 spec。
func (s *ConfigService) ModelsUpsert(m ai.Model, previous string) map[string]any {
	if strings.TrimSpace(m.Provider) == "" || strings.TrimSpace(m.ID) == "" {
		return map[string]any{"ok": false, "reloaded": false, "error": "provider and id required"}
	}
	prevProvider, prevID := "", ""
	if v := strings.TrimPrefix(strings.TrimSpace(previous), "/"); v != "" && strings.Contains(v, "/") {
		if idx := strings.Index(v, "/"); idx > 0 {
			prevProvider, prevID = v[:idx], v[idx+1:]
		}
	}
	path := coding.ModelsFile()
	custom, err := coding.LoadCustomModels(path)
	if err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
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
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
	}
	logx.Infof("[desktop] models.json updated via service (%s/%s)", m.Provider, m.ID)
	resp := reloadAnswer(s.d)
	renamed := renameHit && (prevProvider != m.Provider || prevID != m.ID)
	if renamed && s.d.Backend.Status().Model == prevProvider+"/"+prevID {
		if err := s.d.Backend.SwitchModel(m.Provider + "/" + m.ID); err != nil {
			logx.Warnf("[desktop] re-point renamed model: %v", err)
		}
	}
	if renamed {
		s.syncSettingsDefault(prevProvider+"/"+prevID, m.Provider+"/"+m.ID)
	}
	return resp
}

// syncSettingsDefault 模型改名/删除后同步默认模型指向（与 Web 同构）。
// 全局与项目两个 settings 文件逐个检查：哪个文件持有旧指向就改哪个——
// 合并视图判断会把项目层默认误写进全局，项目旧值仍在且 project wins，
// 同步无效。newSpec 为空表示清除，启动 fallback 链接管。
func (s *ConfigService) syncSettingsDefault(oldSpec, newSpec string) {
	for _, path := range []string{
		coding.SettingsFile(),
		coding.ProjectSettingsFile(s.d.Backend.Status().Cwd),
	} {
		st, err := coding.ReadSettingsFile(path)
		if err != nil {
			logx.Warnf("[desktop] sync settings default: %v", err)
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
			logx.Warnf("[desktop] sync settings default: %v", err)
			continue
		}
		logx.Infof("[desktop] settings default model re-pointed in %s: %s -> %s", path, oldSpec, newSpec)
	}
}

// ModelsDelete 删除自定义模型（内置模型拒绝）并热重载；删除的是当前活跃
// 模型时回退切到第一个可用模型
func (s *ConfigService) ModelsDelete(provider, id string) map[string]any {
	start := time.Now()
	path := coding.ModelsFile()
	custom, err := coding.LoadCustomModels(path)
	if err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
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
	logx.Infof("[desktop] ModelsDelete %s/%s found=%v before=%d cost=%s",
		provider, id, found, len(custom), time.Since(start))
	if !found {
		return map[string]any{"ok": false, "reloaded": false, "error": "not a custom model: " + provider + "/" + id}
	}
	if err := coding.SaveCustomModels(path, keep); err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
	}
	logx.Infof("[desktop] models.json updated via service (removed %s/%s)", provider, id)
	resp := reloadAnswer(s.d)
	if s.d.Backend.Status().Model == provider+"/"+id {
		for _, m := range s.d.Backend.ListModels() {
			if m.Available {
				if err := s.d.Backend.SwitchModel(m.Spec); err != nil {
					logx.Warnf("[desktop] fallback after model delete: %v", err)
				}
				break
			}
		}
	}
	s.syncSettingsDefault(provider+"/"+id, "")
	return resp
}

// Reload 重读 models/auth/permissions 并落地会话级 MCP 开关（扇出全部活跃会话）
func (s *ConfigService) Reload() (map[string]any, error) {
	mcpPending := s.d.Backend.McpInfo().Pending
	if err := reloadAllSessions(s.d); err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{"ok": true, "mcpApplied": mcpPending}, nil
}

// Logs 诊断日志尾部；date 读归档（YYYY-MM-DD[.N]）
func (s *ConfigService) Logs(lines int, date string) (map[string]any, error) {
	result, e := console.Logs(lines, date)
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}

// reloadAnswer 配置写盘后热重载并作答；重载失败不影响已写入的文件。
// 扇出到全部活跃会话（对齐 web reloadAfterConfigWrite）：桌面常驻调度器
// fork 的会话若不重载，会继续持旧模型目录——「改了配置没生效」
func reloadAnswer(d Deps) map[string]any {
	resp := map[string]any{"ok": true, "reloaded": true}
	if err := reloadAllSessions(d); err != nil {
		logx.Warnf("[desktop] reload after config write: %v", err)
		resp["reloaded"] = false
		resp["error"] = err.Error()
	}
	return resp
}

// reloadAllSessions 配置写盘后的全局重载：请求窗口会话走 Backend 接口
// （兼容测试桩），再补齐注册表里的全部活跃实例（调度器 fork、远端 fork），
// 按指针去重。聚合首个错误但不中断——部分失败比其余会话全不重载好
func reloadAllSessions(d Deps) error {
	err := d.Backend.ReloadConfig()
	seen := map[*coding.AgentSession]bool{}
	if d.Session != nil {
		seen[d.Session] = true
	}
	if d.Session != nil && d.Session.SessionManager != nil {
		for _, sess := range d.Session.SessionManager.ActiveSessions() {
			if sess == nil || seen[sess] {
				continue
			}
			seen[sess] = true
			if e := sess.ReloadConfig(); e != nil && err == nil {
				err = e
			}
		}
	}
	return err
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

// maskKey 只留末 4 位，标识"设置了哪把钥匙"而不外泄密钥
func maskKey(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return "…" + key[len(key)-4:]
}

// PermissionsGet 读权限策略（permissions.json；不存在返回空策略）
func (s *ConfigService) PermissionsGet() (map[string]any, error) {
	path := permissionsPath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return map[string]any{"policy": permissions.Policy{}, "path": path}, nil
		}
		return nil, errInternal(err)
	}
	policy, err := permissions.LoadPolicy(path)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	return map[string]any{"policy": policy, "path": path}, nil
}

// PermissionsPut 校验并整体写入 permissions.json（备份 .bak）后热重载
func (s *ConfigService) PermissionsPut(policy permissions.Policy) map[string]any {
	data, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
	}
	if _, err := validatePermissionsData(data); err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
	}
	target := permissionsPath()
	backupFile(target)
	if err := writeFileAtomic(target, data, 0o600); err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
	}
	logx.Infof("[desktop] permissions.json rewritten via service")
	return reloadAnswer(s.d)
}

// permissionsPath permissions.json 路径
func permissionsPath() string {
	return filepath.Join(coding.AgentDir(), "permissions.json")
}
