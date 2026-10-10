package coding

import (
	"errors"

	"doge/internal/agent"
	"doge/pkg/logx"
)

// 会话级 MCP 工具组管理：mcp 代理工具与 direct 提升工具作为一个整体挂载/
// 卸载。启用/停用只记录意图（pending），由 ReloadConfig（重载）统一落地，
// 让用户手动选择生效时机——不打断正在进行的 run。

// SetMcpTools 注入 mcp 工具组并立即挂载（启动路径；幂等）。空切片忽略——
// 无 mcp 配置的进程不产生任何状态。
func (s *AgentSession) SetMcpTools(tools []*agent.AgentTool) {
	if len(tools) == 0 {
		return
	}
	s.mu.Lock()
	s.mcpTools = tools
	s.mcpMounted = true
	s.extraTools = append(s.extraTools, tools...)
	s.mu.Unlock()
	s.addSessionTools(tools)
}

// HasMcp 本进程是否给该会话注入过 mcp 工具组（与挂载与否无关）
func (s *AgentSession) HasMcp() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mcpTools != nil
}

// RefreshMcpTools 把会话的 mcp 工具组替换为给定集合——运行期新增/修改 server
// 后由 reload 分发给所有活动会话（进程启动时才绑定一次是不够的：WebUI 里
// 导入配置时进程早已在运行）。迁移语义：
//
//   - 会话从未有过 mcp 组 → 首次注入并挂载（默认启用）
//   - 已有组 → 原地换新，保持当前挂载状态（用户停用过的会话不会被悄悄打开）
//   - 有待生效的启停意图 → 尊重意图（applyMcp 在下次 reload 落地）
//
// tools 为空表示进程已没有任何可用 mcp 工具，整体卸载。
func (s *AgentSession) RefreshMcpTools(tools []*agent.AgentTool) {
	s.mu.Lock()
	fresh := len(s.mcpTools) == 0 && !s.mcpMounted && s.mcpDesired == nil
	var on bool
	switch {
	case len(tools) == 0:
		on = false // 进程已没有任何可用 mcp 工具，整体卸载
	case fresh:
		on = true
	case s.mcpDesired != nil:
		on = *s.mcpDesired
	default:
		on = s.mcpMounted
	}
	old := s.mcpTools
	s.mcpTools = tools
	unchanged := sameTools(old, tools) && on == s.mcpMounted
	s.mcpMounted = on
	if unchanged {
		s.mu.Unlock()
		return
	}
	oldSet := make(map[*agent.AgentTool]bool, len(old))
	for _, t := range old {
		oldSet[t] = true
	}
	newSet := make(map[*agent.AgentTool]bool, len(tools))
	for _, t := range tools {
		newSet[t] = true
	}
	extra := make([]*agent.AgentTool, 0, len(s.extraTools)+len(tools))
	for _, t := range s.extraTools {
		if oldSet[t] || (newSet[t] && !on) {
			continue // 换掉旧组；卸载态下不保留新组残留
		}
		extra = append(extra, t)
	}
	if on {
		extra = append(extra, tools...)
	}
	s.extraTools = extra
	customPrompt := s.customPrompt
	sessionID := s.Session.Header.ID
	s.mu.Unlock()

	live := make([]*agent.AgentTool, 0, len(extra))
	for _, t := range s.Agent.State().Tools {
		if oldSet[t] || (newSet[t] && !on) {
			continue
		}
		live = append(live, t)
	}
	if on {
		live = append(live, tools...)
	}
	s.Agent.SetTools(live)
	s.Agent.SetSystemPrompt(s.buildSystemPrompt(customPrompt))
	logx.Infof("[session] %s mcp toolset refreshed (%d tool(s), mounted=%v)",
		sessionID, len(tools), on)
}

// sameTools 指针级比较两组工具是否完全一致
func sameTools(a, b []*agent.AgentTool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// McpState 返回本会话 MCP 状态：期望开关、实际挂载、是否有待生效变更。
// 无待生效变更时 desired == mounted。
func (s *AgentSession) McpState() (desired, mounted, pending bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	desired = s.mcpMounted
	if s.mcpDesired != nil {
		desired = *s.mcpDesired
	}
	pending = s.mcpDesired != nil && *s.mcpDesired != s.mcpMounted
	return desired, s.mcpMounted, pending
}

// SetMcpEnabled 记录本会话 MCP 启停意图。不立即改动工具集——由 ReloadConfig
// （重载）统一应用。返回错误表示本进程没有可管理的 mcp 工具组。
func (s *AgentSession) SetMcpEnabled(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mcpTools == nil {
		return errors.New("no mcp toolset attached")
	}
	s.mcpDesired = &on
	return nil
}

// applyMcp 使待生效的启停落地：从 extraTools 与 live agent 移除/补回 mcp 组
// 并重建 system prompt。由 ReloadConfig 调用；无变更时是 no-op。
func (s *AgentSession) applyMcp() {
	s.mu.Lock()
	if s.mcpDesired == nil || s.mcpTools == nil {
		s.mu.Unlock()
		return
	}
	on := *s.mcpDesired
	s.mcpDesired = nil
	if on == s.mcpMounted {
		s.mu.Unlock()
		return
	}
	s.mcpMounted = on
	set := make(map[*agent.AgentTool]bool, len(s.mcpTools))
	for _, t := range s.mcpTools {
		set[t] = true
	}
	extra := make([]*agent.AgentTool, 0, len(s.extraTools)+len(s.mcpTools))
	for _, t := range s.extraTools {
		if !set[t] {
			extra = append(extra, t)
		}
	}
	if on {
		extra = append(extra, s.mcpTools...)
	}
	s.extraTools = extra
	customPrompt := s.customPrompt
	sessionID := s.Session.Header.ID
	s.mu.Unlock()

	// live 工具集同样按指针集合过滤，保留 mesh / per-session 等其它注入；
	// SetTools/SetSystemPrompt 在锁外（与 addSessionTools 同一模式）
	live := make([]*agent.AgentTool, 0, len(extra))
	for _, t := range s.Agent.State().Tools {
		if !set[t] {
			live = append(live, t)
		}
	}
	if on {
		live = append(live, s.mcpTools...)
	}
	s.Agent.SetTools(live)
	s.Agent.SetSystemPrompt(s.buildSystemPrompt(customPrompt))
	state := "unmounted"
	if on {
		state = "mounted"
	}
	logx.Infof("[session] %s mcp tools %s (reload applied)", sessionID, state)
}
