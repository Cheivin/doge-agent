package services

import (
	"encoding/json"

	"doge/internal/coding/automation"
	"doge/internal/console"
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
	result, e := console.AutomationList(v)
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
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
	result, e := console.AutomationCreate(v, &draft)
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}

// Get 单条详情 + 最近 run（runs 控制条数，0 取默认 5）
func (s *AutomationsService) Get(id string, runs int) (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	result, e := console.AutomationGet(v, id, runs)
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
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
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errInvalid(err.Error())
	}
	result, e := console.AutomationPatch(v, id, body)
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}

// Delete 删除任务
func (s *AutomationsService) Delete(id string) error {
	v, err := s.view()
	if err != nil {
		return err
	}
	if e := console.AutomationDelete(v, id); e != nil {
		return consoleErr(e)
	}
	return nil
}

// SetEnabled 暂停/恢复
func (s *AutomationsService) SetEnabled(id string, enabled bool) (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	result, e := console.AutomationSetEnabled(v, id, enabled)
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}

// Run 手动立即触发（异步，不占调度节奏）
func (s *AutomationsService) Run(id string) (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	result, e := console.AutomationRun(v, id)
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}

// Runs 任务台账（limit 0 取默认 10）；调度器不可用视为空台账（同 Web 口径）
func (s *AutomationsService) Runs(id string, limit int) (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	result, e := console.AutomationRuns(v, id, limit)
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}
