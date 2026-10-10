package mcp

import (
	"context"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"doge/internal/agent"
	"doge/internal/coding/permissions"
	"doge/pkg/logx"
)

// directToolNames 解析某 server 的 directTools 设置（server 级优先，否则全局）：
// true=全部工具，[]string=指定工具名
func (m *Manager) directToolNames(cfg *Config, serverName string, s *Server) (bool, []string) {
	var setting any = s.DirectTools
	if setting == nil {
		setting = cfg.Settings.DirectTools
	}
	switch v := setting.(type) {
	case bool:
		return v, nil
	case []any:
		names := make([]string, 0, len(v))
		for _, n := range v {
			if s, ok := n.(string); ok {
				names = append(names, s)
			}
		}
		return false, names
	case []string:
		return false, v
	}
	return false, nil
}

// included 代理模式工具过滤：excludeTools 排除，includeTools 空=全收
func included(s *Server, fullName, toolName string) bool {
	for _, p := range s.ExcludeTools {
		if permissions.MatchPattern(p, fullName) || permissions.MatchPattern(p, toolName) {
			return false
		}
	}
	if len(s.IncludeTools) == 0 {
		return true
	}
	for _, p := range s.IncludeTools {
		if permissions.MatchPattern(p, fullName) || permissions.MatchPattern(p, toolName) {
			return true
		}
	}
	return false
}

// buildDirectTools 把配置为 directTools 的工具提升为一等工具。
// direct 模式需要工具清单：优先磁盘缓存；无缓存且 server 未连接时在本阶段
// 连接一次拉取（direct 模式唯一的连接受限例外——模型需要工具名才能调用）。
func (m *Manager) buildDirectTools(cfg *Config) []*agent.AgentTool {
	m.mu.Lock()
	servers := make(map[string]*server, len(m.servers))
	for name, s := range m.servers {
		servers[name] = s
	}
	m.mu.Unlock()

	var out []*agent.AgentTool
	for name, svr := range servers {
		if svr.cfg.Disabled {
			continue
		}
		all, names := m.directToolNames(cfg, name, svr.cfg)
		if !all && len(names) == 0 {
			continue
		}
		tools := m.cachedTools(svr)
		if len(tools) == 0 {
			if !m.tryRefresh(name) {
				logx.Warnf("[mcp] %s direct tools: %v", name, m.lastError(name))
				continue
			}
			tools = m.cachedTools(svr)
		}
		for _, t := range tools {
			fullName := cfg.ToolName(name, t.Name)
			if all || contains(names, t.Name) {
				if !included(svr.cfg, fullName, t.Name) {
					continue
				}
				out = append(out, m.directTool(name, t))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// cachedTools 取 server 当前元数据快照
func (m *Manager) cachedTools(s *server) []*mcp.Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*mcp.Tool(nil), s.tools...)
}

// directTool 把一个 MCP 工具包装成一等 AgentTool（server__tool 命名）
func (m *Manager) directTool(serverName string, t *mcp.Tool) *agent.AgentTool {
	cfg := m.Config()
	fullName := cfg.ToolName(serverName, t.Name)
	schema, _ := t.InputSchema.(map[string]any)
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	return &agent.AgentTool{
		Name:        fullName,
		Label:       "MCP:" + serverName,
		Description: t.Description,
		Parameters:  schema,
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			if ok, reason := m.approved(serverName, t.Name); !ok {
				return textResult("工具 " + fullName + " 需要批准：" + reason), nil
			}
			out, err := m.callTool(serverName, t.Name, params)
			if err != nil {
				return textResult("错误: " + err.Error()), nil
			}
			return textResult(out), nil
		},
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
