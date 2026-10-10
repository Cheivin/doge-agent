package coding

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
)

// tsAt returns a Unix-millis timestamp at the given local HH:MM on a fixed day.
func tsAt(t *testing.T, hhmm string) int64 {
	t.Helper()
	tt, err := time.ParseInLocation("15:04", hhmm, time.Local)
	if err != nil {
		t.Fatalf("bad time %q: %v", hhmm, err)
	}
	d := time.Date(2026, 8, 19, tt.Hour(), tt.Minute(), 0, 0, time.Local)
	return d.UnixMilli()
}

func TestCostPolicyZeroValue(t *testing.T) {
	var p CostPolicy
	if !p.NoOp() {
		t.Fatal("zero policy should be a no-op")
	}
	if got := p.timeMultiplier(tsAt(t, "10:00")); got != 1 {
		t.Fatalf("zero policy multiplier = %v, want 1", got)
	}
	c := ai.UsageCost{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, Total: 10}
	if got := p.Apply(c, tsAt(t, "10:00")); got != c {
		t.Fatalf("zero policy changed cost: %+v", got)
	}
}

func TestCostPolicyMultiplier(t *testing.T) {
	p := CostPolicy{Currency: "¥", Multiplier: 7.2}
	if p.NoOp() {
		t.Fatal("multiplier policy should not be a no-op")
	}
	c := ai.UsageCost{Input: 1, Output: 1, Total: 2}
	got := p.Apply(c, 0)
	want := ai.UsageCost{Input: 7.2, Output: 7.2, Total: 14.4}
	if got != want {
		t.Fatalf("multiplier apply = %+v, want %+v", got, want)
	}
	// A conversion multiplier is independent of time of day.
	if got := p.timeMultiplier(tsAt(t, "10:00")); got != 1 {
		t.Fatalf("no-period time multiplier = %v, want 1", got)
	}
}

func TestCostPolicyPeakValleySingleWindow(t *testing.T) {
	p := CostPolicy{Periods: []ai.PeakValleyPeriod{
		{Start: "09:00", End: "11:00", Multiplier: 1.5},
	}}
	cases := []struct {
		at   string
		want float64
	}{
		{"08:59", 1},
		{"09:00", 1.5}, // half-open: start is included
		{"10:00", 1.5},
		{"10:59", 1.5},
		{"11:00", 1}, // end is excluded
		{"23:59", 1},
	}
	for _, c := range cases {
		if got := p.timeMultiplier(tsAt(t, c.at)); got != c.want {
			t.Errorf("multiplier at %s = %v, want %v", c.at, got, c.want)
		}
	}
}

func TestCostPolicyMultipleWindowsWrapMidnight(t *testing.T) {
	p := CostPolicy{Periods: []ai.PeakValleyPeriod{
		{Start: "23:00", End: "07:00", Multiplier: 0.5}, // valley, wraps midnight
		{Start: "09:00", End: "11:00", Multiplier: 1.5}, // peak
	}}
	cases := []struct {
		at   string
		want float64
	}{
		{"00:00", 0.5},
		{"06:59", 0.5},
		{"07:00", 1},
		{"10:00", 1.5},
		{"15:00", 1},
		{"22:59", 1},
		{"23:00", 0.5},
	}
	for _, c := range cases {
		if got := p.timeMultiplier(tsAt(t, c.at)); got != c.want {
			t.Errorf("multiplier at %s = %v, want %v", c.at, got, c.want)
		}
	}
}

func TestCostPolicyFullDayAndDegenerate(t *testing.T) {
	allDay := CostPolicy{Periods: []ai.PeakValleyPeriod{
		{Start: "00:00", End: "24:00", Multiplier: 2},
	}}
	if got := allDay.timeMultiplier(tsAt(t, "13:37")); got != 2 {
		t.Fatalf("full-day multiplier = %v, want 2", got)
	}
	// A zero-length window (start == end) matches nothing.
	degenerate := CostPolicy{Periods: []ai.PeakValleyPeriod{
		{Start: "09:00", End: "09:00", Multiplier: 3},
	}}
	if got := degenerate.timeMultiplier(tsAt(t, "09:00")); got != 1 {
		t.Fatalf("degenerate window multiplier = %v, want 1", got)
	}
}

func TestCostPolicyMalformedConfig(t *testing.T) {
	p := CostPolicy{Periods: []ai.PeakValleyPeriod{
		{Start: "oops", End: "11:00", Multiplier: 1.5},
		{Start: "09:00", End: "11:00", Multiplier: 0},
		{Start: "09:00", End: "11:00", Multiplier: -1},
	}}
	if got := p.timeMultiplier(tsAt(t, "10:00")); got != 1 {
		t.Fatalf("malformed multiplier = %v, want 1", got)
	}
	// No timestamp (0) never matches a window.
	if got := p.timeMultiplier(0); got != 1 {
		t.Fatalf("ts=0 multiplier = %v, want 1", got)
	}
}

func TestCostPolicyFromModel(t *testing.T) {
	cost := ai.ModelCost{
		Input: 2, Output: 8, Currency: "¥", Multiplier: 7.2,
		PeakValley: &ai.PeakValley{Periods: []ai.PeakValleyPeriod{
			{Start: "23:00", End: "07:00", Multiplier: 0.5},
		}},
	}
	p := CostPolicyFromModel(cost)
	if p.Currency != "¥" || p.Multiplier != 7.2 || len(p.Periods) != 1 {
		t.Fatalf("policy from model = %+v", p)
	}
	// 02:00 inside the valley: 7.2 × 0.5 = 3.6 on a 1.0 base total.
	if got := p.Apply(ai.UsageCost{Total: 1}, tsAt(t, "02:00")); got.Total != 3.6 {
		t.Fatalf("valley total = %v, want 3.6", got.Total)
	}
	// No peakValley config → no periods.
	if p2 := CostPolicyFromModel(ai.ModelCost{Currency: "€"}); len(p2.Periods) != 0 || p2.Currency != "€" {
		t.Fatalf("unexpected policy: %+v", p2)
	}
}

func TestComputeSessionStatsWithLookup(t *testing.T) {
	lookup := func(provider, model string) CostPolicy {
		if provider == "p" && model == "m" {
			return CostPolicy{Currency: "¥", Multiplier: 2, Periods: []ai.PeakValleyPeriod{
				{Start: "09:00", End: "11:00", Multiplier: 3},
				{Start: "23:00", End: "07:00", Multiplier: 0.5},
			}}
		}
		return CostPolicy{}
	}
	msgs := []*ai.Message{
		{Role: ai.RoleUser, Content: []ai.ContentBlock{ai.TextBlock("hi")}},
		{Role: ai.RoleAssistant, Provider: "p", Model: "m", Timestamp: tsAt(t, "10:00"),
			Usage: &ai.Usage{Input: 100, Output: 100,
				Cost: ai.UsageCost{Input: 0.0001, Output: 0.001, Total: 0.0011}}},
		{Role: ai.RoleToolResult, Provider: "p", Model: "m", Timestamp: tsAt(t, "02:00"),
			Usage: &ai.Usage{Input: 1000,
				Cost: ai.UsageCost{Input: 0.001, Total: 0.001}}},
	}
	st := ComputeSessionStats(msgs, lookup)
	if st.Currency != "¥" {
		t.Fatalf("currency = %q, want ¥", st.Currency)
	}
	if st.Messages != 3 || st.AssistantMessages != 1 || st.ToolMessages != 1 {
		t.Fatalf("message counts wrong: %+v", st)
	}
	// Assistant at 10:00 → peak ×3 then ×2 = ×6: 0.0011×6 = 0.0066
	// Tool at 02:00 → valley ×0.5 then ×2 = ×1: 0.001
	want := 0.0066 + 0.001
	if st.Cost != want {
		t.Fatalf("cost = %v, want %v", st.Cost, want)
	}
	// Unknown models / no lookup fall back to raw costs.
	stRaw := ComputeSessionStats(msgs, func(string, string) CostPolicy { return CostPolicy{} })
	if stRaw.Cost != 0.0011+0.001 {
		t.Fatalf("raw cost = %v, want %v", stRaw.Cost, 0.0011+0.001)
	}
}

func TestModelRuntimeReload(t *testing.T) {
	dir := t.TempDir()
	modelsFile := filepath.Join(dir, "models.json")
	r := NewModelRuntime(LoadAuthStore(filepath.Join(dir, "auth.json")))

	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(modelsFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`[{"id":"m1","provider":"openai","name":"M1","api":"openai_completions","cost":{"input":1,"output":2,"currency":"¥","multiplier":7.2}}]`)
	if err := r.Reload(modelsFile); err != nil {
		t.Fatalf("reload: %v", err)
	}
	m, ok := r.Models.GetModel("openai", "m1")
	if !ok {
		t.Fatal("custom model not registered after reload")
	}
	if m.Cost.Multiplier != 7.2 || m.Cost.Currency != "¥" {
		t.Fatalf("reloaded cost config = %+v", m.Cost)
	}
	// A second reload with changed prices replaces the first.
	write(`[{"id":"m1","provider":"openai","name":"M1","api":"openai_completions","cost":{"input":3,"output":6,"multiplier":1}}]`)
	if err := r.Reload(modelsFile); err != nil {
		t.Fatalf("reload 2: %v", err)
	}
	m2, _ := r.Models.GetModel("openai", "m1")
	if m2.Cost.Input != 3 || m2.Cost.Currency != "" || m2.Cost.Multiplier != 1 {
		t.Fatalf("second reload cost = %+v", m2.Cost)
	}
	// A broken file keeps the previous catalog.
	before := len(r.Models.AllModels())
	if err := os.WriteFile(modelsFile, []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(modelsFile); err == nil {
		t.Fatal("expected error on broken models.json")
	} else if len(r.Models.AllModels()) != before {
		t.Fatal("broken reload changed the catalog")
	}
}

func TestAuthStoreReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(`{"openai":{"type":"api_key","key":"k1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := LoadAuthStore(path)
	if k, _ := s.GetAPIKey("openai"); k != "k1" {
		t.Fatalf("initial key = %q, want k1", k)
	}
	if err := os.WriteFile(path, []byte(`{"openai":{"type":"api_key","key":"k2"},"anthropic":{"type":"api_key","key":"ka"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if k, _ := s.GetAPIKey("openai"); k != "k2" {
		t.Fatalf("reloaded key = %q, want k2", k)
	}
	if k, _ := s.GetAPIKey("anthropic"); k != "ka" {
		t.Fatalf("new provider key missing: %q", k)
	}
	// A missing file clears keys.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("reload missing: %v", err)
	}
	if _, ok := s.GetAPIKey("openai"); ok {
		t.Fatal("keys not cleared after file removal")
	}
}

func TestAgentSessionCostPolicyFor(t *testing.T) {
	runtime := NewModelRuntime(LoadAuthStore(filepath.Join(t.TempDir(), "auth.json")))
	runtime.Models.AddModel(&ai.Model{ID: "m", Provider: "p",
		Cost: ai.ModelCost{Currency: "¥", Multiplier: 7.2}})
	s := &AgentSession{ModelRuntime: runtime}
	if p := s.CostPolicyFor("p", "m"); p.Currency != "¥" || p.Multiplier != 7.2 {
		t.Fatalf("policy for p/m = %+v", p)
	}
	if p := s.CostPolicyFor("p", "nope"); !p.NoOp() {
		t.Fatalf("unknown model policy should be a no-op: %+v", p)
	}
}

func TestRefreshCurrentModel(t *testing.T) {
	dir := t.TempDir()
	modelsFile := filepath.Join(dir, "models.json")
	runtime := NewModelRuntime(LoadAuthStore(filepath.Join(dir, "auth.json")))
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(modelsFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`[{"id":"m1","provider":"p","name":"M1","api":"openai_completions","cost":{"input":1,"output":2}}]`)
	if err := runtime.Reload(modelsFile); err != nil {
		t.Fatal(err)
	}
	m, _ := runtime.Models.GetModel("p", "m1")
	s := &AgentSession{ModelRuntime: runtime,
		Agent: agent.NewAgent(agent.AgentOptions{Model: m})}

	// Change the price, rebuild the catalog, then refresh the current model.
	write(`[{"id":"m1","provider":"p","name":"M1","api":"openai_completions","cost":{"input":5,"output":10,"currency":"¥","multiplier":7.2}}]`)
	if err := runtime.Reload(modelsFile); err != nil {
		t.Fatal(err)
	}
	s.refreshCurrentModel()
	got := s.Agent.State().Model
	if got.Cost.Input != 5 || got.Cost.Output != 10 || got.Cost.Currency != "¥" {
		t.Fatalf("current model price after refresh = %+v", got.Cost)
	}
	if got.Provider != "p" || got.ID != "m1" {
		t.Fatalf("model identity changed: %s/%s", got.Provider, got.ID)
	}
	// A model that vanished from models.json keeps running unchanged.
	write(`[]`)
	if err := runtime.Reload(modelsFile); err != nil {
		t.Fatal(err)
	}
	s.refreshCurrentModel()
	if got := s.Agent.State().Model; got.Cost.Input != 5 {
		t.Fatalf("vanished model should keep its definition, got %+v", got.Cost)
	}
}

// TestComputeSessionStatsCacheHitRate verifies the cache hit rate is
// CacheRead / InputTokens, where InputTokens is the total prompt (which
// already includes the cached portion). Reproduces the user-reported
// 162k/193k case (was wrongly computed as 45.6%).
func TestComputeSessionStatsCacheHitRate(t *testing.T) {
	msgs := []*ai.Message{
		{Role: ai.RoleAssistant, Provider: "p", Model: "m", Usage: &ai.Usage{
			Input: 193, Output: 11, CacheRead: 162, CacheWrite: 0,
		}},
	}
	st := ComputeSessionStats(msgs, nil)
	// 162/193 = 0.8393 → 83.9%
	if st.CacheHitPct != 83.9 {
		t.Errorf("cache hit pct = %v, want 83.9 (162/193)", st.CacheHitPct)
	}
	if st.InputTokens != 193 {
		t.Errorf("input tokens = %v, want 193 (total prompt)", st.InputTokens)
	}
}
