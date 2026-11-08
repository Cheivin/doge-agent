package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// providerInfo /api/config/providers 的单条 provider 描述（引导页与密钥面板共用）
type providerInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	BaseURL     string   `json:"baseUrl,omitempty"`
	RequiresKey bool     `json:"requiresKey"`
	EnvKeys     []string `json:"envKeys,omitempty"`
	// KeyConfigured 报告密钥已就绪（auth.json 或环境变量任一），前端徽标用
	KeyConfigured bool `json:"keyConfigured"`
}

// handleConfigProviders lists the builtin provider catalog enriched with the
// current key state — the data source for the first-run wizard ("也可用环境
// 变量 XXX" 提示) and the settings keys panel. Read-only; setting keys is
// POST /api/config/auth.
func (s *Server) handleConfigProviders(w http.ResponseWriter, r *http.Request) {
	keys, err := readAuthKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
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
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
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

// providerKeyReady 判定 provider 密钥是否已就绪：auth.json 或环境变量任一；
// 免密端点恒就绪
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
	s.configMu.Lock()
	defer s.configMu.Unlock()
	// 原子落盘：中途崩溃不留截断的 settings.json
	if err := writeFileAtomic(coding.SettingsFile(), append(data, '\n'), 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Infof("[web] settings.json rewritten via API")
	s.clearProjectDefaultIfStale(cfg.DefaultProvider, cfg.DefaultModel)
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
	// 锁覆盖读-改-写与 reload：并发写 auth.json 丢更新、或 reload 读到旧文件
	s.configMu.Lock()
	defer s.configMu.Unlock()
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
	s.configMu.Lock()
	defer s.configMu.Unlock()
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
	if err := s.reloadAfterConfigWrite(s.backendFor(r)); err != nil {
		logx.Warnf("[web] reload after config write: %v", err)
		resp["reloaded"] = false
		resp["error"] = err.Error()
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

// handleConfigModelsUpsert adds or replaces one custom model, writes models.json
// back and hot-reloads the catalog. The request may carry ?previous=provider/id:
// an EDIT that renames provider/id then matches the OLD entry and replaces it
// in place — without it a renamed edit appends a duplicate while the old entry
// stays. After the reload, a live session still on the old spec is re-pointed
// to the new one, so editing the active model takes effect everywhere at once.
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
	prevProvider, prevID := "", ""
	if v := r.URL.Query().Get("previous"); v != "" {
		if idx := strings.Index(v, "/"); idx > 0 {
			prevProvider, prevID = v[:idx], v[idx+1:]
		}
	}
	// 锁覆盖 Load→改→Save→重定向默认：并发 upsert 丢更新（后写覆盖先写）
	s.configMu.Lock()
	defer s.configMu.Unlock()
	path := coding.ModelsFile()
	custom, err := coding.LoadCustomModels(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
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
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Infof("[web] models.json updated via API (%s/%s)", m.Provider, m.ID)
	resp := map[string]any{"ok": true, "reloaded": true}
	if err := s.reloadAfterConfigWrite(s.backendFor(r)); err != nil {
		logx.Warnf("[web] reload after config write: %v", err)
		resp["reloaded"] = false
		resp["error"] = err.Error()
	}
	// 改名编辑且当前会话正用旧 key：立即切到新 spec（前端 fetchStatus 轮询
	// 随即把按钮/徽标刷新成新信息）
	renamed := renameHit && (prevProvider != m.Provider || prevID != m.ID)
	if renamed && s.backendFor(r).Status().Model == prevProvider+"/"+prevID {
		if err := s.backendFor(r).SwitchModel(m.Provider + "/" + m.ID); err != nil {
			logx.Warnf("[web] re-point renamed model: %v", err)
		}
	}
	// settings 默认模型指向旧 key：同步改指向（删除场景 newSpec 为空 = 清除，
	// 启动 fallback 链接管），持久事实源不留失效引用
	if renamed {
		s.syncSettingsDefault(prevProvider+"/"+prevID, m.Provider+"/"+m.ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// syncSettingsDefault 模型改名/删除后同步默认模型指向。全局与项目两个
// settings 文件逐个检查：哪个文件持有旧指向就改哪个。按合并视图判断会把
// 项目层默认误写进全局——项目旧值仍在且合并时 project wins，同步无效
// （默认配置不跟着调整即来源于此）。newSpec 为空 = 清除，启动 fallback
// 链接管；写失败仅记日志。
func (s *Server) syncSettingsDefault(oldSpec, newSpec string) {
	for _, path := range []string{
		coding.SettingsFile(),
		coding.ProjectSettingsFile(s.defaultBackend.Status().Cwd),
	} {
		st, err := coding.ReadSettingsFile(path)
		if err != nil {
			logx.Warnf("[web] sync settings default: %v", err)
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
			logx.Warnf("[web] sync settings default: %v", err)
			continue
		}
		logx.Infof("[web] settings default model re-pointed in %s: %s -> %s", path, oldSpec, newSpec)
	}
}

// handleConfigModelsDelete removes one custom model from models.json and hot-
// reloads. A model absent from the custom file is builtin and cannot be
// deleted here.
func (s *Server) handleConfigModelsDelete(w http.ResponseWriter, r *http.Request) {
	provider, id := r.PathValue("provider"), r.PathValue("id")
	s.configMu.Lock()
	defer s.configMu.Unlock()
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
	resp := map[string]any{"ok": true, "reloaded": true}
	if err := s.reloadAfterConfigWrite(s.backendFor(r)); err != nil {
		logx.Warnf("[web] reload after config write: %v", err)
		resp["reloaded"] = false
		resp["error"] = err.Error()
	}
	// 删除的是当前活跃模型：回退切到第一个可用模型，避免会话继续挂在已删
	// 定义上（refreshCurrentModel 只保 identity 不变的条目）；settings 默认
	// 指向被删模型时同步清除（fallback 链接管，持久层不留失效引用）
	removedSpec := provider + "/" + id
	if s.backendFor(r).Status().Model == removedSpec {
		if avail := s.backendFor(r).ListModels(); len(avail) > 0 {
			for _, m := range avail {
				if m.Available {
					if err := s.backendFor(r).SwitchModel(m.Spec); err != nil {
						logx.Warnf("[web] fallback after model delete: %v", err)
					}
					break
				}
			}
		}
	}
	s.syncSettingsDefault(removedSpec, "")
	writeJSON(w, http.StatusOK, resp)
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
