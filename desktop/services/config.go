package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/permissions"
	"doge/pkg/logx"
)

const (
	defaultLogLines = 200
	maxLogLines     = 5000
)

// logDateRe 约束日志归档选择（doge.log.<YYYY-MM-DD>[.N]），防止参数变任意路径访问
var logDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(\.\d+)?$`)

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

// SettingsPut 整体替换全局 settings.json（GET → 编辑 → PUT），启动时生效
func (s *ConfigService) SettingsPut(cfg coding.Settings) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return errInternal(err)
	}
	if err := os.WriteFile(coding.SettingsFile(), append(data, '\n'), 0o600); err != nil {
		return errInternal(err)
	}
	logx.Infof("[desktop] settings.json rewritten via service")
	return nil
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

// ModelsUpsert 新增/替换一个自定义模型（provider+id 匹配）并热重载
func (s *ConfigService) ModelsUpsert(m ai.Model) map[string]any {
	if strings.TrimSpace(m.Provider) == "" || strings.TrimSpace(m.ID) == "" {
		return map[string]any{"ok": false, "reloaded": false, "error": "provider and id required"}
	}
	path := coding.ModelsFile()
	custom, err := coding.LoadCustomModels(path)
	if err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
	}
	replaced := false
	for i, c := range custom {
		if c.Provider == m.Provider && c.ID == m.ID {
			custom[i] = &m
			replaced = true
			break
		}
	}
	if !replaced {
		custom = append(custom, &m)
	}
	if err := coding.SaveCustomModels(path, custom); err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
	}
	logx.Infof("[desktop] models.json updated via service (%s/%s)", m.Provider, m.ID)
	return reloadAnswer(s.d)
}

// ModelsDelete 删除自定义模型（内置模型拒绝）并热重载
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
	return reloadAnswer(s.d)
}

// Reload 重读 models/auth/permissions 并落地会话级 MCP 开关
func (s *ConfigService) Reload() (map[string]any, error) {
	mcpPending := s.d.Backend.McpInfo().Pending
	if err := s.d.Backend.ReloadConfig(); err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{"ok": true, "mcpApplied": mcpPending}, nil
}

// Logs 诊断日志尾部；date 读归档（YYYY-MM-DD[.N]）
func (s *ConfigService) Logs(lines int, date string) (map[string]any, error) {
	if lines <= 0 {
		lines = defaultLogLines
	}
	if lines > maxLogLines {
		lines = maxLogLines
	}
	path := coding.LogFile()
	if date != "" {
		if !logDateRe.MatchString(date) {
			return nil, errInvalid("invalid date (want YYYY-MM-DD)")
		}
		path += "." + date
	}
	tail, err := tailFile(path, lines)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNotFound("log file not found")
		}
		return nil, errInternal(err)
	}
	return map[string]any{"file": path, "lines": tail}, nil
}

// reloadAnswer 配置写盘后热重载并作答；重载失败不影响已写入的文件
func reloadAnswer(d Deps) map[string]any {
	resp := map[string]any{"ok": true, "reloaded": true}
	if err := d.Backend.ReloadConfig(); err != nil {
		logx.Warnf("[desktop] reload after config write: %v", err)
		resp["reloaded"] = false
		resp["error"] = err.Error()
	}
	return resp
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

// tailFile 返回最后 n 行
func tailFile(path string, n int) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
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
