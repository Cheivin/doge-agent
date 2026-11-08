package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding"
	"doge/pkg/logx"
)

// 运行结局
const (
	OutcomeRunning   = "running"
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeStopped   = "stopped"
	OutcomeSkipped   = "skipped"
)

// 退避参数：30s 指数退避、15min 封顶、5 次为限
const (
	backoffBase   = 30 * time.Second
	backoffMax    = 15 * time.Minute
	maxRetryCount = 5
)

// summaryLimit 台账摘要上限（4KB，对齐 mesh 结果卡片）
const summaryLimit = 4 << 10

type (
	// Runner 无人值守执行器：fork automation 会话执行任务 prompt，台账回写、
	// 调度状态结算与来源会话回注都在这里收口
	Runner struct {
		store    *Store
		sess     *coding.AgentSession // fork 模板（共享会话），回注目标
		runsPath string
		cfg      Config
		now      func() time.Time
		sem      chan struct{} // 并发闸门（容量 = MaxConcurrentRuns）
	}
)

// NewRunner 构建执行器；runsDir 为台账目录（automations/）
func NewRunner(store *Store, sess *coding.AgentSession, runsDir string, cfg Config) *Runner {
	c := cfg.WithDefaults()
	return &Runner{
		store:    store,
		sess:     sess,
		runsPath: filepath.Join(runsDir, "runs.jsonl"),
		cfg:      c,
		now:      time.Now,
		sem:      make(chan struct{}, c.MaxConcurrentRuns),
	}
}

// RunsPath 返回台账文件路径
func (r *Runner) RunsPath() string { return r.runsPath }

// tryAcquire 非阻塞占用一个并发槽（调度认领与手动触发共用）
func (r *Runner) tryAcquire() bool {
	select {
	case r.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// release 归还并发槽
func (r *Runner) release() {
	select {
	case <-r.sem:
	default:
	}
}

// Execute 执行一次已认领的任务（schedule 由调度器认领，manual 由 ManualRun
// 认领）；runId 按 trigger 推导（manual 每次随机后缀）。结束统一结算：台账
// 终态行 + 调度状态 + best-effort 回注。
func (r *Runner) Execute(ctx context.Context, a *Automation, trigger string, scheduledAt int64) {
	runID := scheduledRunID(a.ID, scheduledAt)
	if trigger == "manual" {
		runID = a.ID + ":manual:" + newRunSuffix()
	}
	r.execute(ctx, a, trigger, scheduledAt, runID)
}

// execute 带 runID 的执行体（ManualRun 预生成 id，保证返回值与台账行一致）
func (r *Runner) execute(ctx context.Context, a *Automation, trigger string, scheduledAt int64, runID string) {
	// 单次释放（本 goroutine 串行，无需锁）：notify 可能在共享会话上同步
	// 跑一轮 agent（等权限/问题回答时可久阻塞），必须先归还并发槽再通知，
	// 否则默认并发 1 时 notify 卡住全部定时任务；defer 兜底 panic 路径
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			r.release()
		}
	}
	defer releaseOnce()
	start := r.now()
	run := &Run{
		ID:           runID,
		AutomationID: a.ID,
		Trigger:      trigger,
		ScheduledAt:  scheduledAt,
		StartedAt:    start.Unix(),
		Outcome:      OutcomeRunning,
		OwnerPID:     os.Getpid(),
	}
	r.AppendRun(run)
	logx.Infof("[automation] 任务开始 %s (%s) trigger=%s", a.ID, a.Title, trigger)

	outcome, summary, runErr, permanent := r.runOnce(ctx, a, run)

	fin := r.now()
	run.FinishedAt = fin.Unix()
	run.Outcome = outcome
	run.Summary = summary
	if runErr != nil {
		run.Error = runErr.Error()
	}
	r.AppendRun(run)
	logx.Infof("[automation] 任务完成 %s outcome=%s err=%v", a.ID, outcome, runErr)
	r.settle(a, trigger, scheduledAt, outcome, runErr, permanent)
	releaseOnce()
	r.notify(a, run)
}

// runOnce fork 会话执行一次 prompt，返回（结局、摘要、错误、是否永久性错误）
func (r *Runner) runOnce(ctx context.Context, a *Automation, run *Run) (outcome, summary string, runErr error, permanent bool) {
	// 永久性前置校验：cwd 存在、模型可解析
	if info, err := os.Stat(a.Cwd); err != nil || !info.IsDir() {
		return OutcomeFailed, "", fmt.Errorf("cwd not a directory: %s", a.Cwd), true
	}
	model, err := r.resolveModel(a.Model)
	if err != nil {
		return OutcomeFailed, "", err, true
	}
	level := coding.RemoteToolLevel(a.Level)
	if level == "" {
		level = coding.RemoteFull
	}
	fork, err := r.sess.ForkAutomation(a.Cwd, model, level)
	if err != nil {
		return OutcomeFailed, "", err, false
	}
	// 会话头标注任务标题（UIs 在消息卡片上显示派发来源，代替裸的 USER）
	fork.CurrentSession().Header.SourceName = "定时任务 · " + a.Title
	run.SessionID = fork.CurrentSession().Header.ID
	fork.SessionManager.RegisterActive(fork)
	defer fork.SessionManager.UnregisterActive(fork)
	unsub, _ := fork.Subscribe(func(agent.AgentEvent) error { return nil }, coding.ConsumerHeadless)
	defer unsub()

	done := make(chan error, 1)
	go func() { done <- fork.Prompt(a.Prompt) }()
	timeout := time.Duration(r.cfg.RunTimeoutMinutes) * time.Minute
	select {
	case runErr = <-done:
	case <-time.After(timeout):
		fork.Abort()
		runErr = <-done
		return r.resultFrom(fork, errors.New("run timeout"))
	case <-ctx.Done():
		// 宿主进程退出：尽力收尾（该周期已消耗，按已执行结算）
		fork.Abort()
		runErr = <-done
		return r.resultFrom(fork, errors.New("host process exiting"))
	}
	return r.resultFrom(fork, runErr)
}

// resultFrom 从 fork 会话转录提取结局与摘要（末条 assistant 文本，照 mesh
// runTask 同款语义：Prompt 失败/无输出按失败上报，不报空洞的完成）
func (r *Runner) resultFrom(fork *coding.AgentSession, runErr error) (outcome, summary string, err error, permanent bool) {
	if runErr != nil {
		return OutcomeFailed, "", runErr, strings.Contains(runErr.Error(), "no API key")
	}
	msgs := fork.CurrentMessages()
	var last *ai.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == ai.RoleAssistant {
			last = msgs[i]
			break
		}
	}
	if last == nil {
		return OutcomeFailed, "", errors.New("run produced no assistant output"), false
	}
	switch last.StopReason {
	case ai.StopReasonError:
		return OutcomeFailed, "", errors.New(last.ErrorMessage), strings.Contains(last.ErrorMessage, "no API key")
	case ai.StopReasonAborted:
		return OutcomeStopped, "", errors.New("run aborted"), false
	}
	out := last.Text()
	if len(out) > summaryLimit {
		out = out[:summaryLimit]
	}
	return OutcomeSucceeded, out, nil, false
}

// resolveModel 解析任务模型提示：空=跟随共享会话当前模型（装配时已按
// settings 默认 → fallback 链解析，天然涵盖配置漂移回退）
func (r *Runner) resolveModel(hint string) (*ai.Model, error) {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		if r.sess != nil {
			if m := r.sess.Agent.State().Model; m != nil {
				return m, nil
			}
		}
		return nil, errors.New("no model available")
	}
	model, _, err := r.sess.ModelRuntime.ResolveModel(hint)
	return model, err
}

// settle 结算调度状态：成功/停止推进周期，失败按永久/临时分流（退避或终态）。
// 删除后迟到的结算直接丢弃（对 missing row 不复活任务）。
func (r *Runner) settle(a *Automation, trigger string, scheduledAt int64, outcome string, runErr error, permanent bool) {
	now := r.now()
	_ = r.store.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		cur := findByID(list, a.ID)
		if cur == nil {
			return nil, false, nil
		}
		cur.Running = false
		cur.OwnerPID = 0
		cur.ClaimedAt = 0
		cur.RunCountTotal++
		cur.LastRunAt = now.Unix()
		cur.UpdatedAt = now.Unix()
		if trigger == "manual" {
			// 手动触发不占调度节奏：不动 scheduledRunCount / nextRunAt
			return list, true, nil
		}
		switch outcome {
		case OutcomeSucceeded, OutcomeStopped:
			cur.ScheduledRunCount++
			cur.Retry = nil
			// 长 run 期间 maintain 已把错过的周期记 skipped 并推进 NextRunAt——
			// 尊重推进值；从原 scheduledAt 重算会把下次触发回拨到已过去/
			// 已记 skip 的周期（30 分钟 run × 10 分钟间隔：next 从 10:30
			// 回拨到 10:10，再空转一轮 misfire）
			var next time.Time
			if cur.NextRunAt > scheduledAt {
				next = time.Unix(cur.NextRunAt, 0)
			} else {
				n, err := computeNextRun(cur.Schedule, time.Unix(scheduledAt, 0))
				if err == nil {
					next = n
				}
			}
			if next.IsZero() {
				cur.Status = StatusCompleted
				cur.Enabled = false
				cur.NextRunAt = 0
			} else {
				cur.NextRunAt = next.Unix()
			}
			if cur.MaxRuns > 0 && cur.ScheduledRunCount >= cur.MaxRuns {
				cur.Status = StatusCompleted
				cur.Enabled = false
				cur.NextRunAt = 0
			}
		case OutcomeFailed:
			if permanent {
				cur.Status = StatusFailed
				cur.Enabled = false
				cur.Retry = nil
				cur.NextRunAt = 0
				break
			}
			attempts := 1
			if cur.Retry != nil {
				attempts = cur.Retry.Attempts + 1
			}
			errText := ""
			if runErr != nil {
				errText = runErr.Error()
			}
			if attempts >= maxRetryCount {
				// 循环任务退避超限：放弃本轮跳到下一正常周期（清退避态）；
				// 一次性/有限次任务转 failed 终态
				if cur.Recurring {
					cur.Retry = nil
					next, err := computeNextRun(cur.Schedule, now)
					if err != nil || next.IsZero() {
						cur.Status = StatusCompleted
						cur.Enabled = false
						cur.NextRunAt = 0
					} else {
						cur.NextRunAt = next.Unix()
					}
				} else {
					cur.Status = StatusFailed
					cur.Enabled = false
					cur.Retry = nil
					cur.NextRunAt = 0
				}
			} else {
				cur.Retry = &Retry{
					Attempts:  attempts,
					RetryAt:   now.Add(backoffDelay(attempts)).Unix(),
					LastError: errText,
				}
				// nextRunAt 不动：重试复用同一 runId
			}
		}
		return list, true, nil
	})
}

// backoffDelay 第 n 次失败后的退避时长：30s·2^(n-1)，15min 封顶
func backoffDelay(attempts int) time.Duration {
	d := backoffBase << (attempts - 1)
	if d > backoffMax || d <= 0 {
		return backoffMax
	}
	return d
}

// notify 来源会话回注（best-effort）：来源会话仍是执行器绑定的共享会话时，以
// follow-up 注入结果卡片（空闲即跑、运行中由 loop 停点 drain）；来源不在直接
// 跳过——台账已有完整记录，通知失败不影响调度结算。
func (r *Runner) notify(a *Automation, run *Run) {
	if r.sess == nil || a.SourceSessionID == "" {
		return
	}
	sess := r.sess.CurrentSession()
	if sess == nil || sess.Header.ID != a.SourceSessionID {
		return
	}
	body := run.Summary
	if body == "" && run.Error != "" {
		body = run.Error
	}
	if body == "" {
		body = "(无输出)"
	}
	when := time.Unix(run.StartedAt, 0).Format("01-02 15:04")
	msg := ai.NewUserTextMessage(fmt.Sprintf("[定时任务「%s」%s] %s session=%s\n%s",
		a.Title, outcomeLabel(run.Outcome), when, run.SessionID, body))
	msg.Details = map[string]any{
		"kind": "automation-run", "automationId": a.ID,
		"outcome": run.Outcome, "session": run.SessionID,
	}
	if len(r.sess.Agent.State().Messages) == 0 {
		_ = r.sess.Agent.Prompt(msg)
		return
	}
	r.sess.Agent.FollowUp(msg)
	_ = r.sess.Agent.Continue()
}

// outcomeLabel 结局中文标签（回注卡片用）
func outcomeLabel(outcome string) string {
	switch outcome {
	case OutcomeSucceeded:
		return "完成"
	case OutcomeFailed:
		return "失败"
	case OutcomeStopped:
		return "中止"
	}
	return outcome
}

// scheduledRunID 定时触发的 runId：{id}:{scheduledAt}（重试复用）
func scheduledRunID(id string, scheduledAt int64) string {
	return fmt.Sprintf("%s:%d", id, scheduledAt)
}

// AppendRun 追加一行台账（同 ID 多行末行生效）
func (r *Runner) AppendRun(run *Run) {
	if err := appendRunLine(r.runsPath, run); err != nil {
		logx.Errorf("[automation] 台账写入失败: %v", err)
	}
}

// appendRunLine 追加一行 JSON（O_APPEND 单次写，小行原子）。与 SweepRuns
// 的读-重写窗口互斥：append 若落在 sweep 读取之后、rename 之前会随旧文件
// 一起被覆盖丢行；持同一把进程内锁后，append 只可能落在快照前（进 keep）
// 或 rename 后（写入新文件）
func appendRunLine(path string, run *Run) error {
	runsSweepMu.Lock()
	defer runsSweepMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// Runs 读台账：同 ID 取末行，按开始时间新→旧
func (r *Runner) Runs() ([]*Run, error) {
	return readRuns(r.runsPath)
}

// History 某任务的台账（新→旧，limit 上限，默认 10）
func (r *Runner) History(id string, limit int) ([]*Run, error) {
	if limit <= 0 {
		limit = 10
	}
	runs, err := r.Runs()
	if err != nil {
		return nil, err
	}
	var out []*Run
	for _, run := range runs {
		if run.AutomationID == id {
			out = append(out, run)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// Reconcile 启动对账：台账里仍 running 且属主进程已死的记录标 stopped（任务
// 本身不重跑——认领时的周期已按"错过即过"结算）。
func (r *Runner) Reconcile() {
	runs, err := r.Runs()
	if err != nil {
		if !os.IsNotExist(err) {
			logx.Warnf("[automation] 台账对账失败: %v", err)
		}
		return
	}
	now := r.now().Unix()
	for _, run := range runs {
		if run.Outcome != OutcomeRunning {
			continue
		}
		if run.OwnerPID == os.Getpid() || pidAlive(run.OwnerPID) {
			continue
		}
		fin := *run
		fin.FinishedAt = now
		fin.Outcome = OutcomeStopped
		fin.Error = "宿主进程退出"
		r.AppendRun(&fin)
		logx.Warnf("[automation] run %s 属主进程已退出，标记 stopped", run.ID)
	}
}

var runsSweepMu sync.Mutex

// SweepRuns 按保留天数裁剪台账：running 行保留，其余按 FinishedAt（缺省
// StartedAt）早于 cutoff 的删除，重写落盘。
func (r *Runner) SweepRuns() error {
	runsSweepMu.Lock()
	defer runsSweepMu.Unlock()
	runs, err := r.Runs()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cutoff := r.now().AddDate(0, 0, -r.cfg.HistoryRetentionDays).Unix()
	var keep []*Run
	for _, run := range runs {
		ts := run.FinishedAt
		if ts == 0 {
			ts = run.StartedAt
		}
		if run.Outcome == OutcomeRunning || ts >= cutoff {
			keep = append(keep, run)
		}
	}
	if len(keep) == len(runs) {
		return nil
	}
	lock, err := lockFile(r.runsPath + ".lock")
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	var buf []byte
	for _, run := range keep {
		data, err := json.Marshal(run)
		if err != nil {
			return err
		}
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}
	tmp := r.runsPath + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.runsPath)
}

// ManualRun 手动立即触发：单飞认领（不占调度节奏，不动 nextRunAt/计数），
// 认领成功后异步执行，返回 runId。注意返回的 runId 与 Execute 内部生成的
// 后缀不同——以台账为准。
func (r *Runner) ManualRun(a *Automation) (string, error) {
	if !r.tryAcquire() {
		return "", fmt.Errorf("max concurrent runs reached (%d)", r.cfg.MaxConcurrentRuns)
	}
	now := r.now()
	pid := os.Getpid()
	var claimed *Automation
	err := r.store.Mutate(func(list []*Automation) ([]*Automation, bool, error) {
		cur := findByID(list, a.ID)
		if cur == nil {
			return nil, false, fmt.Errorf("automation not found: %s", a.ID)
		}
		if cur.Running {
			return nil, false, errors.New("automation is already running")
		}
		cur.Running = true
		cur.ClaimedAt = now.Unix()
		cur.OwnerPID = pid
		cur.UpdatedAt = now.Unix()
		claimed = cloneAutomation(cur)
		return list, true, nil
	})
	if err != nil {
		r.release()
		return "", err
	}
	// 预生成 runID 并原样传入执行体：返回值与台账行 ID 一致
	runID := a.ID + ":manual:" + newRunSuffix()
	go r.execute(context.Background(), claimed, "manual", now.Unix(), runID)
	return runID, nil
}

// pidAlive 报告进程是否存活（信号 0 探测；权限拒绝视为存活，pid 无效视为死亡）
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// readRuns 全量读台账并按 ID 去重（末行生效）；文件缺失视为空（惰性创建）
func readRuns(path string) ([]*Run, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	byID := map[string]*Run{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var run Run
		if err := json.Unmarshal([]byte(line), &run); err != nil {
			continue // 半行/损坏行跳过
		}
		byID[run.ID] = &run
	}
	out := make([]*Run, 0, len(byID))
	for _, run := range byID {
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt == out[j].StartedAt {
			return out[i].ID > out[j].ID
		}
		return out[i].StartedAt > out[j].StartedAt
	})
	return out, nil
}
