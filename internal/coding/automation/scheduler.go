package automation

import (
	"context"
	"os"
	"time"

	"doge/pkg/logx"
)

// 调度节奏与窗口
const (
	tickInterval  = 20 * time.Second // tick 周期（分钟级任务的触发误差 ±20s，可接受）
	claimLease    = 10 * time.Minute // 认领租约：超时未心跳视为僵尸
	misfireWindow = 5 * time.Minute  // 错过窗口：due 超过该值记 skipped 不补跑
	sweepEvery    = time.Hour        // 台账裁剪周期
)

// Scheduler 进程内调度器（每常驻进程一个）：tick 内完成终态推进、僵尸回收、
// misfire 处理与 due 认领；多进程并存靠 automations.json 的认领字段单飞。
type Scheduler struct {
	store     *Store
	runner    *Runner
	now       func() time.Time
	interval  time.Duration
	lastSweep time.Time
	ctx       context.Context
}

// NewScheduler 构建调度器（interval 测试可覆盖）
func NewScheduler(store *Store, runner *Runner) *Scheduler {
	// ctx 兜底 Background：Start 前直接驱动 Tick（测试/手动）不 panic
	return &Scheduler{store: store, runner: runner, now: time.Now, interval: tickInterval, ctx: context.Background()}
}

// Start 先对账再起 tick 循环，随 ctx 退出（挂载在 app.Assemble，照 StartSweep）
func (s *Scheduler) Start(ctx context.Context) {
	s.ctx = ctx
	s.runner.Reconcile()
	go func() {
		s.Tick()
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Tick()
			}
		}
	}()
}

// Tick 一轮调度：定义文件不存在时零动作（惰性），存在则维护→认领→按需裁剪
func (s *Scheduler) Tick() {
	if _, err := os.Stat(s.store.Path()); err != nil {
		return
	}
	now := s.now()
	s.maintain(now)
	s.claimDue(now)
	s.sweepMaybe(now)
}

// maintain 锁内维护：有限次跑满推进终态、僵尸回收、本进程租约心跳、misfire
// 前推与单飞跳过
func (s *Scheduler) maintain(now time.Time) {
	pid := os.Getpid()
	misfireCutoff := now.Add(-misfireWindow).Unix()
	_ = s.store.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		changed := false
		for _, a := range list {
			// 有限次循环跑满：completed + 出调度
			if a.Status == StatusActive && !a.Running && a.MaxRuns > 0 && a.ScheduledRunCount >= a.MaxRuns {
				a.Status = StatusCompleted
				a.Enabled = false
				a.UpdatedAt = now.Unix()
				changed = true
			}
			// 僵尸回收：租约超时或属主进程已死（另一进程崩溃留下的认领）
			if a.Running && a.OwnerPID != pid && (now.Unix()-a.ClaimedAt > int64(claimLease.Seconds()) || !pidAlive(a.OwnerPID)) {
				a.Running = false
				a.OwnerPID = 0
				a.ClaimedAt = 0
				a.UpdatedAt = now.Unix()
				changed = true
				logx.Warnf("[automation] 任务 %s 认领租约过期，已复位", a.ID)
			}
			// 本进程在跑任务的租约心跳：防长任务（超时 60min > 租约 10min）被误回收
			if a.Running && a.OwnerPID == pid && a.ClaimedAt < now.Unix() {
				a.ClaimedAt = now.Unix()
				changed = true
			}
			// 单飞跳过：上一轮未结束又到点（非 delay）——记 skipped 并推进到
			// 重叠周期；运行结束的结算同样从原 scheduledAt 推导，值一致不冲突
			if a.Running && a.Retry == nil && a.Status == StatusActive &&
				a.Schedule.Kind != ScheduleDelay && a.NextRunAt > 0 && a.NextRunAt <= now.Unix() {
				next, err := computeNextRun(a.Schedule, time.Unix(a.NextRunAt, 0))
				if err == nil && !next.IsZero() && !next.After(now) {
					s.skipRun(a, "上一轮运行未结束，本轮跳过")
					a.NextRunAt = next.Unix()
					a.UpdatedAt = now.Unix()
					changed = true
				}
			}
			if a.Running || !a.Enabled || a.Status != StatusActive || a.Retry != nil {
				continue
			}
			// misfire：due 已超窗口（进程全退/休眠错过）——记 skipped 不补跑
			if a.NextRunAt > 0 && a.NextRunAt <= misfireCutoff {
				s.skipRun(a, "进程未运行，错过触发窗口")
				if a.Schedule.Kind == ScheduleDelay {
					// 一次性错过即终态，绝不按公式重算"下一周期"复活
					a.Status = StatusCompleted
					a.Enabled = false
				} else {
					next, err := computeNextRun(a.Schedule, now)
					if err != nil || next.IsZero() {
						a.Status = StatusCompleted
						a.Enabled = false
					} else {
						a.NextRunAt = next.Unix()
					}
				}
				a.UpdatedAt = now.Unix()
				changed = true
			}
		}
		return list, changed, nil
	})
}

// skipRun 记一条 skipped 台账行（不占 scheduledRunCount）
func (s *Scheduler) skipRun(a *Automation, reason string) {
	s.runner.AppendRun(&Run{
		ID:           scheduledRunID(a.ID, a.NextRunAt),
		AutomationID: a.ID,
		Trigger:      "schedule",
		ScheduledAt:  a.NextRunAt,
		StartedAt:    s.now().Unix(),
		FinishedAt:   s.now().Unix(),
		Outcome:      OutcomeSkipped,
		Error:        reason,
	})
}

// claimDue 认领到期任务：本进程并发满时顺延（不记 skipped，下 tick 重试）
func (s *Scheduler) claimDue(now time.Time) {
	pid := os.Getpid()
	for {
		if !s.runner.tryAcquire() {
			return
		}
		claimed, scheduledAt, ok := s.claimOne(now, pid)
		if !ok {
			s.runner.release()
			return
		}
		go s.runner.Execute(s.ctx, claimed, "schedule", scheduledAt)
	}
}

// claimOne 锁内认领一个 due 任务（条件在锁内重验，防读认领间隙的状态漂移）
func (s *Scheduler) claimOne(now time.Time, pid int) (*Automation, int64, bool) {
	var claimed *Automation
	var scheduledAt int64
	err := s.store.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		for _, a := range list {
			if a.Running || !a.Enabled || a.Status != StatusActive {
				continue
			}
			due := false
			if a.Retry != nil && a.Retry.RetryAt > 0 {
				// 退避中的认领条件只看 retryAt（防 tick 立即重试击穿退避）
				due = a.Retry.RetryAt <= now.Unix()
			} else if a.NextRunAt > 0 {
				due = a.NextRunAt <= now.Unix() && a.NextRunAt > now.Add(-misfireWindow).Unix()
			}
			if !due {
				continue
			}
			a.Running = true
			a.ClaimedAt = now.Unix()
			a.OwnerPID = pid
			a.UpdatedAt = now.Unix()
			claimed = cloneAutomation(a)
			scheduledAt = a.NextRunAt
			return list, true, nil
		}
		return list, false, nil
	})
	if err != nil {
		logx.Warnf("[automation] 认领失败: %v", err)
		return nil, 0, false
	}
	return claimed, scheduledAt, claimed != nil
}

// sweepMaybe 每小时裁剪一次台账
func (s *Scheduler) sweepMaybe(now time.Time) {
	if !s.lastSweep.IsZero() && now.Sub(s.lastSweep) < sweepEvery {
		return
	}
	s.lastSweep = now
	if err := s.runner.SweepRuns(); err != nil {
		logx.Warnf("[automation] 台账裁剪失败: %v", err)
	}
}
