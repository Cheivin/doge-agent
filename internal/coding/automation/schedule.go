package automation

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 枚举步数上限（day/week/month 均 ≈100 年，超出按无下次触发处理，防死循环）
const (
	maxDayIters   = 36600
	maxWeekIters  = 5220
	maxMonthIters = 1200
)

// computeNextRun 计算 from（不含）之后下一次触发时刻，本地时区。一次性
// delay 已过期返回零值（调用方据此终态化，绝不按公式重算"下一周期"复活）。
func computeNextRun(sc Schedule, from time.Time) (time.Time, error) {
	switch sc.Kind {
	case ScheduleDelay:
		at := time.Unix(sc.AnchorAt, 0).Add(time.Duration(sc.DelayMinutes) * time.Minute)
		if !at.After(from) {
			return time.Time{}, nil
		}
		return at, nil
	case ScheduleInterval:
		return nextInterval(sc, from)
	case ScheduleCron:
		spec, err := parseCron(sc.Cron)
		if err != nil {
			return time.Time{}, err
		}
		return spec.next(from), nil
	}
	return time.Time{}, fmt.Errorf("unknown schedule kind %q", sc.Kind)
}

// nextInterval interval 锚点推导：不对齐墙钟（11:46 建"每 10 分钟"从 11:56
// 起，不是 11:50），day/week/month 走日历枚举并结合 At/Weekdays/MonthDays。
func nextInterval(sc Schedule, from time.Time) (time.Time, error) {
	anchor := time.Unix(sc.AnchorAt, 0)
	atH, atM := anchor.Hour(), anchor.Minute()
	if sc.At != "" {
		atH, atM = atoi2(sc.At[:2]), atoi2(sc.At[3:])
	}
	switch sc.IntervalUnit {
	case "minute":
		step := int64(sc.Interval) * 60
		diff := from.Unix() - sc.AnchorAt
		if diff < 0 {
			diff = 0
		}
		k := diff/step + 1
		return time.Unix(sc.AnchorAt+k*step, 0), nil
	case "hour":
		step := time.Duration(sc.Interval) * time.Hour
		// 锚点"当日当时：atM"作第 0 候选：按本地钟表字段构造，不能用
		// Truncate(time.Hour)（按 UTC 截断，半小时时区错位十几分钟）
		base := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), anchor.Hour(), atM, 0, 0, anchor.Location())
		diff := from.Sub(base)
		k := int64(1)
		if diff > 0 {
			k = int64(diff/step) + 1
		}
		return base.Add(time.Duration(k) * step), nil
	case "day":
		first := atTime(anchor, atH, atM)
		for i := 0; i <= maxDayIters; i++ {
			cand := first.AddDate(0, 0, i*sc.Interval)
			if cand.After(from) {
				return cand, nil
			}
		}
		return time.Time{}, nil
	case "week":
		return nextWeek(sc, from, anchor, atH, atM)
	case "month":
		return nextMonth(sc, from, anchor, atH, atM)
	}
	return time.Time{}, fmt.Errorf("invalid intervalUnit %q", sc.IntervalUnit)
}

// nextWeek 周级枚举：锚点所在周为第 0 周，每 step 周一个窗口；Weekdays 空=每天
func nextWeek(sc Schedule, from, anchor time.Time, atH, atM int) (time.Time, error) {
	allowed := map[int]bool{}
	for _, w := range sc.Weekdays {
		allowed[w] = true
	}
	weekStart := atTime(anchor, 0, 0)
	weekStart = weekStart.AddDate(0, 0, -((int(weekStart.Weekday()) + 6) % 7)) // 归到周一
	for w := 0; w <= maxWeekIters; w++ {
		for d := 0; d < 7; d++ {
			cand := weekStart.AddDate(0, 0, w*sc.Interval*7+d)
			if len(allowed) > 0 && !allowed[int(cand.Weekday())] {
				continue
			}
			at := atTime(cand, atH, atM)
			if at.After(from) {
				return at, nil
			}
		}
	}
	return time.Time{}, nil
}

// nextMonth 月级枚举：锚点月为第 0 月，每 step 月一个窗口；MonthDays 空=锚点当日；
// 2/30 类溢出（日期滚进下月）直接跳过该候选
func nextMonth(sc Schedule, from, anchor time.Time, atH, atM int) (time.Time, error) {
	days := sc.MonthDays
	if len(days) == 0 {
		days = []int{anchor.Day()}
	}
	sorted := append([]int(nil), days...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	firstOfMonth := time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, anchor.Location())
	for m := 0; m <= maxMonthIters; m++ {
		base := firstOfMonth.AddDate(0, m*sc.Interval, 0)
		for _, d := range sorted {
			cand := time.Date(base.Year(), base.Month(), d, atH, atM, 0, 0, base.Location())
			if cand.Day() != d {
				// 短月钳到月末：1/31 锚定的月任务 2 月跑 2/28（4/30、6/30 同理），
				// 不再静默跳过短月
				cand = time.Date(base.Year(), base.Month()+1, 0, atH, atM, 0, 0, base.Location())
			}
			if cand.After(from) {
				return cand, nil
			}
		}
	}
	return time.Time{}, nil
}

// atTime 取 t 的日期配上指定时刻
func atTime(t time.Time, h, m int) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), h, m, 0, 0, t.Location())
}

// cronSpec 5 字段 cron 的位图（bit v = 值 v 允许）；domStar/dowStar 记录该字段
// 是否为 *，用于标准的 dom/dow OR 语义
type cronSpec struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

// parseCron 解析 5 字段（分 时 日 月 周）表达式，支持 * , - /；周日=0（7 亦认作周日）
func parseCron(expr string) (*cronSpec, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour dom month dow)", expr)
	}
	minute, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("cron minute: %v", err)
	}
	hour, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("cron hour: %v", err)
	}
	dom, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("cron dom: %v", err)
	}
	month, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("cron month: %v", err)
	}
	dowField, err := parseCronField(fields[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("cron dow: %v", err)
	}
	dowBits := dowField.bits
	if dowBits>>7&1 == 1 { // 7 = 周日
		dowBits |= 1
		dowBits &^= 1 << 7
	}
	return &cronSpec{
		minute: minute.bits, hour: hour.bits, dom: dom.bits, month: month.bits, dow: dowBits,
		domStar: dom.star, dowStar: dowField.star,
	}, nil
}

// cronField 单字段解析结果
type cronField struct {
	bits uint64
	star bool
}

// parseCronField 解析单个字段：*、a、a-b、a-b/n、*/n、a/n（a 起到字段上限）、逗号列表
func parseCronField(f string, min, max int) (cronField, error) {
	out := cronField{}
	if f == "" {
		return out, fmt.Errorf("empty field")
	}
	for _, part := range strings.Split(f, ",") {
		step := 1
		body := part
		if i := strings.Index(part, "/"); i >= 0 {
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s <= 0 {
				return out, fmt.Errorf("bad step in %q", part)
			}
			step = s
			body = part[:i]
		}
		lo, hi := min, max
		switch {
		case body == "*" && step == 1:
			// 纯 * 才是无限制字段；*/n 是受限字段（步进位图），不得置 star
			//（否则 0 9 * * */2 的 dayOK 恒真，每天触发）
			out.star = true
		case body == "*":
			// */n：全范围按 n 步进的受限字段，lo/hi 已是字段边界
		case strings.Contains(body, "-"):
			dash := strings.Index(body, "-")
			l, err1 := strconv.Atoi(body[:dash])
			h, err2 := strconv.Atoi(body[dash+1:])
			if err1 != nil || err2 != nil {
				return out, fmt.Errorf("bad range %q", body)
			}
			lo, hi = l, h
		default:
			v, err := strconv.Atoi(body)
			if err != nil {
				return out, fmt.Errorf("bad value %q", body)
			}
			lo = v
			if strings.Contains(part, "/") {
				hi = max // vixie 语义：a/n = a 到上限按 n 步进
			} else {
				hi = v
			}
		}
		if lo < min || hi > max || lo > hi {
			return out, fmt.Errorf("value out of range (%d-%d) in %q", min, max, part)
		}
		for v := lo; v <= hi; v += step {
			out.bits |= 1 << v
		}
	}
	if out.bits == 0 {
		return out, fmt.Errorf("field matches nothing: %q", f)
	}
	return out, nil
}

// next 从 from（不含）起找下一个匹配时刻，本地时区。逐字段进位：字段不匹配
// 就跳到该字段的下一个允许值，低字段重置为最小允许值（重置为 0 会漏掉
// "分钟只允许 30" 这类表达式）。每轮严格前进，迭代数远小于分钟枚举。
// DST：日跳转一律落到次日 00:00（恒存在，无规范化漂移）；时刻候选构造后
// 校验墙钟未漂移（time.Date 对缺口时刻会顺延，视为该日该墙钟不存在），
// 杜绝"构造出 03:30 → hour 位图只许 2 → 每轮跳一天 → 2000 轮空转返回零值"
// 的春令时死循环（曾把纽约 30 2 * * * 的循环任务永久停用）
func (c *cronSpec) next(from time.Time) time.Time {
	firstMin := lowestBit(c.minute)
	t := from.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 2000; i++ {
		y, m, d := t.Date()
		loc := t.Location()
		if c.month&(1<<int(m)) == 0 {
			// 跳到下一个允许月的 1 号 00:00，时分交给下方字段检查
			nm := time.Date(y, m, 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
			for c.month&(1<<int(nm.Month())) == 0 {
				nm = nm.AddDate(0, 1, 0)
			}
			t = nm
			continue
		}
		if !c.dayOK(t) {
			// 跳到下一日历日 00:00（可能滚进下月，月检查下一轮重查）
			t = time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}
		if c.hour&(1<<t.Hour()) == 0 {
			h := nextBit(c.hour, t.Hour())
			for h >= 0 {
				cand := time.Date(y, m, d, h, firstMin, 0, 0, loc)
				if !clockDrift(cand, h, firstMin) {
					break
				}
				h = nextBit(c.hour, h) // 该墙钟当日不存在（DST 缺口），试下一个
			}
			if h >= 0 {
				t = time.Date(y, m, d, h, firstMin, 0, 0, loc)
			} else {
				t = time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			}
			continue
		}
		if c.minute&(1<<t.Minute()) == 0 {
			m2 := nextBit(c.minute, t.Minute())
			found := -1
			for m2 >= 0 {
				cand := time.Date(y, m, d, t.Hour(), m2, 0, 0, loc)
				if !clockDrift(cand, t.Hour(), m2) {
					found = m2
					break
				}
				m2 = nextBit(c.minute, m2)
			}
			if found >= 0 {
				t = time.Date(y, m, d, t.Hour(), found, 0, 0, loc)
				continue
			}
			if h := nextBit(c.hour, t.Hour()); h >= 0 {
				t = time.Date(y, m, d, h, firstMin, 0, 0, loc)
			} else {
				t = time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			}
			continue
		}
		return t
	}
	// 合理范围内无匹配（如 2 月 30 日）：视为无下次触发
	return time.Time{}
}

// clockDrift 报告构造的墙钟是否被 time.Date 规范化漂移（DST 缺口时刻不
// 存在，会顺延到其后首个有效时刻）。漂移即该日该墙钟不可触发
func clockDrift(t time.Time, h, m int) bool {
	return t.Hour() != h || t.Minute() != m
}

// lowestBit 位图最小允许值
func lowestBit(bits uint64) int {
	for v := 0; v < 64; v++ {
		if bits&(1<<v) != 0 {
			return v
		}
	}
	return 0
}

// nextBit 位图中大于 cur 的最小允许值（无则 -1）
func nextBit(bits uint64, cur int) int {
	for v := cur + 1; v < 64; v++ {
		if bits&(1<<v) != 0 {
			return v
		}
	}
	return -1
}

// dayOK 日字段判定：标准 cron 语义——两字段都限定时 OR，仅一个限定时 AND
func (c *cronSpec) dayOK(t time.Time) bool {
	domOK := c.dom&(1<<t.Day()) != 0
	dowOK := c.dow&(1<<int(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}

// describeSchedule 调度的可读描述（工具 list/get 与日志用）
func describeSchedule(sc Schedule) string {
	switch sc.Kind {
	case ScheduleDelay:
		return fmt.Sprintf("one-shot in %dm", sc.DelayMinutes)
	case ScheduleInterval:
		desc := fmt.Sprintf("every %d %s(s)", sc.Interval, sc.IntervalUnit)
		if sc.At != "" {
			desc += " at " + sc.At
		}
		if len(sc.Weekdays) > 0 {
			desc += fmt.Sprintf(" weekdays=%v", sc.Weekdays)
		}
		if len(sc.MonthDays) > 0 {
			desc += fmt.Sprintf(" monthDays=%v", sc.MonthDays)
		}
		return desc
	case ScheduleCron:
		return "cron " + sc.Cron
	}
	return "unknown schedule"
}
