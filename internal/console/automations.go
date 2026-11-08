package console

import (
	"encoding/json"
	"errors"

	"doge/internal/coding/automation"
)

// defaultRunsLimit 单任务台账默认条数
const defaultRunsLimit = 10

// defaultHistoryRuns 单任务详情附带的最近 run 条数
const defaultHistoryRuns = 5

// AutomationSource 自动化管理面的最小视图（*automation.API 与各传输的视图
// 接口均按方法集满足；nil 判定留在传输侧，未装配的错误类别各自处理）
type AutomationSource interface {
	Cards() ([]*automation.AutomationCard, error)
	Get(id string) (*automation.Automation, error)
	Create(draft *automation.Automation) (*automation.Automation, error)
	Update(id string, p *automation.Patch) (*automation.Automation, error)
	Delete(id string) error
	SetEnabled(id string, enabled bool) (*automation.Automation, error)
	Run(id string) (string, error)
	History(id string, limit int) ([]*automation.Run, error)
}

// AutomationList 面板数据：全部任务卡片（含最近一次 run）
func AutomationList(src AutomationSource) (map[string]any, *Error) {
	cards, err := src.Cards()
	if err != nil {
		return nil, Wrap(err)
	}
	return map[string]any{"automations": cards}, nil
}

// AutomationCreate 建任务（cwd 缺省与来源会话由传输侧补默认）
func AutomationCreate(src AutomationSource, draft *automation.Automation) (map[string]any, *Error) {
	created, err := src.Create(draft)
	if err != nil {
		return nil, Invalid(err.Error())
	}
	return map[string]any{"automation": created}, nil
}

// AutomationGet 单条详情 + 最近 run（runs<=0 取默认条数；无历史补空数组）
func AutomationGet(src AutomationSource, id string, runs int) (map[string]any, *Error) {
	a, err := src.Get(id)
	if err != nil {
		return nil, NotFound(err.Error())
	}
	if runs <= 0 {
		runs = defaultHistoryRuns
	}
	history, _ := src.History(a.ID, runs)
	if history == nil {
		history = []*automation.Run{}
	}
	return map[string]any{"automation": a, "runs": history}, nil
}

// AutomationPatch 部分更新：body 里出现的字段才改（title/prompt/cwd/model/
// level/maxRuns/schedule 整体替换），未出现保持不变
func AutomationPatch(src AutomationSource, id string, body map[string]json.RawMessage) (map[string]any, *Error) {
	p, e := ParseAutomationPatch(body)
	if e != nil {
		return nil, e
	}
	updated, err := src.Update(id, p)
	if err != nil {
		return nil, Invalid(err.Error())
	}
	return map[string]any{"automation": updated}, nil
}

// ParseAutomationPatch 从 JSON 键值构建部分更新，出现的键才改
func ParseAutomationPatch(body map[string]json.RawMessage) (*automation.Patch, *Error) {
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
			return nil, Invalid("invalid schedule: " + err.Error())
		}
		p.Schedule = &sc
	}
	return p, nil
}

// AutomationDelete 删除任务
func AutomationDelete(src AutomationSource, id string) *Error {
	if err := src.Delete(id); err != nil {
		return NotFound(err.Error())
	}
	return nil
}

// AutomationSetEnabled 暂停/恢复（enable 仅 paused 可恢复）
func AutomationSetEnabled(src AutomationSource, id string, enabled bool) (map[string]any, *Error) {
	a, err := src.SetEnabled(id, enabled)
	if err != nil {
		return nil, Invalid(err.Error())
	}
	return map[string]any{"automation": a}, nil
}

// AutomationRun 手动立即触发（异步，不占调度节奏）
func AutomationRun(src AutomationSource, id string) (map[string]any, *Error) {
	runID, err := src.Run(id)
	if err != nil {
		return nil, Invalid(err.Error())
	}
	return map[string]any{"ok": true, "runId": runID}, nil
}

// AutomationRuns 任务台账；调度器不可用视为空台账（两端统一口径）
func AutomationRuns(src AutomationSource, id string, limit int) (map[string]any, *Error) {
	if limit <= 0 {
		limit = defaultRunsLimit
	}
	runs, err := src.History(id, limit)
	if err != nil && !errors.Is(err, automation.ErrUnavailable) {
		return nil, NotFound(err.Error())
	}
	if runs == nil {
		runs = []*automation.Run{}
	}
	return map[string]any{"runs": runs}, nil
}
