package ai

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// Debug helpers: replay a real session through the completions converter.
func TestDebugConvertRealSessionChat(t *testing.T) {
	path := os.Getenv("DOGE_DEBUG_SESSION")
	if path == "" {
		t.Skip("DOGE_DEBUG_SESSION not set")
	}
	msgs, err := loadSessionMessages(path)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("loaded %d messages\n", len(msgs))
	// 排除 error assistant 消息（请求发送时它们还不存在）
	kept := msgs[:0]
	for _, m := range msgs {
		if m.Role == RoleAssistant && m.StopReason == StopReasonError {
			continue
		}
		kept = append(kept, m)
	}
	msgs = kept
	model := &Model{ID: "deepseek-v4-flash", API: APIOpenAICompletions, Provider: "model", Reasoning: true}
	ctx := &Context{Messages: msgs}
	out := convertCompletionsMessages(model, ctx)
	data, _ := json.MarshalIndent(out, "", "  ")
	fmt.Printf("CHAT INPUT JSON:\n%s\n", data)
}

func loadSessionMessages(path string) ([]*Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var msgs []*Message
	for dec.More() {
		var line struct {
			Type    string   `json:"type"`
			Message *Message `json:"message"`
		}
		if err := dec.Decode(&line); err != nil {
			break
		}
		if line.Type == "message" && line.Message != nil {
			msgs = append(msgs, line.Message)
		}
	}
	return msgs, nil
}
