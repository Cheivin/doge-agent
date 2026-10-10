package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"doge/internal/ai"
	"doge/internal/coding"
)

// startMockLLM serves an OpenAI-compatible streaming endpoint that always
// answers immediately (used to build real AgentSessions in these tests).
func startMockLLM(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

// newRealShared builds a real AgentSession over the mock LLM (the TUI's
// shared session seed for per-window tests).
func newRealShared(t *testing.T, llm *httptest.Server) *coding.AgentSession {
	t.Helper()
	dir := t.TempDir()
	manager := coding.NewSessionManager(filepath.Join(dir, "sessions"))
	auth := coding.LoadAuthStore(filepath.Join(dir, "auth.json"))
	runtime := coding.NewModelRuntime(auth)
	runtime.Models.AddProvider(&ai.Provider{ID: "mock", Name: "Mock", BaseURL: llm.URL, RequiresKey: false})
	runtime.Models.AddModel(&ai.Model{
		ID: "mock-model", API: ai.APIOpenAICompletions, Provider: "mock", BaseURL: llm.URL,
		ContextWindow: 100000, MaxTokens: 4096,
	})
	model, _, err := runtime.ResolveModel("mock/mock-model")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := coding.LoadSettings(filepath.Join(dir, "settings.json"), filepath.Join(dir, "proj", ".doge", "settings.json"))
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

// newWindowServer builds a server over a real shared AgentSession.
func newWindowServer(t *testing.T, shared *coding.AgentSession, tuiActive bool) (*Server, *httptest.Server) {
	t.Helper()
	s, err := NewServer(Options{
		Backend: NewBackend(shared, "test", tuiActive),
		Token:   "tok-123",
		Address: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// dialWSC dials /ws for a specific window (clientID).
func dialWSC(t *testing.T, ts *httptest.Server, client *http.Client, windowID string) (*websocket.Conn, context.Context) {
	t.Helper()
	wsURL := "ws" + ts.URL[4:] + "/ws"
	if windowID != "" {
		wsURL += "?client=" + windowID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn, ctx
}

// doWindow sends an API request on behalf of a window.
func doWindow(t *testing.T, client *http.Client, method, url, body, windowID string) int {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("{}")
	}
	req, _ := http.NewRequest(method, url, reader)
	if windowID != "" {
		req.Header.Set("X-Doge-Client", windowID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// doWindowClaim posts a control claim for a window and returns the JSON body.
func doWindowClaim(t *testing.T, client *http.Client, ts *httptest.Server, windowID string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/api/session/claim", strings.NewReader("{}"))
	if windowID != "" {
		req.Header.Set("X-Doge-Client", windowID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("claim %s: %v", windowID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim %s status = %d", windowID, resp.StatusCode)
	}
	m := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return m
}

// readFrameTimeout reads the next WS frame, or reports no frame within the
// timeout.
func readFrameTimeout(t *testing.T, ctx context.Context, conn *websocket.Conn, timeout time.Duration) (map[string]any, bool) {
	t.Helper()
	type res struct {
		m  map[string]any
		ok bool
	}
	ch := make(chan res, 1)
	go func() {
		_, data, err := conn.Read(ctx)
		if err != nil {
			ch <- res{ok: false}
			return
		}
		m := map[string]any{}
		_ = json.Unmarshal(data, &m)
		ch <- res{m: m, ok: true}
	}()
	select {
	case r := <-ch:
		return r.m, r.ok
	case <-time.After(timeout):
		return nil, false
	}
}

// waitPromptIdle waits for an asynchronously-started prompt to actually begin
// (the handlePrompt goroutine may not have been scheduled yet) and then to
// finish, so subsequent prompts aren't rejected as busy.
func waitPromptIdle(shared *coding.AgentSession) {
	deadline := time.Now().Add(3 * time.Second)
	for !shared.Agent.State().IsStreaming {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	shared.WaitForIdle()
}

// TestWindowSessionIsolation: switching sessions in one window must not
// propagate to another window.
func TestWindowSessionIsolation(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	_, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	connA, ctxA := dialWSC(t, ts, c, "winA")
	connB, ctxB := dialWSC(t, ts, c, "winB")
	readJSON(t, ctxA, connA) // snapshot
	readJSON(t, ctxB, connB)

	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winA"); code != http.StatusOK {
		t.Fatalf("winA new session status = %d", code)
	}
	// winA alone receives the reset.
	if m, ok := readFrameTimeout(t, ctxA, connA, 2*time.Second); !ok || m["type"] != "session_reset" {
		t.Fatalf("winA did not get session_reset: %#v ok=%v", m, ok)
	}
	// winB must receive nothing.
	if m, ok := readFrameTimeout(t, ctxB, connB, 500*time.Millisecond); ok {
		t.Fatalf("winB received %#v after winA switched sessions (cross-talk)", m)
	}
}

// TestWindowControlArbitration: the first viewer owns a session; later
// windows observe read-only and may queue for control.
func TestWindowControlArbitration(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	_, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	connA, ctxA := dialWSC(t, ts, c, "winA")
	connB, ctxB := dialWSC(t, ts, c, "winB")
	readJSON(t, ctxA, connA)
	readJSON(t, ctxB, connB)

	// winA (first viewer) owns the shared session → prompt accepted.
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt (owner) = %d, want 202", code)
	}
	// The prompt runs asynchronously on the shared instance; wait for it to
	// actually start and finish so later prompts aren't rejected as busy.
	waitPromptIdle(shared)
	// winB (second viewer) is read-only → prompt rejected.
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusForbidden {
		t.Fatalf("winB prompt (read-only) = %d, want 403", code)
	}
	// winB queues for control.
	if b := doWindowClaim(t, c, ts, "winB"); b["owned"] != false || b["queued"] != true {
		t.Fatalf("winB claim = %#v, want owned=false queued=true", b)
	}
	// winA switches away → releases the session → winB takes over.
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winA"); code != http.StatusOK {
		t.Fatalf("winA new session = %d", code)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt after takeover = %d, want 202", code)
	}
	// winA returns to the original session → now read-only (winB owns it).
	origID := shared.CurrentSession().Header.ID
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/switch", `{"id":"`+origID+`"}`, "winA"); code != http.StatusOK {
		t.Fatalf("winA switch back = %d", code)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusForbidden {
		t.Fatalf("winA prompt after return (winB owns) = %d, want 403", code)
	}
}

// TestWindowControlFIFO: queued windows take control in FIFO order.
func TestWindowControlFIFO(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	_, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	for _, w := range []string{"winA", "winB", "winC"} {
		conn, ctx := dialWSC(t, ts, c, w)
		readJSON(t, ctx, conn)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt = %d", code)
	}
	waitPromptIdle(shared)
	for _, w := range []string{"winB", "winC"} {
		if b := doWindowClaim(t, c, ts, w); b["queued"] != true {
			t.Fatalf("%s claim = %#v, want queued", w, b)
		}
	}
	// A releases → B (FIFO) takes over, C stays read-only.
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winA"); code != http.StatusOK {
		t.Fatalf("winA new = %d", code)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt after release = %d, want 202", code)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winC"); code != http.StatusForbidden {
		t.Fatalf("winC prompt (still queued) = %d, want 403", code)
	}
}

// doWindowTakeover posts a force takeover for a window and returns the JSON
// body ({owned, prevOwner}).
func doWindowTakeover(t *testing.T, client *http.Client, ts *httptest.Server, windowID string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/api/session/takeover", strings.NewReader("{}"))
	if windowID != "" {
		req.Header.Set("X-Doge-Client", windowID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("takeover %s: %v", windowID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("takeover %s status = %d", windowID, resp.StatusCode)
	}
	m := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return m
}

// TestWindowControlForceTakeover: a blocked (queued) window explicitly
// force-takes write control from the current owner — the old owner is released
// immediately, without having to hand off. This is the "强制夺取" path for the
// many-windows / forgot-which-owns-it scenario.
func TestWindowControlForceTakeover(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	_, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	connA, ctxA := dialWSC(t, ts, c, "winA")
	connB, ctxB := dialWSC(t, ts, c, "winB")
	readJSON(t, ctxA, connA)
	readJSON(t, ctxB, connB)

	// winA (first viewer) owns the shared session; winB is read-only + queued.
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt (owner) = %d, want 202", code)
	}
	waitPromptIdle(shared)
	if b := doWindowClaim(t, c, ts, "winB"); b["owned"] != false || b["queued"] != true {
		t.Fatalf("winB claim = %#v, want owned=false queued=true", b)
	}

	// winB explicitly force-takes: previous owner is reported.
	j := doWindowTakeover(t, c, ts, "winB")
	if j["owned"] != true {
		t.Fatalf("takeover = %#v, want owned=true", j)
	}
	if j["prevOwner"] != "winA" {
		t.Fatalf("takeover prevOwner = %#v, want winA", j)
	}

	// winB is now writable; winA is forcibly read-only.
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt after takeover = %d, want 202", code)
	}
	waitPromptIdle(shared)
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusForbidden {
		t.Fatalf("winA prompt after takeover (winB owns) = %d, want 403", code)
	}

	// The displaced owner's status reports writable=false and the new owner.
	st := getStatusFor(t, c, ts, "winA")
	if st["writable"] != false {
		t.Fatalf("winA writable after takeover = %v, want false", st["writable"])
	}
	if st["owner"] != "winB" {
		t.Fatalf("winA owner after takeover = %v, want winB", st["owner"])
	}
}

// TestWindowControlTakeoverFreesOldOwnerImmediately: a takeover works even for
// a window that never queued (blocked by a 403) — it does not need the old
// owner to release.
func TestWindowControlTakeoverFreesOldOwnerImmediately(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	_, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	connA, ctxA := dialWSC(t, ts, c, "winA")
	readJSON(t, ctxA, connA)

	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt = %d, want 202", code)
	}
	waitPromptIdle(shared)

	// winB never opens/claims; it force-takes directly and wins.
	j := doWindowTakeover(t, c, ts, "winB")
	if j["owned"] != true || j["prevOwner"] != "winA" {
		t.Fatalf("takeover = %#v, want owned=true prevOwner=winA", j)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt after direct takeover = %d, want 202", code)
	}
}

// getStatusFor fetches /api/status for a specific window.
func getStatusFor(t *testing.T, client *http.Client, ts *httptest.Server, windowID string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	if windowID != "" {
		req.Header.Set("X-Doge-Client", windowID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("status %s: %v", windowID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %s = %d", windowID, resp.StatusCode)
	}
	m := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return m
}
