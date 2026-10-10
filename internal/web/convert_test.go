package web

import (
	"encoding/json"
	"testing"

	"doge/internal/agent"
	"doge/internal/ai"
)

func decodeEvent(t *testing.T, ev agent.AgentEvent) map[string]any {
	t.Helper()
	data, err := marshalAgentEvent(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestConvertMessageEvent(t *testing.T) {
	ev := agent.AgentEvent{
		Type:    agent.EventMessageUpdate,
		Message: &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{ai.TextBlock("hello")}},
	}
	m := decodeEvent(t, ev)
	if m["type"] != "message_update" {
		t.Fatalf("type = %v", m["type"])
	}
	msg, ok := m["message"].(map[string]any)
	if !ok {
		t.Fatalf("missing message: %v", m)
	}
	if msg["role"] != "assistant" {
		t.Fatalf("role = %v", msg["role"])
	}
	content, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content not array: %#v", msg["content"])
	}
	first := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "hello" {
		t.Fatalf("block = %#v", first)
	}
}

func TestConvertAgentEnd(t *testing.T) {
	ev := agent.AgentEvent{
		Type:     agent.EventAgentEnd,
		Messages: []*ai.Message{ai.NewUserTextMessage("hi"), {Role: ai.RoleAssistant, Content: []ai.ContentBlock{ai.TextBlock("yo")}}},
	}
	m := decodeEvent(t, ev)
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %#v", m["messages"])
	}
}

func TestConvertToolResult(t *testing.T) {
	ev := agent.AgentEvent{
		Type:       agent.EventToolExecutionEnd,
		ToolCallID: "call_1",
		ToolName:   "bash",
		IsError:    true,
		Result: &agent.AgentToolResult{
			Content: []ai.ContentBlock{ai.TextBlock("boom")},
			Details: map[string]any{"exit": 1},
		},
	}
	m := decodeEvent(t, ev)
	if m["type"] != "tool_execution_end" {
		t.Fatalf("type = %v", m["type"])
	}
	if m["toolCallId"] != "call_1" || m["toolName"] != "bash" || m["isError"] != true {
		t.Fatalf("tool fields = %#v", m)
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result: %#v", m)
	}
	if res["isError"] != true {
		t.Fatalf("result.isError = %#v", res["isError"])
	}
}

func TestConvertToolStartArgs(t *testing.T) {
	ev := agent.AgentEvent{
		Type:       agent.EventToolExecutionStart,
		ToolCallID: "call_9",
		ToolName:   "read",
		Args:       map[string]any{"path": "main.go"},
	}
	m := decodeEvent(t, ev)
	if m["type"] != "tool_execution_start" {
		t.Fatalf("type = %v", m["type"])
	}
	args, ok := m["args"].(map[string]any)
	if !ok || args["path"] != "main.go" {
		t.Fatalf("args = %#v", m["args"])
	}
}

func TestConvertTurnEndToolResults(t *testing.T) {
	ev := agent.AgentEvent{
		Type:        agent.EventTurnEnd,
		ToolResults: []*ai.Message{{Role: ai.RoleToolResult, ToolCallID: "call_1", ToolName: "bash"}},
	}
	m := decodeEvent(t, ev)
	results, ok := m["toolResults"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("toolResults = %#v", m["toolResults"])
	}
}
