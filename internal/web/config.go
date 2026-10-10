package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"doge/internal/ai"
	"doge/internal/coding"
	"doge/pkg/logx"
)

const (
	defaultLogLines = 200
	maxLogLines     = 5000
)

// logDateRe bounds the ?date= archive selector (doge.log.<YYYY-MM-DD>[.N]);
// anything else would turn the parameter into arbitrary path access.
var logDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(\.\d+)?$`)

// handleConfigOverview reports where the agent's files live and what the
// effective settings are — a management client's entry point, so it can render
// configuration UIs without knowing the on-disk layout.
func (s *Server) handleConfigOverview(w http.ResponseWriter, r *http.Request) {
	cwd := s.defaultBackend.Status().Cwd
	settings, err := coding.LoadSettings(coding.SettingsFile(), coding.ProjectSettingsFile(cwd))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
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
		"mcp":      s.mcpOverview(),
		"skills":   s.skillsOverview(),
	})
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

// handleSettingsGet returns the effective settings (global + project merged,
// project wins) plus the two file paths, so a client knows what it may edit
// (the global file, via PUT) and what it is only observing.
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	cwd := s.defaultBackend.Status().Cwd
	settings, err := coding.LoadSettings(coding.SettingsFile(), coding.ProjectSettingsFile(cwd))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":    settings.Settings,
		"globalPath":  coding.SettingsFile(),
		"projectPath": coding.ProjectSettingsFile(cwd),
	})
}

// handleSettingsPut replaces the GLOBAL settings.json with the request body
// (a full Settings document; the intended flow is GET → edit → PUT). Project
// settings are left alone. Every settings field applies at startup, so the
// response flags a restart; runtime-reloadable state (models/auth/permissions)
// is not settings but files re-read via POST /api/reload.
func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var cfg coding.Settings
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(coding.SettingsFile(), append(data, '\n'), 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Infof("[web] settings.json rewritten via API")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restartRequired": true})
}

// authEntry mirrors one auth.json record ({"type":"api_key","key":"..."}).
type authEntry struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// handleAuthGet lists the provider keys in auth.json with masked values —
// enough for a client to show what is configured, never the secrets
// themselves (setting them is POST /api/config/auth).
func (s *Server) handleAuthGet(w http.ResponseWriter, r *http.Request) {
	keys, err := readAuthKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	providers := make(map[string]map[string]any, len(keys))
	for p, key := range keys {
		providers[p] = map[string]any{"configured": true, "maskedKey": maskKey(key)}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
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
	req.Provider = strings.TrimSpace(req.Provider)
	if req.Provider == "" {
		writeError(w, http.StatusBadRequest, "provider required")
		return
	}
	store := coding.LoadAuthStore(coding.AuthFile())
	var err error
	if strings.TrimSpace(req.Key) == "" {
		err = store.DeleteAPIKey(req.Provider)
	} else {
		err = store.SetAPIKey(req.Provider, strings.TrimSpace(req.Key))
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Infof("[web] auth.json updated via API (provider %s)", req.Provider)
	s.answerAfterConfigWrite(w, r)
}

// handleAuthDelete removes one provider key and refreshes the session
// runtime.
func (s *Server) handleAuthDelete(w http.ResponseWriter, r *http.Request) {
	provider := strings.TrimSpace(r.PathValue("provider"))
	if provider == "" {
		writeError(w, http.StatusBadRequest, "provider required")
		return
	}
	if err := coding.LoadAuthStore(coding.AuthFile()).DeleteAPIKey(provider); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Infof("[web] auth.json updated via API (provider %s removed)", provider)
	s.answerAfterConfigWrite(w, r)
}

// answerAfterConfigWrite answers a successful config-file write, reporting
// whether the live reload also took. A failed reload leaves the file written
// (the response carries the error; POST /api/reload can retry).
func (s *Server) answerAfterConfigWrite(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"ok": true, "reloaded": true}
	if err := s.backendFor(r).ReloadConfig(); err != nil {
		logx.Warnf("[web] reload after config write: %v", err)
		resp["reloaded"] = false
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// readAuthKeys parses auth.json into provider → key (a missing file is an
// empty map, matching LoadAuthStore).
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

// maskKey keeps only the last 4 characters, so a UI can hint at WHICH key is
// set without ever shipping the secret to the client.
func maskKey(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return "…" + key[len(key)-4:]
}

// handleConfigModelsGet returns the raw custom models array (models.json as
// loaded) — the full field set (baseUrl, cost, thinkingLevelMap, …) config UIs
// need for edit prefill, beyond what /api/models' ModelInfo carries.
func (s *Server) handleConfigModelsGet(w http.ResponseWriter, r *http.Request) {
	custom, err := coding.LoadCustomModels(coding.ModelsFile())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if custom == nil {
		custom = []*ai.Model{}
	}
	writeJSON(w, http.StatusOK, custom)
}

// handleConfigModelsUpsert adds or replaces one custom model (matched by
// provider+id), writes models.json back and hot-reloads the catalog.
func (s *Server) handleConfigModelsUpsert(w http.ResponseWriter, r *http.Request) {
	var m ai.Model
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(m.Provider) == "" || strings.TrimSpace(m.ID) == "" {
		writeError(w, http.StatusBadRequest, "provider and id required")
		return
	}
	path := coding.ModelsFile()
	custom, err := coding.LoadCustomModels(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
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
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Infof("[web] models.json updated via API (%s/%s)", m.Provider, m.ID)
	s.answerAfterConfigWrite(w, r)
}

// handleConfigModelsDelete removes one custom model from models.json and hot-
// reloads. A model absent from the custom file is builtin and cannot be
// deleted here.
func (s *Server) handleConfigModelsDelete(w http.ResponseWriter, r *http.Request) {
	provider, id := r.PathValue("provider"), r.PathValue("id")
	path := coding.ModelsFile()
	custom, err := coding.LoadCustomModels(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
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
		writeError(w, http.StatusBadRequest, "not a custom model: "+provider+"/"+id)
		return
	}
	if err := coding.SaveCustomModels(path, keep); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Infof("[web] models.json updated via API (removed %s/%s)", provider, id)
	s.answerAfterConfigWrite(w, r)
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
	if lines > maxLogLines {
		lines = maxLogLines
	}
	path := coding.LogFile()
	if date := r.URL.Query().Get("date"); date != "" {
		if !logDateRe.MatchString(date) {
			writeError(w, http.StatusBadRequest, "invalid date (want YYYY-MM-DD)")
			return
		}
		path += "." + date
	}
	tail, err := tailFile(path, lines)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "log file not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": path, "lines": tail})
}

// tailFile returns the last n lines. Log files are bounded by rotation
// (10MB), so reading the whole file is fine at these sizes.
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
