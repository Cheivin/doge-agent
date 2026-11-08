package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doge/internal/ai"
)

// TestLoadSessionTornTail 崩溃打断 append 会留下残缺的末行：加载必须容忍
// （丢弃 + 截断），会话仍可用，追加从上一合法条目续写且文件重新合法
func TestLoadSessionTornTail(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(filepath.Join(dir, "sessions"))
	s, err := m.CreateSession("/proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(ai.NewUserTextMessage("q1")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{ai.TextBlock("a1")}, Timestamp: 2}); err != nil {
		t.Fatal(err)
	}

	// 模拟崩溃残迹：文件尾部拼一段残缺 JSON（无换行结尾）
	f, err := os.OpenFile(s.FilePath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"deadbeef","paren`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	loaded, err := m.LoadSession(s.FilePath)
	if err != nil {
		t.Fatalf("load with torn tail: %v", err)
	}
	if len(loaded.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(loaded.Entries))
	}
	if loaded.LeafID != loaded.Entries[1].ID {
		t.Fatalf("leaf = %q, want last valid entry %q", loaded.LeafID, loaded.Entries[1].ID)
	}
	if !loaded.HasAssistant {
		t.Fatal("HasAssistant lost")
	}

	// 追加必须接在合法末条之后，重载即恢复完全合法
	if err := loaded.AppendMessage(&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{ai.TextBlock("a2")}, Timestamp: 3}); err != nil {
		t.Fatalf("append after torn tail: %v", err)
	}
	data, err := os.ReadFile(s.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "deadbeef") {
		t.Fatalf("torn line survived: %s", data)
	}
	again, err := m.LoadSession(s.FilePath)
	if err != nil {
		t.Fatalf("reload after append: %v", err)
	}
	if len(again.Entries) != 3 || again.Entries[2].Message.Text() != "a2" {
		t.Fatalf("entries after append = %d, last = %+v", len(again.Entries), again.Entries[len(again.Entries)-1])
	}
	if again.LeafID != again.Entries[2].ID {
		t.Fatalf("leaf = %q, want %q", again.LeafID, again.Entries[2].ID)
	}
}

// TestLoadSessionMidCorruption 中间行损坏仍是致命错误，不得被尾行容忍掩盖
func TestLoadSessionMidCorruption(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(filepath.Join(dir, "sessions"))
	s, err := m.CreateSession("/proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(ai.NewUserTextMessage("q1")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{ai.TextBlock("a1")}, Timestamp: 2}); err != nil {
		t.Fatal(err)
	}

	// 把第一条消息改写成残缺 JSON（后面还有合法行）
	data, err := os.ReadFile(s.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 3 {
		t.Fatalf("unexpected session file: %s", data)
	}
	lines[1] = `{"id":"corrupted","no`
	if err := os.WriteFile(s.FilePath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.LoadSession(s.FilePath); err == nil {
		t.Fatal("mid-file corruption must error")
	}
}

// TestLoadSessionEmptyTrailingNewline 末尾多出的空行不触发尾行处理
func TestLoadSessionEmptyTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(filepath.Join(dir, "sessions"))
	s, err := m.CreateSession("/proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(ai.NewUserTextMessage("q1")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.FilePath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("\n\n")
	f.Close()

	loaded, err := m.LoadSession(s.FilePath)
	if err != nil {
		t.Fatalf("load with blank trailing lines: %v", err)
	}
	if len(loaded.Entries) != 1 || loaded.LeafID != loaded.Entries[0].ID {
		t.Fatalf("entries = %d", len(loaded.Entries))
	}
}
