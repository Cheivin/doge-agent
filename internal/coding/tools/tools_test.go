package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
)

func tmpDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runTool(t *testing.T, tool *agent.AgentTool, params map[string]any) *agent.AgentToolResult {
	t.Helper()
	result, err := tool.Execute("call_1", params, context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return result
}

func resultText(r *agent.AgentToolResult) string {
	var out strings.Builder
	for _, b := range r.Content {
		if b.Type == ai.ContentTypeText {
			out.WriteString(b.Text)
		}
	}
	return out.String()
}

func TestReadTool(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "line1\nline2\nline3\nline4\nline5\n")

	ops := LocalOperations()
	tool := ReadTool(ops)

	r := runTool(t, tool, map[string]any{"path": path})
	if !strings.Contains(resultText(r), "line1") {
		t.Errorf("read result = %q", resultText(r))
	}
	if !strings.Contains(resultText(r), "1\tline1") || !strings.Contains(resultText(r), "5\tline5") {
		t.Errorf("read result missing line numbers: %q", resultText(r))
	}

	// offset/limit paging.
	r = runTool(t, tool, map[string]any{"path": path, "offset": 3.0, "limit": 2.0})
	text := resultText(r)
	if !strings.Contains(text, "line3") || strings.Contains(text, "line1") {
		t.Errorf("paged read = %q", text)
	}
	if !strings.Contains(text, "3\tline3") || !strings.Contains(text, "4\tline4") {
		t.Errorf("paged read line numbers should reflect real file lines: %q", text)
	}
	if !strings.Contains(text, "Use offset=5 to continue") {
		t.Errorf("paged read missing continue hint: %q", text)
	}

	// Missing file.
	r = runTool(t, tool, map[string]any{"path": filepath.Join(dir, "nope.txt")})
	if !strings.Contains(resultText(r), "read:") {
		t.Errorf("missing file error = %q", resultText(r))
	}
}

func TestReadToolImage(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "img.png")
	writeFile(t, path, "\x89PNG-fake-bytes")

	ops := LocalOperations()
	r := runTool(t, ReadTool(ops), map[string]any{"path": path})
	if len(r.Content) != 1 || r.Content[0].Type != ai.ContentTypeImage || r.Content[0].MimeType != "image/png" {
		t.Errorf("image read = %+v", r.Content)
	}
}

func TestWriteTool(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "nested", "b.txt")
	ops := LocalOperations()
	r := runTool(t, WriteTool(ops), map[string]any{"path": path, "content": "hello"})
	if !strings.Contains(resultText(r), "Successfully wrote 5 bytes") {
		t.Errorf("write result = %q", resultText(r))
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "hello" {
		t.Errorf("file content = %q, %v", data, err)
	}
}

func TestEditTool(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "e.txt")
	writeFile(t, path, "alpha\nbeta\ngamma\n")

	ops := LocalOperations()
	tool := EditTool(ops)

	r := runTool(t, tool, map[string]any{
		"path": path,
		"edits": []any{map[string]any{
			"oldText": "beta",
			"newText": "BETA",
		}},
	})
	if !strings.Contains(resultText(r), "Edited") {
		t.Errorf("edit result = %q", resultText(r))
	}
	data, _ := os.ReadFile(path)
	if string(data) != "alpha\nBETA\ngamma\n" {
		t.Errorf("edited content = %q", data)
	}
	details, _ := r.Details.(map[string]any)
	if diff, ok := details["diff"].(string); !ok || !strings.Contains(diff, "-beta") || !strings.Contains(diff, "+BETA") {
		t.Errorf("diff missing: %v", details["diff"])
	}

	// Non-unique oldText.
	r = runTool(t, tool, map[string]any{
		"path":  path,
		"edits": []any{map[string]any{"oldText": "a", "newText": "x"}},
	})
	if !strings.Contains(resultText(r), "not unique") {
		t.Errorf("non-unique edit should error: %q", resultText(r))
	}

	// Missing oldText.
	r = runTool(t, tool, map[string]any{
		"path":  path,
		"edits": []any{map[string]any{"oldText": "zzz", "newText": "x"}},
	})
	if !strings.Contains(resultText(r), "not found") {
		t.Errorf("missing oldText should error: %q", resultText(r))
	}

	// Overlapping edits.
	writeFile(t, path, "abcdef")
	r = runTool(t, tool, map[string]any{
		"path": path,
		"edits": []any{
			map[string]any{"oldText": "abc", "newText": "x"},
			map[string]any{"oldText": "bcd", "newText": "y"},
		},
	})
	if !strings.Contains(resultText(r), "overlap") {
		t.Errorf("overlapping edits should error: %q", resultText(r))
	}
}

// TestEditToolConcurrent 并发 edit 同一文件不得丢更新：读→改→写全程持
// 路径锁，每个 goroutine 插入一行，结束后三行都必须在（-race 下运行）
func TestEditToolConcurrent(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "c.txt")
	writeFile(t, path, "start\n")

	ops := LocalOperations()
	tool := EditTool(ops)

	lines := []string{"line-a", "line-b", "line-c"}
	var wg sync.WaitGroup
	for _, line := range lines {
		wg.Add(1)
		go func(line string) {
			defer wg.Done()
			r, err := tool.Execute("call", map[string]any{
				"path":  path,
				"edits": []any{map[string]any{"oldText": "start", "newText": "start\n" + line}},
			}, context.Background(), nil)
			if err != nil {
				t.Error(err)
				return
			}
			if !strings.Contains(resultText(r), "Edited") {
				t.Errorf("edit result = %q", resultText(r))
			}
		}(line)
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if !strings.Contains(string(data), line) {
			t.Errorf("lost update: %q missing from %q", line, data)
		}
	}
	// 锚点只插入一次：锁串行化后每个 goroutine 都基于最新内容重读重算
	if got := strings.Count(string(data), "start"); got != 1 {
		t.Errorf("anchor count = %d, want 1: %q", got, data)
	}
}

func TestEditToolFuzzyMatch(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "f.txt")
	// Smart quotes and trailing whitespace in the file.
	writeFile(t, path, "He said \u201chello\u201d  \nnext line\n")

	ops := LocalOperations()
	r := runTool(t, EditTool(ops), map[string]any{
		"path": path,
		"edits": []any{map[string]any{
			"oldText": "He said \"hello\"",
			"newText": "She said \"hi\"",
		}},
	})
	if !strings.Contains(resultText(r), "Edited") {
		t.Fatalf("fuzzy edit failed: %q", resultText(r))
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "She said") {
		t.Errorf("fuzzy edited content = %q", data)
	}
	// Unchanged bytes of untouched lines are preserved.
	if !strings.Contains(string(data), "next line") {
		t.Errorf("untouched line changed: %q", data)
	}
}

func TestBashTool(t *testing.T) {
	ops := LocalOperations()
	tool := BashTool(ops)

	r := runTool(t, tool, map[string]any{"command": "echo hello"})
	if !strings.Contains(resultText(r), "hello") {
		t.Errorf("bash result = %q", resultText(r))
	}

	// Non-zero exit.
	r = runTool(t, tool, map[string]any{"command": "exit 3"})
	if !strings.Contains(resultText(r), "code 3") {
		t.Errorf("exit code error = %q", resultText(r))
	}

	// Timeout.
	start := time.Now()
	r = runTool(t, tool, map[string]any{"command": "sleep 10", "timeout": 1.0})
	if !strings.Contains(resultText(r), "timed out") {
		t.Errorf("timeout error = %q", resultText(r))
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout took too long")
	}

	// Abort cancels the command.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = tool.Execute("call_1", map[string]any{"command": "sleep 30"}, ctx, nil)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("bash did not abort")
	}
}

// TestLocalExecStderrFlood 子进程向 stderr 写 200KiB 而 stdout 迟迟不关时
// 必须立即返回：修复前先排空 stdout 再读 stderr，stderr 管道 64KiB 写满后
// 子进程阻塞 → 互等直到超时被杀
func TestLocalExecStderrFlood(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	ops := LocalOperations()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := time.Now()
	res, err := ops.Exec(ctx, "head -c 204800 /dev/zero >&2; echo STDERR_MARK_OK >&2; echo stdout_done", ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("exec blocked for %v (drain deadlock)", elapsed)
	}
	if res.TimedOut {
		t.Fatal("command unexpectedly timed out")
	}
	// stdout 与 stderr 合并捕获，两端都要拿到
	if !strings.Contains(res.Stdout, "STDERR_MARK_OK") {
		t.Errorf("stderr tail not captured: %q", res.Stdout[len(res.Stdout)-200:])
	}
	if !strings.Contains(res.Stdout, "stdout_done") {
		t.Errorf("stdout not captured: %q", res.Stdout[len(res.Stdout)-200:])
	}
}

func TestGrepTool(t *testing.T) {
	dir := tmpDir(t)
	writeFile(t, filepath.Join(dir, "main.go"), "package main\nfunc main() {}\n")
	writeFile(t, filepath.Join(dir, "other.txt"), "no match here\n")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	writeFile(t, filepath.Join(dir, "sub", "util.go"), "func helper() {}\n")

	ops := LocalOperations()
	tool := GrepTool(ops)

	r := runTool(t, tool, map[string]any{"pattern": "func", "path": dir})
	text := resultText(r)
	if !strings.Contains(text, "main.go:2") || !strings.Contains(text, "util.go:1") {
		t.Errorf("grep result = %q", text)
	}
	if strings.Contains(text, "other.txt") {
		t.Errorf("grep matched wrong file: %q", text)
	}

	// Glob filter.
	r = runTool(t, tool, map[string]any{"pattern": "func", "path": dir, "glob": "*.txt"})
	if resultText(r) != "No matches found" {
		t.Errorf("glob filter result = %q", resultText(r))
	}

	// Literal + ignoreCase.
	r = runTool(t, tool, map[string]any{"pattern": "FUNC", "path": dir, "ignoreCase": true, "literal": true})
	if !strings.Contains(resultText(r), "main.go:2") {
		t.Errorf("ignoreCase result = %q", resultText(r))
	}
}

// TestGrepRelativeFileWithSessionCwd 会话 cwd 与进程 cwd 不同时，单文件
// 相对路径也必须按会话 cwd 解析（web/remote 窗口场景），不得误报 No matches
func TestGrepRelativeFileWithSessionCwd(t *testing.T) {
	sessionDir := tmpDir(t)
	writeFile(t, filepath.Join(sessionDir, "note.txt"), "needle here\n")

	// 进程 cwd（go test 的包目录）在别处，相对路径只随会话 cwd 解析
	ops := WithCwd(LocalOperations(), NewCwdCell(sessionDir))
	r := runTool(t, GrepTool(ops), map[string]any{"pattern": "needle", "path": "note.txt"})
	text := resultText(r)
	if !strings.Contains(text, "note.txt:1") || !strings.Contains(text, "needle here") {
		t.Errorf("grep relative file with session cwd = %q", text)
	}

	// 目录模式回归：同样场景下走 WalkDir 的路径保持可用
	r = runTool(t, GrepTool(ops), map[string]any{"pattern": "needle", "path": "."})
	if !strings.Contains(resultText(r), "needle here") {
		t.Errorf("grep relative dir with session cwd = %q", resultText(r))
	}
}

func TestGrepRespectsGitignore(t *testing.T) {
	dir := tmpDir(t)
	writeFile(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
	writeFile(t, filepath.Join(dir, "ignored.txt"), "secret pattern\n")
	writeFile(t, filepath.Join(dir, "kept.txt"), "secret pattern\n")

	ops := LocalOperations()
	r := runTool(t, GrepTool(ops), map[string]any{"pattern": "secret", "path": dir})
	text := resultText(r)
	if !strings.Contains(text, "kept.txt") {
		t.Errorf("kept file missing: %q", text)
	}
	if strings.Contains(text, "ignored.txt") {
		t.Errorf("gitignored file matched: %q", text)
	}
}

func TestFindTool(t *testing.T) {
	dir := tmpDir(t)
	writeFile(t, filepath.Join(dir, "a.go"), "x")
	writeFile(t, filepath.Join(dir, "b.txt"), "x")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	writeFile(t, filepath.Join(dir, "sub", "c.go"), "x")

	ops := LocalOperations()
	r := runTool(t, FindTool(ops), map[string]any{"pattern": "**/*.go", "path": dir})
	text := resultText(r)
	if !strings.Contains(text, "a.go") || !strings.Contains(text, "sub/c.go") {
		t.Errorf("find result = %q", text)
	}
	if strings.Contains(text, "b.txt") {
		t.Errorf("find matched wrong extension: %q", text)
	}
}

func TestLSTool(t *testing.T) {
	dir := tmpDir(t)
	os.MkdirAll(filepath.Join(dir, "Zdir"), 0o755)
	writeFile(t, filepath.Join(dir, "alpha.txt"), "x")
	writeFile(t, filepath.Join(dir, "beta.go"), "x")

	ops := LocalOperations()
	r := runTool(t, LSTool(ops), map[string]any{"path": dir})
	text := resultText(r)
	if !strings.Contains(text, "alpha.txt") || !strings.Contains(text, "Zdir/") {
		t.Errorf("ls result = %q", text)
	}
	// Case-insensitive sort: alpha before beta before Zdir.
	ai, bi, zi := strings.Index(text, "alpha"), strings.Index(text, "beta"), strings.Index(text, "Zdir")
	if !(ai < bi && bi < zi) {
		t.Errorf("ls not sorted case-insensitively: %q", text)
	}
}

func TestTruncateOutput(t *testing.T) {
	// Long output is truncated to the byte limit.
	long := strings.Repeat("x", MaxOutputBytes+100)
	out := truncateOutput(long)
	if len(out) > MaxOutputBytes {
		t.Errorf("truncate exceeded limit: %d", len(out))
	}
}
