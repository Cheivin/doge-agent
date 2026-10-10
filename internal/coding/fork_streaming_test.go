package coding

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"doge/internal/ai"
)

// TestForkWhileStreaming: forking derives an independent session from static
// configuration only — it must succeed while the source agent is mid-run.
// This is the P0 guarantee that lets the web console open a second
// conversation while the TUI is busy, and the mesh slave fork a session per
// remote task while the local session is active.
func TestForkWhileStreaming(t *testing.T) {
	dir := t.TempDir()
	manager := NewSessionManager(filepath.Join(dir, "sessions"))

	// A streaming endpoint that holds the run open until released, so the
	// source agent provably stays in-flight while we fork.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFn()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"working\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // hold the stream open until the test is done forking
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

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
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// Prompt blocks until the run ends, so drive it in a goroutine and keep
	// the stream open until after the fork.
	promptDone := make(chan error, 1)
	go func() { promptDone <- sess.Prompt("hello") }()

	deadline := time.Now().Add(3 * time.Second)
	for !sess.Agent.State().IsStreaming {
		if time.Now().After(deadline) {
			t.Fatal("agent never entered streaming")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The TUI-owned session is mid-run; a second session must still fork.
	forked, err := sess.ForkNew()
	if err != nil {
		t.Fatalf("fork while streaming: %v", err)
	}
	if forked == nil {
		t.Fatal("fork returned nil session")
	}
	if forked.CurrentSessionID() == sess.CurrentSessionID() {
		t.Fatal("fork shares the source session id")
	}

	// Let the source run finish, then the fork must run independently.
	releaseFn()
	if err := <-promptDone; err != nil {
		t.Fatalf("source prompt: %v", err)
	}

	if err := forked.Prompt("hi"); err != nil {
		t.Fatalf("forked prompt: %v", err)
	}
	forked.WaitForIdle()
	if len(forked.CurrentMessages()) < 2 {
		t.Fatalf("forked transcript too short: %d messages", len(forked.CurrentMessages()))
	}
	if got := forked.CurrentSessionID(); got == sess.CurrentSessionID() {
		t.Fatalf("fork still shares session id after run: %s", got)
	}
}
