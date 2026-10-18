package automation

import "errors"

// ErrUnavailable 管理面未装配（进程未启用定时任务）时 API 的统一答复
var ErrUnavailable = errors.New("automations not available")

// API 定时任务管理面：store + runner 的薄组合，Web 控制台、桌面 Service
// 与 CLI 子命令共用同一套操作（对话内 automation 工具走 store/runner 原始
// 接口，与此互不干扰——同一 flock 保证串行）。nil 接收者安全：调用返回
// ErrUnavailable，便于上层在未装配时优雅降级。
type API struct {
	Store  *Store
	Runner *Runner
}

// NewAPI 构建管理面
func NewAPI(store *Store, runner *Runner) *API {
	return &API{Store: store, Runner: runner}
}

// available 报告管理面是否可用（store 必需；runner 仅执行类操作需要）
func (a *API) available() bool { return a != nil && a.Store != nil }

// List 全部任务快照
func (a *API) List() ([]*Automation, error) {
	if !a.available() {
		return nil, ErrUnavailable
	}
	return a.Store.List()
}

// Get 单条任务
func (a *API) Get(id string) (*Automation, error) {
	if !a.available() {
		return nil, ErrUnavailable
	}
	return a.Store.Get(id)
}

// Create 建任务（管理面来源恒为用户动作，origin 置空；AnchorAt 缺省补当前
// 时间——delay/interval 都从它推导）
func (a *API) Create(draft *Automation) (*Automation, error) {
	if !a.available() {
		return nil, ErrUnavailable
	}
	if draft.Schedule.AnchorAt == 0 {
		draft.Schedule.AnchorAt = a.Store.now().Unix()
	}
	if err := a.Store.Create("", draft); err != nil {
		return nil, err
	}
	return a.Store.Get(draft.ID)
}

// Update 应用可变字段改动（调度变更即重锚重算）
func (a *API) Update(id string, p *Patch) (*Automation, error) {
	if !a.available() {
		return nil, ErrUnavailable
	}
	return a.Store.Update("", id, p)
}

// Delete 删除任务
func (a *API) Delete(id string) error {
	if !a.available() {
		return ErrUnavailable
	}
	return a.Store.Delete("", id)
}

// SetEnabled 暂停/恢复
func (a *API) SetEnabled(id string, enabled bool) (*Automation, error) {
	if !a.available() {
		return nil, ErrUnavailable
	}
	return a.Store.SetEnabled("", id, enabled)
}

// Run 手动立即触发（异步执行，返回 runId；不占调度节奏）
func (a *API) Run(id string) (string, error) {
	if !a.available() || a.Runner == nil {
		return "", ErrUnavailable
	}
	target, err := a.Store.Get(id)
	if err != nil {
		return "", err
	}
	return a.Runner.ManualRun(target)
}

// Runs 全量台账（同 ID 末行生效，新→旧）
func (a *API) Runs() ([]*Run, error) {
	if a == nil || a.Runner == nil {
		return nil, ErrUnavailable
	}
	return a.Runner.Runs()
}

// History 某任务的台账（新→旧）
func (a *API) History(id string, limit int) ([]*Run, error) {
	if a == nil || a.Runner == nil {
		return nil, ErrUnavailable
	}
	return a.Runner.History(id, limit)
}

// AutomationCard 管理面板卡片视图：任务 + 最近一次 run（Web/桌面共用形状）
type AutomationCard struct {
	*Automation
	LastRun *Run `json:"lastRun,omitempty"`
}

// Cards 面板数据：全部任务各附最近一次 run（无记录则省略）
func (a *API) Cards() ([]*AutomationCard, error) {
	list, err := a.List()
	if err != nil {
		return nil, err
	}
	runs, err := a.Runs()
	if err != nil {
		runs = nil // 台账缺失不挡面板（新装环境无 runs.jsonl）
	}
	latest := map[string]*Run{}
	for _, run := range runs { // Runs 已新→旧：首见即最新
		if _, ok := latest[run.AutomationID]; !ok {
			latest[run.AutomationID] = run
		}
	}
	out := make([]*AutomationCard, 0, len(list))
	for _, item := range list {
		out = append(out, &AutomationCard{Automation: item, LastRun: latest[item.ID]})
	}
	return out, nil
}
