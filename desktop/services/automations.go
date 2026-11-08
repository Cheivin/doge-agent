package services

import (
	"encoding/json"
	"errors"

	"doge/internal/coding/automation"
)

type (
	// AutomationsService 定时任务管理面（对应 /api/automations*，与 Web 同构）
	AutomationsService struct {
		d Deps
	}
)

// view 取管理面（挂在 App 装配产物上），未装配返回 unavailable
func (s *AutomationsService) view() (*automation.API, error) {
	if s.d.App == nil || s.d.App.Automation == nil {
		return nil, errUnavailable("automations not available")
	}
	return s.d.App.Automation, nil
}

// List 面板数据：全部任务卡片（含最近一次 run，形状同 Web GET /api/automations）
func (s *AutomationsService) List() (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	cards, err := v.Cards()
	if err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{"automations": cards}, nil
}

// Create 建任务（cwd 缺省取当前会话目录，来源会话记当前会话）
func (s *AutomationsService) Create(draft automation.Automation) (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	if draft.Cwd == "" {
		draft.Cwd = s.d.Backend.Status().Cwd
	}
	draft.SourceSessionID = s.d.Backend.Status().SessionID
	created, err := v.Create(&draft)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	return map[string]any{"automation": created}, nil
}

// Get 单条详情 + 最近 run（runs 控制条数，0 取默认 5）
func (s *AutomationsService) Get(id string, runs int) (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	a, err := v.Get(id)
	if err != nil {
		return nil, errNotFound(err.Error())
	}
	if runs <= 0 {
		runs = 5
	}
	history, _ := v.History(id, runs)
	if history == nil {
		history = []*automation.Run{}
	}
	return map[string]any{"automation": a, "runs": history}, nil
}

// Update 部分更新：fields 为 JSON 键值（title/prompt/cwd/model/level/maxRuns/
// schedule），出现的键才改，形状同 Web PATCH
func (s *AutomationsService) Update(id string, fields map[string]any) (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	p := &automation.Patch{}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errInvalid(err.Error())
	}
	applyStr := func(key string, dst **string) {
		if val, ok := body[key]; ok {
			var s2 string
			if json.Unmarshal(val, &s2) == nil {
				*dst = &s2
			}
		}
	}
	applyStr("title", &p.Title)
	applyStr("prompt", &p.Prompt)
	applyStr("cwd", &p.Cwd)
	applyStr("model", &p.Model)
	applyStr("level", &p.Level)
	if val, ok := body["maxRuns"]; ok {
		var n int
		if json.Unmarshal(val, &n) == nil {
			p.MaxRuns = &n
		}
	}
	if val, ok := body["schedule"]; ok {
		var sc automation.Schedule
		if err := json.Unmarshal(val, &sc); err != nil {
			return nil, errInvalid("invalid schedule: " + err.Error())
		}
		p.Schedule = &sc
	}
	updated, err := v.Update(id, p)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	return map[string]any{"automation": updated}, nil
}

// Delete 删除任务
func (s *AutomationsService) Delete(id string) error {
	v, err := s.view()
	if err != nil {
		return err
	}
	if err := v.Delete(id); err != nil {
		return errNotFound(err.Error())
	}
	return nil
}

// SetEnabled 暂停/恢复
func (s *AutomationsService) SetEnabled(id string, enabled bool) (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	a, err := v.SetEnabled(id, enabled)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	return map[string]any{"automation": a}, nil
}

// Run 手动立即触发（异步，不占调度节奏）
func (s *AutomationsService) Run(id string) (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	runID, err := v.Run(id)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	return map[string]any{"ok": true, "runId": runID}, nil
}

// Runs 任务台账（limit 0 取默认 10）；调度器不可用视为空台账（同 Web 口径）
func (s *AutomationsService) Runs(id string, limit int) (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	runs, err := v.History(id, limit)
	if err != nil && !errors.Is(err, automation.ErrUnavailable) {
		return nil, errNotFound(err.Error())
	}
	if runs == nil {
		runs = []*automation.Run{}
	}
	return map[string]any{"runs": runs}, nil
}
