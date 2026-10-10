package ai

import (
	"testing"
)

func TestValidateToolArguments(t *testing.T) {
	tool := &Tool{
		Name:        "bash",
		Description: "Run a shell command",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
				"timeout": map[string]any{"type": "number"},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
	}

	// Valid object.
	v, err := ValidateToolArguments(tool, map[string]any{"command": "ls", "timeout": 10.0})
	if err != nil {
		t.Fatalf("valid args rejected: %v", err)
	}
	if m, ok := v.(map[string]any); !ok || m["command"] != "ls" {
		t.Errorf("returned args mismatch: %v", v)
	}

	// Valid JSON string (models that stream arguments as strings).
	if _, err := ValidateToolArguments(tool, `{"command": "pwd"}`); err != nil {
		t.Fatalf("string args rejected: %v", err)
	}

	// Missing required field.
	if _, err := ValidateToolArguments(tool, map[string]any{"timeout": 5.0}); err == nil {
		t.Error("missing required field accepted")
	}

	// Unknown field with additionalProperties: false.
	if _, err := ValidateToolArguments(tool, map[string]any{"command": "ls", "bogus": 1}); err == nil {
		t.Error("unknown field accepted")
	}

	// Invalid JSON string.
	if _, err := ValidateToolArguments(tool, `{not json`); err == nil {
		t.Error("invalid JSON string accepted")
	}

	// Invalid type.
	if _, err := ValidateToolArguments(tool, map[string]any{"command": 42}); err == nil {
		t.Error("wrong type accepted")
	}

	// nil → empty object (fails required, but must not panic).
	if _, err := ValidateToolArguments(tool, nil); err == nil {
		t.Error("nil args accepted")
	}
}

func TestValidateToolArgumentsNilTool(t *testing.T) {
	v, err := ValidateToolArguments(nil, map[string]any{"a": 1})
	if err != nil {
		t.Fatalf("nil tool should pass through: %v", err)
	}
	if m, ok := v.(map[string]any); !ok || m["a"] != 1 {
		t.Errorf("args not passed through: %v", v)
	}
}

func TestEstimateTokens(t *testing.T) {
	// 8 chars → 2 tokens (ceil(8/4)).
	if got := EstimateTextTokens("abcdefgh"); got != 2 {
		t.Errorf("EstimateTextTokens = %d, want 2", got)
	}
	m := &Message{
		Role:    RoleUser,
		Content: []ContentBlock{TextBlock("12345678"), ImageBlock([]byte("x"), "image/png")},
	}
	// 2 tokens for text + ceil(4800/4)=1200 for the image.
	if got := EstimateMessageTokens(m); got != 1202 {
		t.Errorf("EstimateMessageTokens = %d, want 1202", got)
	}
	if got := EstimateTokens([]*Message{m}); got != 1202 {
		t.Errorf("EstimateTokens = %d, want 1202", got)
	}
}
