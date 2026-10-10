package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetSkillEnabled 全局启停技能往返：禁用去重追加、启用移除、落盘 settings.json
func TestSetSkillEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)

	if err := SetSkillEnabled("demo", false); err != nil {
		t.Fatal(err)
	}
	// 重复禁用去重
	if err := SetSkillEnabled("demo", false); err != nil {
		t.Fatal(err)
	}
	if set := DisabledSkillSet(dir); !set["demo"] {
		t.Fatalf("disabled set = %v", set)
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "demo") != 1 {
		t.Fatalf("settings.json = %s", string(data))
	}

	if err := SetSkillEnabled("demo", true); err != nil {
		t.Fatal(err)
	}
	if DisabledSkillSet(dir)["demo"] {
		t.Fatal("demo still disabled after enable")
	}
}

// TestDisabledSkillsFiltered 禁用技能在会话加载与热重载时都被剔除，
// SkillsInfo 带出 disabled 状态
func TestDisabledSkillsFiltered(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	t.Setenv("DOGE_CODING_AGENT_DIR", agentDir)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"),
		[]byte(`{"disabledSkills":["demo"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	skillsDir := filepath.Join(dir, "skills")
	for _, name := range []string{"demo", "live"} {
		seedSkillDir(t, filepath.Join(skillsDir, name), name, name+" skill")
	}

	runtime := NewModelRuntime(LoadAuthStore(filepath.Join(agentDir, "auth.json")))
	settings, err := LoadSettings(filepath.Join(agentDir, "settings.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewAgentSession(AgentSessionOptions{
		Cwd:            dir,
		Skills:         LoadSkills(skillsDir),
		SkillsDirs:     []string{skillsDir},
		SessionManager: NewSessionManager(filepath.Join(dir, "sessions")),
		Settings:       settings,
		ModelRuntime:   runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded := sess.loadedSkills(); len(loaded) != 1 || loaded[0].Name != "live" {
		t.Fatalf("loaded = %+v, want only live", loaded)
	}

	// 系统提示里不出现被禁技能
	if prompt := sess.buildSystemPrompt(""); strings.Contains(prompt, "demo skill") {
		t.Fatal("disabled skill leaked into the system prompt")
	}

	// 运行中停用 live，热重载后同样剔除
	if err := SetSkillEnabled("live", false); err != nil {
		t.Fatal(err)
	}
	sess.reloadSkills()
	if loaded := sess.loadedSkills(); len(loaded) != 0 {
		t.Fatalf("loaded after reload = %+v", loaded)
	}
}

// TestSkillsInfoDisabled SkillsInfo 按全局禁用列表标出 disabled
func TestSkillsInfoDisabled(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	t.Setenv("DOGE_CODING_AGENT_DIR", agentDir)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"),
		[]byte(`{"disabledSkills":["demo"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sess := &AgentSession{
		Cwd:    dir,
		skills: []Skill{{Name: "demo", Description: "demo skill"}, {Name: "live", Description: "live skill"}},
	}
	infos := sess.SkillsInfo()
	if len(infos) != 2 {
		t.Fatalf("infos = %+v", infos)
	}
	if infos[0].Name != "demo" || !infos[0].Disabled {
		t.Fatalf("demo info = %+v", infos[0])
	}
	if infos[1].Name != "live" || infos[1].Disabled {
		t.Fatalf("live info = %+v", infos[1])
	}
}

// seedSkillDir 写一个最小技能目录（测试用）
func seedSkillDir(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\nbody"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
