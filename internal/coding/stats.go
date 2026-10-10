package coding

import (
	"math"

	"doge/internal/ai"
)

// SessionStats aggregates token usage across a conversation. It is the
// payload of the /stats command (TUI), the web stats panel, and the
// GET /api/stats endpoint.
type SessionStats struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`

	Messages          int `json:"messages"`
	UserMessages      int `json:"userMessages"`
	AssistantMessages int `json:"assistantMessages"`
	ToolMessages      int `json:"toolMessages"`

	InputTokens     int `json:"inputTokens"`
	OutputTokens    int `json:"outputTokens"`
	ReasoningTokens int `json:"reasoningTokens"`
	CacheRead       int `json:"cacheRead"`
	CacheWrite      int `json:"cacheWrite"`
	TotalTokens     int `json:"totalTokens"`

	// ContextTokens 上下文占用：最后一条带 Usage 的 assistant 消息的 Input+CacheRead
	ContextTokens int `json:"contextTokens"`
	// ContextWindow 当前模型的上下文窗口
	ContextWindow int `json:"contextWindow"`

	// CacheHitPct is cache-read tokens as a share of the total prompt tokens
	// sent to the model (Usage.Input, which already includes the cached
	// portions), in 0..100.
	CacheHitPct float64 `json:"cacheHitPct"`
	// Cost is the summed estimated cost in the display currency (0 when the
	// model has no configured prices in models.json). Currency carries the
	// unit/symbol that prefixes it ("$" when unset).
	Cost     float64 `json:"cost"`
	Currency string  `json:"currency,omitempty"`
}

// ComputeSessionStats aggregates the assistant usage carried by each message.
// Messages without usage are counted but contribute no tokens. lookup
// resolves the cost policy for each message's model (may be nil, in which
// case costs stay raw and the currency is "$").
func ComputeSessionStats(messages []*ai.Message, lookup func(provider, model string) CostPolicy) SessionStats {
	var st SessionStats
	var sent int
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		st.Messages++
		switch msg.Role {
		case ai.RoleUser:
			st.UserMessages++
		case ai.RoleAssistant:
			st.AssistantMessages++
			if st.Model == "" && msg.Model != "" {
				provider := msg.Provider
				if provider == "" {
					provider = "?"
				}
				st.Model = provider + "/" + msg.Model
			}
		case ai.RoleToolResult:
			st.ToolMessages++
		}
		u := msg.Usage
		if u == nil {
			continue
		}
		st.InputTokens += u.Input
		st.OutputTokens += u.Output
		st.ReasoningTokens += u.Reasoning
		st.CacheRead += u.CacheRead
		st.CacheWrite += u.CacheWrite
		if u.TotalTokens > 0 {
			st.TotalTokens += u.TotalTokens
		}
		policy := CostPolicy{}
		if lookup != nil {
			policy = lookup(msg.Provider, msg.Model)
		}
		if st.Currency == "" && policy.Currency != "" {
			st.Currency = policy.Currency
		}
		st.Cost += policy.Apply(u.Cost, msg.Timestamp).Total
		// Usage.Input is the total prompt (incl. cached); CacheRead is the
		// cached subset of it, so the hit rate is CacheRead / Input.
		sent += u.Input
	}
	if sent > 0 {
		st.CacheHitPct = math.Round(float64(st.CacheRead)/float64(sent)*1e3) / 10
	}
	if st.Cost > 0 {
		st.Cost = math.Round(st.Cost*1e6) / 1e6 // cents precision
	}
	return st
}
