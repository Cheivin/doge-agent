package services

import (
	"strings"
	"time"

	"doge/internal/coding"
	"doge/internal/coding/permissions"
	"doge/internal/web"
	"doge/pkg/logx"
)

// tailMessages 转录首屏返回的尾部消息条数，更早的经 MessagesBefore 分片
// 拉取——大转录（含 base64 图片）单次过 IPC 会冻结渲染
const tailMessages = 30

type (
	// SessionService 会话与对话控制（对应 /api/status|session|prompt 等）
	SessionService struct {
		d Deps
	}
)

// Snapshot 连接即快照（session_info 帧）
func (s *SessionService) Snapshot() any { return s.d.Console.Snapshot() }

// Status 实时状态
func (s *SessionService) Status() web.Status { return s.d.Backend.Status() }

// Stats 用量统计
func (s *SessionService) Stats() coding.SessionStats { return s.d.Backend.Stats() }

// Info 当前会话与转录（尾部 tailMessages 条 + earlierCount，更早的分片拉）
func (s *SessionService) Info() (map[string]any, error) {
	start := time.Now()
	info := s.d.Backend.SessionInfo()
	msgs := info.Messages
	earlier := 0
	if len(msgs) > tailMessages {
		earlier = len(msgs) - tailMessages
		msgs = msgs[earlier:]
	}
	logx.Infof("[desktop] Info msgs=%d earlier=%d cost=%s", earlier+len(msgs), earlier, time.Since(start))
	return map[string]any{
		"id":           info.ID,
		"cwd":          info.Cwd,
		"filePath":     info.FilePath,
		"messages":     msgs,
		"sourceName":   info.SourceName,
		"sourcePeerId": info.SourcePeerID,
		"kind":         info.Kind,
		"earlierCount": earlier,
	}, nil
}

// MessagesBefore 分片拉取 index 之前的消息（转录头部按需加载）
func (s *SessionService) MessagesBefore(index, limit int) (map[string]any, error) {
	start := time.Now()
	msgs := s.d.Backend.SessionInfo().Messages
	if index < 0 || index > len(msgs) {
		index = len(msgs)
	}
	if limit <= 0 || limit > 100 {
		limit = tailMessages
	}
	begin := index - limit
	if begin < 0 {
		begin = 0
	}
	logx.Infof("[desktop] MessagesBefore %d-%d/%d cost=%s", begin, index, len(msgs), time.Since(start))
	return map[string]any{"messages": msgs[begin:index], "offset": begin}, nil
}

// List 已持久化会话列表
func (s *SessionService) List() ([]web.SessionSummary, error) {
	sessions, err := s.d.Backend.ListSessions()
	if err != nil {
		return nil, errInternal(err)
	}
	return sessions, nil
}

// Models 可用模型列表；all=true 列全目录（含无凭证模型，配置面板用）
func (s *SessionService) Models(all bool) []web.ModelInfo {
	if all {
		return s.d.Backend.ListAllModels()
	}
	return s.d.Backend.ListModels()
}

// New 新建会话（cwd 空用当前目录），成功后发 session_reset
func (s *SessionService) New(cwd string) error {
	if err := s.d.Backend.NewSessionIn(strings.TrimSpace(cwd)); err != nil {
		return errReadOnly(err.Error())
	}
	s.d.Console.EmitFrame(map[string]any{"type": "session_reset"})
	return nil
}

// Switch 切换到已有会话（id 或路径），成功后发 session_reset
func (s *SessionService) Switch(id string) error {
	start := time.Now()
	if err := s.d.Backend.SwitchSession(id); err != nil {
		return errReadOnly(err.Error())
	}
	logx.Infof("[desktop] Switch %s cost=%s", id, time.Since(start))
	s.d.Console.EmitFrame(map[string]any{"type": "session_reset"})
	return nil
}

// Delete 删除持久化会话（当前会话不可删），成功后发 session_list_changed
func (s *SessionService) Delete(id string) error {
	if err := s.d.Backend.DeleteSession(id); err != nil {
		return errInvalid(err.Error())
	}
	s.d.Console.EmitFrame(map[string]any{"type": "session_list_changed"})
	return nil
}

// Prompt 注入用户消息（attachments 引用暂存区 uploadID），异步执行
func (s *SessionService) Prompt(text string, attachments []string) error {
	text = strings.TrimSpace(text)
	if text == "" && len(attachments) == 0 {
		return errInvalid("empty prompt")
	}
	// 附件前置解析：未知 id 同步失败
	atts := []coding.Attachment{}
	if len(attachments) > 0 {
		if s.d.Uploads == nil {
			return errUnavailable("uploads not available")
		}
		for _, id := range attachments {
			meta, err := s.d.Uploads.Meta(strings.TrimSpace(id))
			if err != nil {
				return errInvalid("unknown attachment: " + id)
			}
			atts = append(atts, coding.AttachmentFromMeta(meta))
		}
	}
	if s.d.Backend.Status().Streaming {
		return errBusy("agent is busy")
	}
	// Prompt 阻塞整个 run，丢给 goroutine 后立即确认
	if len(atts) == 0 {
		go func() { _ = s.d.Backend.Prompt(text) }()
	} else {
		go func() { _ = s.d.Backend.PromptWithAttachments(text, atts) }()
	}
	return nil
}

// Abort 中断当前 run
func (s *SessionService) Abort() {
	s.d.Backend.Abort()
}

// Compact 触发上下文压缩
func (s *SessionService) Compact() error {
	if err := s.d.Backend.Compact(); err != nil {
		return errBusy(err.Error())
	}
	return nil
}

// TurnEdit 回退到第 seq 条 user 消息并替换文本重跑
func (s *SessionService) TurnEdit(seq int, text string) error {
	if strings.TrimSpace(text) == "" {
		return errInvalid("empty message")
	}
	if err := s.d.Backend.RewriteTurn(seq, text); err != nil {
		return errBusy(err.Error())
	}
	return nil
}

// thinkingLevels SetThinking 接受的推理级别
var thinkingLevels = map[string]bool{"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true}

// SetThinking 调整推理级别
func (s *SessionService) SetThinking(level string) error {
	level = strings.TrimSpace(level)
	if !thinkingLevels[level] {
		return errInvalid("invalid thinking level (want minimal|low|medium|high|xhigh)")
	}
	if err := s.d.Backend.SetThinkingLevel(level); err != nil {
		return errBusy(err.Error())
	}
	return nil
}

// SetModel 切换模型
func (s *SessionService) SetModel(spec string) error {
	if strings.TrimSpace(spec) == "" {
		return errInvalid("empty model spec")
	}
	if err := s.d.Backend.SwitchModel(spec); err != nil {
		return errInvalid(err.Error())
	}
	return nil
}

// QuestionAnswer 应答远端任务提问
func (s *SessionService) QuestionAnswer(id, text string) error {
	id = strings.TrimSpace(id)
	text = strings.TrimSpace(text)
	if id == "" || text == "" {
		return errInvalid("id and text are required")
	}
	if !s.d.Backend.AnswerQuestion(id, text) {
		return errNotFound("question not pending")
	}
	return nil
}

// PermissionAnswer 应答权限询问（allowOnce/allowAlways/deny 三选一）
func (s *SessionService) PermissionAnswer(id string, allowOnce, allowAlways, deny bool) error {
	if strings.TrimSpace(id) == "" {
		return errInvalid("id required")
	}
	resp := permissions.AskResponse{AllowOnce: allowOnce, AllowAlways: allowAlways, Deny: deny}
	if !s.d.Console.AnswerPermission(id, resp) {
		return errNotFound("permission ask not pending")
	}
	return nil
}
