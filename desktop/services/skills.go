package services

import (
	"doge/internal/coding"
	"doge/internal/console"
)

type (
	// SkillsService 技能管理（对应 /api/skills*、/api/session/skills）
	SkillsService struct {
		d Deps
	}
)

// List 已安装技能（发现目录重扫，元数据不含正文）
func (s *SkillsService) List() (map[string]any, error) {
	result, e := console.SkillsList(s.d.Backend.Status().Cwd)
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}

// Detail 单个技能全文与文件树
func (s *SkillsService) Detail(name string) (coding.SkillDetail, error) {
	view, e := console.SkillDetail(s.d.Backend.Status().Cwd, name)
	if e != nil {
		return coding.SkillDetail{}, consoleErr(e)
	}
	return view, nil
}

// SessionSkills 当前会话实际加载的技能
func (s *SkillsService) SessionSkills() map[string]any {
	return map[string]any{"skills": s.d.Backend.SkillsInfo()}
}

// SetEnabled 全局启停技能（写全局 settings.json 的 disabledSkills 后热重载）
func (s *SkillsService) SetEnabled(name string, enabled bool) map[string]any {
	resp, e := console.SkillSetEnabled(name, enabled, s.d.consoleDeps())
	if e != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": e.Message}
	}
	return resp
}

// Delete 删除用户级技能（项目级拒绝；名称经扫描反查路径，无穿越）
func (s *SkillsService) Delete(name string) map[string]any {
	resp, e := console.SkillDelete(name, s.d.Backend.Status().Cwd, s.d.consoleDeps())
	if e != nil {
		return map[string]any{"ok": false, "reloaded": false, "error": e.Message}
	}
	return resp
}
