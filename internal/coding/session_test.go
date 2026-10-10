package coding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doge/internal/ai"
)

func TestSessionLazyCreation(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(filepath.Join(dir, "sessions"))
	s, err := m.CreateSession("/tmp/proj")
	if err != nil {
		t.Fatal(err)
	}
	// Nothing on disk until the first message: neither the per-cwd directory
	// nor the session file.
	if _, err := os.Stat(filepath.Dir(s.FilePath)); !os.IsNotExist(err) {
		t.Fatalf("session directory should not exist before any conversation")
	}
	if _, err := os.Stat(s.FilePath); !os.IsNotExist(err) {
		t.Fatalf("session file should not exist before any message")
	}

	// The first message (user or assistant) creates the file: a mid-run
	// session is locatable by other windows (switch/takeover) instead of
	// living only in the creating agent's memory.
	if err := s.AppendMessage(ai.NewUserTextMessage("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.FilePath); os.IsNotExist(err) {
		t.Fatalf("session file should exist after the first message")
	}
	data, err := os.ReadFile(s.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 2 {
		t.Fatalf("expected header + user message, got %d lines", len(lines))
	}

	// The assistant message appends to the same file.
	assistant := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{ai.TextBlock("hi")}, Timestamp: 1}
	if err := s.AppendMessage(assistant); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(s.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header + 2 entries, got %d lines", len(lines))
	}
	var hdr SessionHeader
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Type != "session" || hdr.Version != sessionVersion || hdr.Cwd != "/tmp/proj" {
		t.Errorf("header = %+v", hdr)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(filepath.Join(dir, "sessions"))
	s, err := m.CreateSession("/proj")
	if err != nil {
		t.Fatal(err)
	}
	messages := []*ai.Message{
		ai.NewUserTextMessage("q1"),
		{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{ai.TextBlock("a1")}, Usage: &ai.Usage{Input: 10, Output: 5, TotalTokens: 15}, Timestamp: 2},
		{Role: ai.RoleToolResult, ToolCallID: "c1", ToolName: "bash", Content: []ai.ContentBlock{ai.TextBlock("out")}, Timestamp: 3},
	}
	for _, m := range messages {
		if err := s.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	// Custom entry (model change).
	if err := s.AppendCustomEntry(&SessionEntry{Type: EntryModelChange, Provider: "openai", ModelID: "gpt-5.2"}); err != nil {
		t.Fatal(err)
	}

	loaded, err := m.LoadSession(s.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Entries) != 4 {
		t.Fatalf("loaded entries = %d, want 4", len(loaded.Entries))
	}
	if loaded.Entries[1].Message.Usage.Input != 10 {
		t.Errorf("usage round trip = %+v", loaded.Entries[1].Message.Usage)
	}
	if loaded.Entries[3].Type != EntryModelChange || loaded.Entries[3].ModelID != "gpt-5.2" {
		t.Errorf("model change entry = %+v", loaded.Entries[3])
	}
	if loaded.LeafID != loaded.Entries[3].ID {
		t.Errorf("leaf = %q, want %q", loaded.LeafID, loaded.Entries[3].ID)
	}

	// Context includes all entries.
	ctx := loaded.ContextEntries()
	if len(ctx) != 4 {
		t.Errorf("context entries = %d", len(ctx))
	}
	msgs := ContextEntriesToMessages(ctx)
	if len(msgs) != 3 {
		t.Errorf("context messages = %d", len(msgs))
	}
}

func TestSessionBranching(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(filepath.Join(dir, "sessions"))
	s, err := m.CreateSession("/proj")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"m1", "m2", "m3"} {
		if err := s.AppendMessage(&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.ContentBlock{ai.TextBlock(text)}, Timestamp: time.Now().UnixMilli()}); err != nil {
			t.Fatal(err)
		}
	}
	second := s.Entries[1]

	// Branch from the second entry: new entries fork from there.
	s.Branch(second.ID)
	if err := s.AppendMessage(&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{ai.TextBlock("branch-msg")}, Timestamp: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	ctx := s.ContextEntries()
	texts := []string{}
	for _, e := range ctx {
		if e.Message != nil {
			texts = append(texts, e.Message.Text())
		}
	}
	// The branch context is m1, m2, branch-msg (m3 is on the abandoned path).
	if len(texts) != 3 || texts[0] != "m1" || texts[1] != "m2" || texts[2] != "branch-msg" {
		t.Errorf("branch context = %v", texts)
	}
}

func TestSessionCompactionContext(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(filepath.Join(dir, "sessions"))
	s, err := m.CreateSession("/proj")
	if err != nil {
		t.Fatal(err)
	}
	// 4 assistant messages.
	var ids []string
	for i := range 4 {
		msg := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.ContentBlock{ai.TextBlock(strings.Repeat("x", 10))}, Timestamp: int64(i)}
		if err := s.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.LeafID)
	}

	// Insert a compaction entry summarizing everything before entry 3.
	compaction := &SessionEntry{
		Type:             EntryCompaction,
		Summary:          "summary of early work",
		FirstKeptEntryID: ids[2],
		TokensBefore:     100,
	}
	if err := s.AppendCustomEntry(compaction); err != nil {
		t.Fatal(err)
	}
	// One more message after compaction.
	if err := s.AppendMessage(&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{ai.TextBlock("after")}, Timestamp: 10}); err != nil {
		t.Fatal(err)
	}

	ctx := s.ContextEntries()
	// Context = compaction + kept entries (m3, m4) + after.
	if len(ctx) != 4 {
		t.Fatalf("context entries = %d, want 4 (compaction + kept + after)", len(ctx))
	}
	if ctx[0].Type != EntryCompaction {
		t.Errorf("first context entry should be compaction, got %+v", ctx[0])
	}
	if ctx[1].ID != ids[2] || ctx[2].ID != ids[3] || ctx[3].Message.Text() != "after" {
		t.Errorf("kept entries = %v, %v, %v", ctx[1].ID, ctx[2].ID, ctx[3].Message.Text())
	}
	msgs := ContextEntriesToMessages(ctx)
	if len(msgs) != 4 || !strings.Contains(msgs[0].Text(), "summary of early work") {
		t.Errorf("context messages = %+v", msgs)
	}
}

// TestSessionFileCompat loads a session file in the
// exact format) and validates parsing and context building.
func TestPiSessionFileCompat(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "2026-08-03T12-00-00-0000_abc123.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"abc123","timestamp":1754208000000,"cwd":"/tmp/test-proj"}`,
		`{"id":"e1","parentId":"","timestamp":1754208000001,"type":"message","message":{"role":"user","content":"hello"}}`,
		`{"id":"e2","parentId":"e1","timestamp":1754208000002,"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Let me check."}],"api":"openai-responses","provider":"openai","model":"gpt-5.2","usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":15,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"toolUse","timestamp":1754208000002}}`,
		`{"id":"e3","parentId":"e2","timestamp":1754208000003,"type":"message","message":{"role":"toolResult","toolCallId":"call_1","toolName":"bash","content":[{"type":"text","text":"file contents"}],"details":{},"isError":false,"timestamp":1754208000003}}`,
		`{"id":"e4","parentId":"e3","timestamp":1754208000004,"type":"compaction","summary":"Did file work","firstKeptEntryId":"e3","tokensBefore":50}`,
		`{"id":"e5","parentId":"e4","timestamp":1754208000005,"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Done"}],"stopReason":"stop","timestamp":1754208000005}}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewSessionManager(dir)
	s, err := m.LoadSession(file)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if len(s.Entries) != 5 {
		t.Fatalf("entries = %d", len(s.Entries))
	}
	if s.Header.Cwd != "/tmp/test-proj" || s.Header.Version != 3 {
		t.Errorf("header = %+v", s.Header)
	}
	if s.Entries[1].Message.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", s.Entries[1].Message.Usage)
	}
	if s.Entries[3].Type != EntryCompaction || s.Entries[3].FirstKeptEntryID != "e3" {
		t.Errorf("compaction = %+v", s.Entries[3])
	}

	// Context: compaction + kept(e3) + after(e5); e1/e2 summarized away.
	ctx := s.ContextEntries()
	if len(ctx) != 3 {
		t.Fatalf("context entries = %d, want 3", len(ctx))
	}
	if ctx[0].ID != "e4" || ctx[1].ID != "e3" || ctx[2].ID != "e5" {
		t.Errorf("context ids = %s, %s, %s", ctx[0].ID, ctx[1].ID, ctx[2].ID)
	}
	msgs := ContextEntriesToMessages(ctx)
	if len(msgs) != 3 || msgs[0].Role != ai.RoleUser {
		t.Errorf("context messages = %+v", msgs)
	}
}

func TestListSessions(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(filepath.Join(dir, "sessions"))

	s1, _ := m.CreateSession("/proj-a")
	_ = s1.AppendMessage(&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{ai.TextBlock("x")}})
	time.Sleep(2 * time.Millisecond)
	s2, _ := m.CreateSession("/proj-b")
	_ = s2.AppendMessage(&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{ai.TextBlock("y")}})

	summaries, err := m.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %d", len(summaries))
	}
	// Newest first.
	if summaries[0].ID != s2.Header.ID {
		t.Errorf("newest session = %s, want %s", summaries[0].ID, s2.Header.ID)
	}
}

// TestListSessionsCacheInvalidation: summaries are cached per (size, mtime);
// appending to a session file must refresh its cached message count instead
// of serving the stale entry.
func TestListSessionsCacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(filepath.Join(dir, "sessions"))

	s, _ := m.CreateSession("/proj")
	if err := s.AppendMessage(ai.NewUserTextMessage("one")); err != nil {
		t.Fatal(err)
	}
	countOf := func() int {
		t.Helper()
		summaries, err := m.ListSessions()
		if err != nil {
			t.Fatal(err)
		}
		if len(summaries) != 1 {
			t.Fatalf("summaries = %d, want 1", len(summaries))
		}
		return summaries[0].MessageCount
	}
	if got := countOf(); got != 1 {
		t.Fatalf("first count = %d, want 1", got)
	}
	// Second listing hits the cache; appending (via the manager, like a live
	// agent) changes size/mtime and must invalidate it.
	if got := countOf(); got != 1 {
		t.Fatalf("cached count = %d, want 1", got)
	}
	if err := s.AppendMessage(&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop, Content: []ai.ContentBlock{ai.TextBlock("two")}}); err != nil {
		t.Fatal(err)
	}
	if got := countOf(); got != 2 {
		t.Fatalf("count after append = %d, want 2", got)
	}
}

// TestFindSessionFile: session ids are embedded in file names, so an id (or
// file basename) resolves without scanning file contents; misses and ids
// belonging to other sessions' file names must not resolve.
func TestFindSessionFile(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(filepath.Join(dir, "sessions"))

	sa, _ := m.CreateSession("/proj-a")
	if err := sa.AppendMessage(ai.NewUserTextMessage("a")); err != nil {
		t.Fatal(err)
	}
	sb, _ := m.CreateSession("/proj-b")
	if err := sb.AppendMessage(ai.NewUserTextMessage("b")); err != nil {
		t.Fatal(err)
	}

	if p := m.FindSessionFile(sa.Header.ID); p != sa.FilePath {
		t.Fatalf("FindSessionFile(id) = %q, want %q", p, sa.FilePath)
	}
	if p := m.FindSessionFile(filepath.Base(sb.FilePath)); p != sb.FilePath {
		t.Fatalf("FindSessionFile(basename) = %q, want %q", p, sb.FilePath)
	}
	if p := m.FindSessionFile("no-such-id"); p != "" {
		t.Fatalf("FindSessionFile(miss) = %q, want empty", p)
	}
	if p := m.FindSessionFile(""); p != "" {
		t.Fatalf("FindSessionFile(empty) = %q, want empty", p)
	}
	// A path-like input is not an id lookup.
	if p := m.FindSessionFile("dir/file.jsonl"); p != "" {
		t.Fatalf("FindSessionFile(path) = %q, want empty", p)
	}
}
