package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"doge/internal/coding/automation"
)

// AutomationView exposes the automation manager to the console API. It is
// satisfied structurally by *automation.API; nil (test stubs) answers 503.
type AutomationView interface {
	Cards() ([]*automation.AutomationCard, error)
	Get(id string) (*automation.Automation, error)
	Create(draft *automation.Automation) (*automation.Automation, error)
	Update(id string, p *automation.Patch) (*automation.Automation, error)
	Delete(id string) error
	SetEnabled(id string, enabled bool) (*automation.Automation, error)
	Run(id string) (string, error)
	Runs() ([]*automation.Run, error)
	History(id string, limit int) ([]*automation.Run, error)
}

// defaultRunsLimit 单任务历史默认条数
const defaultRunsLimit = 10

// handleAutomationsList 面板数据：全部任务卡片（含最近一次 run）。
func (s *Server) handleAutomationsList(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	cards, err := s.automations.Cards()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"automations": cards})
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
	created, err := s.automations.Create(&draft)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"automation": created})
}

// handleAutomationsGet 单条详情 + 最近 run（?runs=N 控制条数，默认 5）。
func (s *Server) handleAutomationsGet(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	a, err := s.automations.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	limit := 5
	if v := r.URL.Query().Get("runs"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, _ := s.automations.History(a.ID, limit)
	if runs == nil {
		runs = []*automation.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"automation": a, "runs": runs})
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
	p := &automation.Patch{}
	applyStr := func(key string, dst **string) {
		if raw, ok := body[key]; ok {
			var v string
			if json.Unmarshal(raw, &v) == nil {
				*dst = &v
			}
		}
	}
	applyStr("title", &p.Title)
	applyStr("prompt", &p.Prompt)
	applyStr("cwd", &p.Cwd)
	applyStr("model", &p.Model)
	applyStr("level", &p.Level)
	if raw, ok := body["maxRuns"]; ok {
		var v int
		if json.Unmarshal(raw, &v) == nil {
			p.MaxRuns = &v
		}
	}
	if raw, ok := body["schedule"]; ok {
		var sc automation.Schedule
		if err := json.Unmarshal(raw, &sc); err != nil {
			writeError(w, http.StatusBadRequest, "invalid schedule: "+err.Error())
			return
		}
		p.Schedule = &sc
	}
	updated, err := s.automations.Update(r.PathValue("id"), p)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"automation": updated})
}

// handleAutomationsDelete 删除任务。
func (s *Server) handleAutomationsDelete(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	if err := s.automations.Delete(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
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
		a, err := s.automations.SetEnabled(r.PathValue("id"), enabled)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"automation": a})
	}
}

// handleAutomationsRun 手动立即触发（异步，不占调度节奏）。
func (s *Server) handleAutomationsRun(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	runID, err := s.automations.Run(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "runId": runID})
}

// handleAutomationsRuns 任务台账（?limit=N，默认 10）。
func (s *Server) handleAutomationsRuns(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations not available")
		return
	}
	limit := defaultRunsLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := s.automations.History(r.PathValue("id"), limit)
	if err != nil && !errors.Is(err, automation.ErrUnavailable) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if runs == nil {
		runs = []*automation.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}
