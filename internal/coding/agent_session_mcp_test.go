package coding

import (
	"testing"

	"doge/internal/agent"
)

// 会话级 MCP 启停：SetMcpEnabled 只记意图（pending），ReloadConfig 才真正
// 挂载/卸载工具组并重建 system prompt。
func TestSessionMcpToggle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	s := newToolSession(t, nil, nil)
	if s.HasMcp() {
		t.Fatal("no mcp tools expected before SetMcpTools")
	}

	mcpTools := []*agent.AgentTool{{Name: "mcp"}, {Name: "fake__echo"}}
	s.SetMcpTools(mcpTools)
	if !s.HasMcp() {
		t.Fatal("HasMcp after SetMcpTools")
	}
	if _, _, pending := s.McpState(); pending {
		t.Fatal("fresh mount must not be pending")
	}
	names := toolNames(s)
	if !names["mcp"] || !names["fake__echo"] {
		t.Fatalf("mcp tools missing after mount: %v", names)
	}

	// 停用：只记意图，工具集不动
	if err := s.SetMcpEnabled(false); err != nil {
		t.Fatal(err)
	}
	desired, mounted, pending := s.McpState()
	if desired || !mounted || !pending {
		t.Fatalf("state after disable = desired=%v mounted=%v pending=%v", desired, mounted, pending)
	}
	if !toolNames(s)["mcp"] {
		t.Fatal("disable must not touch the live toolset before reload")
	}

	// 重载生效
	if err := s.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	desired, mounted, pending = s.McpState()
	if desired || mounted || pending {
		t.Fatalf("state after reload = desired=%v mounted=%v pending=%v", desired, mounted, pending)
	}
	names = toolNames(s)
	if names["mcp"] || names["fake__echo"] {
		t.Fatalf("mcp tools still present after unload: %v", names)
	}

	// 再启用 → 重载恢复
	if err := s.SetMcpEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := s.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	names = toolNames(s)
	if !names["mcp"] || !names["fake__echo"] {
		t.Fatalf("mcp tools not restored: %v", names)
	}

	// 无变更时重载是 no-op（不报错、状态不变）
	if err := s.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if _, _, pending := s.McpState(); pending {
		t.Fatal("pending after no-op reload")
	}
}

// fork 继承挂载态；fork 内可独立再启停。
func TestSessionMcpForkInherits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	s := newToolSession(t, nil, nil)
	s.SetMcpTools([]*agent.AgentTool{{Name: "mcp"}})
	if err := s.SetMcpEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := s.ReloadConfig(); err != nil {
		t.Fatal(err)
	}

	fork, err := s.ForkNew()
	if err != nil {
		t.Fatal(err)
	}
	if toolNames(fork)["mcp"] {
		t.Fatal("fork should inherit the unmounted state")
	}
	// fork 内独立启用
	if err := fork.SetMcpEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := fork.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if !toolNames(fork)["mcp"] {
		t.Fatal("fork re-enable failed")
	}
	// 源会话保持停用
	if toolNames(s)["mcp"] {
		t.Fatal("source session must stay unloaded")
	}
}

// 运行期 reload 分发：RefreshMcpTools 处理 首次注入 / 换新组 / 停用态换组 /
// 清空卸载 四种迁移。
func TestSessionMcpRefresh(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	s := newToolSession(t, nil, nil)

	// 从未有组 → 首次注入即挂载
	v1 := []*agent.AgentTool{{Name: "mcp"}, {Name: "a__t1"}}
	s.RefreshMcpTools(v1)
	names := toolNames(s)
	if !names["mcp"] || !names["a__t1"] {
		t.Fatalf("first refresh not mounted: %v", names)
	}

	// 换新组：旧工具被替换
	v2 := []*agent.AgentTool{{Name: "mcp"}, {Name: "b__t2"}}
	s.RefreshMcpTools(v2)
	names = toolNames(s)
	if names["a__t1"] || !names["b__t2"] {
		t.Fatalf("swap failed: %v", names)
	}

	// 停用后再刷新（server 列表变化）：保持卸载，但组引用已更新；重新启用拿到的是新组
	if err := s.SetMcpEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := s.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	v3 := []*agent.AgentTool{{Name: "mcp"}, {Name: "c__t3"}}
	s.RefreshMcpTools(v3)
	if toolNames(s)["c__t3"] {
		t.Fatal("disabled session must stay unloaded across refresh")
	}
	desired, _, pending := s.McpState()
	if desired || pending {
		t.Fatalf("state = desired=%v pending=%v, want all false", desired, pending)
	}
	if err := s.SetMcpEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := s.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	names = toolNames(s)
	if !names["c__t3"] || names["b__t2"] {
		t.Fatalf("re-enable after refresh got wrong set: %v", names)
	}

	// 清空（进程不再有任何 mcp server）→ 整体卸载
	s.RefreshMcpTools(nil)
	if toolNames(s)["mcp"] || toolNames(s)["c__t3"] {
		t.Fatalf("empty refresh must unload: %v", toolNames(s))
	}
	if _, mounted, _ := s.McpState(); mounted {
		t.Fatal("mounted should be false after empty refresh")
	}
}
