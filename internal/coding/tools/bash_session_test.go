package tools

import (
	"context"
	"strings"
	"testing"
)

// TestBashToolSessionEnv verifies §8.5.1 end to end: a bash child process
// spawned by a session's tool set sees that session's DOGE_SESSION_* env vars
// (not a package-global value that concurrent sessions could clobber).
func TestBashToolSessionEnv(t *testing.T) {
	ops := LocalOperations()
	ops.SessionID = "sess-abc"
	ops.SessionFile = "/tmp/sess-abc.jsonl"

	tool := BashTool(ops)
	res, err := tool.Execute("b1", map[string]any{
		"command": "printf 'ID=%s FILE=%s' \"$DOGE_SESSION_ID\" \"$DOGE_SESSION_FILE\"",
		"timeout": float64(10),
	}, context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "ID=sess-abc") {
		t.Errorf("child saw DOGE_SESSION_ID=%q (want sess-abc)", text)
	}
	if !strings.Contains(text, "FILE=/tmp/sess-abc.jsonl") {
		t.Errorf("child saw DOGE_SESSION_FILE=%q (want /tmp/sess-abc.jsonl)", text)
	}
}
