package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"doge/internal/mcp"
	"doge/pkg/logx"
)

// handleMcpStatus reports the MCP manager state: per-server connection status,
// tool metadata and cache/error hints. 404 when MCP is not initialized.
func (s *Server) handleMcpStatus(w http.ResponseWriter, r *http.Request) {
	if s.mcpView == nil {
		writeError(w, http.StatusNotFound, "mcp not enabled")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":    s.mcpView.Enabled(),
		"configPath": s.mcpView.WritePath(),
		"servers":    s.mcpView.Servers(),
	})
}

// handleMcpReconnect immediately disconnects and reconnects one server
// (empty name = all), refreshing the tool cache.
func (s *Server) handleMcpReconnect(w http.ResponseWriter, r *http.Request) {
	if s.mcpView == nil {
		writeError(w, http.StatusNotFound, "mcp not enabled")
		return
	}
	var req struct {
		Server string `json:"server"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.mcpView.Reconnect(strings.TrimSpace(req.Server)); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// 重连可能拉到新的工具清单，direct 工具组同步刷新到各活动会话
	s.redistributeMcp()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMcpSetEnabled enables/disables one server (writes the disabled field
// to the project .doge/mcp.json).
func (s *Server) handleMcpSetEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.mcpView == nil {
			writeError(w, http.StatusNotFound, "mcp not enabled")
			return
		}
		var req struct {
			Server string `json:"server"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if err := s.mcpView.SetEnabled(strings.TrimSpace(req.Server), enabled); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		// 启停改变可用 server 集合，direct 工具组随之刷新
		s.redistributeMcp()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleMcpReload re-reads the mcp config files and rebuilds the server
// registry (disconnects live servers). Config edits take effect here — the
// user chooses when. The freshly built toolset is then re-issued to every
// live session, so servers added while doge was running reach the agent
// without a restart.
func (s *Server) handleMcpReload(w http.ResponseWriter, r *http.Request) {
	if s.mcpView == nil {
		writeError(w, http.StatusNotFound, "mcp not enabled")
		return
	}
	if err := s.mcpView.Reload(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.redistributeMcp()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// redistributeMcp 把最新的 mcp 工具组重发给所有活动会话（TUI 共享会话、各
// web 窗口分叉的会话、远端任务 fork）。
func (s *Server) redistributeMcp() {
	if s.mcpView == nil || s.ctrl == nil {
		return
	}
	tools := s.mcpView.BuildTools()
	for _, sess := range s.ctrl.actives() {
		sess.RefreshMcpTools(tools)
	}
}

// handleMcpServersUpsert imports server definitions in the community
// mcpServers paste format. Both of these are accepted:
//
//	{"playwright": {"command":"npx","args":[...]}}               （内层 map）
//	{"mcpServers": {"playwright": {"command":"npx","args":[...]}}} （完整 .mcp.json 内容）
//
// Servers are field-merged into the global write file. Runtime state is not
// touched — POST /api/mcp/reload applies it.
func (s *Server) handleMcpServersUpsert(w http.ResponseWriter, r *http.Request) {
	if s.mcpView == nil {
		writeError(w, http.StatusNotFound, "mcp not enabled")
		return
	}
	// 只读一次 body；调试日志记前段（排障用）
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body failed")
		return
	}
	preview := string(body)
	if len(preview) > 200 {
		preview = preview[:200] + "…"
	}
	logx.Debugf("[web] mcp config import body(%d): %s", len(body), preview)

	servers, err := parseMcpServersBody(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.mcpView.UpsertServers(servers); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// parseMcpServersBody 解析社区 mcpServers 粘贴格式（apply 接口复用）：
//
//	{"playwright": {"command":"npx","args":[...]}}               （内层 map）
//	{"mcpServers": {"playwright": {"command":"npx","args":[...]}}} （完整 .mcp.json 内容）
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

// handleMcpServersRemove deletes a server definition from the project
// .doge/mcp.json (definitions living only in global/shared files are refused).
func (s *Server) handleMcpServersRemove(w http.ResponseWriter, r *http.Request) {
	if s.mcpView == nil {
		writeError(w, http.StatusNotFound, "mcp not enabled")
		return
	}
	if err := s.mcpView.RemoveServer(r.PathValue("name")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMcpServerDirect 切换某 server 是否默认把全部工具提升为一等工具
// （directTools）。持久化到项目配置并即时重建工具组分发到活动会话。
func (s *Server) handleMcpServerDirect(w http.ResponseWriter, r *http.Request) {
	on, ok := readFlagBody(w, r)
	if !ok {
		return
	}
	if s.mcpView == nil {
		writeError(w, http.StatusNotFound, "mcp not enabled")
		return
	}
	if err := s.mcpView.SetServerDirect(r.PathValue("name"), on); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.redistributeMcp()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMcpServerAutoStart 切换是否随发现/reload 自动启动并连接（开启立即连）。
func (s *Server) handleMcpServerAutoStart(w http.ResponseWriter, r *http.Request) {
	on, ok := readFlagBody(w, r)
	if !ok {
		return
	}
	if s.mcpView == nil {
		writeError(w, http.StatusNotFound, "mcp not enabled")
		return
	}
	if err := s.mcpView.SetServerAutoStart(r.PathValue("name"), on); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// readFlagBody 解析 {"enabled": bool} 请求体
func readFlagBody(w http.ResponseWriter, r *http.Request) (bool, bool) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled (bool) required")
		return false, false
	}
	return *req.Enabled, true
}

// handleSessionMcpGet reports the current session's MCP toggle state.
func (s *Server) handleSessionMcpGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backendFor(r).McpInfo())
}

// handleSessionMcpSet records the session's desired MCP on/off state. It does
// NOT touch the live toolset: the change lands when this window reloads
// (POST /api/reload), which is the manual "刷新生效" step. Remote-task sessions
// are rejected (403).
func (s *Server) handleSessionMcpSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled (bool) required")
		return
	}
	b := s.backendFor(r)
	info := b.McpInfo()
	if !info.Available {
		writeError(w, http.StatusNotFound, "本进程没有可用的 MCP 工具组（未配置或 --no-mcp）")
		return
	}
	if err := b.SetMcpEnabled(*req.Enabled); err != nil {
		if errors.Is(err, errRemoteSessionMcp) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pending": true})
}
