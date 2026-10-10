package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"doge/internal/agent"
	"doge/internal/mcp"
)

// fakeMcpView is a scripted MCP view for the /api/mcp endpoint tests.
type fakeMcpView struct{}

func (fakeMcpView) Enabled() bool { return true }
func (fakeMcpView) Servers() []mcp.ServerInfo {
	return []mcp.ServerInfo{{
		Name: "fake", Transport: "stdio", Enabled: true, Connected: true,
		Tools: []mcp.ToolInfo{{Name: "echo", Description: "echo text"}},
	}}
}
func (fakeMcpView) Reconnect(name string) error                { return nil }
func (fakeMcpView) SetEnabled(name string, enabled bool) error { return nil }
func (fakeMcpView) Setup() error                               { return nil }
func (fakeMcpView) Reload() error                              { return nil }
func (fakeMcpView) UpsertServers(servers map[string]*mcp.Server) error {
	for name, srv := range servers {
		if name == "" || (srv.Command == "" && srv.URL == "") {
			return errors.New("command or url required")
		}
	}
	return nil
}
func (fakeMcpView) RemoveServer(name string) error {
	if name == "global-only" {
		return errors.New("not in project config")
	}
	return nil
}
func (fakeMcpView) BuildTools() []*agent.AgentTool {
	return []*agent.AgentTool{{Name: "mcp"}}
}
func (fakeMcpView) WritePath() string { return "/tmp/mcp.json" }
func (fakeMcpView) SetServerDirect(name string, on bool) error {
	if name == "missing" {
		return errors.New("not found")
	}
	return nil
}
func (fakeMcpView) SetServerAutoStart(name string, on bool) error {
	if name == "missing" {
		return errors.New("not found")
	}
	return nil
}

// buildToolsCalls counts redistribute triggers (reload/enable/reconnect).
type countingMcpView struct {
	fakeMcpView
	mu    sync.Mutex
	calls int
}

func (c *countingMcpView) BuildTools() []*agent.AgentTool {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return []*agent.AgentTool{{Name: "mcp"}}
}

// TestMcpEndpoints: /api/mcp reports the manager state; reconnect and
// enable/disable route through the view; without a view everything is 404;
// the /api/config overview carries the mcp section.
func TestMcpEndpoints(t *testing.T) {
	f := &fakeBackend{}
	s, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	getJSON(t, c, ts.URL+"/api/mcp", http.StatusNotFound)
	postJSON(t, c, ts.URL+"/api/mcp/reconnect", `{"server":"fake"}`, http.StatusNotFound)

	s.mcpView = fakeMcpView{}
	m := getJSON(t, c, ts.URL+"/api/mcp", http.StatusOK)
	if m["enabled"] != true {
		t.Fatalf("mcp = %#v", m)
	}
	servers, _ := m["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("servers = %#v", m["servers"])
	}
	postJSON(t, c, ts.URL+"/api/mcp/reconnect", `{"server":"fake"}`, http.StatusOK)
	postJSON(t, c, ts.URL+"/api/mcp/reconnect", `{invalid`, http.StatusBadRequest)
	postJSON(t, c, ts.URL+"/api/mcp/enable", `{"server":"fake"}`, http.StatusOK)
	postJSON(t, c, ts.URL+"/api/mcp/disable", `{"server":"fake"}`, http.StatusOK)

	// /api/config overview exposes the mcp section
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	m = getJSON(t, c, ts.URL+"/api/config", http.StatusOK)
	mcpSection, _ := m["mcp"].(map[string]any)
	if mcpSection == nil || mcpSection["enabled"] != true {
		t.Fatalf("config mcp section = %#v", m["mcp"])
	}
	if mcpSection["servers"] == nil {
		t.Fatalf("config mcp servers missing: %#v", m["mcp"])
	}
}

// TestSessionMcpEndpoints: per-session MCP state read/toggle; the toggle only
// records intent (pending) until a reload lands it; remote-task sessions are
// rejected with 403.
func TestSessionMcpEndpoints(t *testing.T) {
	f := &fakeBackend{mcpApplied: true} // 初始已挂载
	_, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	m := getJSON(t, c, ts.URL+"/api/session/mcp", http.StatusOK)
	if m["available"] != true || m["enabled"] != true || m["pending"] != false {
		t.Fatalf("initial session mcp = %#v", m)
	}

	// 停用：记录意图，pending=true
	m = postJSON(t, c, ts.URL+"/api/session/mcp", `{"enabled":false}`, http.StatusOK)
	if m["pending"] != true {
		t.Fatalf("toggle response = %#v", m)
	}
	m = getJSON(t, c, ts.URL+"/api/session/mcp", http.StatusOK)
	if m["enabled"] != false || m["mounted"] != true || m["pending"] != true {
		t.Fatalf("state after disable = %#v (want enabled=false mounted=true pending=true)", m)
	}

	// 重载落地：mcpApplied 报告本次重载应用了会话 MCP 变更
	m = postJSON(t, c, ts.URL+"/api/reload", "", http.StatusOK)
	if m["mcpApplied"] != true {
		t.Fatalf("reload response = %#v", m)
	}
	m = getJSON(t, c, ts.URL+"/api/session/mcp", http.StatusOK)
	if m["mounted"] != false || m["pending"] != false {
		t.Fatalf("state after reload = %#v", m)
	}
	// 无 pending 时重载不再报 applied
	m = postJSON(t, c, ts.URL+"/api/reload", "", http.StatusOK)
	if m["mcpApplied"] != false {
		t.Fatalf("second reload response = %#v", m)
	}

	// 非法 body
	postJSON(t, c, ts.URL+"/api/session/mcp", `{}`, http.StatusBadRequest)

	// 远端任务会话 → 403
	f.remoteSession = true
	postJSON(t, c, ts.URL+"/api/session/mcp", `{"enabled":true}`, http.StatusForbidden)
	getJSON(t, c, ts.URL+"/api/session/mcp", http.StatusOK) // 仍可读状态
}

// TestMcpServersCrud: project-config server upsert/remove + config reload.
func TestMcpServersCrud(t *testing.T) {
	f := &fakeBackend{}
	s, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	getJSON(t, c, ts.URL+"/api/mcp", http.StatusNotFound)
	s.mcpView = fakeMcpView{}

	postJSON(t, c, ts.URL+"/api/mcp/servers", `{"web":{"type":"http","url":"https://x/mcp"}}`, http.StatusOK)
	postJSON(t, c, ts.URL+"/api/mcp/servers", `{"bad":{"type":"stdio"}}`, http.StatusBadRequest)
	postJSON(t, c, ts.URL+"/api/mcp/servers", `{invalid`, http.StatusBadRequest)

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/mcp/servers/web", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	req2, _ := http.NewRequest("DELETE", ts.URL+"/api/mcp/servers/global-only", nil)
	resp2, err := c.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("global-only delete = %d (want 400)", resp2.StatusCode)
	}

	postJSON(t, c, ts.URL+"/api/mcp/reload", "", http.StatusOK)
}

// TestMcpServerFlagEndpoints: per-server direct / autostart toggles.
func TestMcpServerFlagEndpoints(t *testing.T) {
	f := &fakeBackend{}
	s, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	s.mcpView = fakeMcpView{}
	postJSON(t, c, ts.URL+"/api/mcp/servers/web/direct", `{"enabled":true}`, http.StatusOK)
	postJSON(t, c, ts.URL+"/api/mcp/servers/web/direct", `{"enabled":false}`, http.StatusOK)
	postJSON(t, c, ts.URL+"/api/mcp/servers/web/autostart", `{"enabled":true}`, http.StatusOK)
	postJSON(t, c, ts.URL+"/api/mcp/servers/missing/autostart", `{"enabled":true}`, http.StatusNotFound)
	postJSON(t, c, ts.URL+"/api/mcp/servers/web/direct", `{}`, http.StatusBadRequest)
}

// TestMcpReloadRedistributes: reload/enable/reconnect 触发 BuildTools（工具组
// 重发到活动会话的分发入口）。
func TestMcpReloadRedistributes(t *testing.T) {
	f := &fakeBackend{}
	s, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	cv := &countingMcpView{}
	s.mcpView = cv
	postJSON(t, c, ts.URL+"/api/mcp/reload", "", http.StatusOK)
	postJSON(t, c, ts.URL+"/api/mcp/enable", `{"server":"x"}`, http.StatusOK)
	postJSON(t, c, ts.URL+"/api/mcp/reconnect", `{"server":""}`, http.StatusOK)

	cv.mu.Lock()
	defer cv.mu.Unlock()
	if cv.calls < 3 {
		t.Fatalf("BuildTools calls = %d, want >= 3 (one per mutating action)", cv.calls)
	}
}

// bearerClient is an http.Client that stamps the access token as a Bearer
// header on every request (the API-client credential channel).
func bearerClient(token string) *http.Client {
	return &http.Client{Transport: bearerTransport{token: token}}
}

type bearerTransport struct {
	token string
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(clone)
}

// TestBearerAuth: the access token in an Authorization header authenticates
// API clients without the login/cookie exchange; a wrong token is rejected.
func TestBearerAuth(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)

	c := bearerClient("tok-123")
	m := getJSON(t, c, ts.URL+"/api/status", http.StatusOK)
	if m["sessionId"] != "sess-1" {
		t.Fatalf("status = %#v", m)
	}
	m = getJSON(t, c, ts.URL+"/api/auth/whoami", http.StatusOK)
	if m["method"] != "bearer" {
		t.Fatalf("whoami = %#v", m)
	}

	bad := bearerClient("nope")
	if resp, err := bad.Get(ts.URL + "/api/status"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong bearer token = %d, want 401", resp.StatusCode)
		}
	}
}

// TestQueryTokenAuth: ?token= authenticates too (WebSocket / EventSource
// connections cannot set headers).
func TestQueryTokenAuth(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)

	if resp, err := http.Get(ts.URL + "/api/status?token=tok-123"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("query token = %d, want 200", resp.StatusCode)
		}
	}
	if resp, err := http.Get(ts.URL + "/api/status?token=wrong"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong query token = %d, want 401", resp.StatusCode)
		}
	}
}

// TestCORSPreflight: with origins configured, a browser preflight is answered
// and the allow-origin header follows the request's origin; without origins
// configured nothing CORS-ish is emitted (same-origin-only default).
func TestCORSPreflight(t *testing.T) {
	f := &fakeBackend{}
	s, ts := newTestServer(t, f)

	req, _ := http.NewRequest("OPTIONS", ts.URL+"/api/status", nil)
	req.Header.Set("Origin", "https://ui.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("cors header without configured origins = %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}

	s.corsOrigins = []string{"https://ui.example.com"}
	req2, _ := http.NewRequest("OPTIONS", ts.URL+"/api/prompt", nil)
	req2.Header.Set("Origin", "https://ui.example.com")
	req2.Header.Set("Access-Control-Request-Method", "POST")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", resp2.StatusCode)
	}
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "https://ui.example.com" {
		t.Fatalf("allow-origin = %q", got)
	}

	// A non-matching origin gets no CORS headers and the request itself is
	// still unauthorized without credentials.
	req3, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req3.Header.Set("Origin", "https://evil.example.net")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("allow-origin for foreign origin = %q", resp3.Header.Get("Access-Control-Allow-Origin"))
	}
}

// TestConfigEndpoints: /api/config reports the layout and effective settings;
// PUT /api/config/settings rewrites the global file and says a restart is
// needed; GET reflects the change.
func TestConfigEndpoints(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)

	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	m := getJSON(t, c, ts.URL+"/api/config", http.StatusOK)
	paths, _ := m["paths"].(map[string]any)
	if paths["settings"] != filepath.Join(dir, "settings.json") {
		t.Fatalf("paths = %#v", paths)
	}
	if m["meshEnabled"] != false {
		t.Fatalf("meshEnabled = %v", m["meshEnabled"])
	}

	put := func(body string, want int) map[string]any {
		req, _ := http.NewRequest("PUT", ts.URL+"/api/config/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("PUT settings status = %d (want %d)", resp.StatusCode, want)
		}
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m = put(`{"defaultProvider":"model","defaultModel":"m1"}`, http.StatusOK)
	if m["restartRequired"] != true {
		t.Fatalf("put response = %#v", m)
	}
	put(`{invalid`, http.StatusBadRequest)

	got := getJSON(t, c, ts.URL+"/api/config/settings", http.StatusOK)
	settings, _ := got["settings"].(map[string]any)
	if settings["defaultModel"] != "m1" {
		t.Fatalf("settings after put = %#v", settings)
	}
}

// TestAuthConfigEndpoints: provider keys are listed masked, stored via POST
// (empty key deletes) and removed via DELETE; the response reports whether
// the live reload took.
func TestAuthConfigEndpoints(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)

	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	m := postJSON(t, c, ts.URL+"/api/config/auth", `{"provider":"openai","key":"sk-secret-1234"}`, http.StatusOK)
	if m["reloaded"] != true {
		t.Fatalf("set response = %#v", m)
	}
	m = getJSON(t, c, ts.URL+"/api/config/auth", http.StatusOK)
	providers, _ := m["providers"].(map[string]any)
	entry, _ := providers["openai"].(map[string]any)
	if entry == nil || entry["maskedKey"] != "…1234" || entry["configured"] != true {
		t.Fatalf("providers = %#v", providers)
	}
	if raw, _ := json.Marshal(m); strings.Contains(string(raw), "sk-secret-1234") {
		t.Fatalf("raw key leaked: %s", raw)
	}

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/config/auth/openai", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	m = getJSON(t, c, ts.URL+"/api/config/auth", http.StatusOK)
	if providers, _ = m["providers"].(map[string]any); len(providers) != 0 {
		t.Fatalf("providers after delete = %#v", providers)
	}

	postJSON(t, c, ts.URL+"/api/config/auth", `{"key":"sk-x"}`, http.StatusBadRequest)
}

// TestLogsEndpoint: ?lines tails the newest lines of the diagnostic log;
// ?date selects a rotated archive and rejects anything path-like.
func TestLogsEndpoint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "doge.log"), []byte("l1\nl2\nl3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "doge.log.2026-08-24"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	m := getJSON(t, c, ts.URL+"/api/logs?lines=2", http.StatusOK)
	lines, _ := m["lines"].([]any)
	if len(lines) != 2 || lines[0] != "l2" {
		t.Fatalf("lines = %#v", lines)
	}
	m = getJSON(t, c, ts.URL+"/api/logs?date=2026-08-24", http.StatusOK)
	if lines, _ = m["lines"].([]any); len(lines) != 1 || lines[0] != "old" {
		t.Fatalf("archived lines = %#v", lines)
	}
	getJSON(t, c, ts.URL+"/api/logs?date=../../etc/passwd", http.StatusBadRequest)
	getJSON(t, c, ts.URL+"/api/logs?date=2026-01-01", http.StatusNotFound)
}

// TestMeshForwardEndpoints: forward rules list/add/enable/disable/remove via
// REST; every endpoint 404s when the mesh is not enabled.
func TestMeshForwardEndpoints(t *testing.T) {
	f := &fakeBackend{}
	s, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	getJSON(t, c, ts.URL+"/api/mesh/forwards", http.StatusNotFound)
	postJSON(t, c, ts.URL+"/api/mesh/forwards", `{"name":"web","mode":"expose","local":"127.0.0.1:8080"}`, http.StatusNotFound)

	s.meshView = fakeMeshView{}
	m := getJSON(t, c, ts.URL+"/api/mesh/forwards", http.StatusOK)
	forwards, _ := m["forwards"].([]any)
	if len(forwards) != 1 {
		t.Fatalf("forwards = %#v", m["forwards"])
	}
	postJSON(t, c, ts.URL+"/api/mesh/forwards", `{"name":"db","mode":"expose","local":"127.0.0.1:5432"}`, http.StatusOK)
	postJSON(t, c, ts.URL+"/api/mesh/forwards", `{invalid`, http.StatusBadRequest)
	postJSON(t, c, ts.URL+"/api/mesh/forwards/web/enable", "", http.StatusOK)
	postJSON(t, c, ts.URL+"/api/mesh/forwards/web/disable", "", http.StatusOK)

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/mesh/forwards/web", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete forward = %d", resp.StatusCode)
	}

	// The overview carries the extended mesh payload.
	m = getJSON(t, c, ts.URL+"/api/mesh", http.StatusOK)
	if m["config"] == nil || m["addrs"] == nil {
		t.Fatalf("mesh payload missing config/addrs: %#v", m)
	}
}

// TestModelsAllParam: ?all=1 returns the full catalog, not just the runnable
// subset.
func TestModelsAllParam(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	m := getJSON(t, c, ts.URL+"/api/models", http.StatusOK)
	if models, _ := m["models"].([]any); len(models) != 1 {
		t.Fatalf("models = %#v", m["models"])
	}
	m = getJSON(t, c, ts.URL+"/api/models?all=1", http.StatusOK)
	if models, _ := m["models"].([]any); len(models) != 2 {
		t.Fatalf("all models = %#v", m["models"])
	}
}

// TestPermissionAnswerNotFound: answering an unknown/expired permission ask
// is a 404; a missing id is a 400.
func TestPermissionAnswerNotFound(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	postJSON(t, c, ts.URL+"/api/permission/answer", `{"id":"ask-99","deny":true}`, http.StatusNotFound)
	postJSON(t, c, ts.URL+"/api/permission/answer", `{"deny":true}`, http.StatusBadRequest)
}

// TestShutdownEndpoint: without a hook wired the endpoint is 503; with one it
// answers 200 and the hook fires.
func TestShutdownEndpoint(t *testing.T) {
	f := &fakeBackend{}
	s, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	postJSON(t, c, ts.URL+"/api/shutdown", "", http.StatusServiceUnavailable)

	fired := make(chan struct{})
	s.shutdown = func() { close(fired) }
	postJSON(t, c, ts.URL+"/api/shutdown", "", http.StatusOK)
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown hook did not fire")
	}
}
