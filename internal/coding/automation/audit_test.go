package automation

import (
	"strings"
	"testing"
	"time"
)

// TestUpdatePreservesMaxRuns：PATCH 语义是"出现的字段才改"——只改标题不得
// 误清 maxRuns；显式传 0 才转无限。
func TestUpdatePreservesMaxRuns(t *testing.T) {
	env := newTestEnv(t)
	a := makeAutomation(env, ScheduleInterval)
	a.MaxRuns = 5
	a = mustCreate(t, env, a)
	title := "只改标题"
	updated, err := env.store.Update("", a.ID, &Patch{Title: &title})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.MaxRuns != 5 {
		t.Fatalf("maxRuns cleared by title-only patch: %d", updated.MaxRuns)
	}
	zero := 0
	updated, err = env.store.Update("", a.ID, &Patch{MaxRuns: &zero})
	if err != nil {
		t.Fatalf("Update maxRuns=0: %v", err)
	}
	if updated.MaxRuns != 0 {
		t.Fatalf("explicit zero should clear: %d", updated.MaxRuns)
	}
}

// TestUpdateTerminalRevive：终态复活必须带有效 nextRunAt——循环任务从当前
// 时刻重算周期复活；一次性 delay 已过期则保持终态（不得复活成僵尸 active）。
func TestUpdateTerminalRevive(t *testing.T) {
	env := newTestEnv(t)

	// completed 的一次性 delay：改标题保持终态
	one := mustCreate(t, env, makeAutomation(env, ScheduleDelay))
	_ = env.store.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		list[0].Status = StatusCompleted
		list[0].Enabled = false
		list[0].NextRunAt = 0
		return list, true, nil
	})
	title := "改标题"
	got, err := env.store.Update("", one.ID, &Patch{Title: &title})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Status != StatusCompleted || got.Enabled {
		t.Fatalf("expired one-shot must stay terminal: %+v", got)
	}

	// failed 的循环任务（NextRunAt 已清零）：改标题复活并重算周期
	rel := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	_ = env.store.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		list[0].Status = StatusFailed
		list[0].Enabled = false
		list[0].NextRunAt = 0
		return list, true, nil
	})
	got, err = env.store.Update("", rel.ID, &Patch{Title: &title})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Status != StatusActive || !got.Enabled {
		t.Fatalf("recurring terminal should revive: %+v", got)
	}
	if got.NextRunAt <= env.clock.Now().Unix() {
		t.Fatalf("revived nextRunAt must be future: %v", time.Unix(got.NextRunAt, 0))
	}
}

// TestManualRunIDMatchesLedger：ManualRun 返回的 runId 与台账行 ID 一致
// （API 响应不再返回假 id）。
func TestManualRunIDMatchesLedger(t *testing.T) {
	env := newTestEnv(t)
	a := mustCreate(t, env, makeAutomation(env, ScheduleInterval))
	runID, err := env.runner.ManualRun(a)
	if err != nil {
		t.Fatalf("ManualRun: %v", err)
	}
	if !strings.Contains(runID, ":manual:") {
		t.Fatalf("runID = %q, want manual form", runID)
	}
	if !poll(t, func() bool {
		got, _ := env.store.Get(a.ID)
		return got.RunCountTotal == 1 && !got.Running
	}) {
		t.Fatalf("manual run not settled: %+v", mustGet(t, env, a.ID))
	}
	runs, _ := env.runner.Runs()
	if len(runs) != 1 || runs[0].ID != runID {
		t.Fatalf("ledger id %q != returned %q (runs=%d)", runs[0].ID, runID, len(runs))
	}
}
