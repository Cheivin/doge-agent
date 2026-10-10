package services

import (
	"encoding/json"
	"errors"
	"strings"

	"doge/internal/mcp"
	"doge/internal/web"
)

type (
	// McpService MCP 服务器管理（对应 /api/mcp*、/api/session/mcp）
	McpService struct {
		d Deps
	}
)

// view 取 MCP 视图，未初始化返回 unavailable
func (s *McpService) view() (web.McpView, error) {
	if s.d.Mcp == nil {
		return nil, errUnavailable("mcp not enabled")
	}
	return s.d.Mcp, nil
}

// redistribute 把最新 mcp 工具组重发给活动会话
func (s *McpService) redistribute() {
	if s.d.Mcp == nil {
		return
	}
	s.d.Session.RefreshMcpTools(s.d.Mcp.BuildTools())
}

// Info 服务器状态、工具清单与缓存/错误提示
func (s *McpService) Info() (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"enabled":    v.Enabled(),
		"configPath": v.WritePath(),
		"servers":    v.Servers(),
	}, nil
}

// SessionState 当前会话的 MCP 开关状态
func (s *McpService) SessionState() web.McpSessionState { return s.d.Backend.McpInfo() }

// SessionSet 记录会话级 MCP 开关（重载时生效；远端任务会话拒绝）
func (s *McpService) SessionSet(enabled bool) (map[string]any, error) {
	info := s.d.Backend.McpInfo()
	if !info.Available {
		return nil, errNotFound("本进程没有可用的 MCP 工具组（未配置或 --no-mcp）")
	}
	if info.Kind == "remote" {
		return nil, errReadOnly("远端任务会话不支持单独配置 MCP")
	}
	if err := s.d.Backend.SetMcpEnabled(enabled); err != nil {
		return nil, errBusy(err.Error())
	}
	return map[string]any{"ok": true, "pending": true}, nil
}

// Reconnect 重连一个 server（空名=全部）
func (s *McpService) Reconnect(server string) error {
	v, err := s.view()
	if err != nil {
		return err
	}
	if err := v.Reconnect(strings.TrimSpace(server)); err != nil {
		return errNotFound(err.Error())
	}
	s.redistribute()
	return nil
}

// Enable 启用一个 server
func (s *McpService) Enable(server string) error { return s.setEnabled(server, true) }

// Disable 停用一个 server
func (s *McpService) Disable(server string) error { return s.setEnabled(server, false) }

func (s *McpService) setEnabled(server string, on bool) error {
	v, err := s.view()
	if err != nil {
		return err
	}
	if err := v.SetEnabled(strings.TrimSpace(server), on); err != nil {
		return errNotFound(err.Error())
	}
	s.redistribute()
	return nil
}

// ServerDirect 切换 server 的一等工具提升（directTools），即时重建工具组
func (s *McpService) ServerDirect(name string, on bool) error {
	v, err := s.view()
	if err != nil {
		return err
	}
	if err := v.SetServerDirect(name, on); err != nil {
		return errNotFound(err.Error())
	}
	s.redistribute()
	return nil
}

// ServerAutoStart 切换 server 自启连接
func (s *McpService) ServerAutoStart(name string, on bool) error {
	v, err := s.view()
	if err != nil {
		return err
	}
	if err := v.SetServerAutoStart(name, on); err != nil {
		return errNotFound(err.Error())
	}
	return nil
}

// ServerRemove 删除 server 定义（仅项目配置可删）
func (s *McpService) ServerRemove(name string) error {
	v, err := s.view()
	if err != nil {
		return err
	}
	if err := v.RemoveServer(name); err != nil {
		return errInvalid(err.Error())
	}
	return nil
}

// UpsertServers 按社区 mcpServers 粘贴格式合并 server 定义，reload 生效
func (s *McpService) UpsertServers(body json.RawMessage) error {
	v, err := s.view()
	if err != nil {
		return err
	}
	servers, err := parseMcpServersBody(body)
	if err != nil {
		return errInvalid(err.Error())
	}
	if err := v.UpsertServers(servers); err != nil {
		return errInvalid(err.Error())
	}
	return nil
}

// Reload 重读 mcp 配置重建 server 注册表，新工具组分发到活动会话
func (s *McpService) Reload() error {
	v, err := s.view()
	if err != nil {
		return err
	}
	if err := v.Reload(); err != nil {
		return errInternal(err)
	}
	s.redistribute()
	return nil
}

// parseMcpServersBody 解析社区 mcpServers 粘贴格式（apply 复用）：
//
//	{"playwright": {"command":"npx","args":[...]}}               （内层 map）
//	{"mcpServers": {"playwright": {"command":"npx","args":[...]}}} （完整 .mcp.json）
func parseMcpServersBody(body []byte) (map[string]*mcp.Server, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) == 0 {
		return nil, errors.New(`invalid request body: want an mcpServers object like {"playwright":{"command":"npx","args":[]}}`)
	}
	if _, ok := raw["mcpServers"]; ok && len(raw) == 1 {
		var servers map[string]*mcp.Server
		if err := json.Unmarshal(raw["mcpServers"], &servers); err != nil || len(servers) == 0 {
			return nil, errors.New(`mcpServers 值必须是一个对象，如 {"playwright":{"command":"npx"}}`)
		}
		return servers, nil
	}
	servers := make(map[string]*mcp.Server, len(raw))
	for k, v := range raw {
		var srv mcp.Server
		if err := json.Unmarshal(v, &srv); err != nil {
			return nil, errors.New("server " + k + ": " + err.Error())
		}
		servers[k] = &srv
	}
	return servers, nil
}
