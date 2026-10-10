package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"doge/internal/agent"
	"doge/internal/ai"
)

// proxyTool 生成 mcp 代理工具：模型通过一个工具按需 search → describe → call，
// 不把成百上千个 MCP 工具 schema 塞进 system prompt（约 200 tokens 固定成本）。
func proxyTool(m *Manager) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "mcp",
		Label:       "MCP",
		Description: "管理并调用外部 MCP（Model Context Protocol）服务器工具。action=search 按关键词检索工具；describe 查看工具参数 schema；call 执行工具（tool 用 server__tool 全名）；status 查看服务器状态；connect/reconnect 连接或重连服务器。未连接时 search/describe/status 基于磁盘缓存即可工作。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type": "string",
					"enum": []string{"search", "describe", "call", "status", "connect", "reconnect"},
				},
				"query":  map[string]any{"type": "string", "description": "search 关键词，按名字/服务器/描述加权匹配"},
				"server": map[string]any{"type": "string", "description": "服务器名过滤（call 时是 server__tool 前缀）"},
				"tool":   map[string]any{"type": "string", "description": "工具全名 server__tool"},
				"args":   map[string]any{"description": "call 的工具参数对象"},
				"limit":  map[string]any{"type": "integer", "description": "search 返回条数，默认 12"},
				"offset": map[string]any{"type": "integer", "description": "search 分页偏移，默认 0"},
			},
			"required": []string{"action"},
		},
		PrepareArguments: func(args any) (map[string]any, error) {
			params, ok := args.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("mcp: arguments must be an object")
			}
			// args 允许传 JSON 字符串（模型有时会这么写）
			if raw, ok := params["args"].(string); ok {
				var decoded any
				if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
					return nil, fmt.Errorf("mcp: args JSON invalid: %v", err)
				}
				params["args"] = decoded
			}
			return params, nil
		},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			action, _ := params["action"].(string)
			switch action {
			case "search":
				return textResult(m.searchText(queryOf(params), serverOf(params), intOf(params, "limit", 12), intOf(params, "offset", 0))), nil
			case "describe":
				return textResult(m.describeText(serverOf(params), toolOf(params))), nil
			case "call":
				server, tool, err := m.splitToolName(toolOf(params))
				if err != nil {
					return textResult("错误: " + err.Error()), nil
				}
				if ok, reason := m.approved(server, tool); !ok {
					return textResult("工具 " + m.toolFullName(server, tool) + " 需要批准：" + reason + "（mcp 代理已放行，内层审批拒绝；请修改 approveTools 配置后重试）"), nil
				}
				out, err := m.callTool(server, tool, params["args"])
				if err != nil {
					return textResult("错误: " + err.Error()), nil
				}
				return textResult(out), nil
			case "status":
				return textResult(m.statusText()), nil
			case "connect", "reconnect":
				return textResult(m.connectText(serverOf(params))), nil
			default:
				return textResult("错误: 未知 action " + action + "（search|describe|call|status|connect|reconnect）"), nil
			}
		},
	}
}

// textResult 文本工具结果
func textResult(text string) *agent.AgentToolResult {
	return &agent.AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(text)}}
}

func queryOf(p map[string]any) string {
	s, _ := p["query"].(string)
	return s
}

func serverOf(p map[string]any) string {
	s, _ := p["server"].(string)
	return s
}

func toolOf(p map[string]any) string {
	s, _ := p["tool"].(string)
	return s
}

func intOf(p map[string]any, key string, def int) int {
	if v, ok := p[key].(float64); ok {
		return int(v)
	}
	return def
}
