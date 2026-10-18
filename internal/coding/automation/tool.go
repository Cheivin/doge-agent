package automation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"doge/internal/agent"
	"doge/internal/coding"
	"doge/internal/coding/tools"
)

// toolDescription 工具硬规则（定时任务实战教训提炼）：相对时间必走 delay、
// interval 锚点推导、cron 只收墙钟时刻、prompt 自包含且禁止再操作 automation
const toolDescription = "Create and manage scheduled automations that fire an unattended agent session with a stored prompt. " +
	"Scheduling rules (STRICT): relative times (\"in 8 minutes\", \"2 hours from now\") MUST use delayMinutes — never convert them to cron or absolute clock times, your sense of \"now\" is stale and a self-computed one-shot cron that already passed silently rolls to next year. " +
	"\"every N minutes/hours\" uses intervalUnit+interval (the host anchors from creation time; do NOT use cron */N for these — it aligns to the wall clock and fires early). " +
	"cron (5 fields: minute hour day month weekday, LOCAL timezone, never convert to UTC) is ONLY for wall-clock times the user explicitly named (\"daily at 9:00\", \"every Monday 09:00\"). " +
	"Content rules: title MUST keep the user's time phrase (e.g. \"每20分钟喝水提醒\", not \"喝水提醒\"). " +
	"prompt MUST be a complete self-contained instruction — it runs unattended in a fresh session with NO current conversation context: never reference \"the file above\" or prior output, include every path/spec/acceptance criterion it needs. " +
	"prompt must NOT instruct creating/modifying/deleting automations (self-replication is forbidden). " +
	"Recurring tasks run unlimited by default; pass maxRuns to bound total scheduled runs (manual runs never count). delay is always one-shot. " +
	"Respect exact user times: no jitter, never fire early."

type (
	// ToolDeps 工具依赖：存储/执行器 + 绑定的共享会话（默认 cwd、来源会话、
	// 防递归 origin 判定）
	ToolDeps struct {
		Store   *Store
		Runner  *Runner
		Session *coding.AgentSession
	}
)

// Tool 构建 automation 工具（action 多态，照 mesh_forward 惯例）。deps 为指针：
// Runner/Session 在会话装配完成后回填，工具闭包执行期才读。
func Tool(d *ToolDeps) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "automation",
		Label:       "Manage scheduled automations",
		Description: toolDescription,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":       map[string]any{"type": "string", "enum": []string{"create", "list", "get", "update", "delete", "enable", "disable", "run", "history"}, "description": "operation to perform"},
				"id":           map[string]any{"type": "string", "description": "automation id (get/update/delete/enable/disable/run/history)"},
				"title":        map[string]any{"type": "string", "description": "create/update: display title, MUST keep the user's time phrase"},
				"prompt":       map[string]any{"type": "string", "description": "create/update: full self-contained instruction delivered on every run"},
				"delayMinutes": map[string]any{"type": "integer", "description": "create: one-shot delay in minutes (1-525600) — for relative times, ALWAYS this, never cron"},
				"cron":         map[string]any{"type": "string", "description": "create/update: 5-field cron (minute hour day month weekday), LOCAL timezone, only for user-named wall-clock times"},
				"intervalUnit": map[string]any{"type": "string", "enum": []string{"minute", "hour", "day", "week", "month"}, "description": "create/update: interval unit, paired with interval"},
				"interval":     map[string]any{"type": "integer", "description": "create/update: every N units (1-200)"},
				"at":           map[string]any{"type": "string", "description": "create/update: HH:MM time-of-day for day/week/month intervals (default: creation time)"},
				"weekdays":     map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "create/update: week interval only, 0-6 (Sunday=0), empty = every day"},
				"monthDays":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "create/update: month interval only, 1-31, empty = anchor day"},
				"maxRuns":      map[string]any{"type": "integer", "description": "create/update: total scheduled-run cap for recurring tasks (0 = unlimited)"},
				"cwd":          map[string]any{"type": "string", "description": "create/update: working directory (default: current session cwd)"},
				"model":        map[string]any{"type": "string", "description": "create/update: provider/model, empty = global default"},
				"level":        map[string]any{"type": "string", "enum": []string{"readonly", "write", "full"}, "description": "create/update: tool tier of the unattended session (default full)"},
				"limit":        map[string]any{"type": "integer", "description": "history: max rows (default 10)"},
			},
			"required": []string{"action"},
		},
		Execute: func(_ string, params map[string]any, _ context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			return executeTool(d, params)
		},
	}
}

// executeTool 工具层校验 + 各 action 分派
func executeTool(d *ToolDeps, params map[string]any) (*agent.AgentToolResult, error) {
	action := strParam(params["action"])
	// 防递归兜底：automation 会话（物理裁剪已根除工具可见性，此处拦未来
	// 新入口绕过裁剪的调用）
	origin := sessionKind(d.Session)
	switch action {
	case "create", "update", "delete", "enable", "disable", "run":
		if origin == coding.SessionKindAutomation {
			return tools.ToolError("automation: scheduled runs must not manage automations (self-replication guard)"), nil
		}
	}
	switch action {
	case "create":
		return toolCreate(d, params)
	case "list":
		return toolList(d)
	case "get":
		return toolGet(d, strParam(params["id"]))
	case "update":
		return toolUpdate(d, params)
	case "delete":
		return toolDelete(d, strParam(params["id"]))
	case "enable":
		return toolSetEnabled(d, strParam(params["id"]), true)
	case "disable":
		return toolSetEnabled(d, strParam(params["id"]), false)
	case "run":
		return toolRun(d, strParam(params["id"]))
	case "history":
		limit := intParam(params["limit"])
		return toolHistory(d, strParam(params["id"]), limit)
	default:
		return tools.ToolError("automation: unknown action " + action), nil
	}
}

// toolCreate 组装并校验新任务（互斥规则在此与 store 层双重实现）
func toolCreate(d *ToolDeps, params map[string]any) (*agent.AgentToolResult, error) {
	title := strings.TrimSpace(strParam(params["title"]))
	prompt := strings.TrimSpace(strParam(params["prompt"]))
	if title == "" || prompt == "" {
		return tools.ToolError("automation create: title and prompt required"), nil
	}
	cwd := strings.TrimSpace(strParam(params["cwd"]))
	if cwd == "" && d.Session != nil {
		cwd = d.Session.GetCwd()
	}
	sc, err := scheduleFromParams(params)
	if err != nil {
		return tools.ToolError("automation create: " + err.Error()), nil
	}
	sc.AnchorAt = time.Now().Unix()
	a := &Automation{
		Title:    title,
		Prompt:   prompt,
		Cwd:      cwd,
		Model:    strings.TrimSpace(strParam(params["model"])),
		Level:    strParam(params["level"]),
		Schedule: *sc,
		MaxRuns:  intParam(params["maxRuns"]),
	}
	if d.Session != nil && d.Session.CurrentSession() != nil {
		a.SourceSessionID = d.Session.CurrentSession().Header.ID
	}
	if err := d.Store.Create(sessionKind(d.Session), a); err != nil {
		return tools.ToolError("automation create: " + err.Error()), nil
	}
	return tools.ToolResult(fmt.Sprintf("Created %s %q\n  schedule: %s\n  next run: %s\n  cwd: %s",
		a.ID, a.Title, describeSchedule(a.Schedule), formatUnix(a.NextRunAt), a.Cwd)), nil
}

// scheduleFromParams 工具层调度互斥校验（三选一）
func scheduleFromParams(params map[string]any) (*Schedule, error) {
	delay := intParam(params["delayMinutes"])
	cron := strings.TrimSpace(strParam(params["cron"]))
	unit := strParam(params["intervalUnit"])
	step := intParam(params["interval"])
	set := 0
	if delay > 0 {
		set++
	}
	if cron != "" {
		set++
	}
	if unit != "" || step > 0 {
		set++
	}
	if set != 1 {
		return nil, errors.New("exactly one of delayMinutes / cron / intervalUnit+interval required")
	}
	sc := &Schedule{}
	switch {
	case delay > 0:
		sc.Kind = ScheduleDelay
		sc.DelayMinutes = delay
	case cron != "":
		sc.Kind = ScheduleCron
		sc.Cron = cron
	default:
		sc.Kind = ScheduleInterval
		sc.IntervalUnit = unit
		sc.Interval = step
		if v, ok := params["weekdays"].([]any); ok {
			for _, w := range v {
				sc.Weekdays = append(sc.Weekdays, intParam(w))
			}
		}
		if v, ok := params["monthDays"].([]any); ok {
			for _, m := range v {
				sc.MonthDays = append(sc.MonthDays, intParam(m))
			}
		}
	}
	if at := strings.TrimSpace(strParam(params["at"])); at != "" {
		sc.At = at
	}
	return sc, nil
}

// toolList 概要表
func toolList(d *ToolDeps) (*agent.AgentToolResult, error) {
	list, err := d.Store.List()
	if err != nil {
		return tools.ToolError("automation list: " + err.Error()), nil
	}
	if len(list) == 0 {
		return tools.ToolResult("No automations. create one with action=create."), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Automations (%d):\n", len(list))
	for _, a := range list {
		fmt.Fprintf(&b, "  %-16s %s\n", a.ID, a.Title)
		fmt.Fprintf(&b, "  %-16s %s  next=%s  status=%s  runs=%d (scheduled %d)\n",
			"", describeSchedule(a.Schedule), formatUnix(a.NextRunAt), a.statusLabel(), a.RunCountTotal, a.ScheduledRunCount)
	}
	return tools.ToolResult(strings.TrimRight(b.String(), "\n")), nil
}

// toolGet 单条详情 + 最近 3 次 run
func toolGet(d *ToolDeps, id string) (*agent.AgentToolResult, error) {
	if id == "" {
		return tools.ToolError("automation get: id required"), nil
	}
	a, err := d.Store.Get(id)
	if err != nil {
		return tools.ToolError("automation get: " + err.Error()), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q\n", a.ID, a.Title)
	fmt.Fprintf(&b, "  schedule: %s (anchor %s)\n", describeSchedule(a.Schedule), formatUnix(a.Schedule.AnchorAt))
	fmt.Fprintf(&b, "  next=%s last=%s status=%s recurring=%v maxRuns=%d\n",
		formatUnix(a.NextRunAt), formatUnix(a.LastRunAt), a.statusLabel(), a.Recurring, a.MaxRuns)
	fmt.Fprintf(&b, "  cwd=%s model=%q level=%s\n", a.Cwd, a.Model, levelLabel(a.Level))
	if a.Retry != nil {
		fmt.Fprintf(&b, "  retry: attempts=%d retryAt=%s lastError=%q\n", a.Retry.Attempts, formatUnix(a.Retry.RetryAt), a.Retry.LastError)
	}
	fmt.Fprintf(&b, "  prompt:\n%s\n", a.Prompt)
	runs, _ := d.Runner.History(a.ID, 3)
	if len(runs) > 0 {
		b.WriteString("  recent runs:\n")
		for _, run := range runs {
			fmt.Fprintf(&b, "    %s %s started=%s session=%s\n", run.ID, run.Outcome, formatUnix(run.StartedAt), run.SessionID)
		}
	}
	return tools.ToolResult(strings.TrimRight(b.String(), "\n")), nil
}

// toolUpdate 可变字段更新
func toolUpdate(d *ToolDeps, params map[string]any) (*agent.AgentToolResult, error) {
	id := strParam(params["id"])
	if id == "" {
		return tools.ToolError("automation update: id required"), nil
	}
	p := &Patch{}
	if v, ok := params["title"]; ok {
		s := strings.TrimSpace(strParam(v))
		p.Title = &s
	}
	if v, ok := params["prompt"]; ok {
		s := strings.TrimSpace(strParam(v))
		p.Prompt = &s
	}
	if v, ok := params["cwd"]; ok {
		s := strings.TrimSpace(strParam(v))
		p.Cwd = &s
	}
	if v, ok := params["model"]; ok {
		s := strings.TrimSpace(strParam(v))
		p.Model = &s
	}
	if v, ok := params["level"]; ok {
		s := strParam(v)
		p.Level = &s
	}
	if v, ok := params["maxRuns"]; ok {
		n := intParam(v)
		p.MaxRuns = &n
	}
	// 调度变更走整体替换：出现任一调度键即以本次参数重建（interval 需与
	// intervalUnit 成对传入，缺项由 store 校验兜底报错）
	for _, key := range []string{"delayMinutes", "cron", "intervalUnit", "interval", "at", "weekdays", "monthDays"} {
		if _, ok := params[key]; !ok {
			continue
		}
		sc, err := scheduleFromParams(params)
		if err != nil {
			return tools.ToolError("automation update: " + err.Error()), nil
		}
		sc.AnchorAt = time.Now().Unix()
		p.Schedule = sc
		break
	}
	a, err := d.Store.Update(sessionKind(d.Session), id, p)
	if err != nil {
		return tools.ToolError("automation update: " + err.Error()), nil
	}
	return tools.ToolResult(fmt.Sprintf("Updated %s %q\n  schedule: %s\n  next run: %s",
		a.ID, a.Title, describeSchedule(a.Schedule), formatUnix(a.NextRunAt))), nil
}

// toolDelete 删除
func toolDelete(d *ToolDeps, id string) (*agent.AgentToolResult, error) {
	if id == "" {
		return tools.ToolError("automation delete: id required"), nil
	}
	if err := d.Store.Delete(sessionKind(d.Session), id); err != nil {
		return tools.ToolError("automation delete: " + err.Error()), nil
	}
	return tools.ToolResult("Deleted " + id), nil
}

// toolSetEnabled 暂停/恢复
func toolSetEnabled(d *ToolDeps, id string, enabled bool) (*agent.AgentToolResult, error) {
	if id == "" {
		return tools.ToolError("automation: id required"), nil
	}
	a, err := d.Store.SetEnabled(sessionKind(d.Session), id, enabled)
	if err != nil {
		return tools.ToolError("automation: " + err.Error()), nil
	}
	return tools.ToolResult(fmt.Sprintf("%s %s (next run %s)", a.ID, a.statusLabel(), formatUnix(a.NextRunAt))), nil
}

// toolRun 手动立即触发（异步，不占调度节奏）
func toolRun(d *ToolDeps, id string) (*agent.AgentToolResult, error) {
	if id == "" {
		return tools.ToolError("automation run: id required"), nil
	}
	a, err := d.Store.Get(id)
	if err != nil {
		return tools.ToolError("automation run: " + err.Error()), nil
	}
	if _, err := d.Runner.ManualRun(a); err != nil {
		return tools.ToolError("automation run: " + err.Error()), nil
	}
	return tools.ToolResult(fmt.Sprintf("Triggered manual run of %s %q (runs in background; check action=history)", a.ID, a.Title)), nil
}

// toolHistory 运行台账
func toolHistory(d *ToolDeps, id string, limit int) (*agent.AgentToolResult, error) {
	if id == "" {
		return tools.ToolError("automation history: id required"), nil
	}
	runs, err := d.Runner.History(id, limit)
	if err != nil {
		return tools.ToolError("automation history: " + err.Error()), nil
	}
	if len(runs) == 0 {
		return tools.ToolResult("No runs recorded for " + id), nil
	}
	var b strings.Builder
	for _, run := range runs {
		fmt.Fprintf(&b, "  %s  %s  trigger=%s started=%s", run.ID, run.Outcome, run.Trigger, formatUnix(run.StartedAt))
		if run.Error != "" {
			fmt.Fprintf(&b, "  err=%q", run.Error)
		}
		b.WriteString("\n")
	}
	return tools.ToolResult(strings.TrimRight(b.String(), "\n")), nil
}

// sessionKind 工具绑定会话的 Kind（防递归 origin）
func sessionKind(s *coding.AgentSession) string {
	if s == nil || s.CurrentSession() == nil {
		return ""
	}
	return s.CurrentSession().Header.Kind
}

// statusLabel 带运行态的状态描述
func (a *Automation) statusLabel() string {
	if a.Running {
		return a.Status + " (running)"
	}
	return a.Status
}

// levelLabel 工具级缺省标注
func levelLabel(level string) string {
	if level == "" {
		return LevelFull
	}
	return level
}

// formatUnix Unix 秒的本地可读时刻（0 输出 "-"）
func formatUnix(ts int64) string {
	if ts == 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

// strParam 字符串参数（宽松取值，照 mesh_forward）
func strParam(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// intParam 整数参数（JSON 数字可能是 float64）
func intParam(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case int64:
		return int(n)
	}
	return 0
}
