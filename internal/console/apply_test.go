package console

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doge/internal/coding"
	"doge/internal/coding/uploads"
	"doge/internal/mcp"
)

// applyEnv 建临时 agentDir 与暂存区（技能 place/apply 全链路落临时目录）
func applyEnv(t *testing.T) (*uploads.Store, string) {
	t.Helper()
	agentDir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", agentDir)
	cwd := t.TempDir()
	store := uploads.NewStore(filepath.Join(agentDir, "uploads"), uploads.Config{})
	return store, cwd
}

// saveUpload 向暂存区放一个文件
func saveUpload(t *testing.T, store *uploads.Store, name, data string) string {
	t.Helper()
	meta, err := store.Save(name, "test", "test", strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return meta.ID
}

func TestParseMcpServersPayload(t *testing.T) {
	// 内层 map 形式
	servers, e := ParseMcpServersPayload([]byte(`{"playwright":{"command":"npx","args":["-y","x"]}}`))
	if e != nil || len(servers) != 1 || servers["playwright"].Command != "npx" {
		t.Fatalf("inner map = %v, %v", servers, e)
	}
	// 完整 .mcp.json 形式
	servers, e = ParseMcpServersPayload([]byte(`{"mcpServers":{"srv":{"url":"http://127.0.0.1:1"}}}`))
	if e != nil || len(servers) != 1 || servers["srv"].URL == "" {
		t.Fatalf("mcpServers wrapper = %v, %v", servers, e)
	}
	// 非法体
	if _, e := ParseMcpServersPayload([]byte(`[]`)); e == nil || e.Kind != KindInvalid {
		t.Fatalf("invalid body err = %v", e)
	}
	if _, e := ParseMcpServersPayload([]byte(`{}`)); e == nil || e.Kind != KindInvalid {
		t.Fatalf("empty body err = %v", e)
	}
}

func TestPlaceUpload(t *testing.T) {
	store, cwd := applyEnv(t)
	id := saveUpload(t, store, "cert.pem", "CERT-DATA")

	// 相对路径落 cwd 子树
	resp, e := PlaceUpload(store, id, "deploy/cert.pem", cwd, false)
	if e != nil {
		t.Fatalf("place: %v", e)
	}
	if resp["ok"] != true || resp["path"] == "" {
		t.Fatalf("resp = %v", resp)
	}
	if data, err := os.ReadFile(filepath.Join(cwd, "deploy", "cert.pem")); err != nil || string(data) != "CERT-DATA" {
		t.Fatalf("placed content = %s, %v", data, err)
	}
	// 非覆盖冲突 busy；覆盖成功
	if _, e := PlaceUpload(store, id, "deploy/cert.pem", cwd, false); e == nil || e.Kind != KindBusy {
		t.Fatalf("conflict err = %v", e)
	}
	if _, e := PlaceUpload(store, id, "deploy/cert.pem", cwd, true); e != nil {
		t.Fatalf("overwrite: %v", e)
	}
	// 越界与未知上传
	if _, e := PlaceUpload(store, id, "/etc/evil", cwd, false); e == nil || e.Kind != KindInvalid {
		t.Fatalf("outside root err = %v", e)
	}
	if _, e := PlaceUpload(store, id, "../../evil", cwd, false); e == nil || e.Kind != KindInvalid {
		t.Fatalf("traversal err = %v", e)
	}
	if _, e := PlaceUpload(store, "nope", "x.txt", cwd, false); e == nil || e.Kind != KindNotFound {
		t.Fatalf("missing upload err = %v", e)
	}
}

func TestApplyUploadModels(t *testing.T) {
	store, _ := applyEnv(t)
	id := saveUpload(t, store, "models.json",
		`[{"id":"my-model","provider":"local","api":"openai-completions","baseUrl":"http://127.0.0.1:9999/v1"}]`)
	reloads := 0
	d := ApplyDeps{Reload: func() error { reloads++; return nil }}

	// dry-run 只预览不落盘
	resp, e := ApplyUpload(store, id, "models", false, d)
	if e != nil {
		t.Fatalf("dry-run: %v", e)
	}
	if resp["dryRun"] != true || !strings.Contains(resp["diff"].(string), "local/my-model") {
		t.Fatalf("dry-run resp = %v", resp)
	}
	if _, err := os.Stat(coding.ModelsFile()); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote models.json")
	}
	// confirm 写盘 + reload
	resp, e = ApplyUpload(store, id, "models", true, d)
	if e != nil {
		t.Fatalf("confirm: %v", e)
	}
	if resp["reloaded"] != true || resp["applied"] != 1 || reloads != 1 {
		t.Fatalf("confirm resp = %v reloads = %d", resp, reloads)
	}
	if _, err := os.Stat(coding.ModelsFile()); err != nil {
		t.Fatalf("models.json missing: %v", err)
	}
	// 重载失败不遮蔽写盘成功
	dFail := ApplyDeps{Reload: func() error { return os.ErrPermission }}
	resp, e = ApplyUpload(store, id, "models", true, dFail)
	if e != nil || resp["reloaded"] != false || resp["error"] == "" {
		t.Fatalf("failed reload resp = %v, %v", resp, e)
	}
	// 非法内容与未知 kind
	bad := saveUpload(t, store, "bad.json", `{"not":"an array"}`)
	if _, e := ApplyUpload(store, bad, "models", false, d); e == nil || e.Kind != KindInvalid {
		t.Fatalf("bad models err = %v", e)
	}
	if _, e := ApplyUpload(store, id, "auth", false, d); e == nil || e.Kind != KindInvalid {
		t.Fatalf("unknown kind err = %v", e)
	}
}

func TestApplyUploadPermissions(t *testing.T) {
	store, _ := applyEnv(t)
	id := saveUpload(t, store, "permissions.json",
		`{"defaultPolicy":{"bash":"ask"},"tools":{"bash":"allow"},"bash":[{"pattern":"git *","decision":"allow"}]}`)
	d := ApplyDeps{}

	resp, e := ApplyUpload(store, id, "permissions", false, d)
	if e != nil || !strings.Contains(resp["diff"].(string), "bash 规则：1 条") {
		t.Fatalf("dry-run = %v, %v", resp, e)
	}
	if _, e := ApplyUpload(store, id, "permissions", true, d); e != nil {
		t.Fatalf("confirm: %v", e)
	}
	if _, err := os.Stat(filepath.Join(coding.AgentDir(), "permissions.json")); err != nil {
		t.Fatalf("permissions.json missing: %v", err)
	}
	// 二次 confirm 产出 .bak 备份
	if _, e := ApplyUpload(store, id, "permissions", true, d); e != nil {
		t.Fatalf("re-confirm: %v", e)
	}
	if _, err := os.Stat(filepath.Join(coding.AgentDir(), "permissions.json.bak")); err != nil {
		t.Fatalf(".bak missing: %v", err)
	}
}

func TestApplyUploadSkillsZip(t *testing.T) {
	store, _ := applyEnv(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("demo/SKILL.md")
	w.Write([]byte("---\nname: demo-skill\ndescription: 打包测试技能\n---\n# demo"))
	w2, _ := zw.Create("demo/assets/ref.md")
	w2.Write([]byte("ref"))
	zw.Close()
	_ = w2
	id := saveUpload(t, store, "demo-skill.zip", buf.String())
	d := ApplyDeps{}

	resp, e := ApplyUpload(store, id, "skills-zip", false, d)
	if e != nil || resp["dryRun"] != true || !strings.Contains(resp["diff"].(string), "demo-skill") {
		t.Fatalf("dry-run = %v, %v", resp, e)
	}
	resp, e = ApplyUpload(store, id, "skills-zip", true, d)
	if e != nil || resp["reloaded"] != true {
		t.Fatalf("confirm = %v, %v", resp, e)
	}
	for _, p := range []string{"SKILL.md", "assets/ref.md"} {
		if _, err := os.Stat(filepath.Join(coding.AgentDir(), "skills", "demo-skill", p)); err != nil {
			t.Fatalf("installed file missing %s: %v", p, err)
		}
	}
}

func TestApplyUploadMcp(t *testing.T) {
	store, _ := applyEnv(t)
	id := saveUpload(t, store, "mcp.json", `{"mcpServers":{"srv":{"command":"npx","args":["-y","x"]}}}`)

	// dry-run 不需要钩子
	resp, e := ApplyUpload(store, id, "mcp", false, ApplyDeps{})
	if e != nil || resp["dryRun"] != true || !strings.Contains(resp["diff"].(string), "将合并 1 个 MCP server") {
		t.Fatalf("dry-run = %v, %v", resp, e)
	}
	// 无钩子 confirm → notfound（web 语义）
	if _, e := ApplyUpload(store, id, "mcp", true, ApplyDeps{}); e == nil || e.Kind != KindNotFound {
		t.Fatalf("no hook err = %v", e)
	}
	// 钩子收到解析结果，作答透传
	var got map[string]*mcp.Server
	applied := func(servers map[string]*mcp.Server) *Error {
		got = servers
		return nil
	}
	resp, e = ApplyUpload(store, id, "mcp", true, ApplyDeps{Mcp: applied})
	if e != nil || resp["applied"] != 1 || resp["reloaded"] != true {
		t.Fatalf("confirm = %v, %v", resp, e)
	}
	if len(got) != 1 || got["srv"] == nil {
		t.Fatalf("hook servers = %v", got)
	}
	// 钩子报错原样透传
	fail := func(map[string]*mcp.Server) *Error { return Busy("conflict") }
	if _, e := ApplyUpload(store, id, "mcp", true, ApplyDeps{Mcp: fail}); e == nil || e.Kind != KindBusy {
		t.Fatalf("hook error = %v", e)
	}
}
