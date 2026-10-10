package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestConfig 写一份初始 mcp.json
func writeTestConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestUpsertRemoveReload 项目文件的 server 增删改 + Reload 重新发现
func TestUpsertRemoveReload(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeTestConfig(t, cfgPath, `{"mcpServers":{"fake":{"command":"/bin/true"}}}`)

	m := New(Options{AgentDir: dir, Cwd: dir, ConfigPath: cfgPath, Version: "test"})
	if err := m.Discover(); err != nil {
		t.Fatal(err)
	}

	// 非法名字拒绝（缓存文件名防穿越）
	if err := m.UpsertServers(map[string]*Server{"../evil": {Command: "x"}}); err == nil {
		t.Fatal("path-like name must be rejected")
	}
	// 缺 command/url 拒绝
	if err := m.UpsertServers(map[string]*Server{"bad": {Type: "stdio"}}); err == nil {
		t.Fatal("missing command/url must be rejected")
	}
	// 非法 type 拒绝
	if err := m.UpsertServers(map[string]*Server{"t": {Type: "websocket", Command: "x"}}); err == nil {
		t.Fatal("unknown type must be rejected")
	}

	// 批量导入（社区粘贴格式）：新增 → 只写主配置文件，不改源文件、不动运行态
	if err := m.UpsertServers(map[string]*Server{
		"web":   {Type: "http", URL: "https://mcp.example.com/mcp"},
		"tools": {Type: "stdio", Command: "npx", Args: []string{"-y", "some-mcp-server"}},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if err != nil || !strings.Contains(string(raw), `"web"`) || !strings.Contains(string(raw), `"type": "stdio"`) {
		t.Fatalf("writePath after import = %s (%v)", raw, err)
	}
	for _, s := range m.Servers() {
		if s.Name == "web" {
			t.Fatal("upsert must not touch runtime registry before reload")
		}
	}

	// Reload 后生效
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range m.Servers() {
		if s.Name == "web" && s.Enabled {
			found = true
		}
	}
	if !found {
		t.Fatal("web server missing after reload")
	}

	// 同名再导入是字段合并
	if err := m.UpsertServers(map[string]*Server{
		"web": {Headers: map[string]string{"Authorization": "Bearer x"}},
	}); err != nil {
		t.Fatal(err)
	}
	prj, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if !strings.Contains(string(prj), "https://mcp.example.com/mcp") || !strings.Contains(string(prj), "Bearer x") {
		t.Fatalf("field merge failed: %s", prj)
	}

	// 写入只读共享文件（dir/.mcp.json）中的 server 不可经 API 删除（不在 writePath 里）
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"shared-only":{"command":"/bin/true"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveServer("shared-only"); err == nil {
		t.Fatal("server only in shared file must be refused")
	}
	// writePath 里定义的 server 可正常删除
	if err := m.RemoveServer("web"); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Servers() {
		if s.Name == "web" {
			t.Fatal("web still present after remove+reload")
		}
	}
	m.Close()
}

// waitCond 轮询等待条件成立（异步连接路径用）
func waitCond(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSetServerFlags direct / autoStart 切换：内存即时生效、项目文件显式落盘、
// reload（跨文件优先级）后仍然保持。
func TestSetServerFlags(t *testing.T) {
	dir := t.TempDir()
	m := New(Options{AgentDir: dir, Cwd: dir, Version: "test"})
	m.cfg = &Config{
		McpServers: map[string]*Server{
			"fake": {
				Command:     os.Args[0],
				Args:        []string{"-test.run=TestHelperMCPServer"},
				Env:         map[string]string{"DOGE_MCP_HELPER": "1"},
				DirectTools: true,
			},
		},
	}
	svr := &server{name: "fake", cfg: m.cfg.McpServers["fake"], mgr: m}
	m.servers = map[string]*server{"fake": svr}
	m.ready = true

	// autostart 开启 → 立即连接
	if err := m.SetServerAutoStart("fake", true); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 5*time.Second, func() bool { return svr.connected() })
	if !svr.connected() {
		t.Fatal("autostart on should connect")
	}

	// direct 开 → BuildTools 含 fake__echo；关 → 消失
	if err := m.SetServerDirect("fake", true); err != nil {
		t.Fatal(err)
	}
	has := func(name string) bool {
		for _, tl := range m.BuildTools() {
			if tl.Name == name {
				return true
			}
		}
		return false
	}
	if !has("fake__echo") {
		t.Fatal("direct tools missing while promoted")
	}
	if err := m.SetServerDirect("fake", false); err != nil {
		t.Fatal(err)
	}
	m.Reconnect("fake")
	if has("fake__echo") {
		t.Fatal("direct tools still present after demote")
	}

	// autostart 关 → 断开
	if err := m.SetServerAutoStart("fake", false); err != nil {
		t.Fatal(err)
	}
	if svr.connected() {
		t.Fatal("autostart off should disconnect")
	}

	// 项目文件显式写出 false（能覆盖全局 true）；默认 writePath 即 <cwd>/.doge/mcp.json
	prjRaw, err := os.ReadFile(filepath.Join(dir, ".doge", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prj Config
	if err := json.Unmarshal(prjRaw, &prj); err != nil {
		t.Fatal(err)
	}
	e := prj.McpServers["fake"]
	if e == nil || e.AutoStart || e.DirectTools != false {
		t.Fatalf("project flags = %#v", e)
	}

	// 跨文件覆盖：Reload 后（项目文件压过无键的全局）仍保持关闭，且不再自动连接
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	m.mu.Lock()
	direct, auto := false, false
	var connected bool
	for _, s := range m.servers {
		all, _ := m.directToolNames(m.cfg, s.name, s.cfg)
		direct, auto = all, s.cfg.AutoStart
		connected = s.connected()
	}
	m.mu.Unlock()
	if connected || direct || auto {
		t.Fatalf("flags must stay off after reload: connected=%v direct=%v auto=%v", connected, direct, auto)
	}
	m.Close()
}

// TestDisabledServerLifecycle 禁用的 server 仍在注册表可见，但不连接不被调用；
// SetEnabled(true) 立即恢复并拉取工具。
func TestDisabledServerLifecycle(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"off": map[string]any{"command": "/nonexistent-mcp-binary", "disabled": true},
			"on":  map[string]any{"command": "/nonexistent-mcp-binary"},
		},
	}
	data, _ := json.Marshal(cfg)
	writeTestConfig(t, cfgPath, string(data))

	m := New(Options{AgentDir: dir, Cwd: dir, ConfigPath: cfgPath, Version: "test"})
	if err := m.Discover(); err != nil {
		t.Fatal(err)
	}
	states := map[string]ServerInfo{}
	for _, s := range m.Servers() {
		states[s.Name] = s
	}
	if s := states["off"]; s.Enabled || s.Connected {
		t.Fatalf("off should be registered but disabled/disconnected: %#v", s)
	}
	if s := states["on"]; !s.Enabled {
		t.Fatal("on should be enabled")
	}
	// 禁用的 server 不参与连接类操作
	if err := m.Reconnect("off"); err == nil {
		t.Fatal("reconnect on disabled server must fail")
	}
	if _, err := m.callTool("off", "x", nil); err == nil {
		t.Fatal("call on disabled server must fail")
	}
	// 启用后进注册表可连（这里二进制不存在，启用会报连接错误但状态已翻转）
	err := m.SetEnabled("off", true)
	if err == nil {
		t.Fatal("expected connect error for nonexistent binary")
	}
	m.mu.Lock()
	svr := m.servers["off"]
	disabled := svr.cfg.Disabled
	m.mu.Unlock()
	if disabled {
		t.Fatal("server should be re-enabled in registry")
	}
	prj, rerr := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if rerr != nil || !strings.Contains(string(prj), `"disabled": false`) {
		t.Fatalf("project disable flag not written: %s (%v)", prj, rerr)
	}
	m.Close()
}
