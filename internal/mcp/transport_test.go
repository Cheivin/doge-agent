package mcp

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestHTTPHeadersApplied 配置的 headers（含 env 插值）必须注入 HTTP 传输的
// 每个出站请求——http 类型 MCP server 的鉴权全靠它。
func TestHTTPHeadersApplied(t *testing.T) {
	var mu sync.Mutex
	var got http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	t.Setenv("DOGE_TEST_TOKEN", "tok-123")
	s := &server{name: "h", mgr: New(Options{Version: "test"}), cfg: &Server{
		URL: ts.URL,
		Headers: map[string]string{
			"Authorization": "Bearer ${DOGE_TEST_TOKEN}",
			"X-Custom":      "v1",
		},
	}}
	tr, err := s.transport()
	if err != nil {
		t.Fatal(err)
	}
	sct, ok := tr.(*mcp.StreamableClientTransport)
	if !ok || sct.HTTPClient == nil {
		t.Fatalf("transport = %T, want *mcp.StreamableClientTransport", tr)
	}
	resp, err := sct.HTTPClient.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if got == nil {
		t.Fatal("request never reached the test server")
	}
	if got.Get("Authorization") != "Bearer tok-123" {
		t.Fatalf("authorization = %q (env interpolation failed?)", got.Get("Authorization"))
	}
	if got.Get("X-Custom") != "v1" {
		t.Fatalf("x-custom = %q", got.Get("X-Custom"))
	}
}

// TestTransportInference type 标注与字段推断
func TestTransportInference(t *testing.T) {
	cases := []struct {
		name string
		cfg  Server
		want string
	}{
		{"stdio by command", Server{Type: "stdio", Command: "npx"}, "stdio"},
		{"http by url", Server{Type: "http", URL: "https://x/mcp"}, "http"},
		{"sse treated as http via url", Server{Type: "sse", URL: "https://x/sse"}, "http"},
		{"default stdio", Server{Command: "foo"}, "stdio"},
	}
	for _, c := range cases {
		if got := transportOf(&c.cfg); got != c.want {
			t.Errorf("%s: transport = %q, want %q", c.name, got, c.want)
		}
	}
	// 无 command/url 的构造报错
	s := &server{name: "bad", cfg: &Server{Type: "stdio"}, mgr: New(Options{Version: "test"})}
	if _, err := s.transport(); err == nil {
		t.Fatal("missing command/url must error")
	}
}
