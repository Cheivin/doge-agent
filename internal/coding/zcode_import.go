// ZCode 会话导入：把 ZCode 桌面端 SQLite 会话库转成 doge 的 JSONL 会话文件
package coding

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"doge/internal/ai"
	"doge/pkg/logx"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动
)

type (
	// zcodeSessionRow 是 ZCode session 表中导入需要的字段
	zcodeSessionRow struct {
		ID          string // 会话 id，形如 sess_xxx
		Directory   string // 会话工作目录
		Title       string // 会话标题
		TimeCreated int64  // 创建时间（毫秒）
	}

	// zcodeMessageRow 是 ZCode message 表的一行
	zcodeMessageRow struct {
		ID   string // 消息 id
		Data string // 原始 JSON
	}

	// zcodeMessageData 是 message.data JSON 中导入用到的字段
	zcodeMessageData struct {
		Role       string `json:"role"`       // user 或 assistant
		ModelID    string `json:"modelId"`    // assistant 模型
		ProviderID string `json:"providerId"` // assistant 供应商
		Time       struct {
			Created   int64 `json:"created"`   // 创建时间（毫秒）
			Completed int64 `json:"completed"` // 完成时间（毫秒）
		} `json:"time"`
		Metadata struct {
			Visibility string `json:"visibility"` // model-only 表示不展示给用户的注入物
		} `json:"metadata"`
		Semantics struct {
			Kind string `json:"kind"` // user_prompt 为真实用户输入，其余是注入物
		} `json:"semantics"`
		Summary *struct {
			Title string `json:"title"` // 摘要标题
			Body  string `json:"body"`  // 摘要正文
		} `json:"summary"` // 压缩摘要消息才有
	}

	// zcodePartData 是 part.data JSON 中导入用到的字段
	zcodePartData struct {
		Type   string `json:"type"`   // text/reasoning/tool/step-start/step-finish/timeline/file/compaction
		Text   string `json:"text"`   // text/reasoning 正文
		CallID string `json:"callID"` // tool 调用 id
		Tool   string `json:"tool"`   // 工具名
		State  struct {
			Status string         `json:"status"` // completed/error/running/pending
			Input  map[string]any `json:"input"`  // 工具入参
			Output string         `json:"output"` // 工具输出
		} `json:"state"`
		SummaryMessageID     string `json:"summaryMessageId"`     // 压缩摘要所在消息 id
		TailStartID          string `json:"tail_start_id"`        // 压缩后保留消息的起始 id
		PreCompactTokenCount int    `json:"preCompactTokenCount"` // 压缩前 token 数
		Time                 struct {
			Start int64 `json:"start"` // 开始时间（毫秒）
			End   int64 `json:"end"`   // 结束时间（毫秒）
		} `json:"time"`
	}

	// zcodeConverted 是一个 ZCode 会话转换出的待写入条目集合
	zcodeConverted struct {
		entries    []*SessionEntry          // 按序待写入
		msgCount   int                      // 消息条目数，0 表示无可导入内容
		firstByMsg map[string]*SessionEntry // zcode 消息 id 到首个条目
		tailOf     map[*SessionEntry]string // 压缩条目到保留起始消息 id
	}
)

// ImportZCodeSessions 把 ZCode 会话库中的 interactive 会话导入 doge 会话目录，
// 返回导入与跳过的数量；目标已存在、无消息、转换或写入失败都算跳过，不中断整体
func ImportZCodeSessions(dbPath string, mgr *SessionManager) (imported, skipped int, err error) {
	db, err := openZCodeDB(dbPath)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = db.Close() }()

	var sessions []zcodeSessionRow
	rows, err := db.Query("SELECT id, directory, title, time_created FROM session WHERE task_type = 'interactive' ORDER BY time_created")
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var zs zcodeSessionRow
		if err := rows.Scan(&zs.ID, &zs.Directory, &zs.Title, &zs.TimeCreated); err != nil {
			rows.Close()
			return 0, 0, err
		}
		sessions = append(sessions, zs)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	for _, zs := range sessions {
		// 会话 id 去掉 sess_ 前缀做 doge 会话 id，文件名安全
		id := strings.TrimPrefix(zs.ID, "sess_")
		if mgr.FindSessionFile(id) != "" {
			skipped++
			continue
		}
		conv, err := convertZCodeSession(db, zs)
		if err != nil {
			logx.Warnf("[zcode-import] %s 转换失败, 跳过: %v", zs.ID, err)
			skipped++
			continue
		}
		if conv.msgCount == 0 {
			skipped++
			continue
		}
		if err := writeZCodeSession(mgr, zs, conv); err != nil {
			logx.Warnf("[zcode-import] %s 写入失败, 跳过: %v", zs.ID, err)
			skipped++
			continue
		}
		imported++
	}
	return imported, skipped, nil
}

// openZCodeDB 只读打开 ZCode 会话库（运行中被写，禁止任何写操作）
func openZCodeDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// 单连接保证 PRAGMA 对后续所有查询生效
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// convertZCodeSession 把一个 ZCode 会话的 message/part 转成 doge 条目（不落盘）
func convertZCodeSession(db *sql.DB, zs zcodeSessionRow) (*zcodeConverted, error) {
	conv := &zcodeConverted{
		firstByMsg: map[string]*SessionEntry{},
		tailOf:     map[*SessionEntry]string{},
	}
	msgs, err := loadZCodeMessages(db, zs.ID)
	if err != nil || len(msgs) == 0 {
		return conv, err
	}
	// 预解析消息 data，压缩摘要回查要用
	dataByID := make(map[string]*zcodeMessageData, len(msgs))
	for _, m := range msgs {
		md := &zcodeMessageData{}
		if err := json.Unmarshal([]byte(m.Data), md); err != nil {
			return nil, fmt.Errorf("parse message %s: %w", m.ID, err)
		}
		dataByID[m.ID] = md
	}

	for _, m := range msgs {
		md := dataByID[m.ID]
		parts, err := loadZCodeParts(db, m.ID)
		if err != nil {
			return nil, err
		}
		switch md.Role {
		case ai.RoleUser:
			// reminder/通知/压缩摘要等注入物不是真实用户输入
			if !zcodeUserVisible(md) {
				continue
			}
			text := zcodeUserText(parts)
			if text == "" {
				continue
			}
			e := &SessionEntry{
				Timestamp: md.Time.Created,
				Type:      EntryMessage,
				Message: &ai.Message{
					Role:      ai.RoleUser,
					Content:   []ai.ContentBlock{ai.TextBlock(text)},
					Timestamp: md.Time.Created,
				},
			}
			conv.entries = append(conv.entries, e)
			conv.firstByMsg[m.ID] = e
			conv.msgCount++

		case ai.RoleAssistant:
			var blocks []ai.ContentBlock
			var results []*SessionEntry
			var compactions []*SessionEntry
			for i := range parts {
				p := &parts[i]
				switch p.Type {
				case "text":
					blocks = append(blocks, ai.TextBlock(p.Text))
				case "reasoning":
					blocks = append(blocks, ai.ThinkingBlock(p.Text))
				case "tool":
					// running/pending 还没有结果，不导出
					if p.State.Status != "completed" && p.State.Status != "error" {
						continue
					}
					blocks = append(blocks, ai.ToolCallBlock(p.CallID, p.Tool, p.State.Input))
					results = append(results, &SessionEntry{
						Timestamp: md.Time.Created,
						Type:      EntryMessage,
						Message: &ai.Message{
							Role:       ai.RoleToolResult,
							Content:    []ai.ContentBlock{ai.TextBlock(p.State.Output)},
							ToolCallID: p.CallID,
							ToolName:   p.Tool,
							IsError:    p.State.Status == "error",
							Timestamp:  md.Time.Created,
						},
					})
				case "compaction":
					// 只导出带摘要的压缩记录；进行中标记和无摘要的手动压缩跳过
					sum := zcodeSummaryBody(dataByID, p.SummaryMessageID)
					if sum == "" {
						continue
					}
					ts := p.Time.End
					if ts == 0 {
						ts = md.Time.Created
					}
					e := &SessionEntry{
						Timestamp:    ts,
						Type:         EntryCompaction,
						Summary:      sum,
						TokensBefore: p.PreCompactTokenCount,
					}
					if p.TailStartID != "" {
						conv.tailOf[e] = p.TailStartID
					}
					compactions = append(compactions, e)
				}
			}
			if len(blocks) > 0 {
				e := &SessionEntry{
					Timestamp: md.Time.Created,
					Type:      EntryMessage,
					Message: &ai.Message{
						Role:      ai.RoleAssistant,
						Provider:  md.ProviderID,
						Model:     md.ModelID,
						Content:   blocks,
						Timestamp: md.Time.Created,
					},
				}
				conv.entries = append(conv.entries, e)
				conv.firstByMsg[m.ID] = e
				conv.msgCount++
			}
			// 工具结果紧跟助手消息，压缩条目收尾（压缩边界在本轮之后）
			conv.entries = append(conv.entries, results...)
			conv.entries = append(conv.entries, compactions...)
			conv.msgCount += len(results)
		}
	}
	return conv, nil
}

// writeZCodeSession 把转换结果落盘为 doge 会话文件，标题写进 session_info
func writeZCodeSession(mgr *SessionManager, zs zcodeSessionRow, conv *zcodeConverted) error {
	id := strings.TrimPrefix(zs.ID, "sess_")
	s, err := mgr.CreateSession(zs.Directory)
	if err != nil {
		return err
	}
	// 覆写为 ZCode 的会话 id 和创建时间，文件名沿用 <时间>_<id>.jsonl 约定
	s.Header.ID = id
	s.Header.Timestamp = zs.TimeCreated
	s.FilePath = filepath.Join(filepath.Dir(s.FilePath), sessionFileName(time.UnixMilli(zs.TimeCreated), id))

	for _, e := range conv.entries {
		// 回填压缩条目的保留起点，起点条目一定先于压缩条目写入
		if from := conv.tailOf[e]; from != "" {
			if t := conv.firstByMsg[from]; t != nil && t.ID != "" {
				e.FirstKeptEntryID = t.ID
			}
		}
		if err := s.AppendCustomEntry(e); err != nil {
			return err
		}
	}
	return s.AppendCustomEntry(&SessionEntry{Type: EntrySessionInfo, Name: zs.Title})
}

// loadZCodeMessages 按 sequence 读出一个会话的全部消息
func loadZCodeMessages(db *sql.DB, sessionID string) ([]zcodeMessageRow, error) {
	rows, err := db.Query("SELECT id, data FROM message WHERE session_id = ? ORDER BY sequence, time_created, id", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []zcodeMessageRow
	for rows.Next() {
		var m zcodeMessageRow
		if err := rows.Scan(&m.ID, &m.Data); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// loadZCodeParts 按 sequence 读出一条消息的全部 part 并解析
func loadZCodeParts(db *sql.DB, messageID string) ([]zcodePartData, error) {
	rows, err := db.Query("SELECT data FROM part WHERE message_id = ? ORDER BY sequence, time_created, id", messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []zcodePartData
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		p := zcodePartData{}
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			return nil, fmt.Errorf("parse part of message %s: %w", messageID, err)
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

// zcodeUserVisible 判断用户消息是否真实用户输入，model-only 及非 user_prompt 语义都是注入物
func zcodeUserVisible(md *zcodeMessageData) bool {
	if md.Metadata.Visibility == "model-only" {
		return false
	}
	switch md.Semantics.Kind {
	case "", "user_prompt":
		return true
	default:
		return false
	}
}

// zcodeUserText 拼接用户消息的文本 part，过滤 system-reminder 注入物
func zcodeUserText(parts []zcodePartData) string {
	var sb strings.Builder
	for _, p := range parts {
		if p.Type != "text" || strings.HasPrefix(p.Text, "<system-reminder>") {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(p.Text)
	}
	return strings.TrimSpace(sb.String())
}

// zcodeSummaryBody 取压缩摘要消息的正文
func zcodeSummaryBody(dataByID map[string]*zcodeMessageData, msgID string) string {
	md := dataByID[msgID]
	if md == nil || md.Summary == nil {
		return ""
	}
	return md.Summary.Body
}
