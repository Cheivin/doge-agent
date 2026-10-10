package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"doge/internal/agent"
)

// ---- 配置发现与合并 ----

// TestConfigMerge 验证多文件合并：全局 → 项目 → 共享，字段级覆盖、disabled
// 只写字段不丢命令、同名 server 高优先级覆盖。
func TestConfigMerge(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "mcp.json")
	project := filepath.Join(dir, ".doge", "mcp.json")
	shared := filepath.Join(dir, ".mcp.json")
	if err := os.MkdirAll(filepath.Dir(project), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(global, `{"mcpServers":{"srv":{"command":"npx","args":["a"],"approveTools":["g*"]}}}`)
	write(project, `{"mcpServers":{"srv":{"args":["b"],"directTools":true}}}`)
	write(shared, `{"mcpServers":{"srv":{"disabled":true}},"settings":{"idleTimeout":60}}`)

	cfg, err := LoadConfigs([]string{global, project, shared})
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.McpServers["srv"]
	if s == nil {
		t.Fatal("srv missing after merge")
	}
	if s.Command != "npx" || len(s.Args) != 1 || s.Args[0] != "b" {
		t.Fatalf("merge lost command/args override: %#v", s)
	}
	if s.DirectTools != true || !s.Disabled {
		t.Fatalf("merge lost directTools/disabled: %#v", s)
	}
	if s.ApproveTools == nil || len(s.ApproveTools) != 1 || s.ApproveTools[0] != "g*" {
		t.Fatalf("merge lost approveTools from global: %#v", s)
	}
	if cfg.IdleTimeout() != 60*time.Second {
		t.Fatalf("idleTimeout = %v", cfg.IdleTimeout())
	}
}

// TestConfigPaths 验证发现路径顺序与 override
func TestConfigPaths(t *testing.T) {
	paths := McpFilePaths("/ag", "/cwd", "")
	if len(paths) != 3 || paths[0] != "/ag/mcp.json" || paths[2] != "/cwd/.mcp.json" {
		t.Fatalf("paths = %v", paths)
	}
	paths = McpFilePaths("/ag", "/cwd", "/tmp/custom.json")
	if paths[0] != "/tmp/custom.json" {
		t.Fatalf("override not honored: %v", paths)
	}
}

// ---- 检索 ----

// newTestManager 构造一个带内嵌工具元数据的 manager（不连接任何 server）
func newTestManager(t *testing.T) *Manager {
	m := New(Options{AgentDir: t.TempDir(), Cwd: t.TempDir(), Version: "test"})
	m.cfg = &Config{McpServers: map[string]*Server{
		"ctx": {Command: "npx"},
		"web": {Command: "npx"},
	}}
	schema := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	m.servers = map[string]*server{
		"ctx": {name: "ctx", cfg: m.cfg.McpServers["ctx"], mgr: m, tools: []*mcp.Tool{
			{Name: "resolve_library_id", Description: "resolve a library to its context7 id", InputSchema: schema},
			{Name: "query_docs", Description: "query docs", InputSchema: schema},
		}},
		"web": {name: "web", cfg: m.cfg.McpServers["web"], mgr: m, tools: []*mcp.Tool{
			{Name: "get_screenshot", Description: "take a screenshot", InputSchema: schema},
		}},
	}
	m.ready = true
	return m
}

// TestSearchRanking hyphen/underscore 等价 + 加权排序
func TestSearchRanking(t *testing.T) {
	m := newTestManager(t)
	// 用带连字符的查询命中带下划线的工具名
	hits := m.searchTools("context7_resolve-library-id", "", 10, 0)
	if len(hits) == 0 || hits[0].name != "resolve_library_id" {
		t.Fatalf("search = %#v", hits)
	}
	// server 过滤
	hits = m.searchTools("", "web", 10, 0)
	if len(hits) != 1 || hits[0].name != "get_screenshot" {
		t.Fatalf("server filter = %#v", hits)
	}
	// 分页
	hits = m.searchTools("", "", 1, 0)
	if len(hits) != 1 {
		t.Fatalf("limit = %#v", hits)
	}
	hits = m.searchTools("", "", 10, 2)
	if len(hits) != 1 {
		t.Fatalf("offset = %#v", hits)
	}
}

// ---- 审批 ----

func TestApproval(t *testing.T) {
	m := newTestManager(t)
	// 未配置审批表：放行
	if ok, _ := m.approved("ctx", "resolve_library_id"); !ok {
		t.Fatal("no approveTools should allow")
	}
	// server 级白名单 glob：命中与未命中
	m.cfg.McpServers["ctx"].ApproveTools = []string{"resolve_*", "web__get_*"}
	if ok, _ := m.approved("ctx", "resolve_library_id"); !ok {
		t.Fatal("glob should allow")
	}
	if ok, _ := m.approved("ctx", "query_docs"); ok {
		t.Fatal("non-approved tool should be denied")
	}
	// 全局 fallback
	m.cfg.McpServers["ctx"].ApproveTools = nil
	m.cfg.Settings.ApproveTools = []string{"query_*"}
	if ok, _ := m.approved("ctx", "query_docs"); !ok {
		t.Fatal("global approveTools should allow")
	}
}

// ---- 输出保护 ----

func TestOutputProtection(t *testing.T) {
	big := strings.Repeat("y", 300<<10)
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: big}}}
	out := formatResult(res)
	if len(out) >= len(big) || !strings.Contains(out, "已截断") {
		t.Fatalf("truncation failed: len=%d", len(out))
	}
	// 二进制 spill
	img := &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{
		MIMEType: "image/png",
		Data:     []byte("aGVsbG8="), // base64("hello")
	}}}
	out = formatResult(img)
	if !strings.Contains(out, "已保存") || !strings.Contains(out, "doge-mcp-") {
		t.Fatalf("binary spill failed: %q", out)
	}
	// 错误结果前缀
	errRes := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "boom"}}}
	if !strings.Contains(formatResult(errRes), "MCP tool error") {
		t.Fatal("isError not marked")
	}
}

// ---- 端到端：fake stdio MCP server ----

// TestHelperMCPServer 是一个最小 MCP stdio server（作为子进程被下面的测试拉起）。
// 只实现 initialize / tools/list / tools/call，换行分隔 JSON-RPC 2.0。
func TestHelperMCPServer(t *testing.T) {
	if os.Getenv("DOGE_MCP_HELPER") != "1" {
		return
	}
	tools := []map[string]any{
		{
			"name":        "echo",
			"description": "echo the input text",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}},
		},
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		// notifications 无 id，不响应
		if len(req.ID) == 0 {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &params)
			result = map[string]any{
				"protocolVersion": params.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-mcp", "version": "0.0.1"},
			}
		case "tools/list":
			result = map[string]any{"tools": tools}
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Name == "echo" {
				var args struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(params.Arguments, &args)
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "echo:" + args.Text}}, "isError": false}
			} else {
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "unknown tool " + params.Name}}, "isError": true}
			}
		default:
			result = map[string]any{}
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		data, _ := json.Marshal(resp)
		fmt.Fprintln(os.Stdout, string(data))
	}
	os.Exit(0)
}

// helperCmd 构造拉起 fake MCP server 的命令
func helperCmd() *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperMCPServer")
	cmd.Env = append(os.Environ(), "DOGE_MCP_HELPER=1")
	return cmd
}

// TestEndToEnd 全链路：配置 → 发现 → 代理工具 → search/status/call → direct
// 工具 → enable/disable 落盘。全部通过真实 stdio 子进程 + 官方 SDK 客户端。
func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"fake": map[string]any{
				"command":        os.Args[0],
				"args":           []string{"-test.run=TestHelperMCPServer"},
				"env":            map[string]string{"DOGE_MCP_HELPER": "1"},
				"directTools":    []string{"echo"},
				"searchKeywords": map[string]any{"echo": []string{"say", "repeat"}},
			},
		},
		"settings": map[string]any{"requestTimeoutMs": 10000},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(Options{AgentDir: dir, Cwd: dir, ConfigPath: cfgPath, Version: "test"})
	if err := m.Discover(); err != nil {
		t.Fatal(err)
	}
	if !m.Enabled() {
		t.Fatal("should be enabled")
	}
	tools := m.BuildTools()
	if len(tools) == 0 {
		t.Fatal("no tools built")
	}
	var proxy *agent.AgentTool
	for _, tl := range tools {
		if tl.Name == "mcp" {
			proxy = tl
		}
	}
	if proxy == nil {
		t.Fatal("mcp proxy tool missing")
	}

	execTool := func(params map[string]any) *agent.AgentToolResult {
		t.Helper()
		res, err := proxy.Execute("call-1", params, context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	text := func(res *agent.AgentToolResult) string {
		if len(res.Content) == 0 {
			return ""
		}
		return res.Content[0].Text
	}

	// search（命中关键词）与 status（触发连接）
	if out := text(execTool(map[string]any{"action": "search", "query": "repeat"})); !strings.Contains(out, "fake__echo") {
		t.Fatalf("search = %q", out)
	}
	if out := text(execTool(map[string]any{"action": "status"})); !strings.Contains(out, "fake") {
		t.Fatalf("status = %q", out)
	}
	// call
	if out := text(execTool(map[string]any{"action": "call", "tool": "fake__echo", "args": map[string]any{"text": "hi"}})); !strings.Contains(out, "echo:hi") {
		t.Fatalf("call = %q", out)
	}
	// describe
	if out := text(execTool(map[string]any{"action": "describe", "tool": "fake__echo"})); !strings.Contains(out, "JSON Schema") {
		t.Fatalf("describe = %q", out)
	}

	// direct 工具：fake__echo 是一等工具且可调用
	var direct *agent.AgentTool
	for _, tl := range tools {
		if tl.Name == "fake__echo" {
			direct = tl
		}
	}
	if direct == nil {
		t.Fatal("direct tool fake__echo missing")
	}
	dr, err := direct.Execute("call-2", map[string]any{"text": "direct"}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text(dr), "echo:direct") {
		t.Fatalf("direct call = %q", text(dr))
	}

	// enable/disable 写项目文件
	if err := m.SetEnabled("fake", false); err != nil {
		t.Fatal(err)
	}
	project := m.writePath
	raw, err := os.ReadFile(project)
	if err != nil {
		t.Fatalf("disable did not write project file: %v", err)
	}
	if !strings.Contains(string(raw), `"disabled": true`) {
		t.Fatalf("project file = %s", raw)
	}
	if err := m.SetEnabled("fake", true); err != nil {
		t.Fatal(err)
	}
	m.Close()
}

// TestProxyNoServer 未配置任何 server：不建缓存目录、不初始化
func TestProxyNoServer(t *testing.T) {
	dir := t.TempDir()
	m := New(Options{AgentDir: dir, Cwd: dir, Version: "test"})
	if err := m.Discover(); err != nil {
		t.Fatal(err)
	}
	if m.Enabled() {
		t.Fatal("no config should not be enabled")
	}
	if tools := m.BuildTools(); len(tools) != 0 {
		t.Fatalf("no config should build no tools, got %d", len(tools))
	}
	if _, err := os.Stat(filepath.Join(dir, "mcp-cache")); err == nil {
		t.Fatal("cache dir created without config (lazy violated)")
	}
}

// TestReconnect 重连：断开后再次连接仍可用
func TestReconnect(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"fake": map[string]any{"command": os.Args[0], "args": []string{"-test.run=TestHelperMCPServer"}, "env": map[string]string{"DOGE_MCP_HELPER": "1"}},
		},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Options{AgentDir: dir, Cwd: dir, ConfigPath: cfgPath, Version: "test"})
	if err := m.Discover(); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	s := m.servers["fake"]
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := s.listTools(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	s.disconnect()
	if err := m.Reconnect("fake"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if !s.connected() {
		t.Fatal("not connected after reconnect")
	}
	m.Close()
}
