package services

import (
	"encoding/json"
	"os"
	"path/filepath"

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
	result, e := console.SettingsGet(s.d.Backend.Status().Cwd)
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}

// SettingsPut 整体替换全局 settings.json（GET → 编辑 → PUT），启动时生效。
// 项目层持有旧默认时同步清掉（project wins 会遮蔽全局新值），默认决定权归全局。
func (s *ConfigService) SettingsPut(cfg coding.Settings) error {
	if e := console.SettingsPut(cfg, s.consoleDeps()); e != nil {
		return consoleErr(e)
	}
	return nil
}

// AuthGet 各 provider 的密钥配置状态（打码，不回传明文）
func (s *ConfigService) AuthGet() (map[string]any, error) {
	result, e := console.AuthList()
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}

// Providers 内置 provider 目录 + 密钥就绪状态（与 Web 同构）。models.json
// 里出现过的自定义 provider 一并补进目录，密钥面板才能管理自定义网关的密钥。
func (s *ConfigService) Providers() (map[string]any, error) {
	result, e := console.Providers()
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}

// AuthSet 写入 provider API 密钥（空 key 等价删除）并热重载
func (s *ConfigService) AuthSet(provider, key string) map[string]any {
	resp, e := console.AuthSet(provider, key, s.consoleDeps())
	if e != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": e.Message}
	}
	return resp
}

// AuthDelete 删除 provider 密钥并热重载
func (s *ConfigService) AuthDelete(provider string) map[string]any {
	resp, e := console.AuthDelete(provider, s.consoleDeps())
	if e != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": e.Message}
	}
	return resp
}

// ModelsGet 自定义模型原始定义（models.json，编辑面板预填用）
func (s *ConfigService) ModelsGet() ([]*ai.Model, error) {
	result, e := console.ModelsGet()
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}

// ModelsUpsert 新增/替换一个自定义模型（provider+id 匹配）并热重载。
// previous 非空（"provider/id"）表示改名编辑：替换旧条目而非追加重复，
// 当前会话落在旧 key 上时同步切到新 spec。
func (s *ConfigService) ModelsUpsert(m ai.Model, previous string) map[string]any {
	resp, e := console.ModelUpsert(m, previous, s.consoleDeps())
	if e != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": e.Message}
	}
	return resp
}

// ModelsDelete 删除自定义模型（内置模型拒绝）并热重载；删除的是当前活跃
// 模型时回退切到第一个可用模型
func (s *ConfigService) ModelsDelete(provider, id string) map[string]any {
	resp, e := console.ModelDelete(provider, id, s.consoleDeps())
	if e != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": e.Message}
	}
	return resp
}

// consoleDeps 组装配置操作的会话上下文（cwd/热重载扇出/模型回退）
func (s *ConfigService) consoleDeps() console.ConfigDeps {
	return console.ConfigDeps{
		Cwd:          s.d.Backend.Status().Cwd,
		ReloadAll:    func() error { return reloadAllSessions(s.d) },
		ModelCurrent: func() string { return s.d.Backend.Status().Model },
		ModelFallback: func() []string {
			var specs []string
			for _, m := range s.d.Backend.ListModels() {
				if m.Available {
					specs = append(specs, m.Spec)
				}
			}
			return specs
		},
		ModelSwitch: func(spec string) error { return s.d.Backend.SwitchModel(spec) },
	}
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
