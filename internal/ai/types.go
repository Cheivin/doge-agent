// Package ai 统一多供应商 LLM API 层，提供消息模型、内容块、流式事件和 API 适配器
package ai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

// 消息角色
const (
	RoleUser       = "user"
	RoleAssistant  = "assistant"
	RoleToolResult = "toolResult"
)

// 停止原因
const (
	StopReasonPending = "pending"
	StopReasonStop    = "stop"
	StopReasonLength  = "length"
	StopReasonToolUse = "toolUse"
	StopReasonError   = "error"
	StopReasonAborted = "aborted"
)

// 内容块类型
const (
	ContentTypeText     = "text"
	ContentTypeThinking = "thinking"
	ContentTypeImage    = "image"
	ContentTypeToolCall = "toolCall"
)

// API 适配器标识
const (
	APIOpenAIResponses   = "openai-responses"
	APIOpenAICompletions = "openai-completions"
	APIAnthropicMessages = "anthropic-messages"
)

// ContentBlock 内容块，区分类型联合体
type ContentBlock struct {
	Type string // 类型

	// 文本块
	Text          string
	TextSignature string

	// 思考块
	Thinking          string
	ThinkingSignature string
	Redacted          bool

	// 图片块，Data 为原始字节，JSON 中 base64 编码
	Data     []byte
	MimeType string

	// 工具调用块
	ToolCallID       string
	ToolName         string
	Arguments        map[string]any
	ThoughtSignature string
}

// TextBlock 返回文本内容块
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: ContentTypeText, Text: text}
}

// ThinkingBlock 返回思考内容块
func ThinkingBlock(thinking string) ContentBlock {
	return ContentBlock{Type: ContentTypeThinking, Thinking: thinking}
}

// ImageBlock 返回图片内容块
func ImageBlock(data []byte, mimeType string) ContentBlock {
	return ContentBlock{Type: ContentTypeImage, Data: data, MimeType: mimeType}
}

// ToolCallBlock 返回工具调用内容块
func ToolCallBlock(id, name string, args map[string]any) ContentBlock {
	if args == nil {
		args = map[string]any{}
	}
	return ContentBlock{Type: ContentTypeToolCall, ToolCallID: id, ToolName: name, Arguments: args}
}

// MarshalJSON 编码内容块，只序列化类型相关字段
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	var m map[string]any
	switch b.Type {
	case ContentTypeText:
		m = map[string]any{"type": ContentTypeText, "text": b.Text}
		if b.TextSignature != "" {
			m["textSignature"] = b.TextSignature
		}
	case ContentTypeThinking:
		m = map[string]any{"type": ContentTypeThinking, "thinking": b.Thinking}
		if b.ThinkingSignature != "" {
			m["thinkingSignature"] = b.ThinkingSignature
		}
		if b.Redacted {
			m["redacted"] = true
		}
	case ContentTypeImage:
		m = map[string]any{
			"type":     ContentTypeImage,
			"data":     base64.StdEncoding.EncodeToString(b.Data),
			"mimeType": b.MimeType,
		}
	case ContentTypeToolCall:
		args := b.Arguments
		if args == nil {
			args = map[string]any{}
		}
		m = map[string]any{
			"type":      ContentTypeToolCall,
			"id":        b.ToolCallID,
			"name":      b.ToolName,
			"arguments": args,
		}
		if b.ThoughtSignature != "" {
			m["thoughtSignature"] = b.ThoughtSignature
		}
	default:
		return nil, fmt.Errorf("unknown content block type: %q", b.Type)
	}
	return json.Marshal(m)
}

// UnmarshalJSON 解码内容块的 JSON 表示
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type              string         `json:"type"`
		Text              string         `json:"text"`
		TextSignature     string         `json:"textSignature"`
		Thinking          string         `json:"thinking"`
		ThinkingSignature string         `json:"thinkingSignature"`
		Redacted          bool           `json:"redacted"`
		Data              string         `json:"data"`
		MimeType          string         `json:"mimeType"`
		ToolCallID        string         `json:"id"`
		ToolName          string         `json:"name"`
		Arguments         map[string]any `json:"arguments"`
		ThoughtSignature  string         `json:"thoughtSignature"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	b.Type = raw.Type
	b.Text = raw.Text
	b.TextSignature = raw.TextSignature
	b.Thinking = raw.Thinking
	b.ThinkingSignature = raw.ThinkingSignature
	b.Redacted = raw.Redacted
	b.MimeType = raw.MimeType
	b.ToolCallID = raw.ToolCallID
	b.ToolName = raw.ToolName
	b.Arguments = raw.Arguments
	b.ThoughtSignature = raw.ThoughtSignature
	if raw.Data != "" {
		decoded, err := base64.StdEncoding.DecodeString(raw.Data)
		if err != nil {
			return fmt.Errorf("invalid base64 image data: %w", err)
		}
		b.Data = decoded
	}
	return nil
}

// Usage 追踪 token 使用量和费用
type Usage struct {
	Input        int `json:"input"`
	Output       int `json:"output"`
	CacheRead    int `json:"cacheRead"`
	CacheWrite   int `json:"cacheWrite"`
	CacheWrite1h int `json:"cacheWrite1h,omitempty"`
	Reasoning    int `json:"reasoning,omitempty"`
	TotalTokens  int `json:"totalTokens"`

	Cost UsageCost `json:"cost"`
}

// UsageCost 计算得出的费用
type UsageCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// CalculateCost 根据每百万 token 价格计算费用
func CalculateCost(u Usage, cost ModelCost) UsageCost {
	fresh := u.Input - u.CacheRead - u.CacheWrite
	if fresh < 0 {
		fresh = 0
	}
	c := UsageCost{
		Input:      float64(fresh) / 1e6 * cost.Input,
		Output:     float64(u.Output) / 1e6 * cost.Output,
		CacheRead:  float64(u.CacheRead) / 1e6 * cost.CacheRead,
		CacheWrite: float64(u.CacheWrite) / 1e6 * cost.CacheWrite,
	}
	c.Total = c.Input + c.Output + c.CacheRead + c.CacheWrite
	return c
}

// Message 统一消息模型
type Message struct {
	mu      sync.RWMutex
	Role    string         `json:"role"`
	Content []ContentBlock `json:"-"` // marshaled by MarshalJSON (string or array)

	// Assistant message metadata.
	API           string `json:"api,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	ResponseModel string `json:"responseModel,omitempty"`
	ResponseID    string `json:"responseId,omitempty"`
	Usage         *Usage `json:"usage,omitempty"`
	StopReason    string `json:"stopReason,omitempty"`
	ErrorMessage  string `json:"errorMessage,omitempty"`

	// Tool result metadata.
	ToolCallID     string   `json:"toolCallId,omitempty"`
	ToolName       string   `json:"toolName,omitempty"`
	Details        any      `json:"details,omitempty"`
	IsError        bool     `json:"isError,omitempty"`
	AddedToolNames []string `json:"addedToolNames,omitempty"`

	Timestamp int64 `json:"timestamp,omitempty"` // 毫秒时间戳
}

// NewUserTextMessage 返回包含单个文本块的用户消息
func NewUserTextMessage(text string) *Message {
	return &Message{
		Role:      RoleUser,
		Content:   []ContentBlock{TextBlock(text)},
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewAssistantMessage 返回空的助手消息用于流式累积
func NewAssistantMessage(api, provider, model string) *Message {
	return &Message{
		Role:      RoleAssistant,
		API:       api,
		Provider:  provider,
		Model:     model,
		Timestamp: time.Now().UnixMilli(),
	}
}

// Text 返回消息中所有文本块的拼接内容
func (m *Message) Text() string {
	var out string
	for _, b := range m.Content {
		if b.Type == ContentTypeText {
			out += b.Text
		}
	}
	return out
}

// Clone 返回消息的深拷贝，安全传递给生命周期更长的消费者
func (m *Message) Clone() *Message {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := &Message{
		Role:          m.Role,
		API:           m.API,
		Provider:      m.Provider,
		Model:         m.Model,
		ResponseModel: m.ResponseModel,
		ResponseID:    m.ResponseID,
		StopReason:    m.StopReason,
		ErrorMessage:  m.ErrorMessage,
		ToolCallID:    m.ToolCallID,
		ToolName:      m.ToolName,
		Details:       m.Details,
		IsError:       m.IsError,
		Timestamp:     m.Timestamp,
	}
	if m.Usage != nil {
		u := *m.Usage
		c.Usage = &u
	}
	c.AddedToolNames = append([]string(nil), m.AddedToolNames...)
	c.Content = make([]ContentBlock, len(m.Content))
	copy(c.Content, m.Content)
	return c
}

// MarshalJSON 编码消息，用户单文本消息编码为字符串，其他编码为数组
func (m *Message) MarshalJSON() ([]byte, error) {
	type raw Message
	aux := struct {
		Content any `json:"content"`
		*raw
	}{raw: (*raw)(m)}

	if m == nil {
		return []byte("null"), nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Role == RoleUser && len(m.Content) == 1 &&
		m.Content[0].Type == ContentTypeText && m.Content[0].TextSignature == "" {
		aux.Content = m.Content[0].Text
	} else {
		blocks := m.Content
		if blocks == nil {
			blocks = []ContentBlock{}
		}
		aux.Content = blocks
	}
	if m.Role == RoleToolResult && m.Details == nil {
		// Details are always persisted as an object for tool results.
		aux.Details = map[string]any{}
	}
	return json.Marshal(aux)
}

// UnmarshalJSON 解码消息，支持字符串或内容块数组
func (m *Message) UnmarshalJSON(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	type raw Message
	aux := struct {
		Content json.RawMessage `json:"content"`
		*raw
	}{raw: (*raw)(m)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	trimmed := bytesTrimSpace(aux.Content)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		m.Content = nil
		return nil
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		m.Content = []ContentBlock{TextBlock(text)}
	case '[':
		var blocks []ContentBlock
		if err := json.Unmarshal(trimmed, &blocks); err != nil {
			return err
		}
		m.Content = blocks
	default:
		return fmt.Errorf("invalid content: expected string or array of blocks, got %s", trimmed[:min(len(trimmed), 16)])
	}
	return nil
}

func bytesTrimSpace(b []byte) []byte {
	start := 0
	for start < len(b) {
		c := b[start]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			start++
			continue
		}
		break
	}
	end := len(b)
	for end > start {
		c := b[end-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			end--
			continue
		}
		break
	}
	return b[start:end]
}

// Tool 工具定义，Parameters 为 JSON Schema
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ModelCost 每百万 token 价格配置
type ModelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`

	Currency   string      `json:"currency,omitempty"`   // 货币符号，如 "$"、"¥"
	Multiplier float64     `json:"multiplier,omitempty"` // 价格到显示值的倍率
	PeakValley *PeakValley `json:"peakValley,omitempty"` // 峰谷电价，nil 表示关闭
}

// PeakValley 峰谷电价时段列表
type PeakValley struct {
	Periods []PeakValleyPeriod `json:"periods,omitempty"`
}

// PeakValleyPeriod 单个峰谷电价时段，Start/End 为本地 "HH:MM" 格式
type PeakValleyPeriod struct {
	Start      string  `json:"start"`      // 开始时间
	End        string  `json:"end"`        // 结束时间，早于 Start 则跨午夜
	Multiplier float64 `json:"multiplier"` // 价格倍率，1 表示不变
}

// Model describes an LLM model and how to reach it.
type Model struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	API      string `json:"api"`
	Provider string `json:"provider"`
	BaseURL  string `json:"baseUrl"`

	Reasoning        bool           `json:"reasoning"`
	ThinkingLevelMap map[string]any `json:"thinkingLevelMap,omitempty"`

	Input []string `json:"input"` // 支持的输入类型：text/image

	Cost          ModelCost `json:"cost"`
	ContextWindow int       `json:"contextWindow"`
	MaxTokens     int       `json:"maxTokens"`

	Headers map[string]string `json:"headers,omitempty"` // 额外请求头，null 抑制默认头

	SessionAffinity string `json:"sessionAffinity,omitempty"` // 缓存亲和性头格式
	Impersonate     string `json:"impersonate,omitempty"`     // 模拟客户端标识
}

// SupportsImages 报告模型是否支持图片输入
func (m *Model) SupportsImages() bool {
	if m == nil {
		return false
	}
	for _, t := range m.Input {
		if t == "image" {
			return true
		}
	}
	return false
}

// Context 传递给流函数的请求上下文
type Context struct {
	SystemPrompt string
	Messages     []*Message
	Tools        []*Tool
}

// Token estimation heuristics:
// roughly one token per 4 characters, with images counting as 4800 chars.

const (
	charsPerToken   = 4
	imageTokenChars = 4800
)

// EstimateTextTokens estimates the token count of a text string.
func EstimateTextTokens(text string) int {
	return (utf8.RuneCountInString(text) + charsPerToken - 1) / charsPerToken
}

// EstimateMessageTokens estimates the token count of a single message.
func EstimateMessageTokens(m *Message) int {
	if m == nil {
		return 0
	}
	var chars int
	for _, b := range m.Content {
		switch b.Type {
		case ContentTypeImage:
			chars += imageTokenChars
		case ContentTypeThinking:
			chars += utf8.RuneCountInString(b.Thinking)
		case ContentTypeToolCall:
			chars += utf8.RuneCountInString(b.ToolName)
			if args, err := json.Marshal(b.Arguments); err == nil {
				chars += len(args)
			}
		default:
			chars += utf8.RuneCountInString(b.Text)
		}
	}
	if m.Role == RoleToolResult && m.Details != nil {
		if details, err := json.Marshal(m.Details); err == nil {
			chars += len(details)
		}
	}
	return (chars + charsPerToken - 1) / charsPerToken
}

// EstimateTokens estimates the token count of a list of messages.
func EstimateTokens(messages []*Message) int {
	total := 0
	for _, m := range messages {
		total += EstimateMessageTokens(m)
	}
	return total
}
