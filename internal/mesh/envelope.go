package mesh

import (
	"errors"
	"fmt"
	"strings"
)

// MsgType Envelope 的 payload 类型
type MsgType string

const (
	MsgTask        MsgType = "task"         // 主→从：执行任务
	MsgResult      MsgType = "result"       // 从→主：任务结果
	MsgQuestion    MsgType = "question"     // 从→主：需要决策
	MsgAnswer      MsgType = "answer"       // 主→从：回答
	MsgProgress    MsgType = "progress"     // 从→主：进展更新
	MsgSubscribe   MsgType = "subscribe"    // 主→从：订阅会话事件流
	MsgEvent       MsgType = "event"        // 从→主：会话事件
	MsgSessionSnap MsgType = "session-snap" // 会话快照（请求或推送）
	MsgAdvertise   MsgType = "advertise"    // 广播 AgentProfile
	MsgInterrupt   MsgType = "interrupt"    // 主→从：中断任务
	MsgConfigPush  MsgType = "config-push"  // 主→从：下发配置
	MsgConfigAck   MsgType = "config-ack"   // 从→主：配置应用结果
	MsgCwdList     MsgType = "cwd-list"     // 请求/响应：列远端可达工作目录
	MsgToolCall    MsgType = "tool-call"    // 主→从：直接调用从侧节点级工具（不经远端 agent）
	MsgToolResult  MsgType = "tool-result"  // 从→主：工具直接调用结果
	MsgError       MsgType = "error"        // 通用拒绝/错误回执
)

type (
	// Envelope 线消息；To 为空表示广播，点对点消息带目标 PeerID
	Envelope struct {
		ID      string  `json:"id"`
		Type    MsgType `json:"type"`
		From    string  `json:"from"`
		To      string  `json:"to,omitempty"`
		ReplyTo string  `json:"replyTo,omitempty"`
		Payload []byte  `json:"payload,omitempty"`
	}

	// AgentProfile 各节点广播的能力声明
	AgentProfile struct {
		PeerID    string            `json:"peerId"`
		Name      string            `json:"name,omitempty"`
		Roles     []string          `json:"roles,omitempty"`
		Version   string            `json:"version,omitempty"`
		OS        string            `json:"os,omitempty"`
		Arch      string            `json:"arch,omitempty"`
		Model     string            `json:"model,omitempty"`
		Models    []string          `json:"models,omitempty"`
		Tools     []string          `json:"tools,omitempty"`
		Skills    []string          `json:"skills,omitempty"`
		MCP       []MCPEndpoint     `json:"mcp,omitempty"`
		Cwds      []string          `json:"cwds,omitempty"`
		Accepting bool              `json:"accepting"`         // 是否接受远端任务（P3）
		Load      int               `json:"load,omitempty"`    // 当前并发远端任务数
		MaxLoad   int               `json:"maxLoad,omitempty"` // 上限
		Tags      map[string]string `json:"tags,omitempty"`
	}

	// MCPEndpoint 已配置的 MCP server
	MCPEndpoint struct {
		Name  string `json:"name"`
		Tools int    `json:"tools"`
	}

	// ForwardRule 端口转发规则
	ForwardRule struct {
		Name    string `json:"name"`
		Mode    string `json:"mode"`             // expose/listen
		Local   string `json:"local,omitempty"`  // expose 要 dial 的地址
		Bind    string `json:"bind,omitempty"`   // listen 监听的接口
		Port    int    `json:"port,omitempty"`   // listen TCP 端口
		Peer    string `json:"peer,omitempty"`   // listen 目标 peer ID
		Remote  string `json:"remote,omitempty"` // listen Peer 暴露的服务名
		Enabled bool   `json:"enabled"`          // 热启停
	}
)

// ValidateRule 在规则加入或启动前校验
func ValidateRule(r ForwardRule) error {
	if r.Name == "" {
		return errors.New("forward rule: name is required")
	}
	switch r.Mode {
	case "expose":
		if r.Local == "" {
			return errors.New("forward rule " + r.Name + ": local is required for expose")
		}
	case "listen":
		if r.Peer == "" {
			return errors.New("forward rule " + r.Name + ": peer is required for listen")
		}
		if r.Remote == "" {
			return errors.New("forward rule " + r.Name + ": remote is required for listen")
		}
		if !strings.HasPrefix(r.Bind, "unix://") && (r.Port < 1 || r.Port > 65535) {
			return fmt.Errorf("forward rule %s: invalid port %d", r.Name, r.Port)
		}
	default:
		return fmt.Errorf("forward rule %s: unknown mode %q (want expose or listen)", r.Name, r.Mode)
	}
	return nil
}

type (
	// ErrorPayload MsgError 的 payload：即时拒绝/错误回执
	ErrorPayload struct {
		Code    string `json:"code"` // "denied" | "busy" | "bad-cwd" | "not-found" | ...
		Message string `json:"message"`
	}

	// TaskPayload MsgTask 的 payload（P3+）
	TaskPayload struct {
		Task     string   `json:"task"`
		Context  string   `json:"context"` // "fresh" | "summary" | "fork"
		Model    string   `json:"model,omitempty"`
		Cwd      string   `json:"cwd,omitempty"`
		Roles    []string `json:"roles,omitempty"`
		Timeout  int      `json:"timeout,omitempty"`
		Session  string   `json:"session,omitempty"`  // 接续从侧会话
		Snapshot []byte   `json:"snapshot,omitempty"` // gzip 压缩的会话上下文
		// Files 随任务下发的文件（已先经 /doge/file/0.1 送到本节点的暂存区，
		// 这里的 UploadID 是从侧 store 里的 ID）
		Files []RemoteFileRef `json:"files,omitempty"`
	}

	// RemoteFileRef 暂存区文件的跨节点引用（sha256 供接收侧完整性核对）
	RemoteFileRef struct {
		UploadID string `json:"uploadId"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		SHA256   string `json:"sha256"`
	}

	// ResultPayload MsgResult 的 payload：远端任务结果
	ResultPayload struct {
		TaskID    string `json:"taskId,omitempty"`
		Session   string `json:"session,omitempty"`   // 任务所在的远端会话 id
		Status    string `json:"status"`              // completed/error/aborted
		Output    string `json:"output,omitempty"`    // 最终 assistant 文本
		Error     string `json:"error,omitempty"`     // 失败原因
		Truncated bool   `json:"truncated,omitempty"` // Output 被截断
	}

	// QuestionPayload MsgQuestion 的 payload：远端任务向主侧问决策
	QuestionPayload struct {
		TaskID  string `json:"taskId,omitempty"`
		Context string `json:"context,omitempty"`
		Prompt  string `json:"prompt"`
	}

	// AnswerPayload MsgAnswer 的 payload：主侧对 question 的回答
	AnswerPayload struct {
		Answer string `json:"answer"`
	}

	// ProgressPayload MsgProgress 的 payload：远端任务状态更新
	ProgressPayload struct {
		TaskID  string `json:"taskId,omitempty"`
		Status  string `json:"status,omitempty"`
		Message string `json:"message,omitempty"`
	}

	// ToolCallPayload MsgToolCall 的 payload：主侧点名直接执行从侧节点级工具。
	// 只允许节点级工具（mesh_forward/mesh_peers/remote_agent_status），
	// 会话工具（read/bash 等）不走此通道，避免绕过远端任务权限分级
	ToolCallPayload struct {
		Tool string         `json:"tool"`
		Args map[string]any `json:"args,omitempty"`
	}

	// ToolResultPayload MsgToolResult 的 payload：工具直接调用的执行结果
	ToolResultPayload struct {
		Status string `json:"status"`           // completed | error
		Output string `json:"output,omitempty"` // 工具结果文本
		Error  string `json:"error,omitempty"`  // 失败原因
	}
)
