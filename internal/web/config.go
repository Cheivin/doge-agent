package web

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"

	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/console"
	"doge/pkg/logx"
)

const (
	defaultLogLines = 200
	maxLogLines     = 5000
)

// handleConfigOverview reports where the agent's files live and what the
// effective settings are — a management client's entry point, so it can render
// configuration UIs without knowing the on-disk layout. The `setup` node is
// the first-run signal: it is always derived from the real config state
// (usable models + default model), never from a completed-once marker, so
// wiping the config or moving machines naturally re-triggers the wizard.
func (s *Server) handleConfigOverview(w http.ResponseWriter, r *http.Request) {
	cwd := s.defaultBackend.Status().Cwd
	settings, err := coding.LoadSettings(coding.SettingsFile(), coding.ProjectSettingsFile(cwd))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	setup := SetupOverview(s.defaultBackend, settings.Settings)
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     Version,
		"agentDir":    coding.AgentDir(),
		"cwd":         cwd,
		"meshEnabled": s.meshView != nil,
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
		"setup":    setup,
		"mcp":      s.mcpOverview(),
		"skills":   s.skillsOverview(),
	})
}

// SetupOverview 组装就绪判定：有可用模型（密钥或本地端点任一）且已设默认模型。
// 环境变量里的密钥同样计入可用（ResolveAPIKey 全链），不打扰环境变量用户。
// Web 与桌面共用（桌面 Overview 同步该节）。
func SetupOverview(b Backend, s coding.Settings) map[string]any {
	hasUsable := b.HasUsableModel()
	hasDefault := s.DefaultProvider != "" && s.DefaultModel != ""
	return map[string]any{
		"ready":           hasUsable && hasDefault,
		"hasUsableModel":  hasUsable,
		"hasDefaultModel": hasDefault,
	}
}

// handleConfigProviders lists the builtin provider catalog enriched with the
// current key state - the data source for the first-run wizard and the
// settings keys panel. Read-only; setting keys is POST /api/config/auth.
func (s *Server) handleConfigProviders(w http.ResponseWriter, r *http.Request) {
	result, e := console.Providers()
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// skillsOverview 返回 /api/config 的 skills 段：发现目录 + 已装技能名（供面板
// 一屏概览，正文不在此列）。
func (s *Server) skillsOverview() map[string]any {
	cwd := s.defaultBackend.Status().Cwd
	skills := coding.LoadSkills(coding.SkillsDirs(cwd)...)
	names := make([]string, 0, len(skills))
	for _, sk := range skills {
		names = append(names, sk.Name)
	}
	return map[string]any{
		"dirs":  coding.SkillsDirs(cwd),
		"count": len(skills),
		"names": names,
	}
}

// mcpOverview 返回 /api/config 的 mcp 段（未初始化时只有 enabled=false）
func (s *Server) mcpOverview() map[string]any {
	if s.mcpView == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled": s.mcpView.Enabled(),
		"servers": s.mcpView.Servers(),
	}
}


// handleSettingsGet 生效设置（全局 + 项目合并）与两个文件路径
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	result, e := console.SettingsGet(s.defaultBackend.Status().Cwd)
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleSettingsPut replaces the GLOBAL settings.json with the request body
// (a full Settings document; the intended flow is GET → edit → PUT). Project
// settings are left alone. Every settings field applies at startup, so the
// response flags a restart.
func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var cfg coding.Settings
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if e := console.SettingsPut(cfg, s.consoleDeps(s.defaultBackend)); e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restartRequired": true})
}

// clearProjectDefaultIfStale 项目 settings 持有默认模型且与请求体的默认不同
// 时，清掉项目层旧默认：合并时 project wins，只写全局不生效——UI 改默认
// 显示已保存但实际仍是旧值即来源于此。清场后默认模型的决定权归全局。
func (s *Server) clearProjectDefaultIfStale(provider, model string) {
	path := coding.ProjectSettingsFile(s.defaultBackend.Status().Cwd)
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
		logx.Warnf("[web] clear project default: %v", err)
		return
	}
	logx.Infof("[web] project-level stale default cleared: %s", path)
}

// authEntry mirrors one auth.json record ({"type":"api_key","key":"..."}).
type authEntry struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// handleAuthGet 各 provider 的密钥配置状态（打码，不回传明文）
func (s *Server) handleAuthGet(w http.ResponseWriter, r *http.Request) {
	result, e := console.AuthList()
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleAuthSet stores one provider API key (empty key deletes it) and
// refreshes the session runtime, mirroring POST /api/reload.
func (s *Server) handleAuthSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Key      string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, e := console.AuthSet(req.Provider, req.Key, s.consoleDeps(s.defaultBackend))
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAuthDelete removes one provider key and refreshes the session runtime.
func (s *Server) handleAuthDelete(w http.ResponseWriter, r *http.Request) {
	resp, e := console.AuthDelete(r.PathValue("provider"), s.consoleDeps(s.defaultBackend))
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}


// reloadAfterConfigWrite 配置写盘后的全局重载：请求窗口会话先走 Backend 接口
// 重载（兼容测试桩），再补齐 TUI 共享会话与控制表全部活跃会话（各窗口分叉、
// 远端/定时任务 fork），按实例去重。只重载请求窗口的会话时，其余会话仍持旧
// 模型定义——「改了配置没生效」即来源于此。
func (s *Server) reloadAfterConfigWrite(b Backend) error {
	if err := b.ReloadConfig(); err != nil {
		return err
	}
	seen := map[*coding.AgentSession]bool{}
	if sb, ok := b.(*sessionBackend); ok {
		seen[sb.currentAgent()] = true
	}
	if s.shared != nil {
		seen[s.shared] = true
	}
	if s.ctrl != nil {
		for _, sess := range s.ctrl.actives() {
			seen[sess] = true
		}
	}
	var firstErr error
	for sess := range seen {
		if sess == nil {
			continue
		}
		if err := sess.ReloadConfig(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// handleConfigModelsGet returns the raw custom models array (models.json as
// loaded) - the full field set (baseUrl, cost, thinkingLevelMap, ...) config
// UIs need for edit prefill, beyond what /api/models' ModelInfo carries.
func (s *Server) handleConfigModelsGet(w http.ResponseWriter, r *http.Request) {
	result, e := console.ModelsGet()
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleConfigModelsUpsert adds or replaces one custom model, writes models.json
// and hot-reloads. ?previous=provider/id turns the call into a rename edit
// (replace the old entry instead of appending a duplicate).
func (s *Server) handleConfigModelsUpsert(w http.ResponseWriter, r *http.Request) {
	var m ai.Model
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, e := console.ModelUpsert(m, r.URL.Query().Get("previous"), s.consoleDeps(s.defaultBackend))
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleConfigModelsDelete removes one custom model from models.json and hot-
// reloads. A model absent from the custom file is builtin and cannot be
// deleted here.
func (s *Server) handleConfigModelsDelete(w http.ResponseWriter, r *http.Request) {
	provider, id := r.PathValue("provider"), r.PathValue("id")
	resp, e := console.ModelDelete(provider, id, s.consoleDeps(s.defaultBackend))
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// answerAfterConfigWrite answers a successful config-file write, reporting
// whether the live reload also took. A failed reload leaves the file written
// (the response carries the error; POST /api/reload can retry).
func (s *Server) answerAfterConfigWrite(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"ok": true, "reloaded": true}
	if err := s.reloadAfterConfigWrite(s.backendFor(r)); err != nil {
		logx.Warnf("[web] reload after config write: %v", err)
		resp["reloaded"] = false
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// consoleDeps 组装配置操作的会话上下文（cwd/热重载扇出/模型回退）
func (s *Server) consoleDeps(b Backend) console.ConfigDeps {
	return console.ConfigDeps{
		Cwd:          b.Status().Cwd,
		ReloadAll:    func() error { return s.reloadAfterConfigWrite(b) },
		ModelCurrent: func() string { return b.Status().Model },
		ModelFallback: func() []string {
			var specs []string
			for _, m := range b.ListModels() {
				if m.Available {
					specs = append(specs, m.Spec)
				}
			}
			return specs
		},
		ModelSwitch: func(spec string) error { return b.SwitchModel(spec) },
	}
}

// handleLogs tails the diagnostic log (logx): ?lines=N caps the count
// (default 200, max 5000), ?date=YYYY-MM-DD reads a rotated archive instead
// of today's file. This is the "what happened" timeline a client shows next
// to a misbehaving run.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	lines := defaultLogLines
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lines = n
		}
	}
	result, e := console.Logs(lines, r.URL.Query().Get("date"))
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
