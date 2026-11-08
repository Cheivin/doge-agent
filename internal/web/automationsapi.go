package web

import (
	"encoding/json"
	"net/http"
	"strconv"

	"doge/internal/coding/automation"
	"doge/internal/console"
)

// handleAutomationsList 面板数据：全部任务卡片（含最近一次 run）。
func (s *Server) handleAutomationsList(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	result, e := console.AutomationList(s.automations)
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleAutomationsCreate 建任务：body 即 Automation 文档（title/prompt/
// schedule 必填，cwd 缺省取服务进程当前目录；来源会话记为共享会话，run 结束
// 结果卡片回注可见）。
func (s *Server) handleAutomationsCreate(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	var draft automation.Automation
	if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if draft.Cwd == "" {
		draft.Cwd = s.defaultBackend.Status().Cwd
	}
	draft.SourceSessionID = s.defaultBackend.Status().SessionID
	result, e := console.AutomationCreate(s.automations, &draft)
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleAutomationsGet 单条详情 + 最近 run（?runs=N 控制条数，默认 5）。
func (s *Server) handleAutomationsGet(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	limit := 0
	if v := r.URL.Query().Get("runs"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	result, e := console.AutomationGet(s.automations, r.PathValue("id"), limit)
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleAutomationsPatch 部分更新：body 里出现的字段才改（title/prompt/cwd/
// model/level/maxRuns/schedule 整体替换），未出现保持不变。
func (s *Server) handleAutomationsPatch(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, e := console.AutomationPatch(s.automations, r.PathValue("id"), body)
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleAutomationsDelete 删除任务。
func (s *Server) handleAutomationsDelete(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	if e := console.AutomationDelete(s.automations, r.PathValue("id")); e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAutomationsSetEnabled 暂停/恢复（enable 仅 paused 可恢复）。
func (s *Server) handleAutomationsSetEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.automations == nil {
			writeError(w, http.StatusServiceUnavailable, "automations not available")
			return
		}
		result, e := console.AutomationSetEnabled(s.automations, r.PathValue("id"), enabled)
		if e != nil {
			writeError(w, e.Kind.HTTPStatus(), e.Message)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// handleAutomationsRun 手动立即触发（异步，不占调度节奏）。
func (s *Server) handleAutomationsRun(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	result, e := console.AutomationRun(s.automations, r.PathValue("id"))
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleAutomationsRuns 任务台账（?limit=N，默认 10）。
func (s *Server) handleAutomationsRuns(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	result, e := console.AutomationRuns(s.automations, r.PathValue("id"), limit)
	if e != nil {
		writeError(w, e.Kind.HTTPStatus(), e.Message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
