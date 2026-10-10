package mcp

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"doge/internal/coding/permissions"
	"doge/internal/coding/tools"
)

// formatResult 把 CallToolResult 序列化为文本：文本内容直接拼接；二进制内容
// （image/audio，base64）解码写临时文件并给路径；其余内容 JSON 序列化。
// 总长超过 MaxOutputBytes 时截断，并附完整输出文件路径。
func formatResult(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			b.WriteString(v.Text)
		case *mcp.ImageContent:
			b.WriteString(spillBinary(v.MIMEType, v.Data))
		case *mcp.AudioContent:
			b.WriteString(spillBinary(v.MIMEType, v.Data))
		default:
			if data, err := json.Marshal(v); err == nil {
				b.Write(data)
			}
		}
		b.WriteString("\n")
	}
	text := b.String()
	if res.IsError {
		text = "MCP tool error:\n" + text
	}
	if len(text) <= tools.MaxOutputBytes {
		return text
	}
	// 超限：全文落临时文件，正文截断并给路径（模型可再 read）
	path := writeTemp([]byte(text))
	truncated := text[:tools.MaxOutputBytes]
	truncated += "\n…（已截断，完整输出在 " + path + "）"
	return truncated
}

// spillBinary 把 base64 二进制内容解码到临时文件，返回路径说明
func spillBinary(mimeType string, data []byte) string {
	if len(data) == 0 {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return "（二进制内容解码失败，已省略）"
	}
	path := writeTemp(raw)
	return "（二进制内容 " + mimeType + " 已保存: " + path + "）"
}

// writeTemp 写临时文件（os.CreateTemp 自动建在系统临时目录）
func writeTemp(data []byte) string {
	f, err := os.CreateTemp("", "doge-mcp-*")
	if err != nil {
		return "<无法创建临时文件>"
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "<临时文件写入失败>"
	}
	_ = f.Close()
	return name
}

// approved 内层工具审批：server 级 approveTools > 全局 approveTools（glob）。
// 配置了审批表但工具未命中 → 拒绝（headless fail-closed，不执行）。
func (m *Manager) approved(serverName, toolName string) (bool, string) {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if cfg == nil {
		return true, ""
	}
	s := cfg.McpServers[serverName]
	patterns := s.ApproveTools
	if len(patterns) == 0 {
		patterns = cfg.Settings.ApproveTools
	}
	if len(patterns) == 0 {
		return true, "" // 未配置审批表：外层权限门已把关
	}
	full := cfg.ToolName(serverName, toolName)
	for _, p := range patterns {
		if permissions.MatchPattern(p, full) || permissions.MatchPattern(p, toolName) {
			return true, ""
		}
	}
	return false, "未在 approveTools 白名单中（需要先在配置里批准 " + full + "）"
}
