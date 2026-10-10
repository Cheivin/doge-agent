package ai

import (
	"encoding/json"
	"testing"
)

func TestContentBlockRoundTrip(t *testing.T) {
	blocks := []ContentBlock{
		TextBlock("hello"),
		{Type: ContentTypeText, Text: "sig", TextSignature: "abc123"},
		ThinkingBlock("reasoning here"),
		{Type: ContentTypeThinking, Thinking: "r", Redacted: true},
		ImageBlock([]byte{0x89, 0x50, 0x4e, 0x47}, "image/png"),
		ToolCallBlock("call_1", "bash", map[string]any{"command": "ls", "n": 1.0}),
		ToolCallBlock("call_2", "empty", nil),
	}
	data, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded []ContentBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded) != len(blocks) {
		t.Fatalf("got %d blocks, want %d", len(decoded), len(blocks))
	}
	for i, b := range decoded {
		want := blocks[i]
		if b.Type != want.Type {
			t.Errorf("block %d: type = %q, want %q", i, b.Type, want.Type)
		}
		switch want.Type {
		case ContentTypeText:
			if b.Text != want.Text || b.TextSignature != want.TextSignature {
				t.Errorf("block %d: text round trip mismatch: %+v", i, b)
			}
		case ContentTypeThinking:
			if b.Thinking != want.Thinking || b.Redacted != want.Redacted || b.ThinkingSignature != want.ThinkingSignature {
				t.Errorf("block %d: thinking round trip mismatch: %+v", i, b)
			}
		case ContentTypeImage:
			if string(b.Data) != string(want.Data) || b.MimeType != want.MimeType {
				t.Errorf("block %d: image round trip mismatch: %+v", i, b)
			}
		case ContentTypeToolCall:
			if b.ToolCallID != want.ToolCallID || b.ToolName != want.ToolName {
				t.Errorf("block %d: tool call id/name mismatch: %+v", i, b)
			}
			if len(b.Arguments) != len(want.Arguments) {
				t.Errorf("block %d: arguments mismatch: %+v vs %+v", i, b.Arguments, want.Arguments)
			}
			if want.ToolCallID == "call_2" {
				// nil arguments must round-trip as an object, not null.
				raw, _ := json.Marshal(b)
				if len(b.Arguments) != 0 {
					t.Errorf("block %d: expected empty arguments object, got %v", i, b.Arguments)
				}
				if jsonContains(raw, `"arguments":{}`) == false {
					t.Errorf("block %d: arguments should be serialized as an object, got %s", i, raw)
				}
			}
		}
	}
}

func jsonContains(data []byte, substr string) bool {
	return len(data) >= len(substr) && string(data) != "" && contains(string(data), substr)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestMessageRoundTrip(t *testing.T) {
	cases := []*Message{
		NewUserTextMessage("hello world"),
		{
			Role: RoleUser,
			Content: []ContentBlock{
				TextBlock("what is in this image?"),
				ImageBlock([]byte("imagedata"), "image/jpeg"),
			},
			Timestamp: 1750000000000,
		},
		{
			Role:       RoleAssistant,
			Content:    []ContentBlock{TextBlock("I'll check."), ToolCallBlock("c1", "read", map[string]any{"path": "a.txt"})},
			API:        "openai-responses",
			Provider:   "openai",
			Model:      "gpt-5.2",
			StopReason: StopReasonToolUse,
			Usage: &Usage{
				Input: 100, Output: 50, TotalTokens: 150,
				Cost: UsageCost{Input: 0.1, Output: 0.2, Total: 0.3},
			},
			Timestamp: 1750000000001,
		},
		{
			Role: RoleToolResult, ToolCallID: "c1", ToolName: "read",
			Content:   []ContentBlock{TextBlock("file contents")},
			Details:   map[string]any{"truncated": false},
			IsError:   false,
			Timestamp: 1750000000002,
		},
	}
	for i, m := range cases {
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		var got Message
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("case %d unmarshal: %v (json: %s)", i, err, data)
		}
		if got.Role != m.Role {
			t.Errorf("case %d: role = %q, want %q", i, got.Role, m.Role)
		}
		if len(got.Content) != len(m.Content) {
			t.Errorf("case %d: content len = %d, want %d (json: %s)", i, len(got.Content), len(m.Content), data)
			continue
		}
		for j := range got.Content {
			if got.Content[j].Type != m.Content[j].Type {
				t.Errorf("case %d: block %d type mismatch: %+v", i, j, got.Content[j])
			}
		}
		if got.Timestamp != m.Timestamp {
			t.Errorf("case %d: timestamp = %d, want %d", i, got.Timestamp, m.Timestamp)
		}
		if m.StopReason != "" && got.StopReason != m.StopReason {
			t.Errorf("case %d: stopReason = %q, want %q", i, got.StopReason, m.StopReason)
		}
	}
}

func TestUserMessageContentStringEncoding(t *testing.T) {
	m := NewUserTextMessage("hi")
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["content"].(string); !ok {
		t.Errorf("single text user message content should be a string, got %s", data)
	}
	// Assistant messages always encode content as an array.
	am := &Message{Role: RoleAssistant, Content: []ContentBlock{TextBlock("hi")}}
	data, _ = json.Marshal(am)
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["content"].([]any); !ok {
		t.Errorf("assistant message content should be an array, got %s", data)
	}
}

func TestToolResultDetailsDefault(t *testing.T) {
	m := &Message{Role: RoleToolResult, ToolCallID: "c", ToolName: "t"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["details"].(map[string]any); !ok {
		t.Errorf("tool result details should default to an object, got %s", data)
	}
}

// TestSessionLineCompat parses a session line in the on-disk JSONL format.
func TestPiSessionLineCompat(t *testing.T) {
	line := `{"role":"assistant","content":[{"type":"text","text":"Let me check the file."}],"api":"openai-responses","provider":"openai","model":"gpt-5.2","usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":15,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":1750000000000}`
	var m Message
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("failed to parse session line: %v", err)
	}
	if m.Role != RoleAssistant || m.StopReason != "stop" || m.Model != "gpt-5.2" {
		t.Errorf("parsed message mismatch: %+v", &m)
	}
	if len(m.Content) != 1 || m.Content[0].Text != "Let me check the file." {
		t.Errorf("content mismatch: %+v", m.Content)
	}
	if m.Usage == nil || m.Usage.Input != 10 || m.Usage.TotalTokens != 15 {
		t.Errorf("usage mismatch: %+v", m.Usage)
	}
	if m.Timestamp != 1750000000000 {
		t.Errorf("timestamp mismatch: %d", m.Timestamp)
	}
}

// TestCalculateCostCacheNotDoubleCharged verifies the cached portion of the
// prompt is priced only at the (cheaper) cache-read rate, not also at the
// full input rate. Usage.Input is the TOTAL prompt (incl. cached).
func TestCalculateCostCacheNotDoubleCharged(t *testing.T) {
	cost := ModelCost{Input: 10, Output: 20, CacheRead: 1, CacheWrite: 2} // per 1M tokens
	u := Usage{Input: 1_000_000, CacheRead: 800_000, CacheWrite: 0, Output: 0}
	// fresh = 1M - 800k = 200k → 200k * 10/1M = 2.0
	// cacheRead = 800k * 1/1M = 0.8
	// total = 2.0 + 0.8 = 2.8 (not 10.8, which would double-charge the cache)
	c := CalculateCost(u, cost)
	if c.Input != 2.0 {
		t.Errorf("fresh input cost = %v, want 2.0", c.Input)
	}
	if c.CacheRead != 0.8 {
		t.Errorf("cacheRead cost = %v, want 0.8", c.CacheRead)
	}
	if c.Total != 2.8 {
		t.Errorf("total = %v, want 2.8 (no double-charge)", c.Total)
	}
}
