package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doge/internal/coding/uploads"
)

// applyServer 带上传暂存区 + 固定 agentDir 的测试环境
func applyServer(t *testing.T, cfg uploads.Config) (*Server, *httptest.Server, *http.Client) {
	t.Helper()
	t.Setenv("DOGE_CODING_AGENT_DIR", t.TempDir()) // AgentDir/skills/models.json 等落临时目录
	f := &fakeBackend{}
	s, ts := newUploadsServer(t, f, cfg)
	c := login(t, ts, "tok-123")
	return s, ts, c
}

// uploadRaw 以 multipart 提交单个二进制文件
func uploadRaw(t *testing.T, c *http.Client, url, filename string, data []byte) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Post(url, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUploadPlace(t *testing.T) {
	_, ts, c := applyServer(t, uploads.Config{})
	m := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{"server.crt": "CERT-DATA"})
	id := firstEntry(t, m)["id"].(string)

	// fakeBackend 的 cwd 是 /tmp：相对路径落其下
	postJSON(t, c, ts.URL+"/api/uploads/"+id+"/place", `{"dest":"deploy/cert.pem"}`, http.StatusOK)
	if _, err := os.Stat("/tmp/deploy/cert.pem"); err != nil {
		t.Fatalf("placed file missing: %v", err)
	}
	defer os.RemoveAll("/tmp/deploy")
	// 越界目的地
	postJSON(t, c, ts.URL+"/api/uploads/"+id+"/place", `{"dest":"/etc/evil"}`, http.StatusBadRequest)
	postJSON(t, c, ts.URL+"/api/uploads/"+id+"/place", `{"dest":"../../evil"}`, http.StatusBadRequest)
	// 非覆盖的重复写入
	postJSON(t, c, ts.URL+"/api/uploads/"+id+"/place", `{"dest":"deploy/cert.pem"}`, http.StatusConflict)
	postJSON(t, c, ts.URL+"/api/uploads/"+id+"/place", `{"dest":"deploy/cert.pem","overwrite":true}`, http.StatusOK)
}

func TestUploadApplyModels(t *testing.T) {
	_, ts, c := applyServer(t, uploads.Config{})
	m := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{
		"models.json": `[{"id":"my-model","provider":"local","api":"openai-completions","baseUrl":"http://127.0.0.1:9999/v1"}]`,
	})
	id := firstEntry(t, m)["id"].(string)

	// dry-run：预览不落盘
	r := postJSON(t, c, ts.URL+"/api/uploads/"+id+"/apply", `{"kind":"models"}`, http.StatusOK)
	if r["dryRun"] != true || !strings.Contains(r["diff"].(string), "local/my-model") {
		t.Fatalf("apply models dry-run = %#v", r)
	}
	if _, err := os.Stat(filepath.Join(agentDirForTest(), "models.json")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote models.json")
	}
	// confirm：写盘 + reload
	r = postJSON(t, c, ts.URL+"/api/uploads/"+id+"/apply", `{"kind":"models","confirm":true}`, http.StatusOK)
	if r["reloaded"] != true || r["applied"] != float64(1) {
		t.Fatalf("apply models confirm = %#v", r)
	}
	if _, err := os.Stat(filepath.Join(agentDirForTest(), "models.json")); err != nil {
		t.Fatalf("models.json missing: %v", err)
	}

	// 非法内容被拒
	bad := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{"bad.json": `{"not":"an array"}`})
	postJSON(t, c, ts.URL+"/api/uploads/"+firstEntry(t, bad)["id"].(string)+"/apply", `{"kind":"models"}`, http.StatusBadRequest)
}

func TestUploadApplySkillsZip(t *testing.T) {
	_, ts, c := applyServer(t, uploads.Config{})
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("demo/SKILL.md")
	w.Write([]byte("---\nname: demo-skill\ndescription: 打包测试技能\n---\n# demo"))
	w2, _ := zw.Create("demo/assets/ref.md")
	w2.Write([]byte("ref"))
	zw.Close()
	_ = w2

	m := uploadRaw(t, c, ts.URL+"/api/uploads", "demo-skill.zip", buf.Bytes())
	id := firstEntry(t, m)["id"].(string)

	r := postJSON(t, c, ts.URL+"/api/uploads/"+id+"/apply", `{"kind":"skills-zip"}`, http.StatusOK)
	if r["dryRun"] != true || !strings.Contains(r["diff"].(string), "demo-skill") {
		t.Fatalf("skills dry-run = %#v", r)
	}
	if _, err := os.Stat(filepath.Join(agentDirForTest(), "skills", "demo-skill")); !os.IsNotExist(err) {
		t.Fatal("dry-run installed skill")
	}
	r = postJSON(t, c, ts.URL+"/api/uploads/"+id+"/apply", `{"kind":"skills-zip","confirm":true}`, http.StatusOK)
	if r["reloaded"] != true {
		t.Fatalf("skills confirm = %#v", r)
	}
	installed := filepath.Join(agentDirForTest(), "skills", "demo-skill")
	for _, p := range []string{"SKILL.md", "assets/ref.md"} {
		if _, err := os.Stat(filepath.Join(installed, p)); err != nil {
			t.Fatalf("installed file missing %s: %v", p, err)
		}
	}
}

func TestUploadApplyPermissions(t *testing.T) {
	_, ts, c := applyServer(t, uploads.Config{})
	m := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{
		"permissions.json": `{"defaultPolicy":{"bash":"ask"},"tools":{"bash":"allow"},"bash":[{"pattern":"git *","decision":"allow"}]}`,
	})
	id := firstEntry(t, m)["id"].(string)

	r := postJSON(t, c, ts.URL+"/api/uploads/"+id+"/apply", `{"kind":"permissions"}`, http.StatusOK)
	if r["dryRun"] != true || !strings.Contains(r["diff"].(string), "bash 规则：1 条") {
		t.Fatalf("apply permissions dry-run = %#v", r)
	}
	r = postJSON(t, c, ts.URL+"/api/uploads/"+id+"/apply", `{"kind":"permissions","confirm":true}`, http.StatusOK)
	if r["reloaded"] != true {
		t.Fatalf("apply permissions confirm = %#v", r)
	}
	if _, err := os.Stat(filepath.Join(agentDirForTest(), "permissions.json")); err != nil {
		t.Fatalf("permissions.json missing: %v", err)
	}
}

func TestUploadApplyMcpRequiresView(t *testing.T) {
	s, ts, c := applyServer(t, uploads.Config{})
	m := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{
		"mcp.json": `{"mcpServers":{"srv":{"command":"npx","args":["-y","x"]}}}`,
	})
	id := firstEntry(t, m)["id"].(string)
	// dry-run 只解析不需要 mcpView
	r := postJSON(t, c, ts.URL+"/api/uploads/"+id+"/apply", `{"kind":"mcp"}`, http.StatusOK)
	if r["dryRun"] != true {
		t.Fatalf("dry-run = %#v", r)
	}
	// confirm 需要 mcpView
	postJSON(t, c, ts.URL+"/api/uploads/"+id+"/apply", `{"kind":"mcp","confirm":true}`, http.StatusNotFound)
	// 有 mcpView 的 confirm 走通
	s.mcpView = fakeMcpView{}
	r = postJSON(t, c, ts.URL+"/api/uploads/"+id+"/apply", `{"kind":"mcp","confirm":true}`, http.StatusOK)
	if r["reloaded"] != true || r["applied"] != float64(1) {
		t.Fatalf("mcp confirm = %#v", r)
	}
}

func TestUploadApplyUnknownKind(t *testing.T) {
	_, ts, c := applyServer(t, uploads.Config{})
	m := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{"x.txt": "x"})
	postJSON(t, c, ts.URL+"/api/uploads/"+firstEntry(t, m)["id"].(string)+"/apply", `{"kind":"auth"}`, http.StatusBadRequest)
}

// firstEntry 取上传响应的第一项
func firstEntry(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	entries, ok := m["uploads"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatalf("no uploads in %#v", m)
	}
	return entries[0].(map[string]any)
}

// agentDirForTest 取当前测试的 DOGE_CODING_AGENT_DIR
func agentDirForTest() string {
	return os.Getenv("DOGE_CODING_AGENT_DIR")
}
