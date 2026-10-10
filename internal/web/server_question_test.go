package web

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding"
)

// newRealSession builds a real AgentSession (mock catalog, no LLM calls in
// this test — the question flow never runs the model).
func newRealSession(t *testing.T) *coding.AgentSession {
	t.Helper()
	dir := t.TempDir()
	manager := coding.NewSessionManager(filepath.Join(dir, "sessions"))
	auth := coding.LoadAuthStore(filepath.Join(dir, "auth.json"))
	runtime := coding.NewModelRuntime(auth)
	runtime.Models.AddProvider(&ai.Provider{ID: "webmock", Name: "webmock", RequiresKey: false})
	runtime.Models.AddModel(&ai.Model{ID: "m", API: "webmock", Provider: "webmock", ContextWindow: 100000})
	model, _, err := runtime.ResolveModel("webmock/m")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := coding.LoadSettings(filepath.Join(dir, "s.json"), filepath.Join(dir, "p", ".doge", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := coding.NewAgentSession(coding.AgentSessionOptions{
		Cwd: dir, Model: model, SessionManager: manager, Settings: settings, ModelRuntime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// TestServerMeshQuestionRoundTrip verifies the full web question-bypass path
// with a REAL session: a connected browser makes the session interactive, the
// question event reaches the browser over the WebSocket, and POST
// /api/question/answer delivers the human's reply to the waiting channel
// (out-of-band, never through the agent loop). This is the "webui 在线时的
// 远端任务提问入口" the mesh bridge routes through.
func TestServerMeshQuestionRoundTrip(t *testing.T) {
	sess := newRealSession(t)
	_, ts := newTestServer(t, NewBackend(sess, "test", false))
	c := login(t, ts, "tok-123")

	// Connect a browser: it drives the session's kind to interactive.
	conn, ctx := newAuthedWS(t, ts, c)
	readJSON(t, ctx, conn) // consume the initial session_info snapshot
	waitFor(t, "session interactive after browser connect", func() bool {
		return sess.InteractiveCount() > 0
	})

	// The master bridge would call AskQuestion here (simulated).
	answerCh, err := sess.AskQuestion(agent.Question{
		ID: "q-1", Prompt: "Which port should the remote task bind?", Kind: "mesh", From: "peer-b", Task: "task-1",
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	// The browser receives the question event with the id.
	frame := readJSON(t, ctx, conn)
	if frame["type"] != string(agent.EventQuestion) {
		t.Fatalf("browser frame = %#v, want question event", frame)
	}
	q, _ := frame["question"].(map[string]any)
	if q == nil || q["id"] != "q-1" || q["prompt"] != "Which port should the remote task bind?" {
		t.Fatalf("question payload = %#v", q)
	}

	// The user answers via /api/question/answer.
	body, _ := json.Marshal(map[string]string{"id": "q-1", "text": "use 8080"})
	postJSON(t, c, ts.URL+"/api/question/answer", string(body), 200)

	// The waiting channel receives the answer.
	select {
	case ans := <-answerCh:
		if ans != "use 8080" {
			t.Fatalf("answer = %q", ans)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("answer not delivered to the channel")
	}
}

// TestServerMeshQuestionNotPending: answering an unknown/question-not-pending
// id returns 404.
func TestServerMeshQuestionNotPending(t *testing.T) {
	sess := newRealSession(t)
	_, ts := newTestServer(t, NewBackend(sess, "test", false))
	c := login(t, ts, "tok-123")

	body, _ := json.Marshal(map[string]string{"id": "nope", "text": "x"})
	postJSON(t, c, ts.URL+"/api/question/answer", string(body), 404)
}
