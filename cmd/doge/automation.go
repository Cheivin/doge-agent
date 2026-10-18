package main

// Automation CLI subcommands: `doge automation list|add|rm|enable|disable|run|
// history|get`. All but `run` are thin file operations — a resident doge
// process (TUI/Web/desktop with the scheduler mounted) picks changes up within
// one 20s tick, mirroring `doge mesh forward` (declarative file + hot reload).
// `run` assembles a short-lived app in THIS process and executes the task
// synchronously (a manual run is opportunistic: it does not need the
// scheduler, and it never advances the schedule).

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"doge/internal/app"
	"doge/internal/coding"
	"doge/internal/coding/automation"
)

// automationStore 构建直接读写定义文件的存储（无会话依赖）
func automationStore() *automation.Store {
	return automation.NewStore(coding.AutomationsFile(), automation.DefaultConfig())
}

// runAutomationCommand 分发 automation 子命令，返回进程退出码
func runAutomationCommand(args []string) int {
	if len(args) == 0 {
		automationUsage()
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "list", "ls":
		return automationListCmd()
	case "add":
		return automationAddCmd(rest)
	case "get":
		return automationGetCmd(rest)
	case "rm", "remove":
		return automationRmCmd(rest)
	case "enable":
		return automationToggleCmd(rest, true)
	case "disable":
		return automationToggleCmd(rest, false)
	case "run":
		return automationRunCmd(rest)
	case "history":
		return automationHistoryCmd(rest)
	case "help", "-h", "--help":
		automationUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "doge automation: unknown subcommand %q\n", cmd)
		automationUsage()
		return 2
	}
}

// automationListCmd 列出全部任务
func automationListCmd() int {
	list, err := automationStore().List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Println("No automations. Create one with `doge automation add`.")
		return 0
	}
	fmt.Printf("Automations (%d):\n", len(list))
	for _, a := range list {
		state := a.Status
		if a.Running {
			state += " (running)"
		}
		fmt.Printf("  %-16s %s\n", a.ID, a.Title)
		fmt.Printf("  %-16s %s  next=%s  status=%s  runs=%d (scheduled %d)\n",
			"", describeScheduleCLI(a.Schedule), formatUnixCLI(a.NextRunAt), state, a.RunCountTotal, a.ScheduledRunCount)
	}
	fmt.Println("\n(config: " + coding.AutomationsFile() + ")")
	return 0
}

// parseIntervalSpec 解析 --every 的 "20m|2h|3d|1w|2mo" 形式
func parseIntervalSpec(spec string) (unit string, step int, err error) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	for _, suffix := range []string{"mo", "m", "h", "d", "w"} {
		if strings.HasSuffix(spec, suffix) {
			n := 0
			if _, err := fmt.Sscanf(strings.TrimSuffix(spec, suffix), "%d", &n); err != nil || n <= 0 {
				return "", 0, fmt.Errorf("invalid interval %q", spec)
			}
			switch suffix {
			case "mo":
				return "month", n, nil
			case "m":
				return "minute", n, nil
			case "h":
				return "hour", n, nil
			case "d":
				return "day", n, nil
			case "w":
				return "week", n, nil
			}
		}
	}
	return "", 0, fmt.Errorf("invalid interval %q (want 20m / 2h / 3d / 1w / 2mo)", spec)
}

// automationAddCmd 建任务：三选一调度参数（--delay-minutes / --cron / --every）
func automationAddCmd(args []string) int {
	fs := flag.NewFlagSet("automation add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	title := fs.String("title", "", "task title (keep the user's time phrase)")
	prompt := fs.String("prompt", "", "self-contained instruction delivered on every run")
	delay := fs.Int("delay-minutes", 0, "one-shot delay in minutes (relative times)")
	cron := fs.String("cron", "", "5-field cron (minute hour day month weekday), LOCAL timezone")
	every := fs.String("every", "", "interval with unit suffix: 20m / 2h / 3d / 1w / 2mo")
	at := fs.String("at", "", "HH:MM time-of-day for day/week/month intervals")
	cwd := fs.String("cwd", "", "working directory (default: current)")
	model := fs.String("model", "", "provider/model, empty = global default")
	level := fs.String("level", "full", "tool tier: readonly | write | full")
	maxRuns := fs.Int("max-runs", 0, "total scheduled-run cap for recurring tasks (0 = unlimited)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *title == "" || *prompt == "" {
		fmt.Fprintln(os.Stderr, "doge automation add: --title and --prompt are required")
		return 2
	}
	set := 0
	for _, v := range []int{*delay, len(*cron), len(*every)} {
		if v > 0 {
			set++
		}
	}
	if set != 1 {
		fmt.Fprintln(os.Stderr, "doge automation add: exactly one of --delay-minutes / --cron / --every is required")
		return 2
	}
	sc := automation.Schedule{AnchorAt: time.Now().Unix()}
	switch {
	case *delay > 0:
		sc.Kind = automation.ScheduleDelay
		sc.DelayMinutes = *delay
	case *cron != "":
		sc.Kind = automation.ScheduleCron
		sc.Cron = *cron
	default:
		unit, step, err := parseIntervalSpec(*every)
		if err != nil {
			fmt.Fprintf(os.Stderr, "doge automation add: %v\n", err)
			return 2
		}
		sc.Kind = automation.ScheduleInterval
		sc.IntervalUnit = unit
		sc.Interval = step
		sc.At = *at
	}
	if *cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			*cwd = wd
		}
	}
	draft := &automation.Automation{
		Title: *title, Prompt: *prompt, Cwd: *cwd,
		Model: *model, Level: *level, Schedule: sc, MaxRuns: *maxRuns,
	}
	if err := automationStore().Create("", draft); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	fmt.Printf("Created %s %q\n", draft.ID, draft.Title)
	fmt.Printf("  schedule: %s\n  next run: %s\n  cwd: %s\n",
		describeScheduleCLI(draft.Schedule), formatUnixCLI(draft.NextRunAt), draft.Cwd)
	return 0
}

// automationGetCmd 单条详情 + 最近 3 次 run
func automationGetCmd(args []string) int {
	if len(args) == 0 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "doge automation get: an id argument is required")
		return 2
	}
	store := automationStore()
	a, err := store.Get(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	fmt.Printf("%s %q\n", a.ID, a.Title)
	fmt.Printf("  schedule: %s (anchor %s)\n", describeScheduleCLI(a.Schedule), formatUnixCLI(a.Schedule.AnchorAt))
	fmt.Printf("  next=%s last=%s status=%s recurring=%v maxRuns=%d\n",
		formatUnixCLI(a.NextRunAt), formatUnixCLI(a.LastRunAt), a.Status, a.Recurring, a.MaxRuns)
	fmt.Printf("  cwd=%s model=%q level=%s\n", a.Cwd, a.Model, a.Level)
	fmt.Printf("  prompt:\n%s\n", a.Prompt)
	runs := automation.NewRunner(store, nil, coding.AutomationsDir(), automation.DefaultConfig())
	history, _ := runs.History(a.ID, 3)
	for _, run := range history {
		fmt.Printf("  %s  %s  trigger=%s started=%s session=%s\n",
			run.ID, run.Outcome, run.Trigger, formatUnixCLI(run.StartedAt), run.SessionID)
	}
	return 0
}

// automationRmCmd 删除任务
func automationRmCmd(args []string) int {
	if len(args) == 0 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "doge automation rm: an id argument is required")
		return 2
	}
	if err := automationStore().Delete("", args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	fmt.Println("Deleted " + args[0])
	return 0
}

// automationToggleCmd 暂停/恢复
func automationToggleCmd(args []string, enable bool) int {
	if len(args) == 0 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "doge automation: an id argument is required")
		return 2
	}
	a, err := automationStore().SetEnabled("", args[0], enable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	fmt.Printf("%s %s (next run %s)\n", a.ID, a.Status, formatUnixCLI(a.NextRunAt))
	return 0
}

// automationRunCmd 手动立即触发：本进程装配一个短命 app 同步执行（手动触发
// 不占调度节奏；常驻进程的调度器不受影响）
func automationRunCmd(args []string) int {
	if len(args) == 0 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "doge automation run: an id argument is required")
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	env, err := app.Prepare(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	a, err := app.Assemble(env, app.Options{Version: versionString()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	defer a.Close()
	target, err := a.Automation.Get(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	fmt.Printf("Triggering manual run of %s %q ...\n", target.ID, target.Title)
	if _, err := a.Automation.Run(target.ID); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	// 轮询认领状态直到结算（上限 = 单次超时 + 缓冲）
	deadline := time.Now().Add(90 * time.Minute)
	for time.Now().Before(deadline) {
		cur, err := a.Automation.Get(target.ID)
		if err != nil {
			break // 任务被并发删除等：直接看台账
		}
		if !cur.Running {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	runs, _ := a.Automation.History(target.ID, 1)
	if len(runs) > 0 {
		run := runs[0]
		fmt.Printf("run %s: outcome=%s (started %s, session %s)\n",
			run.ID, run.Outcome, formatUnixCLI(run.StartedAt), run.SessionID)
		if run.Error != "" {
			fmt.Println("  error: " + run.Error)
			return 1
		}
		if run.Summary != "" {
			fmt.Println("  summary: " + run.Summary)
		}
	}
	return 0
}

// automationHistoryCmd 任务台账
func automationHistoryCmd(args []string) int {
	fs := flag.NewFlagSet("automation history", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	limit := fs.Int("limit", 10, "max rows")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 || rest[0] == "" {
		fmt.Fprintln(os.Stderr, "doge automation history: an id argument is required")
		return 2
	}
	runs := automation.NewRunner(automationStore(), nil, coding.AutomationsDir(), automation.DefaultConfig())
	history, err := runs.History(rest[0], *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	if len(history) == 0 {
		fmt.Println("No runs recorded for " + rest[0])
		return 0
	}
	for _, run := range history {
		line := fmt.Sprintf("  %s  %-9s trigger=%-8s started=%s", run.ID, run.Outcome, run.Trigger, formatUnixCLI(run.StartedAt))
		if run.Error != "" {
			line += "  err=" + run.Error
		}
		fmt.Println(line)
	}
	return 0
}

// describeScheduleCLI 调度可读描述（CLI 层复用 automation 包同款文案）
func describeScheduleCLI(sc automation.Schedule) string {
	switch sc.Kind {
	case automation.ScheduleDelay:
		return fmt.Sprintf("one-shot in %dm", sc.DelayMinutes)
	case automation.ScheduleInterval:
		desc := fmt.Sprintf("every %d %s(s)", sc.Interval, sc.IntervalUnit)
		if sc.At != "" {
			desc += " at " + sc.At
		}
		return desc
	case automation.ScheduleCron:
		return "cron " + sc.Cron
	}
	return "unknown schedule"
}

// formatUnixCLI Unix 秒的本地可读时刻
func formatUnixCLI(ts int64) string {
	if ts == 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

// automationUsage 子命令帮助
func automationUsage() {
	fmt.Fprint(os.Stderr, `doge automation — scheduled automations

Usage:
  doge automation list                                    list all automations
  doge automation add --title T --prompt P --every 20m [--at 09:00] [--cwd DIR]
                          [--model provider/model] [--level readonly|write|full] [--max-runs N]
  doge automation add --title T --prompt P --delay-minutes 120            one-shot delay
  doge automation add --title T --prompt P --cron "0 9 * * 1"             wall-clock cron (LOCAL tz)
  doge automation get <id>                                detail + recent runs
  doge automation rm <id>                                 delete
  doge automation enable <id> / disable <id>              resume / pause
  doge automation run <id>                                trigger now (executes in THIS process)
  doge automation history <id> [--limit N]                run ledger

All commands except `+"`run`"+` are file operations on
`+coding.AutomationsFile()+` — a resident doge process (TUI/Web/desktop)
picks changes up within one 20s scheduler tick. The prompt must be a complete
self-contained instruction: it runs unattended in a fresh session with no
conversation context. Boundary: automations only fire while a doge process is
alive; missed windows (>5min) are skipped, not replayed.
`)
}
