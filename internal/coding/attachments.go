package coding

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"doge/internal/ai"
	"doge/internal/coding/uploads"
)

// Attachment 是一条进对话的附件（来自上传暂存区），自带内容路径，
// 不回查 Store——web/mesh 等来源解析出路径后统一走这里。
type Attachment struct {
	Path   string
	Name   string
	MIME   string
	Size   int64
	SHA256 string
}

// AttachmentFromMeta 由暂存区元数据构造附件
func AttachmentFromMeta(m *uploads.Meta) Attachment {
	return Attachment{Path: m.Path, Name: m.Name, MIME: m.MIME, Size: m.Size, SHA256: m.SHA256}
}

// PromptWithAttachments 发送带附件的用户消息：图片在模型支持且不超上限时
// 直接多模态注入，小文本全文注入，其余以引用行提示 agent 用 read 工具读取。
// 运行中与 Prompt 一致，转为 steering 消息在当前轮后注入。
func (s *AgentSession) PromptWithAttachments(text string, atts []Attachment) error {
	msg := s.buildAttachmentMessage(text, atts)
	if s.Agent.State().IsStreaming {
		s.Agent.Steer(msg)
		return nil
	}
	s.checkCompaction()
	return s.Agent.Prompt(msg)
}

// buildAttachmentMessage 组装带附件的 user 消息
func (s *AgentSession) buildAttachmentMessage(text string, atts []Attachment) *ai.Message {
	cfg := s.Settings.UploadsConfig()
	supportsImages := false
	if m := s.Agent.State().Model; m != nil {
		supportsImages = m.SupportsImages()
	}
	var blocks []ai.ContentBlock
	if text = strings.TrimSpace(s.expandCommands(text)); text != "" {
		blocks = append(blocks, ai.TextBlock(text))
	}
	for i, att := range atts {
		switch {
		case strings.HasPrefix(att.MIME, "image/") && supportsImages && att.Size <= cfg.MaxImageInlineBytes:
			if data, err := os.ReadFile(att.Path); err == nil {
				blocks = append(blocks, ai.ImageBlock(data, att.MIME))
				continue
			}
			blocks = append(blocks, referenceBlock(i, len(atts), att, "图片读取失败，可稍后重试"))
		case isTextual(att) && 0 < att.Size && att.Size <= cfg.MaxTextInlineBytes:
			if data, err := os.ReadFile(att.Path); err == nil && utf8.Valid(data) {
				blocks = append(blocks, ai.TextBlock(fmt.Sprintf("[文件 %s, %s]\n%s", att.Name, uploads.HumanSize(att.Size), data)))
				continue
			}
			blocks = append(blocks, referenceBlock(i, len(atts), att, ""))
		default:
			hint := "可用 read 工具读取"
			if strings.HasPrefix(att.MIME, "image/") && !supportsImages {
				hint = "当前模型不支持图片输入，可用 read 工具读取"
			} else if strings.HasPrefix(att.MIME, "image/") {
				hint = "图片过大未直接注入，可用 read 工具读取"
			}
			blocks = append(blocks, referenceBlock(i, len(atts), att, hint))
		}
	}
	return &ai.Message{Role: ai.RoleUser, Content: blocks}
}

// referenceBlock 生成附件引用行（大文件默认注入形态）
func referenceBlock(i, total int, att Attachment, hint string) ai.ContentBlock {
	var b strings.Builder
	fmt.Fprintf(&b, "[附件 %d/%d] %s\n  size=%d mime=%s sha256=%s", i+1, total, att.Path, att.Size, att.MIME, shortHash(att.SHA256))
	if hint != "" {
		fmt.Fprintf(&b, " %s", hint)
	}
	return ai.TextBlock(b.String())
}

// isTextual 按 MIME 判断是否文本类内容（DetectContentType 对有效文本返回 text/*）
func isTextual(att Attachment) bool {
	return strings.HasPrefix(att.MIME, "text/")
}

// shortHash 哈希截前 8 位
func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
