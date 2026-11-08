package web

import (
	"encoding/json"
	"net/http"

	"doge/internal/console"
)

// handleSkillsList reports the installed skills (a fresh scan of the discovery
// dirs) with metadata only — the SKILL.md body stays out of the list. The
// session-level view (what the agent actually has loaded) is /api/session/skills.
func (s *Server) handleSkillsList(w http.ResponseWriter, r *http.Request) {
	result, e := console.SkillsList(s.defaultBackend.Status().Cwd)
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleSkillsGet returns one skill's full detail: metadata, the SKILL.md body
// and the bundle file tree (references/templates/scripts), so the panel can
// browse what the skill ships without a shell.
func (s *Server) handleSkillsGet(w http.ResponseWriter, r *http.Request) {
	view, e := console.SkillDetail(s.defaultBackend.Status().Cwd, r.PathValue("name"))
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleSkillsSetEnabled 全局启停技能：写入全局 settings.json 的 disabledSkills
// 后热重载（会话重扫技能目录时剔除/恢复）。
func (s *Server) handleSkillsSetEnabled(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, e := console.SkillSetEnabled(r.PathValue("name"), req.Enabled, s.writeDeps(r))
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSkillsDelete 删除用户级技能目录（~/.doge/agent/skills 下）；项目级技能
// 拒绝删除。名称经技能扫描反查文件路径，再校验落在用户级目录内，无路径穿越。
func (s *Server) handleSkillsDelete(w http.ResponseWriter, r *http.Request) {
	resp, e := console.SkillDelete(r.PathValue("name"), s.defaultBackend.Status().Cwd, s.writeDeps(r))
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeDeps 配置写盘类操作的会话上下文：热重载扇出到请求窗口的会话与全部
// 活跃会话（同 answerAfterConfigWrite 语义）
func (s *Server) writeDeps(r *http.Request) console.ConfigDeps {
	return console.ConfigDeps{
		Cwd:       s.defaultBackend.Status().Cwd,
		ReloadAll: func() error { return s.reloadAfterConfigWrite(s.backendFor(r)) },
	}
}

// handleSessionSkills reports the active session's loaded skills — the set
// behind its skill tool — mirroring /api/session/mcp.
func (s *Server) handleSessionSkills(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"skills": s.backendFor(r).SkillsInfo(),
	})
}
