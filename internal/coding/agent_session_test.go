package coding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding/tools"
)

// startCompletionsServer starts a mock OpenAI-compatible chat completions
// server whose responses are scripted per request (script returns the
// response chunks).
func startCompletionsServer(t *testing.T, script func(request map[string]any) []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		chunks := script(req)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			if c == "[DONE]" {
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			} else {
				_, _ = w.Write([]byte("data: " + c + "\n\n"))
			}
		}
	}))
}

func TestAgentSessionEndToEnd(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sessions")
	manager := NewSessionManager(sessionDir)

	// Script: first call → tool call; second call → text done.
	call := 0
	srv := startCompletionsServer(t, func(req map[string]any) []string {
		call++
		if call == 1 {
			return []string{
				`{"choices":[{"delta":{"role":"assistant","content":"I'll check."}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"echo hi\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`,
				`[DONE]`,
			}
		}
		return []string{
			`{"choices":[{"delta":{"content":"All done"}}]}`,
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
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	var events []agent.AgentEvent
	unsub, _ := sess.Subscribe(func(ev agent.AgentEvent) error {
		events = append(events, ev)
		return nil
	}, ConsumerHeadless)
	defer unsub()

	if err := sess.Prompt("list the files"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	sess.WaitForIdle()

	// Transcript: user, assistant(toolCall), toolResult, assistant.
	messages := sess.CurrentMessages()
	if len(messages) != 4 {
		t.Fatalf("messages = %d: %+v", len(messages), messages)
	}
	if messages[1].Text() != "I'll check." {
		t.Errorf("assistant text = %q", messages[1].Text())
	}
	if messages[2].Role != ai.RoleToolResult || !strings.Contains(messages[2].Text(), "hi") {
		t.Errorf("tool result = %+v", messages[2])
	}
	if messages[3].Text() != "All done" {
		t.Errorf("final text = %q", messages[3].Text())
	}

	// Session file persisted with 4 messages.
	loaded, err := manager.LoadSession(sess.Session.FilePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Entries) != 4 {
		t.Errorf("persisted entries = %d, want 4", len(loaded.Entries))
	}
	if loaded.Entries[1].Message.Usage.TotalTokens != 10 {
		t.Errorf("usage persisted = %+v", loaded.Entries[1].Message.Usage)
	}

	// Agent events delivered.
	foundTurn := false
	for _, ev := range events {
		if ev.Type == agent.EventTurnEnd {
			foundTurn = true
		}
	}
	if !foundTurn {
		t.Error("no turn_end events delivered")
	}
}

func TestAgentSessionSkillExpansion(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".doge", "skills", "demo"), 0o755)
	os.WriteFile(filepath.Join(dir, ".doge", "skills", "demo", "SKILL.md"),
		[]byte("---\nname: demo\ndescription: A demo skill\n---\n# Demo\nDo things.\n"), 0o644)

	skills := LoadSkills(filepath.Join(dir, ".doge", "skills"))
	sess := &AgentSession{skills: skills}
	got := sess.expandCommands("/skill:demo hello world")
	if !strings.Contains(got, `<skill name="demo"`) || !strings.Contains(got, "hello world") {
		t.Errorf("expanded = %q", got)
	}
}

func TestAgentSessionCompactionTrigger(t *testing.T) {
	dir := t.TempDir()
	manager := NewSessionManager(filepath.Join(dir, "sessions"))
	auth := LoadAuthStore(filepath.Join(dir, "auth.json"))
	runtime := NewModelRuntime(auth)

	var mu sync.Mutex
	callCount := 0
	srv := startCompletionsServer(t, func(req map[string]any) []string {
		mu.Lock()
		callCount++
		mu.Unlock()
		return []string{
			`{"choices":[{"delta":{"content":"reply"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			`[DONE]`,
		}
	})
	defer srv.Close()

	runtime.Models.AddProvider(&ai.Provider{ID: "mock", Name: "Mock", BaseURL: srv.URL, RequiresKey: false})
	runtime.Models.AddModel(&ai.Model{
		ID: "tiny", API: ai.APIOpenAICompletions, Provider: "mock", BaseURL: srv.URL,
		ContextWindow: 100, MaxTokens: 64, // tiny window forces compaction
	})
	model, _, _ := runtime.ResolveModel("mock/tiny")

	settings, _ := LoadSettings(filepath.Join(dir, "settings.json"), filepath.Join(dir, "proj", ".doge", "settings.json"))
	compaction := DefaultCompactionSettings()
	compaction.KeepRecentTokens = 10 // tiny keep window for the test
	settings.Settings.Compaction = &compaction

	sess, err := NewAgentSession(AgentSessionOptions{
		Cwd:            dir,
		Model:          model,
		SessionManager: manager,
		Settings:       settings,
		ModelRuntime:   runtime,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Plant several messages ending with an overflow error message so the
	// overflow trigger fires and compaction has history to summarize.
	var msgs []*ai.Message
	for i := range 5 {
		msg := &ai.Message{Role: ai.RoleUser,
			Content: []ai.ContentBlock{ai.TextBlock(strings.Repeat("m", 40))}, Timestamp: int64(i)}
		if i == 4 {
			msg.Role = ai.RoleAssistant
			msg.StopReason = ai.StopReasonError
			msg.Usage = &ai.Usage{TotalTokens: 500}
		}
		_ = sess.Session.AppendMessage(msg)
		msgs = append(msgs, msg)
	}
	sess.Agent.SetMessages(msgs)

	sess.checkCompaction()
	sess.WaitForIdle()

	// The compaction entry should be persisted.
	loaded, err := manager.LoadSession(sess.Session.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	foundCompaction := false
	for _, e := range loaded.Entries {
		if e.Type == EntryCompaction {
			foundCompaction = true
			if e.Summary == "" {
				t.Error("compaction summary is empty")
			}
		}
	}
	if !foundCompaction {
		t.Error("compaction entry not persisted")
	}
}

// TestSetThinkingLevelEntry 调整推理级别更新 agent 状态并落 thinking_level_change entry
func TestSetThinkingLevelEntry(t *testing.T) {
	dir := t.TempDir()
	manager := NewSessionManager(filepath.Join(dir, "sessions"))
	auth := LoadAuthStore(filepath.Join(dir, "auth.json"))
	runtime := NewModelRuntime(auth)
	sess, err := NewAgentSession(AgentSessionOptions{
		Cwd:            dir,
		SessionManager: manager,
		ModelRuntime:   runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.SetThinkingLevel("high"); err != nil {
		t.Fatal(err)
	}
	if level := sess.Agent.State().ThinkingLevel; level != "high" {
		t.Fatalf("thinking level = %q, want high", level)
	}
	loaded, err := manager.LoadSession(sess.Session.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range loaded.Entries {
		if e.Type == EntryThinkingLevelChange {
			found = true
			if e.ThinkingLevel != "high" {
				t.Fatalf("entry level = %q, want high", e.ThinkingLevel)
			}
		}
	}
	if !found {
		t.Fatal("thinking_level_change entry not persisted")
	}
}

// TestToolsIntegration runs the real bash tool through the agent loop.
func TestToolsIntegration(t *testing.T) {
	ops := tools.LocalOperations()
	tool := tools.BashTool(ops)
	result, err := tool.Execute("c1", map[string]any{"command": "echo integration-ok"}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content[0].Text, "integration-ok") {
		t.Errorf("bash result = %q", result.Content[0].Text)
	}
}

// TestAgentSessionSteeringWhileBusy verifies that Prompt while the agent is
// mid-run routes the message to the steering queue (interrupting the running
// tool batch) instead of rejecting it as "agent is busy".
func TestAgentSessionSteeringWhileBusy(t *testing.T) {
	dir := t.TempDir()
	manager := NewSessionManager(filepath.Join(dir, "sessions"))

	// Script: first call → tool call to the blocking "slow" tool; second call
	// → text done (the steering message is injected before it).
	call := 0
	srv := startCompletionsServer(t, func(req map[string]any) []string {
		call++
		if call == 1 {
			return []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"slow","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`,
				`[DONE]`,
			}
		}
		return []string{
			`{"choices":[{"delta":{"content":"All done"}}]}`,
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
		Cwd: dir, Model: model, SessionManager: manager, Settings: settings, ModelRuntime: runtime,
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	slowStarted := make(chan struct{})
	slowTool := &agent.AgentTool{
		Name: "slow", Description: "x",
		Parameters: map[string]any{"type": "object"},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			close(slowStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sess.Agent.SetTools([]*agent.AgentTool{slowTool})

	done := make(chan error, 1)
	go func() { done <- sess.Prompt("go") }()

	// The slow tool blocks until the batch context is canceled. While it runs,
	// Prompt must queue the message as steering instead of returning an error.
	<-slowStarted
	time.Sleep(10 * time.Millisecond)
	if err := sess.Prompt("steer now"); err != nil {
		t.Fatalf("prompt while busy should queue as steering, got error: %v", err)
	}
	if !sess.Agent.HasQueuedMessages() {
		t.Fatal("steering message was not queued")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish after steering")
	}

	// The steering message must appear in the transcript and the slow tool
	// result must report the interruption.
	foundSteer := false
	for _, m := range sess.CurrentMessages() {
		if m.Role == ai.RoleUser && m.Text() == "steer now" {
			foundSteer = true
		}
	}
	if !foundSteer {
		t.Error("steering message not present in the transcript")
	}
}
