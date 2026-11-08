package console

import (
	"os"
	"path/filepath"
	"testing"

	"doge/internal/coding"
)

// skillEnv 建临时 agentDir + 项目 cwd，各放一个技能
func skillEnv(t *testing.T) (agentDir, cwd string) {
	t.Helper()
	agentDir = t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", agentDir)
	cwd = t.TempDir()
	writeSkill(t, filepath.Join(agentDir, "skills", "user-skill"), "用户级技能")
	writeSkill(t, filepath.Join(cwd, ".doge", "skills", "proj-skill"), "项目级技能")
	return agentDir, cwd
}

// writeSkill 落一个最小 SKILL.md 技能目录
func writeSkill(t *testing.T, dir, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + filepath.Base(dir) + "\ndescription: " + description + "\n---\n# body"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsListDetail(t *testing.T) {
	_, cwd := skillEnv(t)

	result, e := SkillsList(cwd)
	if e != nil {
		t.Fatalf("SkillsList: %v", e)
	}
	infos := result["skills"].([]coding.SkillInfo)
	if len(infos) != 2 {
		t.Fatalf("skills = %v", infos)
	}
	if result["dirs"] == nil {
		t.Fatal("dirs missing")
	}

	view, e := SkillDetail(cwd, "user-skill")
	if e != nil {
		t.Fatalf("SkillDetail: %v", e)
	}
	if view.Content == "" || view.Name != "user-skill" {
		t.Fatalf("detail = %+v", view.SkillInfo)
	}
	if _, e := SkillDetail(cwd, "nope"); e == nil || e.Kind != KindNotFound {
		t.Fatalf("missing skill err = %v", e)
	}
}

func TestSkillSetEnabled(t *testing.T) {
	_, cwd := skillEnv(t)
	reloaded := false
	d := ConfigDeps{ReloadAll: func() error { reloaded = true; return nil }}

	resp, e := SkillSetEnabled("user-skill", false, d)
	if e != nil {
		t.Fatalf("SkillSetEnabled: %v", e)
	}
	if resp["ok"] != true || resp["reloaded"] != true || !reloaded {
		t.Fatalf("resp = %v reloaded = %v", resp, reloaded)
	}
	if set := coding.DisabledSkillSet(cwd); !set["user-skill"] {
		t.Fatal("disabledSkills not written")
	}
	list, _ := SkillsList(cwd)
	for _, info := range list["skills"].([]coding.SkillInfo) {
		if info.Name == "user-skill" && !info.Disabled {
			t.Fatalf("list disabled flag = %v", info)
		}
	}
}

func TestSkillDelete(t *testing.T) {
	agentDir, cwd := skillEnv(t)
	reloaded := false
	d := ConfigDeps{ReloadAll: func() error { reloaded = true; return nil }}

	// 非法名称与项目级技能拒绝
	if _, e := SkillDelete("../escape", cwd, d); e == nil || e.Kind != KindInvalid {
		t.Fatalf("invalid name err = %v", e)
	}
	if _, e := SkillDelete("proj-skill", cwd, d); e == nil || e.Kind != KindInvalid {
		t.Fatalf("project skill err = %v", e)
	}
	// 用户级技能删除目录并热重载
	if _, e := SkillDelete("user-skill", cwd, d); e != nil {
		t.Fatalf("delete: %v", e)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "skills", "user-skill")); !os.IsNotExist(err) {
		t.Fatal("skill dir still exists")
	}
	if !reloaded {
		t.Fatal("reload not triggered")
	}
	// 删过即 not found
	if _, e := SkillDelete("user-skill", cwd, d); e == nil || e.Kind != KindNotFound {
		t.Fatalf("deleted skill err = %v", e)
	}
}
