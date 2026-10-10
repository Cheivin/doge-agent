package coding

import (
	"os"
	"path/filepath"
	"testing"

	"doge/internal/ai"
)

// TestReloadSkillsConcurrentAccess 回归 reloadSkills 数据竞争：web /api/reload
// （或 skills-zip apply）从 HTTP goroutine 写 s.skills 时，与技能读取并发不得
// race（-race 下生效；修复前 reloadSkills 无锁写）
func TestReloadSkillsConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, ".doge", "skills")
	skillFile := filepath.Join(skillsDir, "demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillFile, []byte("---\nname: demo\ndescription: demo skill\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

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
	settings, err := LoadSettings(filepath.Join(dir, "settings.json"), filepath.Join(dir, ".doge", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewAgentSession(AgentSessionOptions{
		Cwd:            dir,
		Model:          model,
		SessionManager: NewSessionManager(filepath.Join(dir, "sessions")),
		Settings:       settings,
		ModelRuntime:   runtime,
		SkillsDirs:     []string{skillsDir},
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// 交替增删技能文件，强制每次 reload 都真正写入 s.skills
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if i%2 == 0 {
				_ = os.Remove(skillFile)
			} else if err := os.WriteFile(skillFile, []byte("---\nname: demo\ndescription: demo skill\n---\nbody"), 0o644); err != nil {
				t.Error(err)
				return
			}
			sess.reloadSkills()
		}
	}()
	for i := 0; i < 200; i++ {
		_ = sess.loadedSkills()
		_ = sess.expandCommands("/skill:demo")
	}
	<-done
}
