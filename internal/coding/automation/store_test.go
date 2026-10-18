package automation

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"doge/internal/ai"
	"doge/internal/coding"
)

// fakeMode 假流适配器的行为（并发安全：runner 在 goroutine 里读）
type fakeMode struct {
	mu   sync.Mutex
	fail bool
}

var theFakeMode = &fakeMode{}

func (f *fakeMode) set(fail bool) {
	f.mu.Lock()
	f.fail = fail
	f.mu.Unlock()
}

func (f *fakeMode) get() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail
}

// fakeReplyText 假回复正文（同样并发安全）
type fakeText struct {
	mu sync.Mutex
	s  string
}

var theFakeText = &fakeText{}

func (f *fakeText) set(s string) {
	f.mu.Lock()
	f.s = s
	f.mu.Unlock()
}

func (f *fakeText) get() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.s == "" {
		return "定时任务执行完毕"
	}
	return f.s
}

func init() {
	// 假 API 适配器：按 theFakeMode 返回完成或错误消息
	ai.RegisterAPIStream("automation-fake", func(model *ai.Model, ctx *ai.Context, opts *ai.StreamOptions) *ai.EventStream {
		s := ai.NewEventStream(4)
		go func() {
			if theFakeMode.get() {
				msg := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonError, ErrorMessage: "fake gateway 500"}
				s.Push(ai.ErrorEvent(msg))
				s.End(msg)
				return
			}
			msg := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop}
			msg.Content = []ai.ContentBlock{ai.TextBlock(theFakeText.get())}
			s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: msg})
			s.Push(ai.DoneEvent(msg))
			s.End(msg)
		}()
		return s
	})
}

// newTestSession 构建绑定假模型的共享会话（fork 模板）
func newTestSession(t *testing.T) (*coding.AgentSession, string) {
	t.Helper()
	dir := t.TempDir()
	manager := coding.NewSessionManager(filepath.Join(dir, "sessions"))
	auth := coding.LoadAuthStore(filepath.Join(dir, "auth.json"))
	runtime := coding.NewModelRuntime(auth)
	runtime.Models.AddProvider(&ai.Provider{ID: "fake", Name: "Fake", RequiresKey: false})
	runtime.Models.AddModel(&ai.Model{ID: "m", API: "automation-fake", Provider: "fake", ContextWindow: 100000, MaxTokens: 4096})
	model, _, err := runtime.ResolveModel("fake/m")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := coding.LoadSettings(filepath.Join(dir, "s.json"), filepath.Join(dir, "p.settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := coding.NewAgentSession(coding.AgentSessionOptions{
		Cwd: dir, Model: model, SessionManager: manager, Settings: settings, ModelRuntime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess, dir
}

// newTestEnv 构建共享会话 + store + runner 与可控时钟
type testEnv struct {
	sess   *coding.AgentSession
	store  *Store
	runner *Runner
	sched  *Scheduler
	clock  *fakeClock
	dir    string
}

// fakeClock 可控时钟（scheduler/store/runner 三处共用）
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	t.Setenv("DOGE_CODING_AGENT_DIR", t.TempDir())
	sess, dir := newTestSession(t)
	clock := newFakeClock(time.Date(2026, 10, 16, 12, 0, 0, 0, time.Local))
	store := NewStore(filepath.Join(dir, "automations.json"), DefaultConfig())
	store.now = clock.Now
	runner := NewRunner(store, sess, filepath.Join(dir, "automations"), DefaultConfig())
	runner.now = clock.Now
	sched := NewScheduler(store, runner)
	sched.now = clock.Now
	return &testEnv{sess: sess, store: store, runner: runner, sched: sched, clock: clock, dir: dir}
}

// makeAutomation 测试用任务（interval 分钟级，跑得快）
func makeAutomation(env *testEnv, kind string) *Automation {
	sc := Schedule{Kind: ScheduleInterval, IntervalUnit: "minute", Interval: 10, AnchorAt: env.clock.Now().Unix()}
	if kind == ScheduleDelay {
		sc = Schedule{Kind: ScheduleDelay, DelayMinutes: 1, AnchorAt: env.clock.Now().Unix()}
	}
	if kind == ScheduleCron {
		sc = Schedule{Kind: ScheduleCron, Cron: "*/10 * * * *", AnchorAt: env.clock.Now().Unix()}
	}
	return &Automation{
		Title: "每10分钟喝水提醒", Prompt: "输出 ok", Cwd: env.dir,
		Schedule: sc, SourceSessionID: env.sess.CurrentSession().Header.ID,
	}
}

// mustCreate 建任务并回读（拿落盘后的 id/nextRunAt）
func mustCreate(t *testing.T, env *testEnv, a *Automation) *Automation {
	t.Helper()
	if err := env.store.Create("", a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := env.store.Get(a.ID)
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	return got
}

// poll 等待条件成立（默认 3s），失败给最终状态
func poll(t *testing.T, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fn()
}

/* ---------------- store ---------------- */

func TestStoreCreateAndValidate(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	if a.ID == "" || a.Status != StatusActive || !a.Enabled || !a.Recurring {
		t.Fatalf("created = %+v", a)
	}
	if a.NextRunAt != env.clock.Now().Add(10*time.Minute).Unix() {
		t.Fatalf("nextRunAt = %v, want +10min", time.Unix(a.NextRunAt, 0))
	}
}

func TestStoreValidationMatrix(t *testing.T) {
	env := newTestEnv(t)
	base := makeAutomation(env, ScheduleDelay)
	cases := []struct {
		name string
		mut  func(a *Automation)
	}{
		{"no title", func(a *Automation) { a.Title = "" }},
		{"no prompt", func(a *Automation) { a.Prompt = "" }},
		{"no cwd", func(a *Automation) { a.Cwd = "" }},
		{"bad level", func(a *Automation) { a.Level = "root" }},
		{"delay with cron", func(a *Automation) { a.Schedule.Cron = "* * * * *" }},
		{"delay out of range", func(a *Automation) { a.Schedule.DelayMinutes = maxDelayMinutes + 1 }},
		{"delay with maxRuns", func(a *Automation) { a.MaxRuns = 3 }},
		{"interval no unit", func(a *Automation) {
			a.Schedule = Schedule{Kind: ScheduleInterval, Interval: 10, AnchorAt: base.Schedule.AnchorAt}
		}},
		{"interval too big", func(a *Automation) {
			a.Schedule = Schedule{Kind: ScheduleInterval, IntervalUnit: "minute", Interval: maxIntervalStep + 1, AnchorAt: base.Schedule.AnchorAt}
		}},
		{"interval with delay", func(a *Automation) {
			a.Schedule = Schedule{Kind: ScheduleInterval, IntervalUnit: "minute", Interval: 5, DelayMinutes: 5, AnchorAt: base.Schedule.AnchorAt}
		}},
		{"minute unit with weekdays", func(a *Automation) {
			a.Schedule = Schedule{Kind: ScheduleInterval, IntervalUnit: "minute", Interval: 5, Weekdays: []int{1}, AnchorAt: base.Schedule.AnchorAt}
		}},
		{"week with monthdays", func(a *Automation) {
			a.Schedule = Schedule{Kind: ScheduleInterval, IntervalUnit: "week", Interval: 1, MonthDays: []int{1}, AnchorAt: base.Schedule.AnchorAt}
		}},
		{"bad weekday", func(a *Automation) {
			a.Schedule = Schedule{Kind: ScheduleInterval, IntervalUnit: "week", Interval: 1, Weekdays: []int{9}, AnchorAt: base.Schedule.AnchorAt}
		}},
		{"bad monthday", func(a *Automation) {
			a.Schedule = Schedule{Kind: ScheduleInterval, IntervalUnit: "month", Interval: 1, MonthDays: []int{0}, AnchorAt: base.Schedule.AnchorAt}
		}},
		{"bad at", func(a *Automation) {
			a.Schedule = Schedule{Kind: ScheduleInterval, IntervalUnit: "day", Interval: 1, At: "9:00", AnchorAt: base.Schedule.AnchorAt}
		}},
		{"bad cron", func(a *Automation) { a.Schedule = Schedule{Kind: ScheduleCron, Cron: "99 * * * *"} }},
		{"cron with delay", func(a *Automation) { a.Schedule = Schedule{Kind: ScheduleCron, Cron: "* * * * *", DelayMinutes: 3} }},
		{"unknown kind", func(a *Automation) { a.Schedule.Kind = "daily" }},
	}
	for _, c := range cases {
		a := makeAutomation(env, ScheduleDelay)
		c.mut(a)
		if err := Validate(a); err == nil {
			t.Errorf("%s: should fail validation", c.name)
		}
		if err := env.store.Create("", a); err == nil {
			t.Errorf("%s: store should reject", c.name)
		}
	}
}

func TestStoreMaxAutomations(t *testing.T) {
	env := newTestEnv(t)
	env.store.cfg.MaxAutomations = 2
	for i := 0; i < 2; i++ {
		if err := env.store.Create("", makeAutomation(env, ScheduleDelay)); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if err := env.store.Create("", makeAutomation(env, ScheduleDelay)); err == nil {
		t.Fatal("third create should hit the cap")
	}
}

func TestStoreExternalEditReload(t *testing.T) {
	// 外部写入（CLI/手工编辑）在锁内重读生效：Mutate 基于最新文件内容
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleDelay))
	// 直接改文件标题
	list, err := env.store.List()
	if err != nil {
		t.Fatal(err)
	}
	list[0].Title = "外部改名"
	_ = env.store.save(list)
	// Mutate 内部重读：改名应被看见并保留
	got, _ := env.store.Get(a.ID)
	if got.Title != "外部改名" {
		t.Fatalf("external edit not reloaded: %q", got.Title)
	}
}

func TestStoreUpdateReschedAndRunningGuard(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	newTitle := "每5分钟改名提醒"
	newInterval := 5
	updated, err := env.store.Update("", a.ID, &Patch{
		Title: &newTitle,
		Schedule: &Schedule{
			Kind: ScheduleInterval, IntervalUnit: "minute", Interval: newInterval,
			AnchorAt: env.clock.Now().Unix(),
		},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Title != newTitle {
		t.Fatalf("title = %q", updated.Title)
	}
	if updated.NextRunAt != env.clock.Now().Add(5*time.Minute).Unix() {
		t.Fatalf("nextRunAt not rescheduled: %v", time.Unix(updated.NextRunAt, 0))
	}
	// 运行中拒绝更新
	_ = env.store.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		list[0].Running = true
		return list, true, nil
	})
	if _, err := env.store.Update("", a.ID, &Patch{}); err == nil {
		t.Fatal("update while running should fail")
	}
}

func TestStoreSetEnabled(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	// active → paused
	paused, err := env.store.SetEnabled("", a.ID, false)
	if err != nil || paused.Status != StatusPaused || paused.Enabled {
		t.Fatalf("disable: %v %+v", err, paused)
	}
	// 非 paused 恢复被拒
	if _, err := env.store.SetEnabled("", a.ID, false); err == nil {
		t.Fatal("double disable should fail")
	}
	// paused → active：nextRunAt 重算到未来
	env.clock.Advance(30 * time.Minute)
	resumed, err := env.store.SetEnabled("", a.ID, true)
	if err != nil || resumed.Status != StatusActive {
		t.Fatalf("enable: %v", err)
	}
	if resumed.NextRunAt <= env.clock.Now().Unix() {
		t.Fatalf("resumed nextRunAt should be future: %v", time.Unix(resumed.NextRunAt, 0))
	}
}

func TestStoreRecursionGuard(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleDelay))
	const origin = "automation"
	if err := env.store.Create(origin, makeAutomation(env, ScheduleDelay)); !errors.Is(err, ErrAutomationRecursion) {
		t.Fatalf("create: %v", err)
	}
	if _, err := env.store.Update(origin, a.ID, &Patch{}); !errors.Is(err, ErrAutomationRecursion) {
		t.Fatalf("update: %v", err)
	}
	if err := env.store.Delete(origin, a.ID); !errors.Is(err, ErrAutomationRecursion) {
		t.Fatalf("delete: %v", err)
	}
	if _, err := env.store.SetEnabled(origin, a.ID, false); !errors.Is(err, ErrAutomationRecursion) {
		t.Fatalf("setEnabled: %v", err)
	}
	// 正常来源照常工作
	if err := env.store.Delete("", a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
