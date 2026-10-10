package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doge/internal/agent"
	"doge/internal/ai"
)

func TestBuildSystemPrompt(t *testing.T) {
	projectCtx := "# AGENTS\nAlways use tabs.\n"

	prompt := BuildSystemPrompt(SystemPromptOptions{
		Tools: []*agent.AgentTool{{
			Name: "read", Label: "Read", Description: "Read a file",
		}},
		ProjectContext: projectCtx,
		Cwd:            "/tmp/proj",
	})
	if !strings.Contains(prompt, "agent harness") {
		t.Errorf("missing default template: %q", prompt)
	}
	if !strings.Contains(prompt, "Operating rules") || !strings.Contains(prompt, "Lead with the outcome") {
		t.Errorf("missing harness operating guide: %q", prompt)
	}
	if !strings.Contains(prompt, "Available tools:") || !strings.Contains(prompt, "- read:") {
		t.Errorf("missing tools list: %q", prompt)
	}
	if !strings.Contains(prompt, "<project_context>") || !strings.Contains(prompt, "Always use tabs") {
		t.Errorf("missing project context: %q", prompt)
	}
	if !strings.Contains(prompt, "Current working directory: /tmp/proj") {
		t.Errorf("missing cwd: %q", prompt)
	}
	if !strings.Contains(prompt, "Platform:") {
		t.Errorf("missing platform line: %q", prompt)
	}
	if !strings.Contains(prompt, "Current date: ") {
		t.Errorf("missing current date: %q", prompt)
	}
}

func TestCustomSystemPrompt(t *testing.T) {
	prompt := BuildSystemPrompt(SystemPromptOptions{
		CustomPrompt: "You are my private assistant.",
		Cwd:          "/tmp",
	})
	if !strings.Contains(prompt, "private assistant") {
		t.Errorf("custom prompt missing: %q", prompt)
	}
	if strings.Contains(prompt, "Available tools:") {
		t.Errorf("default tool list should be omitted for custom prompts: %q", prompt)
	}
}

func TestLoadProjectContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("project rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadProjectContext(dir); !strings.Contains(got, "project rules") {
		t.Errorf("project context = %q", got)
	}
	if got := LoadProjectContext(t.TempDir()); got != "" {
		t.Errorf("empty dir context = %q", got)
	}
}

func TestLoadSkills(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "my-skill"), 0o755)
	content := "---\nname: my-skill\ndescription: Does things\n---\n# Skill instructions\nDo X.\n"
	if err := os.WriteFile(filepath.Join(dir, "my-skill", "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// A plain .md in the root is also a skill.
	os.WriteFile(filepath.Join(dir, "other.md"), []byte("---\ndescription: Another\n---\nDo Y.\n"), 0o644)
	// Invalid skill (no description) is dropped.
	os.WriteFile(filepath.Join(dir, "bad.md"), []byte("no frontmatter\n"), 0o644)

	skills := LoadSkills(dir)
	if len(skills) != 2 {
		t.Fatalf("skills = %d, want 2", len(skills))
	}
	byName := map[string]Skill{}
	for _, s := range skills {
		byName[s.Name] = s
	}
	if s, ok := byName["my-skill"]; !ok || !strings.Contains(s.Content, "Do X") || s.Description != "Does things" {
		t.Errorf("my-skill = %+v", byName["my-skill"])
	}
	if _, ok := byName["other"]; !ok {
		t.Errorf("other.md skill missing: %v", byName)
	}
	if _, ok := byName["bad"]; ok {
		t.Error("invalid skill should be dropped")
	}

	// Model-visible formatting.
	formatted := FormatSkillsForPrompt(skills)
	if !strings.Contains(formatted, "<available_skills>") || !strings.Contains(formatted, "<skill><name>my-skill</name>") {
		t.Errorf("format = %q", formatted)
	}

	// Skill command expansion.
	expanded := ExpandSkillCommand(skills, "/skill:my-skill do it now")
	if !strings.Contains(expanded, `<skill name="my-skill"`) || !strings.Contains(expanded, "do it now") {
		t.Errorf("expand = %q", expanded)
	}
	if got := ExpandSkillCommand(skills, "/skill:nope x"); got != "/skill:nope x" {
		t.Errorf("unknown skill should pass through: %q", got)
	}
}

func TestExpandPromptTemplate(t *testing.T) {
	tmpl := PromptTemplate{Content: "Fix $1 with $@ and ${2:-default} and ${@:2}"}
	got := ExpandPromptTemplate(tmpl, "alpha beta gamma")
	if got != "Fix alpha with alpha beta gamma and beta and beta gamma" {
		t.Errorf("expand = %q", got)
	}
	got = ExpandPromptTemplate(PromptTemplate{Content: "no args: ${1:-fallback}"}, "")
	if got != "no args: fallback" {
		t.Errorf("default arg = %q", got)
	}
}

// TestGlobalContextLayering 全局指令块存在且排在项目上下文之前（全局管
// "我是谁、在什么环境"，项目可覆盖细节约定）
func TestGlobalContextLayering(t *testing.T) {
	prompt := BuildSystemPrompt(SystemPromptOptions{
		GlobalContext:  "# 全局人设\n中文回复，谨慎删除。",
		ProjectContext: "# 项目约定\n用 tab 缩进。",
		Cwd:            "/tmp/proj",
	})
	if !strings.Contains(prompt, "<user_instructions>") || !strings.Contains(prompt, "全局人设") {
		t.Errorf("missing global context: %q", prompt)
	}
	if strings.Index(prompt, "<user_instructions>") > strings.Index(prompt, "<project_context>") {
		t.Errorf("global context must precede project context")
	}
}

// TestLoadGlobalContext 全局指令文件读取（DOGE_CODING_AGENT_DIR 隔离），
// ReloadConfig 后改动生效
func TestLoadGlobalContext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	if got := LoadGlobalContext(); got != "" {
		t.Fatalf("empty agent dir should yield no global context, got %q", got)
	}
	global := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(global, []byte("v1 全局指令"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadGlobalContext(); got != "v1 全局指令" {
		t.Fatalf("global context = %q", got)
	}

	// 会话级：新会话带上全局块，改文件 + ReloadConfig 后热生效
	proj := t.TempDir()
	srv := startCompletionsServer(t, func(req map[string]any) []string {
		return []string{
			`{"choices":[{"delta":{"content":"ok"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		}
	})
	defer srv.Close()
	auth := LoadAuthStore(filepath.Join(dir, "auth.json"))
	runtime := NewModelRuntime(auth)
	runtime.Models.AddProvider(&ai.Provider{ID: "mock", Name: "Mock", BaseURL: srv.URL, RequiresKey: false})
	runtime.Models.AddModel(&ai.Model{
		ID: "mock-model", API: ai.APIOpenAICompletions, Provider: "mock", BaseURL: srv.URL,
		ContextWindow: 100000, MaxTokens: 4096,
	})
	model, _, err := runtime.ResolveModel("mock/mock-model")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := LoadSettings(filepath.Join(dir, "settings.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewAgentSession(AgentSessionOptions{
		Cwd:            proj,
		Model:          model,
		SessionManager: NewSessionManager(filepath.Join(dir, "sessions")),
		Settings:       settings,
		ModelRuntime:   runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p := sess.Agent.State().SystemPrompt; !strings.Contains(p, "v1 全局指令") {
		t.Fatalf("session prompt missing global context: %q", p[:200])
	}
	if err := os.WriteFile(global, []byte("v2 全局指令"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sess.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if p := sess.Agent.State().SystemPrompt; !strings.Contains(p, "v2 全局指令") {
		t.Fatalf("reload did not pick up global context change: %q", p[:200])
	}
}
