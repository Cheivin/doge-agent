package coding

import (
	"strings"
	"time"

	"doge/internal/ai"
)

// CostPolicy adjusts a model's base cost for display: a currency conversion
// multiplier and per-window time-of-day multipliers (peak/valley pricing).
// The zero value leaves costs unchanged.
type CostPolicy struct {
	Currency   string
	Multiplier float64
	Periods    []ai.PeakValleyPeriod
}

// CostPolicyFromModel derives the display policy from a model's cost config
// (models.json). Prices are per-million tokens in any currency; Multiplier
// converts them to the display unit.
func CostPolicyFromModel(cost ai.ModelCost) CostPolicy {
	p := CostPolicy{Currency: cost.Currency, Multiplier: cost.Multiplier}
	if cost.PeakValley != nil {
		p.Periods = cost.PeakValley.Periods
	}
	return p
}

// CostPolicyFor resolves the cost policy for the message's model from the
// catalog; a zero policy (raw cost, "$") is returned when unknown.
func (s *AgentSession) CostPolicyFor(provider, model string) CostPolicy {
	if s == nil || s.ModelRuntime == nil || provider == "" {
		return CostPolicy{}
	}
	if m, ok := s.ModelRuntime.Models.GetModel(provider, model); ok {
		return CostPolicyFromModel(m.Cost)
	}
	return CostPolicy{}
}

// NoOp reports whether the policy leaves costs unchanged (no conversion
// multiplier and no time-of-day windows).
func (p CostPolicy) NoOp() bool {
	return p.multiplier() == 1 && len(p.Periods) == 0
}

// multiplier returns the display conversion factor, defaulting to 1.
func (p CostPolicy) multiplier() float64 {
	if p.Multiplier > 0 {
		return p.Multiplier
	}
	return 1
}

// timeMultiplier returns the time-of-day cost multiplier for a
// Unix-millisecond timestamp (local wall clock), 1 when no window matches or
// none configured.
func (p CostPolicy) timeMultiplier(ts int64) float64 {
	if len(p.Periods) == 0 || ts <= 0 {
		return 1
	}
	t := time.UnixMilli(ts)
	mins := t.Hour()*60 + t.Minute()
	for _, per := range p.Periods {
		if periodContains(per, mins) {
			if per.Multiplier > 0 {
				return per.Multiplier
			}
			return 1
		}
	}
	return 1
}

// Apply scales a base UsageCost into the display currency for the message
// timestamp (multiplier × time-of-day multiplier).
func (p CostPolicy) Apply(cost ai.UsageCost, ts int64) ai.UsageCost {
	m := p.multiplier() * p.timeMultiplier(ts)
	cost.Input *= m
	cost.Output *= m
	cost.CacheRead *= m
	cost.CacheWrite *= m
	cost.Total *= m
	return cost
}

// periodContains reports whether the given minute-of-day (0..1439) falls
// inside the half-open window [Start, End), handling windows that wrap
// midnight. A degenerate zero-length window (Start == End) matches nothing.
func periodContains(p ai.PeakValleyPeriod, mins int) bool {
	start := parseHM(p.Start)
	end := parseHM(p.End)
	if start < 0 || end < 0 {
		return false
	}
	if start < end {
		return mins >= start && mins < end
	}
	if start > end {
		return mins >= start || mins < end // wraps past midnight
	}
	return false
}

// parseHM parses "HH:MM" into minutes since midnight; -1 when malformed.
// "24:00" is accepted as end-of-day for full-day windows.
func parseHM(s string) int {
	s = strings.TrimSpace(s)
	if s == "24:00" {
		return 24 * 60
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return -1
	}
	return t.Hour()*60 + t.Minute()
}
