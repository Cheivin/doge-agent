package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/permissions"
	"doge/internal/mesh"
)

// fakeMeshView is a scripted mesh view for the /api/mesh endpoint test.
type fakeMeshView struct{}

func (fakeMeshView) Self() mesh.AgentProfile {
	return mesh.AgentProfile{PeerID: "self-id", Name: "node-a", Roles: []string{"master"}}
}
func (fakeMeshView) Peers() []mesh.PeerInfo {
	return []mesh.PeerInfo{{ID: "peer-b", Profile: mesh.AgentProfile{Name: "node-b", Roles: []string{"worker"}, Accepting: true}}}
}
func (fakeMeshView) Tasks() []mesh.TaskStatus {
	return []mesh.TaskStatus{{TaskID: "t1", Peer: "peer-b", Status: "running", Progress: "working"}}
}
func (fakeMeshView) Addrs() []string { return []string{"/ip4/192.168.1.10/tcp/4001"} }
func (fakeMeshView) Config() mesh.Config {
	return mesh.Config{Name: "node-a", AllowRemoteTasks: true}
}
func (fakeMeshView) ForwardList() []mesh.ForwardRule {
	return []mesh.ForwardRule{{Name: "web", Mode: "expose", Local: "127.0.0.1:8080"}}
}
func (fakeMeshView) ForwardAdd(rule mesh.ForwardRule) error       { return nil }
func (fakeMeshView) ForwardRemove(name string) error              { return nil }
func (fakeMeshView) ForwardSetEnabled(name string, en bool) error { return nil }

// TestServerMeshEndpoint: /api/mesh reports this node, connected peers and
// in-flight remote tasks; without a mesh view it is 404.
func TestServerMeshEndpoint(t *testing.T) {
	f := &fakeBackend{}
	s, ts := newTestServer(t, f)
	c := login(t, ts, "tok-123")

	// No mesh view → 404.
	req, _ := http.NewRequest("GET", ts.URL+"/api/mesh", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("mesh without view = %d, want 404", resp.StatusCode)
	}

	// With a mesh view → self/peers/tasks.
	s.meshView = fakeMeshView{}
	m := getJSON(t, c, ts.URL+"/api/mesh", http.StatusOK)
	self, _ := m["self"].(map[string]any)
	if self["peerId"] != "self-id" || self["name"] != "node-a" {
		t.Fatalf("self = %#v", self)
	}
	peers, _ := m["peers"].([]any)
	if len(peers) != 1 {
		t.Fatalf("peers = %#v", m["peers"])
	}
	tasks, _ := m["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %#v", m["tasks"])
	}
}

var errAgentBusy = errors.New("agent is busy")

// fakeBackend is a Backend stub for API tests.
type fakeBackend struct {
	streaming   bool
	promptErr   error
	compactErr  error
	modelErr    error
	modelSpec   string
	thinkingErr error
	thinkingSet string
	sessions    []SessionSummary
	askH        permissions.AskHandler
	subscribed  []agent.AgentListener

	// remoteSession / mcpDesired / mcpApplied drive the per-session MCP
	// endpoint tests (the fake mirrors the real pending → reload-on-apply
	// semantics).
	remoteSession bool
	mcpDesired    *bool
	mcpApplied    bool

	// kinds records the interactivity-kind transitions the server pushed via
	// setKind (browser connect/disconnect), guarded for the WS goroutine.
	kindsMu sync.Mutex
	kinds   []coding.ConsumerKind
}

// Push delivers an event to every registered listener (simulates an agent run
// producing events through the backend).
func (f *fakeBackend) Push(ev agent.AgentEvent) {
	for _, l := range f.subscribed {
		_ = l(ev)
	}
}

// Kinds returns a copy of the recorded setKind transitions.
func (f *fakeBackend) Kinds() []coding.ConsumerKind {
	f.kindsMu.Lock()
	defer f.kindsMu.Unlock()
	return append([]coding.ConsumerKind(nil), f.kinds...)
}

func (f *fakeBackend) Prompt(text string) error {
	if f.promptErr != nil {
		return f.promptErr
	}
	if f.streaming {
		return errAgentBusy
	}
	return nil
}
func (f *fakeBackend) PromptWithAttachments(text string, atts []coding.Attachment) error {
	return f.Prompt(text)
}
func (f *fakeBackend) Abort()         {}
func (f *fakeBackend) Compact() error { return f.compactErr }
func (f *fakeBackend) SwitchModel(spec string) error {
	f.modelSpec = spec
	return f.modelErr
}
func (f *fakeBackend) SetThinkingLevel(level string) error {
	f.thinkingSet = level
	return f.thinkingErr
}
func (f *fakeBackend) NewSession() error                      { return nil }
func (f *fakeBackend) NewSessionIn(cwd string) error          { return nil }
func (f *fakeBackend) SwitchSession(id string) error          { return nil }
func (f *fakeBackend) DeleteSession(id string) error          { return nil }
func (f *fakeBackend) RewriteTurn(seq int, text string) error { return nil }
func (f *fakeBackend) Stats() coding.SessionStats             { return coding.SessionStats{} }

// ReloadConfig lands the pending per-session MCP toggle (mirrors the real
// session's apply-on-reload behaviour).
func (f *fakeBackend) ReloadConfig() error {
	if f.mcpDesired != nil {
		f.mcpApplied = *f.mcpDesired
		f.mcpDesired = nil
	}
	return nil
}
func (f *fakeBackend) Status() Status {
	return Status{
		SessionID:    "sess-1",
		Cwd:          "/tmp",
		Model:        "model/deepseek-v4-flash",
		Thinking:     "max",
		Streaming:    f.streaming,
		PendingTools: []string{},
		Version:      "test",
	}
}
func (f *fakeBackend) ListModels() []ModelInfo {
	return []ModelInfo{{Spec: "model/a", Name: "A", Provider: "model", ID: "a"}}
}
func (f *fakeBackend) ListAllModels() []ModelInfo {
	return []ModelInfo{
		{Spec: "model/a", Name: "A", Provider: "model", ID: "a"},
		{Spec: "model/b", Name: "B", Provider: "model", ID: "b"},
	}
}
func (f *fakeBackend) SessionInfo() SessionInfo {
	return SessionInfo{
		ID:       "sess-1",
		Cwd:      "/tmp",
		FilePath: "/tmp/s.jsonl",
		Messages: []*ai.Message{ai.NewUserTextMessage("hi")},
	}
}
func (f *fakeBackend) ListSessions() ([]SessionSummary, error) {
	return f.sessions, nil
}
func (f *fakeBackend) StreamingMessage() *ai.Message { return nil }
func (f *fakeBackend) Subscribe(l agent.AgentListener) (func(), func(coding.ConsumerKind)) {
	f.subscribed = append(f.subscribed, l)
	return func() {}, func(k coding.ConsumerKind) {
		f.kindsMu.Lock()
		f.kinds = append(f.kinds, k)
		f.kindsMu.Unlock()
	}
}
func (f *fakeBackend) InstallPermissionHandler(h permissions.AskHandler) {
	f.askH = h
}
func (f *fakeBackend) AnswerQuestion(id, text string) bool { return true }
func (f *fakeBackend) McpInfo() McpSessionState {
	st := McpSessionState{Available: true, Mounted: f.mcpApplied, Enabled: f.mcpApplied}
	if f.remoteSession {
		st.Kind = "remote"
		st.Enabled = true
		return st
	}
	if f.mcpDesired != nil {
		st.Enabled = *f.mcpDesired
		st.Pending = *f.mcpDesired != f.mcpApplied
	}
	return st
}
func (f *fakeBackend) SetMcpEnabled(on bool) error {
	if f.remoteSession {
		return errRemoteSessionMcp
	}
	f.mcpDesired = &on
	return nil
}
func (f *fakeBackend) SkillsInfo() []coding.SkillInfo {
	return []coding.SkillInfo{{Name: "demo", Description: "A demo skill", Location: "/tmp/demo/SKILL.md", ContentLength: 10}}
}

// newTestServer builds a server over a backend with a fixed token.
func newTestServer(t *testing.T, f Backend) (*Server, *httptest.Server) {
	t.Helper()
	s, err := NewServer(Options{Backend: f, Token: "tok-123", Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func login(t *testing.T, ts *httptest.Server, token string) *http.Client {
	t.Helper()
	body := strings.NewReader(`{"token":"` + token + `"}`)
	req, _ := http.NewRequest("POST", ts.URL+"/api/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	if len(resp.Cookies()) == 0 {
		t.Fatal("no session cookie set")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(ts.URL)
	jar.SetCookies(u, resp.Cookies())
	return &http.Client{Jar: jar}
}

func getJSON(t *testing.T, client *http.Client, url string, want int) map[string]any {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("GET %s status = %d (want %d)", url, resp.StatusCode, want)
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func postJSON(t *testing.T, client *http.Client, url, payload string, want int) map[string]any {
	t.Helper()
	var reader *strings.Reader
	if payload != "" {
		reader = strings.NewReader(payload)
	} else {
		reader = strings.NewReader("{}")
	}
	req, _ := http.NewRequest("POST", url, reader)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("POST %s status = %d (want %d)", url, resp.StatusCode, want)
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAuthRequired(t *testing.T) {
	f := &fakeBackend{}
	s, ts := newTestServer(t, f)
	_ = s

	// No cookie -> 401.
	if resp, err := http.Get(ts.URL + "/api/status"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status without auth = %d", resp.StatusCode)
		}
	}
	// WS and prompt endpoints are protected too.
	req, _ := http.NewRequest("POST", ts.URL+"/api/prompt", strings.NewReader(`{"text":"x"}`))
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("prompt without auth = %d", resp.StatusCode)
		}
	}
	// Login page is public.
	if resp, err := http.Get(ts.URL + "/"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("index without auth = %d", resp.StatusCode)
		}
	}
}

func TestLoginWrongToken(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	body := strings.NewReader(`{"token":"nope"}`)
	resp, err := http.Post(ts.URL+"/api/auth/login", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", resp.StatusCode)
	}
}

func TestStatusEndpoint(t *testing.T) {
	f := &fakeBackend{streaming: true}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")
	m := getJSON(t, client, ts.URL+"/api/status", http.StatusOK)
	if m["streaming"] != true {
		t.Fatalf("streaming = %v", m["streaming"])
	}
}

func TestPromptConflictWhenBusy(t *testing.T) {
	f := &fakeBackend{streaming: true}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")
	postJSON(t, client, ts.URL+"/api/prompt", `{"text":"hi"}`, http.StatusConflict)
}

func TestPromptAccepted(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")
	postJSON(t, client, ts.URL+"/api/prompt", `{"text":"hello"}`, http.StatusAccepted)
	postJSON(t, client, ts.URL+"/api/prompt", `{"text":"   "}`, http.StatusBadRequest)
}

func TestAbortCompactModel(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")
	postJSON(t, client, ts.URL+"/api/abort", "", http.StatusOK)
	postJSON(t, client, ts.URL+"/api/compact", "", http.StatusOK)
	postJSON(t, client, ts.URL+"/api/model", `{"spec":"model/b"}`, http.StatusOK)
}

func TestModelsEndpoint(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")
	m := getJSON(t, client, ts.URL+"/api/models", http.StatusOK)
	models, ok := m["models"].([]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %#v", m["models"])
	}
}

func TestSessionAndList(t *testing.T) {
	f := &fakeBackend{sessions: []SessionSummary{{ID: "sess-1", Cwd: "/tmp"}}}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")
	m := getJSON(t, client, ts.URL+"/api/session", http.StatusOK)
	if m["id"] != "sess-1" {
		t.Fatalf("session id = %v", m["id"])
	}
	ml := getJSON(t, client, ts.URL+"/api/sessions", http.StatusOK)
	if list, ok := ml["sessions"].([]any); !ok || len(list) != 1 {
		t.Fatalf("sessions = %#v", ml["sessions"])
	}
}

func TestLogoutRevokes(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")
	postJSON(t, client, ts.URL+"/api/auth/logout", "", http.StatusOK)
	// After logout the same cookie is rejected.
	getStatus, err := client.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	getStatus.Body.Close()
	if getStatus.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status after logout = %d", getStatus.StatusCode)
	}
}

// TestGzipCompression: responses to clients that accept gzip are compressed
// (large JSON — session transcripts, message lists — benefits most), while
// clients without Accept-Encoding get plain responses.
func TestGzipCompression(t *testing.T) {
	h := compression(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"data": strings.Repeat("y", 8192)})
	}))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// Client advertises gzip → Content-Encoding: gzip.
	req, _ := http.NewRequest("GET", ts.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}

	// Client without Accept-Encoding → uncompressed.
	req2, _ := http.NewRequest("GET", ts.URL, nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.Header.Get("Content-Encoding") != "" {
		t.Fatalf("no accept-encoding got content-encoding = %q", resp2.Header.Get("Content-Encoding"))
	}
}
