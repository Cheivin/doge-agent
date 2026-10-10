package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doge/internal/ai"
	"doge/pkg/logx"
)

func TestExportToHTML(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(filepath.Join(dir, "sessions"))
	s, err := m.CreateSession("/proj")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.AppendMessage(ai.NewUserTextMessage("hello"))
	_ = s.AppendMessage(&ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{ai.TextBlock("hi <there> & more")}})

	outPath := filepath.Join(dir, "session.html")
	if err := ExportToHTML(s, outPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	if !strings.Contains(html, "<html>") || !strings.Contains(html, "hi &lt;there&gt;") {
		t.Errorf("html = %q", html[:200])
	}
	if !strings.Contains(html, "/proj") {
		t.Errorf("cwd missing from html")
	}
}

func TestDebugLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doge.log")
	if err := logx.Setup(logx.Options{Path: path, Level: logx.LevelDebug}); err != nil {
		t.Fatal(err)
	}
	defer logx.Close()
	logx.Debugf("event: %s", "test")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "event: test") {
		t.Errorf("log = %q", data)
	}
}
