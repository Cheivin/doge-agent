package web

import (
	"encoding/json"
	"net/http"

	"doge/internal/console"
	"doge/internal/mcp"
)

// handleUploadPlace 把暂存区文件复制到白名单目的地（harness 直写，不经
// permissions；守卫是白名单根 + EvalSymlinks 复核 + web token）。
// dest 相对路径相对当前会话 cwd。
func (s *Server) handleUploadPlace(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not available")
		return
	}
	var req struct {
		Dest      string `json:"dest"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, e := console.PlaceUpload(s.uploads, r.PathValue("id"), req.Dest, s.backendFor(r).Status().Cwd, req.Overwrite)
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUploadApply 应用暂存区文件为配置（kind: mcp/models/skills-zip/
// permissions）。双段确认：默认 dry-run 只校验+预览；confirm:true 才写盘并
// 触发 reload。auth.json / mesh.json / settings.json 不提供 apply。
func (s *Server) handleUploadApply(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not available")
		return
	}
	var req struct {
		Kind    string `json:"kind"`
		Confirm bool   `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, e := console.ApplyUpload(s.uploads, r.PathValue("id"), req.Kind, req.Confirm, console.ApplyDeps{
		Reload: func() error { return s.reloadAfterConfigWrite(s.backendFor(r)) },
		Mcp:    s.applyMcpServers,
	})
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// applyMcpServers kind=mcp 的 confirm 段：合并进项目 .doge/mcp.json，重建
// server 注册表并把新工具组分发到活动会话（等价 /api/mcp/servers +
// /api/mcp/reload）。mcp 未启用报 404（desktop 侧为 unavailable，类别留传输侧）。
func (s *Server) applyMcpServers(servers map[string]*mcp.Server) *console.Error {
	if s.mcpView == nil {
		return console.NotFound("mcp not enabled")
	}
	var err error
	console.LockApply(func() { err = s.mcpView.UpsertServers(servers) })
	if err != nil {
		return console.Invalid(err.Error())
	}
	if err := s.mcpView.Reload(); err != nil {
		return console.Wrap(err)
	}
	s.redistributeMcp()
	return nil
}
