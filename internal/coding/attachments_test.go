package coding

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doge/internal/ai"
	"doge/internal/coding/uploads"
)

// newAttachmentSession 构建带 mock 模型的会话（不发起 run，只测消息组装）
func newAttachmentSession(t *testing.T, imageInput bool) *AgentSession {
	t.Helper()
	dir := t.TempDir()
	auth := LoadAuthStore(filepath.Join(dir, "auth.json"))
	runtime := NewModelRuntime(auth)
	input := []string{"text"}
	if imageInput {
		input = append(input, "image")
	}
	runtime.Models.AddModel(&ai.Model{
		ID: "mock-model", API: ai.APIOpenAICompletions, Provider: "mock", BaseURL: "http://127.0.0.1:1",
		ContextWindow: 100000, MaxTokens: 4096, Input: input,
	})
	model, _, err := runtime.ResolveModel("mock/mock-model")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := LoadSettings(filepath.Join(dir, "settings.json"), filepath.Join(dir, "proj", ".doge", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewAgentSession(AgentSessionOptions{
		Cwd:            dir,
		Model:          model,
		SessionManager: NewSessionManager(filepath.Join(dir, "sessions")),
		Settings:       settings,
		ModelRuntime:   runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestBuildAttachmentMessage(t *testing.T) {
	store := uploads.NewStore(t.TempDir(), uploads.Config{})
	png, err := store.Save("shot.png", "web", "", bytes.NewReader(append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("p"), 64)...)))
	if err != nil {
		t.Fatal(err)
	}
	txt, err := store.Save("note.md", "web", "", strings.NewReader("# 标题\n正文"))
	if err != nil {
		t.Fatal(err)
	}
	big, err := store.Save("data.bin", "web", "", bytes.NewReader(bytes.Repeat([]byte{0x00, 0x01, 0xfe, 0xff}, 1000)))
	if err != nil {
		t.Fatal(err)
	}

	sess := newAttachmentSession(t, true)
	msg := sess.buildAttachmentMessage("看下这三个文件", []Attachment{
		AttachmentFromMeta(png), AttachmentFromMeta(txt), AttachmentFromMeta(big),
	})
	if msg.Role != ai.RoleUser || len(msg.Content) != 4 {
		t.Fatalf("role=%s blocks=%d", msg.Role, len(msg.Content))
	}
	if msg.Content[0].Type != ai.ContentTypeText || msg.Content[0].Text != "看下这三个文件" {
		t.Fatalf("first block = %#v", msg.Content[0])
	}
	if msg.Content[1].Type != ai.ContentTypeImage || msg.Content[1].MimeType != "image/png" {
		t.Fatalf("png block = %#v", msg.Content[1])
	}
	if msg.Content[2].Type != ai.ContentTypeText || !strings.Contains(msg.Content[2].Text, "[文件 note.md") || !strings.Contains(msg.Content[2].Text, "标题") {
		t.Fatalf("text block = %#v", msg.Content[2])
	}
	ref := msg.Content[3].Text
	if !strings.Contains(ref, "[附件 3/3]") || !strings.Contains(ref, "data.bin") || !strings.Contains(ref, "read") {
		t.Fatalf("reference block = %q", ref)
	}
}

func TestBuildAttachmentMessageNoImageSupport(t *testing.T) {
	store := uploads.NewStore(t.TempDir(), uploads.Config{})
	png, err := store.Save("shot.png", "web", "", bytes.NewReader(append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("p"), 64)...)))
	if err != nil {
		t.Fatal(err)
	}

	// 模型不支持图片 → 引用行降级，提示原因
	sess := newAttachmentSession(t, false)
	msg := sess.buildAttachmentMessage("", []Attachment{AttachmentFromMeta(png)})
	if len(msg.Content) != 1 || msg.Content[0].Type != ai.ContentTypeText {
		t.Fatalf("blocks = %#v", msg.Content)
	}
	if !strings.Contains(msg.Content[0].Text, "当前模型不支持图片输入") {
		t.Fatalf("ref = %q", msg.Content[0].Text)
	}
}

func TestBuildAttachmentMessageEmptyText(t *testing.T) {
	store := uploads.NewStore(t.TempDir(), uploads.Config{})
	txt, err := store.Save("a.txt", "web", "", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	sess := newAttachmentSession(t, true)
	msg := sess.buildAttachmentMessage("  ", []Attachment{AttachmentFromMeta(txt)})
	// 空文本不产生空 text 块，消息只有附件块
	if len(msg.Content) != 1 || !strings.Contains(msg.Content[0].Text, "hello") {
		t.Fatalf("blocks = %#v", msg.Content)
	}
}

func TestReloadSkillsHotRefresh(t *testing.T) {
	t.Setenv("DOGE_CODING_AGENT_DIR", t.TempDir())
	sess := newAttachmentSession(t, false)
	if len(sess.skills) != 0 {
		t.Fatalf("initial skills = %d", len(sess.skills))
	}
	// 装一个技能（.md 直接算技能）后 reload
	dir := filepath.Join(sess.Cwd, ".doge", "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: demo\ndescription: 热刷新测试\n---\nbody"
	if err := os.WriteFile(filepath.Join(dir, "demo.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sess.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if len(sess.skills) != 1 || sess.skills[0].Name != "demo" {
		t.Fatalf("skills after reload = %+v", sess.skills)
	}
	hasSkillTool := false
	for _, tool := range sess.Agent.State().Tools {
		if tool.Name == "skill" {
			hasSkillTool = true
		}
	}
	if !hasSkillTool {
		t.Fatal("skill tool missing after hot reload")
	}
	// 删掉技能再 reload：skill 工具应被摘除
	if err := os.Remove(filepath.Join(dir, "demo.md")); err != nil {
		t.Fatal(err)
	}
	if err := sess.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if len(sess.skills) != 0 {
		t.Fatalf("skills after removal = %+v", sess.skills)
	}
	for _, tool := range sess.Agent.State().Tools {
		if tool.Name == "skill" {
			t.Fatal("skill tool should be removed when no skills left")
		}
	}
}
