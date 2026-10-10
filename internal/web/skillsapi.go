package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"doge/internal/coding"
	"doge/pkg/logx"
)

// handleSkillsList reports the installed skills (a fresh scan of the discovery
// dirs) with metadata only — the SKILL.md body stays out of the list. The
// session-level view (what the agent actually has loaded) is /api/session/skills.
func (s *Server) handleSkillsList(w http.ResponseWriter, r *http.Request) {
	cwd := s.defaultBackend.Status().Cwd
	skills := coding.LoadSkills(coding.SkillsDirs(cwd)...)
	disabled := coding.DisabledSkillSet(cwd)
	infos := make([]coding.SkillInfo, 0, len(skills))
	for _, sk := range skills {
		infos = append(infos, coding.SkillInfo{
			Name:                   sk.Name,
			Description:            sk.Description,
			Location:               sk.FilePath,
			DisableModelInvocation: sk.DisableModelInvocation,
			ContentLength:          len(sk.Content),
			Disabled:               disabled[sk.Name],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dirs":   coding.SkillsDirs(cwd),
		"skills": infos,
	})
}

// handleSkillsGet returns one skill's full detail: metadata, the SKILL.md body
// and the bundle file tree (references/templates/scripts), so the panel can
// browse what the skill ships without a shell.
func (s *Server) handleSkillsGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cwd := s.defaultBackend.Status().Cwd
	disabled := coding.DisabledSkillSet(cwd)
	for _, sk := range coding.LoadSkills(coding.SkillsDirs(cwd)...) {
		if sk.Name == name {
			view := coding.SkillDetailView(sk)
			view.Disabled = disabled[sk.Name]
			writeJSON(w, http.StatusOK, view)
			return
		}
	}
	writeError(w, http.StatusNotFound, "skill not found: "+name)
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
	name := r.PathValue("name")
	if err := coding.SetSkillEnabled(name, req.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Infof("[web] skill %s enabled=%v via API", name, req.Enabled)
	s.answerAfterConfigWrite(w, r)
}

// handleSkillsDelete 删除用户级技能目录（~/.doge/agent/skills 下）；项目级技能
// 拒绝删除。名称经技能扫描反查文件路径，再校验落在用户级目录内，无路径穿越。
func (s *Server) handleSkillsDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		writeError(w, http.StatusBadRequest, "invalid skill name")
		return
	}
	cwd := s.defaultBackend.Status().Cwd
	userDir := filepath.Join(coding.AgentDir(), "skills")
	target := ""
	for _, sk := range coding.LoadSkills(coding.SkillsDirs(cwd)...) {
		if sk.Name != name {
			continue
		}
		if sk.FilePath != userDir && !strings.HasPrefix(sk.FilePath, userDir+string(os.PathSeparator)) {
			writeError(w, http.StatusBadRequest, "only user-level skills can be deleted")
			return
		}
		// SKILL.md 技能删整个目录，散装 .md 技能只删文件
		if filepath.Base(sk.FilePath) == "SKILL.md" {
			target = filepath.Dir(sk.FilePath)
		} else {
			target = sk.FilePath
		}
		break
	}
	if target == "" {
		writeError(w, http.StatusNotFound, "skill not found: "+name)
		return
	}
	if err := os.RemoveAll(target); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Infof("[web] skill %s deleted via API (%s)", name, target)
	s.answerAfterConfigWrite(w, r)
}

// handleSessionSkills reports the active session's loaded skills — the set
// behind its skill tool — mirroring /api/session/mcp.
func (s *Server) handleSessionSkills(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"skills": s.backendFor(r).SkillsInfo(),
	})
}
