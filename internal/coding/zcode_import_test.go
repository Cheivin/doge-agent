package coding

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doge/internal/ai"
)

// newZCodeTestDB 在临时目录建最小 ZCode 库 schema，返回库路径
func newZCodeTestDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "zcode.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := `
CREATE TABLE session (id text primary key, directory text not null, title text not null, time_created integer not null, time_updated integer not null, task_type text not null default 'interactive');
CREATE TABLE message (id text primary key, session_id text not null, sequence integer, time_created integer not null, data text not null);
CREATE TABLE part (id text primary key, message_id text not null, sequence integer, time_created integer not null, data text not null);
`
	for _, stmt := range strings.Split(schema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath
}

// zcodeJSON 序列化测试样本，失败直接终止
func zcodeJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seedZCodeFixtures 写入覆盖各转换场景的样本会话
func seedZCodeFixtures(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	sess := func(id, dir, title string, created int64, taskType string) {
		exec("INSERT INTO session (id, directory, title, time_created, time_updated, task_type) VALUES (?,?,?,?,?,?)",
			id, dir, title, created, created+60000, taskType)
	}
	msg := func(id, sessionID string, seq int, created int64, data string) {
		exec("INSERT INTO message (id, session_id, sequence, time_created, data) VALUES (?,?,?,?,?)",
			id, sessionID, seq, created, data)
	}
	part := func(id, messageID string, seq int, created int64, data string) {
		exec("INSERT INTO part (id, message_id, sequence, time_created, data) VALUES (?,?,?,?,?)",
			id, messageID, seq, created, data)
	}

	// 会话 t1：普通对话 + 工具调用（completed/error）+ system-reminder 过滤
	sess("sess_t1", "/tmp/proj-a", "导入测试会话", 1789820635315, "interactive")
	msg("msg_u1", "sess_t1", 0, 1789820635435, zcodeJSON(t, map[string]any{
		"role": "user",
		"time": map[string]any{"created": 1789820635435},
	}))
	part("p1", "msg_u1", 0, 1789820635435, zcodeJSON(t, map[string]any{
		"type": "text", "text": "帮我看看这个项目",
	}))
	part("p2", "msg_u1", 1, 1789820635436, zcodeJSON(t, map[string]any{
		"type": "text", "text": "<system-reminder>\nThe TodoWrite tool hasn't been used recently.",
	}))
	msg("msg_a1", "sess_t1", 1, 1789820635440, zcodeJSON(t, map[string]any{
		"role": "assistant", "modelId": "GLM-5.3", "providerId": "builtin:test",
		"time": map[string]any{"created": 1789820635440, "completed": 1789820649009},
	}))
	part("p3", "msg_a1", 0, 1789820635440, zcodeJSON(t, map[string]any{"type": "step-start"}))
	part("p4", "msg_a1", 1, 1789820635441, zcodeJSON(t, map[string]any{
		"type": "reasoning", "text": "先看看目录结构",
	}))
	part("p5", "msg_a1", 2, 1789820635442, zcodeJSON(t, map[string]any{
		"type": "text", "text": "我来帮你看看",
	}))
	part("p6", "msg_a1", 3, 1789820635443, zcodeJSON(t, map[string]any{
		"type": "tool", "callID": "call_1", "tool": "Read",
		"state": map[string]any{"status": "completed", "input": map[string]any{"path": "/tmp/a.go"}, "output": "file content"},
	}))
	part("p7", "msg_a1", 4, 1789820635444, zcodeJSON(t, map[string]any{
		"type": "tool", "callID": "call_2", "tool": "Bash",
		"state": map[string]any{"status": "error", "input": map[string]any{"command": "ls"}, "output": "boom"},
	}))
	part("p8", "msg_a1", 5, 1789820649009, zcodeJSON(t, map[string]any{"type": "step-finish"}))
	msg("msg_a2", "sess_t1", 2, 1789820649010, zcodeJSON(t, map[string]any{
		"role": "assistant", "modelId": "GLM-5.3", "providerId": "builtin:test",
		"time": map[string]any{"created": 1789820649010, "completed": 1789820655483},
	}))
	part("p9", "msg_a2", 0, 1789820649010, zcodeJSON(t, map[string]any{
		"type": "text", "text": "看完了",
	}))

	// 会话 t2：只有 system-reminder 的用户消息，过滤后为空应整会话跳过
	sess("sess_t2", "/tmp/proj-b", "提醒过滤会话", 1789820636000, "interactive")
	msg("msg_u2", "sess_t2", 0, 1789820636100, zcodeJSON(t, map[string]any{
		"role": "user",
		"time": map[string]any{"created": 1789820636100},
	}))
	part("p10", "msg_u2", 0, 1789820636100, zcodeJSON(t, map[string]any{
		"type": "text", "text": "<system-reminder>\nContinue working toward the active session",
	}))

	// 会话 t3：subagent_child 应跳过
	sess("sess_t3", "/tmp/proj-a", "子代理会话", 1789820637000, "subagent_child")
	msg("msg_u3", "sess_t3", 0, 1789820637100, zcodeJSON(t, map[string]any{
		"role": "user", "time": map[string]any{"created": 1789820637100},
	}))
	part("p11", "msg_u3", 0, 1789820637100, zcodeJSON(t, map[string]any{
		"type": "text", "text": "子任务",
	}))

	// 会话 t4：零消息应跳过
	sess("sess_t4", "/tmp/proj-c", "空会话", 1789820638000, "interactive")

	// 会话 t5：压缩，保留起点指向第二条用户消息
	sess("sess_t5", "/tmp/proj-a", "压缩会话", 1789820639000, "interactive")
	msg("msg_v1", "sess_t5", 0, 1789820639100, zcodeJSON(t, map[string]any{
		"role": "user", "time": map[string]any{"created": 1789820639100},
	}))
	part("p12", "msg_v1", 0, 1789820639100, zcodeJSON(t, map[string]any{
		"type": "text", "text": "早先的问题",
	}))
	msg("msg_v2", "sess_t5", 1, 1789820639200, zcodeJSON(t, map[string]any{
		"role": "user", "time": map[string]any{"created": 1789820639200},
	}))
	part("p13", "msg_v2", 0, 1789820639200, zcodeJSON(t, map[string]any{
		"type": "text", "text": "保留的问题",
	}))
	msg("msg_v3", "sess_t5", 2, 1789820639300, zcodeJSON(t, map[string]any{
		"role": "assistant", "modelId": "GLM-5.3", "providerId": "builtin:test",
		"time": map[string]any{"created": 1789820639300, "completed": 1789820639400},
	}))
	part("p14", "msg_v3", 0, 1789820639400, zcodeJSON(t, map[string]any{
		"type":                 "compaction",
		"summaryMessageId":     "msg_sum",
		"tail_start_id":        "msg_v2",
		"preCompactTokenCount": 115145,
		"time":                 map[string]any{"start": 1789820639350, "end": 1789820639400},
	}))
	msg("msg_sum", "sess_t5", 3, 1789820639390, zcodeJSON(t, map[string]any{
		"role":    "user",
		"time":    map[string]any{"created": 1789820639390},
		"summary": map[string]any{"title": "Compact summary", "body": "Summary:\n## 主要请求"},
	}))
	msg("msg_v4", "sess_t5", 4, 1789820639500, zcodeJSON(t, map[string]any{
		"role": "assistant", "modelId": "GLM-5.3", "providerId": "builtin:test",
		"time": map[string]any{"created": 1789820639500, "completed": 1789820639600},
	}))
	part("p15", "msg_v4", 0, 1789820639500, zcodeJSON(t, map[string]any{
		"type": "text", "text": "压缩后的回答",
	}))
}

// TestImportZCodeSessions 覆盖导入、过滤、跳过与重新加载
func TestImportZCodeSessions(t *testing.T) {
	dir := t.TempDir()
	dbPath := newZCodeTestDB(t)
	seedZCodeFixtures(t, dbPath)
	mgr := NewSessionManager(filepath.Join(dir, "sessions"))

	imported, skipped, err := ImportZCodeSessions(dbPath, mgr)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// subagent_child 不在导入范围内（t3），跳过的是空会话 t4 和纯注入物 t2
	if imported != 2 || skipped != 2 {
		t.Fatalf("imported=%d skipped=%d, want 2/2", imported, skipped)
	}

	// 重新导入应全部跳过（目标已存在）
	imported2, skipped2, err := ImportZCodeSessions(dbPath, mgr)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if imported2 != 0 || skipped2 != 4 {
		t.Fatalf("re-import imported=%d skipped=%d, want 0/4", imported2, skipped2)
	}

	// t2 只有注入物，t3 是子代理会话，都不应生成会话文件
	if got := mgr.FindSessionFile("t2"); got != "" {
		t.Fatalf("t2 should be skipped, got %s", got)
	}
	if got := mgr.FindSessionFile("t3"); got != "" {
		t.Fatalf("t3 should be excluded, got %s", got)
	}

	s := loadImported(t, mgr, "t1")
	if s.Header.ID != "t1" || s.Header.Cwd != "/tmp/proj-a" || s.Header.Version != 3 || s.Header.Type != "session" {
		t.Fatalf("bad header: %+v", s.Header)
	}
	if s.Header.Timestamp != 1789820635315 {
		t.Fatalf("header timestamp=%d", s.Header.Timestamp)
	}
	// user + assistant + 2 toolResult + assistant + session_info = 6 条
	if len(s.Entries) != 6 {
		t.Fatalf("t1 entries=%d, want 6", len(s.Entries))
	}

	e0 := s.Entries[0]
	if e0.Message == nil || e0.Message.Role != ai.RoleUser || e0.Message.Text() != "帮我看看这个项目" {
		t.Fatalf("entry0: %+v", e0.Message)
	}
	if e0.Timestamp != 1789820635435 {
		t.Fatalf("entry0 timestamp=%d", e0.Timestamp)
	}

	e1 := s.Entries[1]
	m1 := e1.Message
	if m1 == nil || m1.Role != ai.RoleAssistant {
		t.Fatalf("entry1 role: %+v", m1)
	}
	if m1.Model != "GLM-5.3" || m1.Provider != "builtin:test" {
		t.Fatalf("entry1 model/provider: %s/%s", m1.Model, m1.Provider)
	}
	if len(m1.Content) != 4 ||
		m1.Content[0].Type != ai.ContentTypeThinking || m1.Content[0].Thinking != "先看看目录结构" ||
		m1.Content[1].Type != ai.ContentTypeText || m1.Content[1].Text != "我来帮你看看" ||
		m1.Content[2].Type != ai.ContentTypeToolCall || m1.Content[2].ToolCallID != "call_1" || m1.Content[2].ToolName != "Read" ||
		m1.Content[3].ToolCallID != "call_2" || m1.Content[3].ToolName != "Bash" {
		t.Fatalf("entry1 content: %+v", m1.Content)
	}
	if args, want := m1.Content[2].Arguments["path"], "/tmp/a.go"; args != want {
		t.Fatalf("call_1 input path=%v, want %v", args, want)
	}

	e2 := s.Entries[2]
	tr1 := e2.Message
	if tr1 == nil || tr1.Role != ai.RoleToolResult || tr1.ToolCallID != "call_1" || tr1.ToolName != "Read" || tr1.IsError {
		t.Fatalf("entry2: %+v", tr1)
	}
	if tr1.Text() != "file content" {
		t.Fatalf("entry2 text=%q", tr1.Text())
	}

	e3 := s.Entries[3]
	tr2 := e3.Message
	if tr2 == nil || tr2.Role != ai.RoleToolResult || tr2.ToolCallID != "call_2" || !tr2.IsError || tr2.Text() != "boom" {
		t.Fatalf("entry3: %+v", tr2)
	}

	if txt := s.Entries[4].Message.Text(); txt != "看完了" {
		t.Fatalf("entry4 text=%q", txt)
	}
	info := s.Entries[5]
	if info.Type != EntrySessionInfo || info.Name != "导入测试会话" {
		t.Fatalf("entry5: type=%s name=%s", info.Type, info.Name)
	}

	// 压缩会话：压缩条目带摘要、tokensBefore 和回填的保留起点
	s5 := loadImported(t, mgr, "t5")
	var comp *SessionEntry
	for _, e := range s5.Entries {
		if e.Type == EntryCompaction {
			comp = e
		}
	}
	if comp == nil {
		t.Fatal("t5 has no compaction entry")
	}
	if comp.Summary != "Summary:\n## 主要请求" || comp.TokensBefore != 115145 {
		t.Fatalf("compaction summary/tokens: %q %d", comp.Summary, comp.TokensBefore)
	}
	if comp.FirstKeptEntryID == "" || comp.FirstKeptEntryID != s5.Entries[1].ID {
		t.Fatalf("firstKept=%q, want entry id of %q", comp.FirstKeptEntryID, s5.Entries[1].ID)
	}
	// doge 上下文裁剪：压缩前的早先消息应被摘要掉（末尾带 session_info 叶子）
	ctx := s5.ContextEntries()
	if len(ctx) != 4 || ctx[0].Type != EntryCompaction || ctx[1].Message.Text() != "保留的问题" ||
		ctx[2].Message.Text() != "压缩后的回答" {
		t.Fatalf("context entries: %+v", ctx)
	}
}

// loadImported 按 id 找到导入的会话并重新加载
func loadImported(t *testing.T, mgr *SessionManager, id string) *Session {
	t.Helper()
	path := mgr.FindSessionFile(id)
	if path == "" {
		t.Fatalf("session %s not imported", id)
	}
	s, err := mgr.LoadSession(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return s
}

// TestZCodeImportCompactionWithoutSummary 无摘要的压缩 part 不产出压缩条目
func TestZCodeImportCompactionWithoutSummary(t *testing.T) {
	dir := t.TempDir()
	dbPath := newZCodeTestDB(t)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec("INSERT INTO session (id, directory, title, time_created, time_updated, task_type) VALUES (?,?,?,?,?,?)",
		"sess_c1", "/tmp/proj-d", "手动压缩", int64(1789820640000), int64(1789820641000), "interactive")
	mustExec("INSERT INTO message (id, session_id, sequence, time_created, data) VALUES (?,?,?,?,?)", "msg_w1", "sess_c1", 0,
		int64(1789820640100), zcodeJSON(t, map[string]any{
			"role": "user", "time": map[string]any{"created": 1789820640100},
		}))
	mustExec("INSERT INTO part (id, message_id, sequence, time_created, data) VALUES (?,?,?,?,?)", "q1", "msg_w1", 0,
		int64(1789820640100), zcodeJSON(t, map[string]any{
			"type": "text", "text": "问题",
		}))
	mustExec("INSERT INTO message (id, session_id, sequence, time_created, data) VALUES (?,?,?,?,?)", "msg_w2", "sess_c1", 1,
		int64(1789820640200), zcodeJSON(t, map[string]any{
			"role": "assistant", "time": map[string]any{"created": 1789820640200},
		}))
	mustExec("INSERT INTO part (id, message_id, sequence, time_created, data) VALUES (?,?,?,?,?)", "q2", "msg_w2", 0,
		int64(1789820640200), zcodeJSON(t, map[string]any{
			"type": "compaction", "compactReason": "manual",
		}))

	mgr := NewSessionManager(filepath.Join(dir, "sessions"))
	imported, skipped, err := ImportZCodeSessions(dbPath, mgr)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 1 || skipped != 0 {
		t.Fatalf("imported=%d skipped=%d, want 1/0", imported, skipped)
	}
	s := loadImported(t, mgr, "c1")
	for _, e := range s.Entries {
		if e.Type == EntryCompaction {
			t.Fatal("compaction entry should be skipped without summary")
		}
	}
}

// TestZCodeImportDirtyMessage 脏消息数据只跳过该会话，不影响整体导入
func TestZCodeImportDirtyMessage(t *testing.T) {
	dir := t.TempDir()
	dbPath := newZCodeTestDB(t)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec("INSERT INTO session (id, directory, title, time_created, time_updated, task_type) VALUES (?,?,?,?,?,?)",
		"sess_bad", "/tmp/proj-e", "脏数据", int64(1789820641000), int64(1789820642000), "interactive")
	mustExec("INSERT INTO message (id, session_id, sequence, time_created, data) VALUES (?,?,?,?,?)",
		"msg_x1", "sess_bad", 0, int64(1789820641100), "{not-json")
	mustExec("INSERT INTO session (id, directory, title, time_created, time_updated, task_type) VALUES (?,?,?,?,?,?)",
		"sess_good", "/tmp/proj-e", "正常会话", int64(1789820642000), int64(1789820643000), "interactive")
	mustExec("INSERT INTO message (id, session_id, sequence, time_created, data) VALUES (?,?,?,?,?)",
		"msg_y1", "sess_good", 0, int64(1789820642100), zcodeJSON(t, map[string]any{
			"role": "user", "time": map[string]any{"created": 1789820642100},
		}))
	mustExec("INSERT INTO part (id, message_id, sequence, time_created, data) VALUES (?,?,?,?,?)",
		"r1", "msg_y1", 0, int64(1789820642100), zcodeJSON(t, map[string]any{"type": "text", "text": "你好"}))

	mgr := NewSessionManager(filepath.Join(dir, "sessions"))
	imported, skipped, err := ImportZCodeSessions(dbPath, mgr)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 1 || skipped != 1 {
		t.Fatalf("imported=%d skipped=%d, want 1/1", imported, skipped)
	}
	if mgr.FindSessionFile("bad") != "" {
		t.Fatal("dirty session should be skipped")
	}
	if mgr.FindSessionFile("good") == "" {
		t.Fatal("good session should be imported")
	}
}

// TestZCodeImportFileShape 校验落盘 JSONL 的行数与首行 header
func TestZCodeImportFileShape(t *testing.T) {
	dir := t.TempDir()
	dbPath := newZCodeTestDB(t)
	seedZCodeFixtures(t, dbPath)
	mgr := NewSessionManager(filepath.Join(dir, "sessions"))
	if _, _, err := ImportZCodeSessions(dbPath, mgr); err != nil {
		t.Fatalf("import: %v", err)
	}
	path := mgr.FindSessionFile("t1")
	if path == "" {
		t.Fatal("t1 not imported")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 7 { // header + 6 entries
		t.Fatalf("lines=%d, want 7", len(lines))
	}
	var hdr SessionHeader
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Type != "session" || hdr.ID != "t1" {
		t.Fatalf("header: %+v", hdr)
	}
	// 文件名沿用 <时间>_<id>.jsonl 约定
	base := filepath.Base(path)
	if !strings.HasSuffix(base, "_t1.jsonl") || !strings.HasPrefix(base, "2026") {
		t.Fatalf("file name: %s", base)
	}
}
