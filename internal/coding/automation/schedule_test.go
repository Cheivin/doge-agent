package automation

import (
	"testing"
	"time"
)

// at 解析 "2006-01-02 15:04" 本地时刻
func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestComputeNextRunDelay(t *testing.T) {
	anchor := at("2026-10-16 12:00")
	sc := Schedule{Kind: ScheduleDelay, DelayMinutes: 8, AnchorAt: anchor.Unix()}
	next, err := computeNextRun(sc, anchor)
	if err != nil {
		t.Fatalf("computeNextRun: %v", err)
	}
	if !next.Equal(anchor.Add(8 * time.Minute)) {
		t.Fatalf("delay next = %v, want %v", next, anchor.Add(8*time.Minute))
	}
	// 一次性已过期：零值（绝不复活到下一周期）
	next, err = computeNextRun(sc, anchor.Add(9*time.Minute))
	if err != nil || !next.IsZero() {
		t.Fatalf("expired delay = %v err=%v, want zero", next, err)
	}
}

func TestComputeNextRunIntervalMinute(t *testing.T) {
	// 11:46 建"每 10 分钟"：锚点推导，从 11:56 起而不是对齐墙钟的 11:50
	anchor := at("2026-10-16 11:46")
	sc := Schedule{Kind: ScheduleInterval, IntervalUnit: "minute", Interval: 10, AnchorAt: anchor.Unix()}
	next, err := computeNextRun(sc, anchor)
	if err != nil {
		t.Fatalf("computeNextRun: %v", err)
	}
	if !next.Equal(at("2026-10-16 11:56")) {
		t.Fatalf("first = %v, want 11:56", next)
	}
	// 从 11:57 查：下一档 12:06
	next, _ = computeNextRun(sc, at("2026-10-16 11:57"))
	if !next.Equal(at("2026-10-16 12:06")) {
		t.Fatalf("second = %v, want 12:06", next)
	}
	// 正好在触发点上：严格大于 from
	next, _ = computeNextRun(sc, at("2026-10-16 11:56"))
	if !next.Equal(at("2026-10-16 12:06")) {
		t.Fatalf("on-dot = %v, want 12:06", next)
	}
}

func TestComputeNextRunIntervalHour(t *testing.T) {
	// 09:46 建"每 2 小时"，At 缺省取锚点分钟
	anchor := at("2026-10-16 09:46")
	sc := Schedule{Kind: ScheduleInterval, IntervalUnit: "hour", Interval: 2, AnchorAt: anchor.Unix()}
	next, _ := computeNextRun(sc, anchor)
	if !next.Equal(at("2026-10-16 11:46")) {
		t.Fatalf("hour next = %v, want 11:46", next)
	}
	// 显式 At="30"：分钟对齐 30 分
	sc.At = "07:30"
	next, _ = computeNextRun(sc, at("2026-10-16 10:00"))
	if !next.Equal(at("2026-10-16 11:30")) {
		t.Fatalf("hour at next = %v, want 11:30", next)
	}
}

func TestComputeNextRunIntervalDay(t *testing.T) {
	anchor := at("2026-10-16 18:23")
	sc := Schedule{Kind: ScheduleInterval, IntervalUnit: "day", Interval: 2, AnchorAt: anchor.Unix()}
	next, _ := computeNextRun(sc, anchor)
	if !next.Equal(at("2026-10-18 18:23")) {
		t.Fatalf("day next = %v, want 10-18 18:23", next)
	}
	// At 覆盖当日时刻
	sc.At = "09:00"
	next, _ = computeNextRun(sc, at("2026-10-17 20:00"))
	if !next.Equal(at("2026-10-18 09:00")) {
		t.Fatalf("day at = %v, want 10-18 09:00", next)
	}
}

func TestComputeNextRunIntervalWeek(t *testing.T) {
	// 2026-10-16 是周五；锚点周（10-12 周一起）为第 0 周
	anchor := at("2026-10-16 15:00")
	sc := Schedule{Kind: ScheduleInterval, IntervalUnit: "week", Interval: 1, AnchorAt: anchor.Unix()}
	// 空 weekdays = 每天：从周日 10-18 起每天都可触发
	next, _ := computeNextRun(sc, anchor)
	if !next.Equal(at("2026-10-17 15:00")) {
		t.Fatalf("week allday next = %v, want 10-17 15:00", next)
	}
	// 限定周一/周四（1=周一…周日用 0）
	sc.Weekdays = []int{1, 4}
	next, _ = computeNextRun(sc, anchor)
	if !next.Equal(at("2026-10-19 15:00")) {
		t.Fatalf("week days next = %v, want 10-19 (周一) 15:00", next)
	}
	// 步长 2：跳过一个日历周
	sc.Interval = 2
	next, _ = computeNextRun(sc, anchor)
	if !next.Equal(at("2026-10-26 15:00")) {
		t.Fatalf("week step2 next = %v, want 10-26 (下周周一) 15:00", next)
	}
}

func TestComputeNextRunIntervalMonth(t *testing.T) {
	anchor := at("2026-10-16 09:00")
	sc := Schedule{Kind: ScheduleInterval, IntervalUnit: "month", Interval: 1, AnchorAt: anchor.Unix()}
	next, _ := computeNextRun(sc, anchor)
	if !next.Equal(at("2026-11-16 09:00")) {
		t.Fatalf("month next = %v, want 11-16 09:00", next)
	}
	// 1 月 31 日每 1 个月：2 月无 31 日，滚月跳过守卫应落到 3 月 31 日
	anchor = at("2026-01-31 09:00")
	sc = Schedule{Kind: ScheduleInterval, IntervalUnit: "month", Interval: 1, AnchorAt: anchor.Unix()}
	next, _ = computeNextRun(sc, anchor)
	if !next.Equal(at("2026-03-31 09:00")) {
		t.Fatalf("month overflow next = %v, want 03-31 09:00", next)
	}
	// 指定 monthDays：anchor 月内 15 号先触发，再下一档跨到 12-01（步长 2 跳过 11 月）
	anchor = at("2026-10-01 09:00")
	sc = Schedule{Kind: ScheduleInterval, IntervalUnit: "month", Interval: 2, MonthDays: []int{1, 15}, AnchorAt: anchor.Unix()}
	next, _ = computeNextRun(sc, at("2026-10-02 09:00"))
	if !next.Equal(at("2026-10-15 09:00")) {
		t.Fatalf("monthdays in-month = %v, want 10-15 09:00", next)
	}
	next, _ = computeNextRun(sc, at("2026-10-16 09:00"))
	if !next.Equal(at("2026-12-01 09:00")) {
		t.Fatalf("monthdays next window = %v, want 12-01 09:00", next)
	}
}

func TestCronNext(t *testing.T) {
	cases := []struct {
		expr string
		from string
		want string
	}{
		{"0 9 * * *", "2026-10-16 10:00", "2026-10-17 09:00"},     // 每天 9 点
		{"0 9 * * 1", "2026-10-16 10:00", "2026-10-19 09:00"},     // 每周一（10-19 是周一）
		{"30 8 * * *", "2026-10-16 08:30", "2026-10-17 08:30"},    // 正好在触发点 → 次日
		{"*/15 * * * *", "2026-10-16 08:07", "2026-10-16 08:15"},  // 步进
		{"0 0 1 * *", "2026-10-16 10:00", "2026-11-01 00:00"},     // 每月 1 号零点（整点进位不丢）
		{"0 12 15 * *", "2026-10-16 10:00", "2026-11-15 12:00"},   // 月推进
		{"10,40 9 * * *", "2026-10-16 09:11", "2026-10-16 09:40"}, // 列表
		{"0 9-11 * * *", "2026-10-16 09:30", "2026-10-16 10:00"},  // 范围
		{"0 0 29 2 *", "2026-10-16 10:00", "2028-02-29 00:00"},    // 闰年 2 月 29
		{"0 9 * * 7", "2026-10-16 10:00", "2026-10-18 09:00"},     // 7 = 周日
	}
	for _, c := range cases {
		spec, err := parseCron(c.expr)
		if err != nil {
			t.Fatalf("parseCron(%q): %v", c.expr, err)
		}
		got := spec.next(at(c.from))
		if !got.Equal(at(c.want)) {
			t.Errorf("cron %q from %s = %v, want %s", c.expr, c.from, got, c.want)
		}
	}
}

func TestCronDomDowOr(t *testing.T) {
	// 标准语义：dom 与 dow 都限定时 OR——15 号或每周五
	spec, err := parseCron("0 0 15 * 5")
	if err != nil {
		t.Fatalf("parseCron: %v", err)
	}
	// 2026-10-16 周五：10-16（周五）先于 10-15（已过）→ 下一个是 10-16？from 取 10-15 01:00
	got := spec.next(at("2026-10-15 01:00"))
	if !got.Equal(at("2026-10-16 00:00")) {
		t.Fatalf("or-semantics next = %v, want 10-16 00:00 (friday)", got)
	}
	// 只限定 dom：dow 的 * 不参与
	spec2, _ := parseCron("0 0 15 * *")
	got2 := spec2.next(at("2026-10-16 00:00"))
	if !got2.Equal(at("2026-11-15 00:00")) {
		t.Fatalf("dom-only next = %v, want 11-15", got2)
	}
}

func TestCronNeverMatches(t *testing.T) {
	// 2 月 30 日不存在：零值（无下次触发）
	spec, err := parseCron("0 0 30 2 *")
	if err != nil {
		t.Fatalf("parseCron: %v", err)
	}
	if got := spec.next(at("2026-10-16 10:00")); !got.IsZero() {
		t.Fatalf("never-match = %v, want zero", got)
	}
}

func TestParseCronErrors(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "a * * * *", "*/0 * * * *", "5-1 * * * *", "1,2, * * *"} {
		if _, err := parseCron(expr); err == nil {
			t.Errorf("parseCron(%q) should fail", expr)
		}
	}
}

func TestDescribeSchedule(t *testing.T) {
	if got := describeSchedule(Schedule{Kind: ScheduleDelay, DelayMinutes: 5}); got != "one-shot in 5m" {
		t.Fatalf("delay desc = %q", got)
	}
	if got := describeSchedule(Schedule{Kind: ScheduleInterval, IntervalUnit: "minute", Interval: 20}); got != "every 20 minute(s)" {
		t.Fatalf("interval desc = %q", got)
	}
}
