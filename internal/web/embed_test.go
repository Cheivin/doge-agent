package web

// 静态服务与新赋能端点的回归：共享 UI 内核（webui.FS）经 / 正常出，
// /platform.js 改写到 platform-web.js；桌面能力反向赋能端点形状与桌面绑定一致。

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// getBody GET 并返回状态码 + 响应体
func getBody(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(data)
}

// doBody 任意方法请求并返回状态码 + 响应体
func doBody(t *testing.T, client *http.Client, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(data)
}

// TestStaticSharedUI 共享前端经 webui.FS 服务；/platform.js 即 platform-web.js
func TestStaticSharedUI(t *testing.T) {
	_, ts := newTestServer(t, &fakeBackend{})
	client := &http.Client{}

	code, html := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("GET / = %d", code)
	}
	for _, want := range []string{`src="/platform.js"`, `src="/app.js"`, `href="/style.css"`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html 缺少 %q", want)
		}
	}

	code, platform := getBody(t, client, ts.URL+"/platform.js")
	if code != 200 {
		t.Fatalf("GET /platform.js = %d", code)
	}
	if !strings.Contains(platform, "export function api") || !strings.Contains(platform, "export const caps") {
		t.Errorf("/platform.js 未命中 platform-web.js 内容")
	}
	for _, path := range []string{"/app.js", "/style.css"} {
		if c, _ := getBody(t, client, ts.URL+path); c != 200 {
			t.Errorf("GET %s = %d", path, c)
		}
	}
}

// TestDesktopParityEndpoints 反向赋能端点：形状对齐桌面绑定，均需鉴权
func TestDesktopParityEndpoints(t *testing.T) {
	t.Setenv("DOGE_CODING_AGENT_DIR", t.TempDir())
	_, ts := newTestServer(t, &fakeBackend{})
	client := login(t, ts, "tok-123")

	// 转录分片：空转录 → 空 messages + offset 0
	code, body := getBody(t, client, ts.URL+"/api/session/messages?before=10&limit=5")
	if code != 200 || !strings.Contains(body, `"offset":0`) {
		t.Errorf("GET /api/session/messages = %d %s", code, body)
	}

	// 权限策略：GET（空）→ PUT → GET 读回
	code, body = getBody(t, client, ts.URL+"/api/permissions")
	if code != 200 || !strings.Contains(body, `"policy"`) || !strings.Contains(body, `"path"`) {
		t.Errorf("GET /api/permissions = %d %s", code, body)
	}
	code, body = doBody(t, client, "PUT", ts.URL+"/api/permissions", `{"defaultPolicy":{"bash":"ask"}}`)
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Errorf("PUT /api/permissions = %d %s", code, body)
	}
	if _, body = getBody(t, client, ts.URL+"/api/permissions"); !strings.Contains(body, `"bash":"ask"`) {
		t.Errorf("permissions 未读回写入值: %s", body)
	}

	// mesh 运行时：未注入钩子 → 503
	if code, _ = getBody(t, client, ts.URL+"/api/mesh/running"); code != http.StatusServiceUnavailable {
		t.Errorf("GET /api/mesh/running = %d，want 503（无运行时钩子）", code)
	}
	for _, path := range []string{"/api/mesh/start", "/api/mesh/stop"} {
		if code, _ = doBody(t, client, "POST", ts.URL+path, ""); code != http.StatusServiceUnavailable {
			t.Errorf("POST %s = %d，want 503（无运行时钩子）", path, code)
		}
	}

	// mesh.json 文件级读写：GET → POST（token 留空保留现值）→ GET 读回
	code, body = getBody(t, client, ts.URL+"/api/mesh/config")
	if code != 200 || !strings.Contains(body, `"config"`) {
		t.Errorf("GET /api/mesh/config = %d %s", code, body)
	}
	code, body = doBody(t, client, "POST", ts.URL+"/api/mesh/config", `{"name":"n1","allowRemoteTasks":true,"token":""}`)
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Errorf("POST /api/mesh/config = %d %s", code, body)
	}
	if _, body = getBody(t, client, ts.URL+"/api/mesh/config"); !strings.Contains(body, `"name":"n1"`) {
		t.Errorf("mesh.json 未读回写入值: %s", body)
	}

	// 未鉴权一律拒绝
	bare := &http.Client{}
	for _, path := range []string{"/api/permissions", "/api/mesh/config", "/api/session/messages"} {
		if c, _ := getBody(t, bare, ts.URL+path); c == http.StatusOK {
			t.Errorf("%s 未鉴权即 200", path)
		}
	}
}
