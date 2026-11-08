package console

import (
	"os"
	"path/filepath"
	"strings"

	"doge/internal/coding"
	"doge/pkg/logx"
)

// SkillsList 已安装技能清单（发现目录重扫，元数据不含正文）。会话级视图
// （agent 实际加载的集合）由各传输的会话接口提供。
func SkillsList(cwd string) (map[string]any, *Error) {
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

// SkillDetail 单个技能全文与文件树（元数据 + SKILL.md 正文 + 包内文件树）
func SkillDetail(cwd, name string) (coding.SkillDetail, *Error) {
	disabled := coding.DisabledSkillSet(cwd)
	for _, sk := range coding.LoadSkills(coding.SkillsDirs(cwd)...) {
		if sk.Name == name {
			view := coding.SkillDetailView(sk)
			view.Disabled = disabled[sk.Name]
			return view, nil
		}
	}
	return coding.SkillDetail{}, NotFound("skill not found: " + name)
}

// SkillSetEnabled 全局启停技能：写入全局 settings.json 的 disabledSkills 后
// 热重载（会话重扫技能目录时剔除/恢复）
func SkillSetEnabled(name string, enabled bool, d ConfigDeps) (map[string]any, *Error) {
	if err := coding.SetSkillEnabled(name, enabled); err != nil {
		return nil, Wrap(err)
	}
	logx.Infof("[console] skill %s enabled=%v", name, enabled)
	return reloadFor(d), nil
}

// SkillDelete 删除用户级技能目录（~/.doge/agent/skills 下），项目级技能拒绝；
// 名称经技能扫描反查文件路径，再校验落在用户级目录内，无路径穿越
func SkillDelete(name, cwd string, d ConfigDeps) (map[string]any, *Error) {
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return nil, Invalid("invalid skill name")
	}
	userDir := filepath.Join(coding.AgentDir(), "skills")
	target := ""
	for _, sk := range coding.LoadSkills(coding.SkillsDirs(cwd)...) {
		if sk.Name != name {
			continue
		}
		if sk.FilePath != userDir && !strings.HasPrefix(sk.FilePath, userDir+string(os.PathSeparator)) {
			return nil, Invalid("only user-level skills can be deleted")
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
		return nil, NotFound("skill not found: " + name)
	}
	if err := os.RemoveAll(target); err != nil {
		return nil, Wrap(err)
	}
	logx.Infof("[console] skill %s deleted (%s)", name, target)
	return reloadFor(d), nil
}
