package services

import (
	"os"
	"path/filepath"
	"strings"

	"doge/internal/coding"
	"doge/pkg/logx"
)

type (
	// SkillsService 技能管理（对应 /api/skills*、/api/session/skills）
	SkillsService struct {
		d Deps
	}
)

// List 已安装技能（发现目录重扫，元数据不含正文）
func (s *SkillsService) List() (map[string]any, error) {
	cwd := s.d.Backend.Status().Cwd
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
	return map[string]any{"dirs": coding.SkillsDirs(cwd), "skills": infos}, nil
}

// Detail 单个技能全文与文件树
func (s *SkillsService) Detail(name string) (coding.SkillDetail, error) {
	cwd := s.d.Backend.Status().Cwd
	disabled := coding.DisabledSkillSet(cwd)
	for _, sk := range coding.LoadSkills(coding.SkillsDirs(cwd)...) {
		if sk.Name == name {
			view := coding.SkillDetailView(sk)
			view.Disabled = disabled[sk.Name]
			return view, nil
		}
	}
	return coding.SkillDetail{}, errNotFound("skill not found: " + name)
}

// SessionSkills 当前会话实际加载的技能
func (s *SkillsService) SessionSkills() map[string]any {
	return map[string]any{"skills": s.d.Backend.SkillsInfo()}
}

// SetEnabled 全局启停技能（写全局 settings.json 的 disabledSkills 后热重载）
func (s *SkillsService) SetEnabled(name string, enabled bool) map[string]any {
	if err := coding.SetSkillEnabled(name, enabled); err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
	}
	logx.Infof("[desktop] skill %s enabled=%v via service", name, enabled)
	return reloadAnswer(s.d)
}

// Delete 删除用户级技能（项目级拒绝；名称经扫描反查路径，无穿越）
func (s *SkillsService) Delete(name string) map[string]any {
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return map[string]any{"ok": false, "reloaded": false, "error": "invalid skill name"}
	}
	cwd := s.d.Backend.Status().Cwd
	userDir := filepath.Join(coding.AgentDir(), "skills")
	target := ""
	for _, sk := range coding.LoadSkills(coding.SkillsDirs(cwd)...) {
		if sk.Name != name {
			continue
		}
		if sk.FilePath != userDir && !strings.HasPrefix(sk.FilePath, userDir+string(os.PathSeparator)) {
			return map[string]any{"ok": false, "reloaded": false, "error": "only user-level skills can be deleted"}
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
		return map[string]any{"ok": false, "reloaded": false, "error": "skill not found: " + name}
	}
	if err := os.RemoveAll(target); err != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": err.Error()}
	}
	logx.Infof("[desktop] skill %s deleted via service (%s)", name, target)
	return reloadAnswer(s.d)
}
