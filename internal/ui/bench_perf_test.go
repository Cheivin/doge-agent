package ui

import (
	"fmt"
	"strings"
	"testing"

	"doge/internal/ai"
)

func benchModel() *model {
	return &model{
		expandedThinking: map[string]bool{}, expandedTools: map[string]bool{},
		height: 40, width: 120, cwd: "/Users/devuser/workspace/cheivin/go/doge",
		sess: &fakeSession{}, foldLines: map[int]string{}, input: newInputBox(),
		historyIdx: -1, version: "0.1.0", showHeader: true,
	}
}

// longTranscript builds a session with historyMsgCount turns; each assistant
// reply has text of textSize bytes (roughly like a real session that has read
// files and answered at length).
func longTranscript(historyMsgCount, textSize int) *model {
	m := benchModel()
	para := strings.Repeat("这里有一段说明文字，描述代码逻辑和上下文。", textSize/40)
	for i := 0; i < historyMsgCount; i++ {
		m.messages = append(m.messages,
			&ai.Message{Role: ai.RoleUser, Content: []ai.ContentBlock{{Type: ai.ContentTypeText, Text: fmt.Sprintf("第 %d 个问题是什么？", i)}}},
			&ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{
				{Type: ai.ContentTypeThinking, Thinking: strings.Repeat("思考过程若干。", 50)},
				{Type: ai.ContentTypeText, Text: para + fmt.Sprintf("（回答 %d）", i)},
			}},
		)
		if i%3 == 0 {
			m.messages = append(m.messages, &ai.Message{Role: ai.RoleToolResult, ToolName: "bash", Content: []ai.ContentBlock{
				{Type: ai.ContentTypeText, Text: strings.Repeat("文件内容行，模拟工具输出。\n", 8)},
			}})
		}
	}
	return m
}

func BenchmarkViewMidSession(b *testing.B) {
	m := longTranscript(30, 2000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

func BenchmarkViewLongSession(b *testing.B) {
	m := longTranscript(120, 2000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

func BenchmarkViewStreamingWorst(b *testing.B) {
	// Long committed history + a streaming assistant message already several
	// KB in: the per-chunk re-render cost while streaming.
	m := longTranscript(120, 2000)
	m.streaming = &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{
		{Type: ai.ContentTypeThinking, Thinking: strings.Repeat("思考中文本。", 800)},
		{Type: ai.ContentTypeText, Text: strings.Repeat("正在生成的回复内容，一段解释。", 600)},
	}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

func BenchmarkViewStreamingGrow(b *testing.B) {
	// Emulates successive deltas: each frame the streaming message grew.
	para := strings.Repeat("正在生成的回复内容，一段解释。", 40)
	m := longTranscript(30, 2000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.streaming = &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{
			{Type: ai.ContentTypeThinking, Thinking: strings.Repeat("思考中文本。", i%800)},
			{Type: ai.ContentTypeText, Text: strings.Repeat(para, (i%40)+1)},
		}}
		_ = m.View()
	}
}

// BenchmarkStreamingFullRender measures the cost of one throttled full render
// of a mid-size streaming snapshot (the ~12fps glamour re-parse).
func BenchmarkStreamingFullRender(b *testing.B) {
	md := "# 标题\n\n正文段落 **加粗** 与 `代码`。\n\n1. 一\n2. 二\n\n```go\nfunc main() { fmt.Println(\"hi\") }\n```\n"
	var text strings.Builder
	for i := 0; i < 20; i++ {
		text.WriteString(md)
		text.WriteString("\n\n")
	}
	m := longTranscript(30, 2000)
	m.streaming = &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{
		{Type: ai.ContentTypeText, Text: text.String()},
	}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.streamCache = streamingCache{}
		_ = m.View()
	}
}
