package automation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doge/internal/coding"
)

/* ---------------- runner ---------------- */

func TestRunnerExecuteSuccess(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleDelay))
	done := make(chan struct{})
	go func() {
		env.runner.Execute(context.Background(), a, "schedule", a.NextRunAt)
		close(done)
	}()
	if !poll(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("Execute did not finish in time")
	}
	// 台账：running 行 + succeeded 终态行，摘要来自末条 assistant
	runs, err := env.runner.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded {
		t.Fatalf("runs = %+v", runs)
	}
	if runs[0].Summary != theFakeText.get() {
		t.Fatalf("summary = %q", runs[0].Summary)
	}
	if runs[0].SessionID == "" {
		t.Fatal("run row missing session id")
	}
	// 任务结算：一次性 delay 完成 + 计数
	got, _ := env.store.Get(a.ID)
	if got.Status != StatusCompleted || got.Enabled || got.ScheduledRunCount != 1 || got.Running {
		t.Fatalf("settled = %+v", got)
	}
	if got.NextRunAt != 0 {
		t.Fatalf("one-shot nextRunAt should be cleared: %v", time.Unix(got.NextRunAt, 0))
	}
	// 执行会话落盘且 Kind=automation
	p := env.sess.SessionManager.FindSessionFile(runs[0].SessionID)
	if p == "" {
		t.Fatalf("run session file not found for id %s", runs[0].SessionID)
	}
	sess, err := env.sess.SessionManager.LoadSession(p)
	if err != nil {
		t.Fatalf("load run session: %v", err)
	}
	if sess.Header.Kind != coding.SessionKindAutomation {
		t.Fatalf("run session kind = %q", sess.Header.Kind)
	}
}

func TestRunnerExecuteSessionKind(t *testing.T) {
	env := newTestEnv(t)
	fork, err := env.sess.ForkAutomation(env.dir, nil, coding.RemoteFull)
	if err != nil {
		t.Fatalf("ForkAutomation: %v", err)
	}
	if fork.CurrentSession().Header.Kind != coding.SessionKindAutomation {
		t.Fatalf("kind = %q", fork.CurrentSession().Header.Kind)
	}
}

func TestRunnerPermanentFailureBadCwd(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	a.Cwd = filepath.Join(env.dir, "no-such-dir")
	_ = env.store.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		list[0].Cwd = a.Cwd
		return list, true, nil
	})
	env.runner.Execute(context.Background(), a, "schedule", a.NextRunAt)
	got, _ := env.store.Get(a.ID)
	if got.Status != StatusFailed || got.Enabled || got.Retry != nil {
		t.Fatalf("permanent failure should be terminal: %+v", got)
	}
	runs, _ := env.runner.Runs()
	if len(runs) != 1 || runs[0].Outcome != OutcomeFailed || !strings.Contains(runs[0].Error, "cwd") {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestRunnerTransientFailureBackoff(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	theFakeMode.set(true)
	defer theFakeMode.set(false)
	env.runner.Execute(context.Background(), a, "schedule", a.NextRunAt)
	got, _ := env.store.Get(a.ID)
	if got.Status != StatusActive || got.Retry == nil || got.Retry.Attempts != 1 {
		t.Fatalf("transient failure should backoff: %+v", got)
	}
	if got.Retry.RetryAt != got.Retry.RetryAt-1 && got.Retry.RetryAt <= env.clock.Now().Unix() {
		t.Fatalf("retryAt should be future: %v", time.Unix(got.Retry.RetryAt, 0))
	}
	if got.NextRunAt != a.NextRunAt {
		t.Fatalf("nextRunAt must stay put during backoff: %v", time.Unix(got.NextRunAt, 0))
	}
	if got.ScheduledRunCount != 0 {
		t.Fatalf("failed run should not count: %d", got.ScheduledRunCount)
	}
}

func TestRunnerManualRunPaces(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	before := a.NextRunAt
	if _, err := env.runner.ManualRun(a); err != nil {
		t.Fatalf("ManualRun: %v", err)
	}
	if !poll(t, func() bool {
		got, _ := env.store.Get(a.ID)
		return got.RunCountTotal == 1 && !got.Running
	}) {
		got, _ := env.store.Get(a.ID)
		t.Fatalf("manual run not settled: %+v", got)
	}
	got, _ := env.store.Get(a.ID)
	if got.NextRunAt != before {
		t.Fatalf("manual run must not move nextRunAt: %v vs %v", time.Unix(got.NextRunAt, 0), time.Unix(before, 0))
	}
	if got.ScheduledRunCount != 0 {
		t.Fatalf("manual run must not count scheduled: %d", got.ScheduledRunCount)
	}
	runs, _ := env.runner.Runs()
	if len(runs) != 1 || runs[0].Trigger != "manual" || !strings.Contains(runs[0].ID, ":manual:") {
		t.Fatalf("manual runs = %+v", runs)
	}
	// 运行中的任务拒绝再次手动触发
	_ = env.store.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		list[0].Running = true
		return list, true, nil
	})
	if _, err := env.runner.ManualRun(a); err == nil {
		t.Fatal("manual run while running should be rejected")
	}
}

func TestRunnerNotifyInjectsFollowUp(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleDelay))
	env.runner.Execute(context.Background(), a, "schedule", a.NextRunAt)
	// 回注是 FollowUp + Continue：轮询共享会话转录出现结果卡片
	if !poll(t, func() bool {
		for _, m := range env.sess.CurrentMessages() {
			if m.Role == "user" && strings.Contains(m.Text(), "[定时任务「每10分钟喝水提醒」完成]") {
				return true
			}
		}
		return false
	}) {
		t.Fatal("follow-up card not injected into source session")
	}
}

func TestRunnerReconcileDeadOwner(t *testing.T) {
	env := newTestEnv(t)
	// 属主进程已死的 running 行 → stopped；存活属主（本进程）不动
	env.runner.AppendRun(&Run{ID: "auto-x:1", AutomationID: "auto-x", Outcome: OutcomeRunning, StartedAt: 1, OwnerPID: 99999999})
	env.runner.AppendRun(&Run{ID: "auto-y:2", AutomationID: "auto-y", Outcome: OutcomeRunning, StartedAt: 1, OwnerPID: selfPID()})
	env.runner.Reconcile()
	runs, _ := env.runner.Runs()
	byID := map[string]*Run{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	if byID["auto-x:1"] == nil || byID["auto-x:1"].Outcome != OutcomeStopped {
		t.Fatalf("dead-owner run = %+v", byID["auto-x:1"])
	}
	if byID["auto-y:2"] == nil || byID["auto-y:2"].Outcome != OutcomeRunning {
		t.Fatalf("live-owner run should stay: %+v", byID["auto-y:2"])
	}
}

func TestRunnerSweepOldRows(t *testing.T) {
	env := newTestEnv(t)
	old := env.clock.Now().AddDate(0, 0, -env.runner.cfg.HistoryRetentionDays-1).Unix()
	env.runner.AppendRun(&Run{ID: "old:1", AutomationID: "a", Outcome: OutcomeSucceeded, StartedAt: old, FinishedAt: old})
	env.runner.AppendRun(&Run{ID: "new:1", AutomationID: "a", Outcome: OutcomeSucceeded, StartedAt: env.clock.Now().Unix(), FinishedAt: env.clock.Now().Unix()})
	if err := env.runner.SweepRuns(); err != nil {
		t.Fatalf("SweepRuns: %v", err)
	}
	runs, _ := env.runner.Runs()
	if len(runs) != 1 || runs[0].ID != "new:1" {
		t.Fatalf("swept runs = %+v", runs)
	}
}

/* ---------------- scheduler ---------------- */

func TestSchedulerClaimDue(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	// 未到点：不认领
	env.clock.Advance(5 * time.Minute)
	env.sched.Tick()
	got, _ := env.store.Get(a.ID)
	if got.Running {
		t.Fatal("claimed before due")
	}
	// 到点：认领并异步执行；跑完结算下一周期
	env.clock.Advance(6 * time.Minute)
	env.sched.Tick()
	if !poll(t, func() bool {
		got, _ := env.store.Get(a.ID)
		return got.ScheduledRunCount == 1 && !got.Running
	}) {
		got, _ := env.store.Get(a.ID)
		t.Fatalf("scheduled run not settled: %+v", got)
	}
	got, _ = env.store.Get(a.ID)
	if got.NextRunAt != a.NextRunAt+600 {
		t.Fatalf("nextRunAt = %v, want original+10min (anchor grid)", time.Unix(got.NextRunAt, 0))
	}
}

func TestSchedulerMisfireSkips(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	// 错过超过 5 分钟窗口：记 skipped + 前推到下一个未来触发点
	env.clock.Advance(30 * time.Minute)
	env.sched.Tick()
	got, _ := env.store.Get(a.ID)
	if got.Running || got.ScheduledRunCount != 0 {
		t.Fatalf("misfired task must not run: %+v", got)
	}
	if got.NextRunAt <= env.clock.Now().Unix() {
		t.Fatalf("misfired nextRunAt should advance: %v", time.Unix(got.NextRunAt, 0))
	}
	runs, _ := env.runner.Runs()
	if len(runs) != 1 || runs[0].Outcome != OutcomeSkipped {
		t.Fatalf("skipped row = %+v", runs)
	}
}

func TestSchedulerMisfireOneShotTerminal(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleDelay))
	env.clock.Advance(2 * time.Hour)
	env.sched.Tick()
	got, _ := env.store.Get(a.ID)
	// 一次性错过即终态：绝不按公式重算复活
	if got.Status != StatusCompleted || got.Enabled {
		t.Fatalf("missed one-shot = %+v", got)
	}
	runs, _ := env.runner.Runs()
	if len(runs) != 1 || runs[0].Outcome != OutcomeSkipped {
		t.Fatalf("skipped row = %+v", runs)
	}
}

func TestSchedulerZombieRecovery(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	// 另一进程认领后崩溃：running + 死属主 + 过期租约；nextRunAt 保持未来
	// （纯回收场景，不与认领/misfire 交叠）
	_ = env.store.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		list[0].Running = true
		list[0].OwnerPID = 99999999
		list[0].ClaimedAt = env.clock.Now().Add(-20 * time.Minute).Unix()
		return list, true, nil
	})
	env.sched.Tick()
	got := mustGet(t, env, a.ID)
	if got.Running {
		t.Fatal("zombie claim not recovered")
	}
	if got.NextRunAt != a.NextRunAt {
		t.Fatalf("future nextRunAt must stay: %v", time.Unix(got.NextRunAt, 0))
	}
	// 本进程的认领不被回收（租约心跳路径）
	_ = env.store.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		list[0].Running = true
		list[0].OwnerPID = pidSelf
		list[0].ClaimedAt = env.clock.Now().Unix()
		return list, true, nil
	})
	env.sched.Tick()
	if got := mustGet(t, env, a.ID); !got.Running {
		t.Fatal("own live claim must not be recovered")
	}
}

func TestSchedulerRetryClaimOnRetryAt(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	env.clock.Advance(10 * time.Minute)
	// 首跑失败进入退避
	theFakeMode.set(true)
	env.sched.Tick()
	if !poll(t, func() bool {
		got, _ := env.store.Get(a.ID)
		return got.Retry != nil && got.Retry.Attempts == 1 && !got.Running
	}) {
		t.Fatalf("backoff not entered: %+v", mustGet(t, env, a.ID))
	}
	// 退避中（retryAt 未到）：不认领
	env.clock.Advance(10 * time.Second)
	env.sched.Tick()
	if mustGet(t, env, a.ID).Running {
		t.Fatal("claimed during backoff window")
	}
	// retryAt 到点 + 转为成功：重试成功结算，runId 复用同一 scheduledAt
	theFakeMode.set(false)
	env.clock.Advance(25 * time.Second)
	env.sched.Tick()
	if !poll(t, func() bool {
		got, _ := env.store.Get(a.ID)
		return got.ScheduledRunCount == 1 && got.Retry == nil && !got.Running
	}) {
		t.Fatalf("retry not settled: %+v", mustGet(t, env, a.ID))
	}
	runs, _ := env.runner.Runs()
	for _, r := range runs {
		if r.ID != scheduledRunID(a.ID, a.NextRunAt) {
			t.Fatalf("retry run id %q, want reuse %q", r.ID, scheduledRunID(a.ID, a.NextRunAt))
		}
	}
}

func TestSchedulerMaxRunsCompletes(t *testing.T) {
	env := newTestEnv(t)
	a := makeAutomation(env, ScheduleInterval)
	a.MaxRuns = 1
	a = mustCreate(t, env, a)
	env.clock.Advance(10 * time.Minute)
	env.sched.Tick()
	if !poll(t, func() bool {
		got, _ := env.store.Get(a.ID)
		return got.Status == StatusCompleted && got.ScheduledRunCount == 1
	}) {
		t.Fatalf("maxRuns=1 not completed: %+v", mustGet(t, env, a.ID))
	}
	// 终态任务不再认领
	env.clock.Advance(30 * time.Minute)
	env.sched.Tick()
	if mustGet(t, env, a.ID).Running {
		t.Fatal("completed task must not be claimed")
	}
}

func TestSchedulerConcurrencyCapDefers(t *testing.T) {
	// 并发上限 1：占用槽位时到期任务顺延（不记 skipped，下轮再认领）
	env := newTestEnv(t)
	if !env.runner.tryAcquire() {
		t.Fatal("acquire failed on empty runner")
	}
	defer env.runner.release()
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	env.clock.Advance(10 * time.Minute)
	env.sched.Tick()
	got, _ := env.store.Get(a.ID)
	if got.Running {
		t.Fatal("claimed while concurrency cap held")
	}
	runs, _ := env.runner.Runs()
	if len(runs) != 0 {
		t.Fatalf("deferral must not record skipped: %+v", runs)
	}
	// 释放后下一 tick 正常认领
	env.runner.release()
	env.sched.Tick()
	if !poll(t, func() bool {
		got, _ := env.store.Get(a.ID)
		return got.ScheduledRunCount == 1
	}) {
		t.Fatalf("not claimed after release: %+v", mustGet(t, env, a.ID))
	}
}

func TestSchedulerNoFileNoOp(t *testing.T) {
	// 定义文件不存在：tick 纯内存，零磁盘动作不 panic
	env := newTestEnv(t)
	env.sched.Tick()
	if runs, err := env.runner.Runs(); err == nil && len(runs) > 0 {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}

// mustGet 断言式回读
func mustGet(t *testing.T, env *testEnv, id string) *Automation {
	t.Helper()
	got, err := env.store.Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return got
}

// selfPID 当前进程 pid（存活属主样本）
func selfPID() int { return pidSelf }

// pidSelf 当前测试进程 pid
var pidSelf = os.Getpid()
