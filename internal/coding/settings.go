package coding

import (
	"encoding/json"
	"os"
	"slices"

	"doge/internal/coding/uploads"
)

// WebSettings configures the optional web console (settings.json "web").
// CLI flags (--web/--port/--web-host/--web-token/--no-tui/--web-cors) take
// precedence; these are the defaults when the flags are absent.
type WebSettings struct {
	Enabled bool   `json:"enabled,omitempty"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
	Token   string `json:"token,omitempty"`
	NoTui   bool   `json:"noTui,omitempty"`
	// CorsOrigins allows browser origins to call the API cross-origin (a web
	// UI deployed separately from the doge process). "*" allows every origin;
	// empty keeps the same-origin-only default.
	CorsOrigins []string `json:"corsOrigins,omitempty"`
}

// UploadSettings configures the upload staging area (settings.json "uploads"):
// size limits, quota and retention for ~/.doge/agent/uploads. Zero values fall
// back to uploads.DefaultConfig.
type UploadSettings struct {
	MaxRequestBytes     int64 `json:"maxRequestBytes,omitempty"`
	MaxFileBytes        int64 `json:"maxFileBytes,omitempty"`
	QuotaBytes          int64 `json:"quotaBytes,omitempty"`
	RetentionDays       int   `json:"retentionDays,omitempty"`
	MaxImageInlineBytes int64 `json:"maxImageInlineBytes,omitempty"`
	MaxTextInlineBytes  int64 `json:"maxTextInlineBytes,omitempty"`
}

// Settings following the settings.json (a subset): global + project merged,
// project wins.
type Settings struct {
	DefaultProvider      string `json:"defaultProvider,omitempty"`
	DefaultModel         string `json:"defaultModel,omitempty"`
	DefaultThinkingLevel string `json:"defaultThinkingLevel,omitempty"`

	Compaction *CompactionSettings `json:"compaction,omitempty"`

	// StreamIdleSeconds bounds how long a provider may go silent (no data on an
	// open connection, or no response headers) before the stream aborts as
	// stalled. 0 uses the default (120s). Applies process-wide at startup.
	StreamIdleSeconds int `json:"streamIdleSeconds,omitempty"`

	// LogLevel sets the diagnostic log verbosity: debug|info|warn|error|off
	// (default info). The --log-level flag overrides it.
	LogLevel string `json:"logLevel,omitempty"`

	// DisabledSkills 全局禁用的技能名：会话加载技能时剔除，模型不可见也不可调用
	DisabledSkills []string `json:"disabledSkills,omitempty"`

	Web *WebSettings `json:"web,omitempty"`

	Uploads *UploadSettings `json:"uploads,omitempty"`
}

// SettingsManager loads global + project settings with deep merge (project
// wins), following the settings-manager.
type SettingsManager struct {
	GlobalPath  string
	ProjectPath string
	Settings    Settings
}

// LoadSettings reads and merges the global and project settings files.
func LoadSettings(globalPath, projectPath string) (*SettingsManager, error) {
	m := &SettingsManager{GlobalPath: globalPath, ProjectPath: projectPath}
	global, err := readSettingsFile(globalPath)
	if err != nil {
		return nil, err
	}
	project, err := readSettingsFile(projectPath)
	if err != nil {
		return nil, err
	}
	m.Settings = mergeSettings(global, project)
	return m, nil
}

func readSettingsFile(path string) (Settings, error) {
	var s Settings
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	return s, nil
}

// mergeSettings deep-merges project over global (scalar fields: project wins;
// struct fields: recursive).
func mergeSettings(global, project Settings) Settings {
	if project.DefaultProvider != "" {
		global.DefaultProvider = project.DefaultProvider
	}
	if project.DefaultModel != "" {
		global.DefaultModel = project.DefaultModel
	}
	if project.DefaultThinkingLevel != "" {
		global.DefaultThinkingLevel = project.DefaultThinkingLevel
	}
	if project.Compaction != nil {
		global.Compaction = project.Compaction
	}
	if project.StreamIdleSeconds > 0 {
		global.StreamIdleSeconds = project.StreamIdleSeconds
	}
	if project.DisabledSkills != nil {
		global.DisabledSkills = project.DisabledSkills
	}
	if project.Web != nil {
		global.Web = project.Web
	}
	if project.Uploads != nil {
		global.Uploads = project.Uploads
	}
	return global
}

// WebConfig returns the effective web console settings (nil when unset).
func (s *SettingsManager) WebConfig() *WebSettings {
	if s == nil {
		return nil
	}
	return s.Settings.Web
}

// UploadsConfig 返回生效的上传配置（未配置时全取默认值）
func (s *SettingsManager) UploadsConfig() uploads.Config {
	if s == nil || s.Settings.Uploads == nil {
		return uploads.DefaultConfig()
	}
	u := s.Settings.Uploads
	return uploads.Config{
		MaxRequestBytes:     u.MaxRequestBytes,
		MaxFileBytes:        u.MaxFileBytes,
		QuotaBytes:          u.QuotaBytes,
		RetentionDays:       u.RetentionDays,
		MaxImageInlineBytes: u.MaxImageInlineBytes,
		MaxTextInlineBytes:  u.MaxTextInlineBytes,
	}.WithDefaults()
}

// Compaction returns the effective compaction settings with defaults.
func (s *SettingsManager) Compaction() CompactionSettings {
	if s.Settings.Compaction != nil {
		c := *s.Settings.Compaction
		if c.ReserveTokens == 0 {
			c.ReserveTokens = DefaultCompactionSettings().ReserveTokens
		}
		if c.KeepRecentTokens == 0 {
			c.KeepRecentTokens = DefaultCompactionSettings().KeepRecentTokens
		}
		return c
	}
	return DefaultCompactionSettings()
}

// SaveSettings 覆盖写一份 settings 文件
func SaveSettings(path string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// SetSkillEnabled 全局启停技能：禁用追加（去重）、启用移除，写回全局 settings.json
func SetSkillEnabled(name string, enabled bool) error {
	s, err := readSettingsFile(SettingsFile())
	if err != nil {
		return err
	}
	if enabled {
		s.DisabledSkills = slices.DeleteFunc(s.DisabledSkills, func(v string) bool { return v == name })
	} else if !slices.Contains(s.DisabledSkills, name) {
		s.DisabledSkills = append(s.DisabledSkills, name)
	}
	return SaveSettings(SettingsFile(), s)
}

// DisabledSkillSet 全局+项目 settings 合并后的禁用技能名集合
func DisabledSkillSet(cwd string) map[string]bool {
	m, err := LoadSettings(SettingsFile(), ProjectSettingsFile(cwd))
	if err != nil {
		return nil
	}
	if len(m.Settings.DisabledSkills) == 0 {
		return nil
	}
	set := make(map[string]bool, len(m.Settings.DisabledSkills))
	for _, name := range m.Settings.DisabledSkills {
		set[name] = true
	}
	return set
}
