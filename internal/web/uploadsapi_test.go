package web

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"doge/internal/coding/uploads"
)

// newUploadsServer 构建带上传暂存区的测试服务器
func newUploadsServer(t *testing.T, f Backend, cfg uploads.Config) (*Server, *httptest.Server) {
	t.Helper()
	s, err := NewServer(Options{
		Backend: f,
		Token:   "tok-123",
		Address: "127.0.0.1:0",
		Uploads: uploads.NewStore(t.TempDir(), cfg),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// uploadFiles 以 multipart 提交若干文件
func uploadFiles(t *testing.T, c *http.Client, url string, files map[string]string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range files {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(fw, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
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

func TestUploadsLifecycle(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newUploadsServer(t, f, uploads.Config{})
	c := login(t, ts, "tok-123")

	m := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{"../../notes.txt": "hello"})
	entries, _ := m["uploads"].([]any)
	if len(entries) != 1 {
		t.Fatalf("uploads = %#v", m)
	}
	entry, _ := entries[0].(map[string]any)
	if entry["name"] != "notes.txt" || entry["error"] != nil {
		t.Fatalf("entry = %#v", entry)
	}
	id, _ := entry["id"].(string)
	if len(id) != 64 {
		t.Fatalf("id = %q", id)
	}

	// 列表与单查
	list := getJSON(t, c, ts.URL+"/api/uploads", http.StatusOK)
	if ups, _ := list["uploads"].([]any); len(ups) != 1 {
		t.Fatalf("list = %#v", list)
	}
	got := getJSON(t, c, ts.URL+"/api/uploads/"+id, http.StatusOK)
	if got["name"] != "notes.txt" || got["mimeType"] != "text/plain; charset=utf-8" {
		t.Fatalf("meta = %#v", got)
	}

	// 下载内容
	resp, err := c.Get(ts.URL + "/api/uploads/" + id + "/content")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("content = %q ct = %q", body, resp.Header.Get("Content-Type"))
	}

	// 删除后 404
	if code := deleteURL(t, c, ts.URL+"/api/uploads/"+id); code != http.StatusOK {
		t.Fatalf("delete status = %d", code)
	}
	getJSON(t, c, ts.URL+"/api/uploads/"+id, http.StatusNotFound)
}

func TestUploadsPromptAttachments(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newUploadsServer(t, f, uploads.Config{})
	c := login(t, ts, "tok-123")

	m := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{"a.txt": "content-a"})
	entries, _ := m["uploads"].([]any)
	entry, _ := entries[0].(map[string]any)
	id, _ := entry["id"].(string)

	// 空文本 + 附件合法；未知附件同步 400
	postJSON(t, c, ts.URL+"/api/prompt", `{"text":"","attachments":["`+id+`"]}`, http.StatusAccepted)
	postJSON(t, c, ts.URL+"/api/prompt", `{"text":"","attachments":["deadbeef"]}`, http.StatusBadRequest)
	postJSON(t, c, ts.URL+"/api/prompt", `{"text":""}`, http.StatusBadRequest)
}

func TestUploadsTooLarge(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newUploadsServer(t, f, uploads.Config{MaxFileBytes: 4})
	c := login(t, ts, "tok-123")

	m := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{"small.txt": "ab", "big.txt": "way too long"})
	entries, _ := m["uploads"].([]any)
	if len(entries) != 2 {
		t.Fatalf("uploads = %#v", m)
	}
	var failed int
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		if entry["error"] != nil {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("expected 1 per-file failure, got %d (%#v)", failed, entries)
	}
}

// TestUploadsRequestTooLarge 请求体整体超限 → 413（区别于单文件逐项失败）
func TestUploadsRequestTooLarge(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newUploadsServer(t, f, uploads.Config{MaxRequestBytes: 64})
	c := login(t, ts, "tok-123")

	m := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{"a.txt": "x"})
	if m["error"] != "request too large" {
		t.Fatalf("body = %#v", m)
	}
}

func TestUploadsUnavailable(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f) // 无 Uploads
	c := login(t, ts, "tok-123")
	getJSON(t, c, ts.URL+"/api/uploads", http.StatusServiceUnavailable)
	uploadResp := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{"a.txt": "x"})
	if _, ok := uploadResp["error"]; !ok {
		t.Fatalf("expected error body, got %#v", uploadResp)
	}
}

// deleteURL 发 DELETE 并返回状态码
func deleteURL(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestUploadsContentDisposition 存储 XSS 防护：嗅探为 text/html / svg 的文件
// 必须 attachment + nosniff（内联即同源执行，cookie 自动附带），图片才 inline
func TestUploadsContentDisposition(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newUploadsServer(t, f, uploads.Config{})
	c := login(t, ts, "tok-123")

	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("fake png body")...)
	m := uploadFiles(t, c, ts.URL+"/api/uploads", map[string]string{
		"evil.html": "<script>alert(1)</script>",
		"evil.svg":  "<svg xmlns=\"http://www.w3.org/2000/svg\" onload=\"alert(1)\"></svg>",
		"pic.png":   string(png),
	})
	ids := map[string]string{}
	for _, e := range m["uploads"].([]any) {
		entry, _ := e.(map[string]any)
		ids[entry["name"].(string)], _ = entry["id"].(string)
	}

	cases := map[string]string{
		"evil.html": "attachment",
		"evil.svg":  "attachment",
		"pic.png":   "inline",
	}
	for name, want := range cases {
		resp, err := c.Get(ts.URL + "/api/uploads/" + ids[name] + "/content")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		disp := resp.Header.Get("Content-Disposition")
		if !strings.HasPrefix(disp, want) {
			t.Fatalf("%s: disposition = %q, want %s prefix (ct=%s)", name, disp, want, resp.Header.Get("Content-Type"))
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: missing nosniff", name)
		}
		_ = body
	}
}
