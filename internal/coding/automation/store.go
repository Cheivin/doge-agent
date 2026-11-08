// Package automation 定时任务：automations.json 存储（flock 跨进程协调）、
// 结构化调度引擎、进程内调度器、无人值守执行器与 automation 工具。
//
// 设计要点（见 docs/automation-design.md）：定义与调度状态合一在
// automations.json（0600、可手工编辑、tmp+rename 原子写、flock 保护读改写）；
// 运行台账 append-only 落 automations/runs.jsonl；多进程并存靠文件认领单飞。
package automation

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"doge/internal/coding"
)

// 调度类型（Schedule.Kind 三选一，互斥校验在工具层与 store 层双重实现）
const (
	ScheduleDelay    = "delay"    // 一次性：创建后 N 分钟触发
	ScheduleInterval = "interval" // 循环：每 interval 个 intervalUnit，锚点推导
	ScheduleCron     = "cron"     // 循环：5 字段本地时区 cron，墙钟时刻
)

// 任务状态
const (
	StatusActive    = "active"
	StatusPaused    = "paused"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// 工具级（照远端任务分级裁剪）
const (
	LevelReadonly = "readonly"
	LevelWrite    = "write"
	LevelFull     = "full"
)

// 校验上限（与工具 schema 描述一致，双重实现的另一半）
const (
	maxDelayMinutes = 525600 // 一年
	maxIntervalStep = 200
)

// ErrAutomationRecursion automation 会话内禁止再建/改/删任务（防自我复制兜底）
var ErrAutomationRecursion = errors.New("automation sessions must not manage automations")

type (
	// Schedule 结构化调度规则（权威）：每 N 单位一律走 interval 锚点推导，
	// cron 只用于用户点名的墙钟时刻——规避 */N 提前触发与字段步长上限
	Schedule struct {
		Kind         string `json:"kind"`                   // delay | interval | cron
		DelayMinutes int    `json:"delayMinutes,omitempty"` // delay：触发延迟（分钟）
		Cron         string `json:"cron,omitempty"`         // cron：5 字段表达式
		IntervalUnit string `json:"intervalUnit,omitempty"` // interval：minute | hour | day | week | month
		Interval     int    `json:"interval,omitempty"`     // interval：步长
		At           string `json:"at,omitempty"`           // interval 日/周/月级当日时刻 "HH:MM"
		Weekdays     []int  `json:"weekdays,omitempty"`     // interval=week：星期 0-6（周日=0），空=每天
		MonthDays    []int  `json:"monthDays,omitempty"`    // interval=month：日期 1-31，空=锚点当日
		AnchorAt     int64  `json:"anchorAt"`               // 创建/修改锚点（Unix 秒），interval 推导起点
	}

	// Retry 退避状态：认领条件只看 RetryAt（防 tick 击穿退避）
	Retry struct {
		Attempts  int    `json:"attempts"`
		RetryAt   int64  `json:"retryAt,omitempty"`
		LastError string `json:"lastError,omitempty"`
	}

	// Automation 定时任务：定义与调度状态合一
	Automation struct {
		ID                string   `json:"id"`
		Title             string   `json:"title"`           // 展示标题，保留用户原话中的时间短语
		Prompt            string   `json:"prompt"`          // 每次触发投递的自包含指令
		Cwd               string   `json:"cwd"`             // 执行工作目录
		Model             string   `json:"model,omitempty"` // provider/id，空=跟随默认模型
		Level             string   `json:"level,omitempty"` // readonly | write | full，空=full
		Schedule          Schedule `json:"schedule"`
		Recurring         bool     `json:"recurring"`         // delay 恒 false，interval/cron 恒 true
		MaxRuns           int      `json:"maxRuns,omitempty"` // 有限次循环的总次数上限，0=无限
		Enabled           bool     `json:"enabled"`           // 入调度开关（由 status 推导同步）
		Status            string   `json:"status"`            // active | paused | completed | failed
		NextRunAt         int64    `json:"nextRunAt,omitempty"`
		LastRunAt         int64    `json:"lastRunAt,omitempty"`
		ScheduledRunCount int      `json:"scheduledRunCount"` // 定时触发计数（manual 不计）
		RunCountTotal     int      `json:"runCountTotal"`     // 含 manual 的展示总数
		Running           bool     `json:"running,omitempty"` // 单飞认领锁
		ClaimedAt         int64    `json:"claimedAt,omitempty"`
		OwnerPID          int      `json:"ownerPID,omitempty"`
		Retry             *Retry   `json:"retry,omitempty"`
		SourceSessionID   string   `json:"sourceSessionId,omitempty"` // 会话内创建时记录来源（回注用）
		CreatedAt         int64    `json:"createdAt"`
		UpdatedAt         int64    `json:"updatedAt"`
	}

	// Run 运行台账行（append-only，同 ID 多行末行生效）
	Run struct {
		ID           string `json:"id"` // {automationID}:{scheduledAt} 或 {automationID}:manual:{hex}
		AutomationID string `json:"automationId"`
		Trigger      string `json:"trigger"`               // schedule | manual
		ScheduledAt  int64  `json:"scheduledAt,omitempty"` // 认领时的 nextRunAt（重试复用 runId 依据）
		StartedAt    int64  `json:"startedAt"`
		FinishedAt   int64  `json:"finishedAt,omitempty"`
		Outcome      string `json:"outcome"` // running | succeeded | failed | stopped | skipped
		SessionID    string `json:"sessionId,omitempty"`
		Summary      string `json:"summary,omitempty"` // 末条 assistant 文本截 4KB
		Error        string `json:"error,omitempty"`
		OwnerPID     int    `json:"ownerPid,omitempty"`
	}

	// Config 调度与执行配置（settings.json automation 节，零值取默认）
	Config struct {
		MaxConcurrentRuns    int
		RunTimeoutMinutes    int
		HistoryRetentionDays int
		MaxAutomations       int
	}

	// Store automations.json 存储：所有读改写都在 flock 内进行
	Store struct {
		path string
		cfg  Config
		now  func() time.Time
	}
)

// DefaultConfig 默认配置：并发 1、单次 60 分钟、台账留 30 天、上限 50 条
func DefaultConfig() Config {
	return Config{
		MaxConcurrentRuns:    1,
		RunTimeoutMinutes:    60,
		HistoryRetentionDays: 30,
		MaxAutomations:       50,
	}
}

// WithDefaults 补齐零值字段
func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	if c.MaxConcurrentRuns <= 0 {
		c.MaxConcurrentRuns = d.MaxConcurrentRuns
	}
	if c.RunTimeoutMinutes <= 0 {
		c.RunTimeoutMinutes = d.RunTimeoutMinutes
	}
	if c.HistoryRetentionDays <= 0 {
		c.HistoryRetentionDays = d.HistoryRetentionDays
	}
	if c.MaxAutomations <= 0 {
		c.MaxAutomations = d.MaxAutomations
	}
	return c
}

// ConfigFromSettings settings.json automation 节转 Config（nil 取默认）
func ConfigFromSettings(s *coding.AutomationSettings) Config {
	c := Config{}
	if s != nil {
		c = Config{
			MaxConcurrentRuns:    s.MaxConcurrentRuns,
			RunTimeoutMinutes:    s.RunTimeoutMinutes,
			HistoryRetentionDays: s.HistoryRetentionDays,
			MaxAutomations:       s.MaxAutomations,
		}
	}
	return c.WithDefaults()
}

// NewStore 构建存储（文件惰性创建：无任务不落盘）
func NewStore(path string, cfg Config) *Store {
	return &Store{path: path, cfg: cfg.WithDefaults(), now: time.Now}
}

// Path 返回定义文件路径
func (s *Store) Path() string { return s.path }

// List 快照读全部任务（rename 原子写保证读者看到完整文件，无需锁）
func (s *Store) List() ([]*Automation, error) {
	return s.load()
}

// Get 按 id 取单条
func (s *Store) Get(id string) (*Automation, error) {
	list, err := s.load()
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, fmt.Errorf("automation not found: %s", id)
}

// Create 校验并落盘新任务：origin 为调用来源会话 Kind（automation 会话拒绝）
func (s *Store) Create(origin string, a *Automation) error {
	if origin == coding.SessionKindAutomation {
		return ErrAutomationRecursion
	}
	now := s.now()
	return s.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		if err := Validate(a); err != nil {
			return nil, false, err
		}
		if len(list) >= s.cfg.MaxAutomations {
			return nil, false, fmt.Errorf("automation limit reached (%d)", s.cfg.MaxAutomations)
		}
		a.ID = newAutomationID()
		a.Recurring = a.Schedule.Kind != ScheduleDelay
		a.Status = StatusActive
		a.Enabled = true
		a.Running = false
		a.Retry = nil
		a.CreatedAt = now.Unix()
		a.UpdatedAt = now.Unix()
		next, err := computeNextRun(a.Schedule, now)
		if err != nil {
			return nil, false, err
		}
		if next.IsZero() {
			return nil, false, errors.New("schedule has no upcoming run")
		}
		a.NextRunAt = next.Unix()
		return append(list, a), true, nil
	})
}

// Update 应用可变字段改动：调度变更即重锚（AnchorAt=now）并重算 nextRunAt、清退避
func (s *Store) Update(origin, id string, p *Patch) (*Automation, error) {
	if origin == coding.SessionKindAutomation {
		return nil, ErrAutomationRecursion
	}
	now := s.now()
	var out *Automation
	err := s.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		cur := findByID(list, id)
		if cur == nil {
			return nil, false, fmt.Errorf("automation not found: %s", id)
		}
		if cur.Running {
			return nil, false, errors.New("automation is running; update after it finishes")
		}
		if p.Title != nil {
			cur.Title = *p.Title
		}
		if p.Prompt != nil {
			cur.Prompt = *p.Prompt
		}
		if p.Cwd != nil {
			cur.Cwd = *p.Cwd
		}
		if p.Model != nil {
			cur.Model = *p.Model
		}
		if p.Level != nil {
			cur.Level = *p.Level
		}
		if p.MaxRuns != nil {
			cur.MaxRuns = *p.MaxRuns
		}
		if p.Schedule != nil {
			cur.Schedule = *p.Schedule
		}
		if err := Validate(cur); err != nil {
			return nil, false, err
		}
		cur.Recurring = cur.Schedule.Kind != ScheduleDelay
		// maxRuns 只随显式传值变更（含显式 0 = 转无限）：PATCH 语义是"出现的
		// 字段才改"，不做隐式清零
		if p.Schedule != nil {
			cur.Schedule.AnchorAt = now.Unix()
			next, err := computeNextRun(cur.Schedule, now)
			if err != nil {
				return nil, false, err
			}
			if next.IsZero() {
				return nil, false, errors.New("schedule has no upcoming run")
			}
			cur.NextRunAt = next.Unix()
			cur.Retry = nil
		}
		if cur.Status == StatusCompleted || cur.Status == StatusFailed {
			// 终态复活：调度已换（上面重算过 nextRunAt）或循环任务可从当前
			// 时刻重算周期；一次性 delay 已过期（无 schedule 改动）则保持终态
			// ——否则复活成 nextRunAt=0 的僵尸 active，永远不被调度
			if cur.NextRunAt > 0 || cur.Recurring {
				next := time.Unix(cur.NextRunAt, 0)
				if cur.NextRunAt == 0 {
					next, _ = computeNextRun(cur.Schedule, now)
				}
				if !next.IsZero() {
					cur.Status = StatusActive
					cur.NextRunAt = next.Unix()
					cur.Retry = nil
				}
			}
		}
		cur.Enabled = cur.Status == StatusActive
		cur.UpdatedAt = now.Unix()
		out = cloneAutomation(cur)
		return list, true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete 删除任务（origin 校验同上）
func (s *Store) Delete(origin, id string) error {
	if origin == coding.SessionKindAutomation {
		return ErrAutomationRecursion
	}
	return s.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		keep := list[:0]
		found := false
		for _, a := range list {
			if a.ID == id {
				found = true
				continue
			}
			keep = append(keep, a)
		}
		if !found {
			return nil, false, fmt.Errorf("automation not found: %s", id)
		}
		return keep, true, nil
	})
}

// SetEnabled 暂停/恢复：恢复仅 paused 可（重算 nextRunAt、清退避）
func (s *Store) SetEnabled(origin, id string, enabled bool) (*Automation, error) {
	if origin == coding.SessionKindAutomation {
		return nil, ErrAutomationRecursion
	}
	now := s.now()
	var out *Automation
	err := s.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		cur := findByID(list, id)
		if cur == nil {
			return nil, false, fmt.Errorf("automation not found: %s", id)
		}
		if enabled {
			if cur.Status != StatusPaused {
				return nil, false, fmt.Errorf("only paused automations can be enabled (status=%s)", cur.Status)
			}
			next, err := computeNextRun(cur.Schedule, now)
			if err != nil {
				return nil, false, err
			}
			if next.IsZero() {
				return nil, false, errors.New("schedule has no upcoming run")
			}
			cur.Status = StatusActive
			cur.Enabled = true
			cur.NextRunAt = next.Unix()
			cur.Retry = nil
		} else {
			if cur.Status != StatusActive {
				return nil, false, fmt.Errorf("only active automations can be disabled (status=%s)", cur.Status)
			}
			cur.Status = StatusPaused
			cur.Enabled = false
		}
		cur.UpdatedAt = now.Unix()
		out = cloneAutomation(cur)
		return list, true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Patch 可变字段改动（指针区分"未传"与"显式清除"）
type Patch struct {
	Title    *string
	Prompt   *string
	Cwd      *string
	Model    *string
	Level    *string
	MaxRuns  *int
	Schedule *Schedule
}

// Mutate flock 保护的读-改-写：fn 拿到最新列表并返回（新列表, 是否变更, 错误）；
// 变更时 tmp+rename 原子写回（0600）。外部写入（CLI/手工编辑/别的进程）天然
// 生效——每次都在锁内重读文件。
func (s *Store) Mutate(fn func(list []*Automation) ([]*Automation, bool, error)) error {
	lock, err := lockFile(s.path + ".lock")
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	list, err := s.load()
	if err != nil {
		return err
	}
	next, changed, err := fn(list)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return s.save(next)
}

// load 读定义文件（缺失视为空列表，与惰性创建语义一致）
func (s *Store) load() ([]*Automation, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var list []*Automation
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// save 原子写回（tmp+rename，0600）
func (s *Store) save(list []*Automation) error {
	if list == nil {
		list = []*Automation{}
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Validate store 层双重校验（工具 schema 的互斥规则在此重复实现，防旧客户端
// /内部调用绕过工具层）
func Validate(a *Automation) error {
	if a == nil {
		return errors.New("automation required")
	}
	if a.Title == "" {
		return errors.New("title required")
	}
	if a.Prompt == "" {
		return errors.New("prompt required")
	}
	if a.Cwd == "" {
		return errors.New("cwd required")
	}
	// cwd 创建时即校验：绝对路径且存在为目录。相对路径/~ 展开失败要到首次
	// 运行（可能数天后）才报错，期间任务反复失败空转
	if !filepath.IsAbs(a.Cwd) {
		return fmt.Errorf("cwd must be an absolute path: %s", a.Cwd)
	}
	if info, err := os.Stat(a.Cwd); err != nil || !info.IsDir() {
		return fmt.Errorf("cwd not a directory: %s", a.Cwd)
	}
	switch a.Level {
	case "", LevelReadonly, LevelWrite, LevelFull:
	default:
		return fmt.Errorf("invalid level %q (want readonly|write|full)", a.Level)
	}
	sc := a.Schedule
	switch sc.Kind {
	case ScheduleDelay:
		if sc.DelayMinutes < 1 || sc.DelayMinutes > maxDelayMinutes {
			return fmt.Errorf("delayMinutes out of range (1-%d)", maxDelayMinutes)
		}
		if sc.Cron != "" || sc.IntervalUnit != "" || sc.Interval != 0 || len(sc.Weekdays) > 0 || len(sc.MonthDays) > 0 {
			return errors.New("delay schedule must not carry cron/interval fields")
		}
		if a.MaxRuns > 0 {
			return errors.New("maxRuns applies to recurring schedules only")
		}
	case ScheduleInterval:
		switch sc.IntervalUnit {
		case "minute", "hour":
			if len(sc.Weekdays) > 0 || len(sc.MonthDays) > 0 || sc.At != "" {
				return fmt.Errorf("intervalUnit %s must not carry at/weekdays/monthDays", sc.IntervalUnit)
			}
		case "day":
			if len(sc.Weekdays) > 0 || len(sc.MonthDays) > 0 {
				return errors.New("intervalUnit day must not carry weekdays/monthDays")
			}
		case "week":
			if len(sc.MonthDays) > 0 {
				return errors.New("intervalUnit week must not carry monthDays")
			}
		case "month":
			if len(sc.Weekdays) > 0 {
				return errors.New("intervalUnit month must not carry weekdays")
			}
		default:
			return errors.New("intervalUnit required: minute|hour|day|week|month")
		}
		if sc.Interval < 1 || sc.Interval > maxIntervalStep {
			return fmt.Errorf("interval out of range (1-%d)", maxIntervalStep)
		}
		if sc.DelayMinutes != 0 || sc.Cron != "" {
			return errors.New("interval schedule must not carry delay/cron fields")
		}
		for _, w := range sc.Weekdays {
			if w < 0 || w > 6 {
				return errors.New("weekdays out of range (0-6, Sunday=0)")
			}
		}
		for _, d := range sc.MonthDays {
			if d < 1 || d > 31 {
				return errors.New("monthDays out of range (1-31)")
			}
		}
		if sc.At != "" && !validAt(sc.At) {
			return errors.New("at must be HH:MM (00:00-23:59)")
		}
	case ScheduleCron:
		if _, err := parseCron(sc.Cron); err != nil {
			return err
		}
		if sc.DelayMinutes != 0 || sc.IntervalUnit != "" || sc.Interval != 0 || len(sc.Weekdays) > 0 || len(sc.MonthDays) > 0 {
			return errors.New("cron schedule must not carry delay/interval fields")
		}
	default:
		return errors.New("schedule kind required: delay|interval|cron")
	}
	if a.MaxRuns > 0 && sc.Kind == ScheduleDelay {
		return errors.New("maxRuns applies to recurring schedules only")
	}
	return nil
}

// validAt 校验 "HH:MM"
func validAt(at string) bool {
	if len(at) != 5 || at[2] != ':' {
		return false
	}
	h, m := at[:2], at[3:]
	for _, s := range []string{h, m} {
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return false
			}
		}
	}
	hh, mm := atoi2(h), atoi2(m)
	return hh >= 0 && hh <= 23 && mm >= 0 && mm <= 59
}

// atoi2 两位数字解析（非数字返回 -1）
func atoi2(s string) int {
	v := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return -1
		}
		v = v*10 + int(s[i]-'0')
	}
	return v
}

// lockFile 打开锁文件并独占 flock（跨进程读改写协调）
func lockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// unlockFile 解锁并关闭
func unlockFile(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// findByID 列表内按 id 查找
func findByID(list []*Automation, id string) *Automation {
	for _, a := range list {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// cloneAutomation 深拷贝（调度/退避字段独立，认领快照用）
func cloneAutomation(a *Automation) *Automation {
	out := *a
	if a.Retry != nil {
		r := *a.Retry
		out.Retry = &r
	}
	return &out
}

// newAutomationID auto-<8 字节 hex>，同 session id 风格
func newAutomationID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "auto-" + hex.EncodeToString(b[:])
}

// newRunSuffix 手动触发的 run id 后缀（4 字节 hex）
func newRunSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
