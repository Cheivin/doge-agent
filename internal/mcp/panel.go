package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// toolFullName 一个 MCP 工具的一等名（server__tool）
func (m *Manager) toolFullName(server, tool string) string {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if cfg == nil {
		return server + "__" + tool
	}
	return cfg.ToolName(server, tool)
}

// splitToolName 把 server__tool 拆成 server 与 tool
func (m *Manager) splitToolName(full string) (string, string, error) {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	sep := "__"
	if cfg != nil && cfg.DirectToolPrefix() != "" {
		sep = cfg.DirectToolPrefix()
	}
	idx := strings.Index(full, sep)
	if idx <= 0 || idx == len(full)-len(sep) {
		return "", "", fmt.Errorf("tool 必须是 server%s tool 全名（如 chrome%sget_screenshot），实际为 %q", sep, sep, full)
	}
	return full[:idx], full[idx+len(sep):], nil
}

// searchText 检索结果文本
func (m *Manager) searchText(query, server string, limit, offset int) string {
	hits := m.searchTools(query, server, limit, offset)
	if len(hits) == 0 {
		if query == "" {
			return "没有可用的 MCP 工具（/mcp status 查看服务器状态）"
		}
		return "没有匹配 " + query + " 的 MCP 工具（试试更短的关键词，或 /mcp tools 看全部）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "匹配 %d 个 MCP 工具（调用用全名 server__tool）:\n", len(hits))
	for _, h := range hits {
		desc := h.desc
		if len(desc) > 120 {
			desc = desc[:120] + "…"
		}
		fmt.Fprintf(&b, "  %s  %s\n", m.toolFullName(h.server, h.name), desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

// describeText 查看工具完整参数 schema
func (m *Manager) describeText(server, full string) string {
	if full == "" {
		return "用法: mcp describe 需要 tool=server__tool"
	}
	srv, tool, err := m.splitToolName(full)
	if err != nil {
		return "错误: " + err.Error()
	}
	if srv == "" && server != "" {
		srv = server
	}
	t := m.findTool(srv, tool)
	if t == nil {
		// 缓存没有且未连接：尝试连接刷新一次
		if m.tryRefresh(srv) {
			t = m.findTool(srv, tool)
		}
	}
	if t == nil {
		return fmt.Sprintf("工具 %s 未找到（/mcp tools %s 查看该服务器的工具列表）", full, srv)
	}
	schema, _ := json.MarshalIndent(t.InputSchema, "", "  ")
	var b strings.Builder
	fmt.Fprintf(&b, "工具 %s\n", m.toolFullName(srv, tool))
	if t.Description != "" {
		fmt.Fprintf(&b, "描述: %s\n", t.Description)
	}
	b.WriteString("参数 (JSON Schema):\n")
	b.WriteString(string(schema))
	return b.String()
}

// tryRefresh 连接 server 并刷新工具缓存；禁用的 server 不连接，失败静默
func (m *Manager) tryRefresh(serverName string) bool {
	m.mu.Lock()
	s := m.servers[serverName]
	m.mu.Unlock()
	if s == nil || s.cfg.Disabled {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout())
	defer cancel()
	_, err := s.listTools(ctx)
	return err == nil
}

// statusText /mcp status 面板
func (m *Manager) statusText() string {
	infos := m.Servers()
	if len(infos) == 0 {
		return "未配置 MCP server（在 ~/.doge/agent/mcp.json、<cwd>/.doge/mcp.json 或 .mcp.json 里加 mcpServers）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "MCP 服务器 (%d):\n", len(infos))
	for _, info := range infos {
		state := "● 已连接"
		if !info.Enabled {
			state = "× 已禁用"
		} else if !info.Connected {
			state = "○ 未连接"
		}
		cacheNote := ""
		if info.Cache {
			cacheNote = "（缓存）"
		}
		fmt.Fprintf(&b, "  %-16s %-6s %s %s 工具数=%d%s\n",
			info.Name, info.Transport, state, cacheNote, len(info.Tools), errSuffix(info.Error))
	}
	return strings.TrimRight(b.String(), "\n")
}

// connectText 立即连接/重连指定（或全部）server 并刷新缓存
func (m *Manager) connectText(serverName string) string {
	targets := m.pick(serverName)
	if len(targets) == 0 {
		return fmt.Sprintf("MCP server %q 未找到（/mcp status 查看）", serverName)
	}
	var failed []string
	for _, s := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout())
		_, err := s.listTools(ctx)
		cancel()
		if err != nil {
			failed = append(failed, s.name+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return "连接失败: " + strings.Join(failed, "; ")
	}
	if serverName == "" {
		return "已连接全部 MCP server 并刷新工具缓存"
	}
	return "已连接 " + serverName
}

// errSuffix 状态面板附带的最近错误
func errSuffix(errMsg string) string {
	if errMsg == "" {
		return ""
	}
	return "（上次错误: " + errMsg + "）"
}
