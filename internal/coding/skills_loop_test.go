package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doge/internal/ai"
)

func testSkillDir(t *testing.T) (string, []Skill) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, ".doge", "skills")
	os.MkdirAll(filepath.Join(root, "demo"), 0o755)
	os.WriteFile(filepath.Join(root, "demo", "SKILL.md"),
		[]byte("---\nname: demo\ndescription: A demo skill\n---\n# Demo\nDo the thing.\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "secret"), 0o755)
	os.WriteFile(filepath.Join(root, "secret", "SKILL.md"),
		[]byte("---\nname: secret\ndescription: User-only skill\ndisable-model-invocation: true\n---\n# Secret\nUser only.\n"), 0o644)
	return dir, LoadSkills(root)
}

func TestResolveSkillBlocks(t *testing.T) {
	_, skills := testSkillDir(t)
	byName := map[string]Skill{}
	for _, s := range skills {
		byName[s.Name] = s
	}
	demo := byName["demo"]

	text := `I'll use the skill now: <skill name="demo" location="/whatever"/> ok?`
	got := ResolveSkillBlocks(text, skills)
	if len(got) != 1 {
		t.Fatalf("resolved = %d blocks, want 1", len(got))
	}
	if !strings.Contains(got[0], "Do the thing.") {
		t.Errorf("block content missing skill body: %q", got[0])
	}
	if strings.Contains(got[0], "<skill name=") {
		t.Errorf("model-emitted form should not carry a re-triggering marker: %q", got[0])
	}

	// Duplicate blocks resolve once.
	dup := `<skill name="demo" location="a"/> then <skill name="demo" location="b"/>`
	if got := ResolveSkillBlocks(dup, skills); len(got) != 1 {
		t.Errorf("dup blocks = %d, want 1", len(got))
	}

	// Unknown and disabled skills are skipped.
	mixed := `<skill name="nope" location="x"/> <skill name="secret" location="y"/>`
	if got := ResolveSkillBlocks(mixed, skills); len(got) != 0 {
		t.Errorf("skipped blocks should resolve to none, got %d", len(got))
	}

	_ = demo // keep reference; ensures demo is loaded above
}

func TestSkillTool(t *testing.T) {
	_, skills := testSkillDir(t)
	tool := SkillTool(skills)

	res, err := tool.Execute("id", map[string]any{"name": "demo"}, nil, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "Do the thing.") || !strings.Contains(text, "Location: ") {
		t.Errorf("skill tool content = %q", text)
	}

	// Unknown skill.
	res, _ = tool.Execute("id", map[string]any{"name": "nope"}, nil, nil)
	if !strings.Contains(res.Content[0].Text, "Available skills") {
		t.Errorf("unknown skill error = %q", res.Content[0].Text)
	}

	// Disabled skills are not model-invocable.
	res, _ = tool.Execute("id", map[string]any{"name": "secret"}, nil, nil)
	if !strings.Contains(res.Content[0].Text, "not invocable by the model") {
		t.Errorf("disabled skill error = %q", res.Content[0].Text)
	}
}

func TestFormatSkillsForPromptInstructions(t *testing.T) {
	_, skills := testSkillDir(t)
	formatted := FormatSkillsForPrompt(skills)
	if !strings.Contains(formatted, `call the "skill" tool`) {
		t.Errorf("prompt missing invocation instruction: %q", formatted)
	}
	if strings.Contains(formatted, "secret") {
		t.Errorf("disabled skill should not be advertised: %q", formatted)
	}
}

func TestExpandSkillCommandEmbedsContent(t *testing.T) {
	_, skills := testSkillDir(t)
	expanded := ExpandSkillCommand(skills, "/skill:demo make it pop")
	if !strings.Contains(expanded, `<skill name="demo"`) {
		t.Errorf("marker missing: %q", expanded)
	}
	if !strings.Contains(expanded, "Do the thing.") {
		t.Errorf("skill body missing: %q", expanded)
	}
	if !strings.Contains(expanded, "make it pop") {
		t.Errorf("args missing: %q", expanded)
	}
}

// TestAgentSessionSkillBlockInjection drives a full session where the model
// emits a <skill name location/> block; the harness must load the skill and
// deliver the content as a follow-up user message for the next turn.
func TestAgentSessionSkillBlockInjection(t *testing.T) {
	dir, skills := testSkillDir(t)
	sessionDir := filepath.Join(dir, "sessions")
	manager := NewSessionManager(sessionDir)

	call := 0
	srv := startCompletionsServer(t, func(req map[string]any) []string {
		call++
		if call == 1 {
			return []string{
				`{"choices":[{"delta":{"content":"I will use the skill: <skill name=\"demo\" location=\"/x\"/>"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`,
				`[DONE]`,
			}
		}
		return []string{
			`{"choices":[{"delta":{"content":"Done per the skill."}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}`,
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
	settings, err := LoadSettings(filepath.Join(dir, "settings.json"), filepath.Join(dir, "proj", ".doge", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}

	sess, err := NewAgentSession(AgentSessionOptions{
		Cwd:            dir,
		Model:          model,
		SessionManager: manager,
		Settings:       settings,
		ModelRuntime:   runtime,
		Skills:         skills,
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := sess.Prompt("use the skill"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	sess.WaitForIdle()

	messages := sess.CurrentMessages()
	// user, assistant(block), user(skill content), assistant(done).
	if len(messages) != 4 {
		t.Fatalf("messages = %d: %+v", len(messages), messages)
	}
	if messages[2].Role != ai.RoleUser || !strings.Contains(messages[2].Text(), "Do the thing.") {
		t.Errorf("skill content not injected as user message: %+v", messages[2])
	}
	if messages[3].Text() != "Done per the skill." {
		t.Errorf("final text = %q", messages[3].Text())
	}
}

// TestSkillToolRegistered verifies the skill tool lands in the session toolset
// when skills are configured, and is absent otherwise.
func TestSkillToolRegistered(t *testing.T) {
	dir, skills := testSkillDir(t)
	sessionDir := filepath.Join(dir, "sessions")
	manager := NewSessionManager(sessionDir)

	srv := startCompletionsServer(t, func(req map[string]any) []string {
		return []string{
			`{"choices":[{"delta":{"content":"ok"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			`[DONE]`,
		}
	})
	defer srv.Close()
	auth := LoadAuthStore(filepath.Join(dir, "auth.json"))
	runtime := NewModelRuntime(auth)
	runtime.Models.AddProvider(&ai.Provider{ID: "mock", Name: "Mock", BaseURL: srv.URL, RequiresKey: false})
	runtime.Models.AddModel(&ai.Model{ID: "m", API: ai.APIOpenAICompletions, Provider: "mock", BaseURL: srv.URL, ContextWindow: 100000, MaxTokens: 4096})
	model, _, err := runtime.ResolveModel("mock/m")
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := LoadSettings(filepath.Join(dir, "settings.json"), filepath.Join(dir, "proj", ".doge", "settings.json"))

	sess, err := NewAgentSession(AgentSessionOptions{
		Cwd: dir, Model: model, SessionManager: manager, Settings: settings, ModelRuntime: runtime, Skills: skills,
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	hasSkill := false
	for _, tl := range sess.Agent.State().Tools {
		if tl.Name == "skill" {
			hasSkill = true
		}
	}
	if !hasSkill {
		t.Error("skill tool not registered when skills are present")
	}

	sess2, err := NewAgentSession(AgentSessionOptions{
		Cwd: dir, Model: model, SessionManager: manager, Settings: settings, ModelRuntime: runtime,
	})
	if err != nil {
		t.Fatalf("new session (no skills): %v", err)
	}
	for _, tl := range sess2.Agent.State().Tools {
		if tl.Name == "skill" {
			t.Error("skill tool registered without skills")
		}
	}
}
