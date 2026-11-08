package web

// 桌面端能力反向赋能的 Web 端点：与 desktop/services 的绑定同形状（前端共享
// UI 内核 app.js 只写一遍，能力经 platform caps 逐端开启）。全部经 requireAuth。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/permissions"
	"doge/internal/console"
	"doge/internal/mesh"
)

// sessionTailMessages 转录分片默认条数（与 desktop services.tailMessages 一致）
const sessionTailMessages = 30

// handleSessionMessages 分片返回当前转录 index 之前的消息（大转录按需加载；
// 形状与桌面端 SessionService.MessagesBefore 一致：messages/offset/userOffset，
// userOffset 为本批之前未加载消息里的 user 条数，供 turn/edit 对齐全量序号）。
func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	msgs := s.backendFor(r).SessionInfo().Messages
	index, _ := strconv.Atoi(r.URL.Query().Get("before"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if index < 0 || index > len(msgs) {
		index = len(msgs)
	}
	if limit <= 0 || limit > 100 {
		limit = sessionTailMessages
	}
	begin := index - limit
	if begin < 0 {
		begin = 0
	}
	user := 0
	for _, m := range msgs[:begin] {
		if m != nil && m.Role == ai.RoleUser {
			user++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages":   msgs[begin:index],
		"offset":     begin,
		"userOffset": user,
	})
}

// permissionsPath 全局权限策略文件（与桌面端 ConfigService 同一定位）
func permissionsPath() string {
	return filepath.Join(coding.AgentDir(), "permissions.json")
}

// handlePermissionsGet 读权限策略（文件不存在返回空策略，形状与桌面绑定一致）
func (s *Server) handlePermissionsGet(w http.ResponseWriter, r *http.Request) {
	path := permissionsPath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"policy": permissions.Policy{}, "path": path})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	policy, err := permissions.LoadPolicy(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": policy, "path": path})
}

// handlePermissionsPut 校验并整体写入 permissions.json（备份 .bak）后全局热
// 重载。保存失败/重载失败沿用桌面绑定的形状（200 + ok:false），前端按
// j.ok === false 分支提示。
func (s *Server) handlePermissionsPut(w http.ResponseWriter, r *http.Request) {
	var policy permissions.Policy
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "reloaded": false, "error": "invalid request body"})
		return
	}
	data, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "reloaded": false, "error": err.Error()})
		return
	}
	if _, e := console.ValidatePermissionsData(data); e != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "reloaded": false, "error": e.Message})
		return
	}
	target := permissionsPath()
	console.BackupFile(target)
	if err := console.WriteFileAtomic(target, data, 0o600); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "reloaded": false, "error": err.Error()})
		return
	}
	resp := map[string]any{"ok": true, "reloaded": true}
	if err := s.reloadAfterConfigWrite(s.backendFor(r)); err != nil {
		resp["reloaded"] = false
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// meshConfigPath mesh.json 路径（与桌面端 MeshService 同一定位）
func meshConfigPath() string {
	return filepath.Join(coding.AgentDir(), "mesh.json")
}

// handleMeshRunning mesh 节点运行状态（无运行时钩子 503：web 宿主可能只持
// 只读 MeshView，节点启停入口在桌面壳等持有 App 的宿主手里）
func (s *Server) handleMeshRunning(w http.ResponseWriter, r *http.Request) {
	if s.meshRuntime == nil {
		writeError(w, http.StatusServiceUnavailable, "mesh runtime not available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"running": s.meshRuntime.Running()})
}

// handleMeshStart 运行时启动 mesh 节点（mesh.json 里须有 token）
func (s *Server) handleMeshStart(w http.ResponseWriter, r *http.Request) {
	if s.meshRuntime == nil {
		writeError(w, http.StatusServiceUnavailable, "mesh runtime not available")
		return
	}
	if err := s.meshRuntime.Start(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMeshStop 停用 mesh 节点（未运行 404，对齐桌面绑定）
func (s *Server) handleMeshStop(w http.ResponseWriter, r *http.Request) {
	if s.meshRuntime == nil {
		writeError(w, http.StatusServiceUnavailable, "mesh runtime not available")
		return
	}
	if !s.meshRuntime.Running() {
		writeError(w, http.StatusNotFound, "mesh not running")
		return
	}
	s.meshRuntime.Stop()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMeshConfigGet 读 mesh.json（含 token，本地配置面板自用——调用方已持
// 访问令牌，/api/mesh 的 config 字段同样如此）
func (s *Server) handleMeshConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := mesh.LoadConfig(meshConfigPath())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": cfg, "path": meshConfigPath()})
}

// handleMeshConfigPut 写 mesh.json 并热应用（运行中则重启节点）；token 留空
// 保留现值。失败沿用桌面绑定的形状（200 + ok:false）。
func (s *Server) handleMeshConfigPut(w http.ResponseWriter, r *http.Request) {
	var cfg mesh.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "invalid request body"})
		return
	}
	path := meshConfigPath()
	cur, err := mesh.LoadConfig(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if cfg.Token == "" {
		cfg.Token = cur.Token
	}
	if err := mesh.SaveConfig(path, &cfg); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	restarted := false
	if s.meshRuntime != nil && s.meshRuntime.Running() {
		s.meshRuntime.Stop()
		if err := s.meshRuntime.Start(); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarted": false, "error": "配置已保存，节点重启失败: " + err.Error()})
			return
		}
		restarted = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarted": restarted})
}
