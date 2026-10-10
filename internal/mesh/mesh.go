// Package mesh 是 doge 的 P2P 层，按构建标签拆分：接口、协议类型、token 与配置
// 放这里（无 tag，默认构建不引入 libp2p 依赖）；libp2p 实现在
// internal/mesh/libp2p（+build mesh）；禁用 stub 在 stub.go（+build !mesh）。
package mesh

import (
	"context"
	"time"

	"doge/internal/coding/uploads"
)

// Capability 节点广播的粗粒度能力标签（如 "agent"、"forward:8080"），
// 细粒度能力描述由每次 advertise 广播的 AgentProfile 携带
type Capability string

// EnvelopeHandler 处理收到的 Envelope，错误记日志、不中断其它订阅者
type EnvelopeHandler func(env Envelope) error

// PeerInfo 已连接且认证通过的 peer 快照
type PeerInfo struct {
	ID      string        `json:"id"`
	Addrs   []string      `json:"addrs,omitempty"`
	Caps    []Capability  `json:"caps,omitempty"`
	Profile AgentProfile  `json:"profile,omitempty"`
	Latency time.Duration `json:"latency,omitempty"`
}

// TaskStatus 一个异步远端任务的状态，暴露给 UI（web /api/mesh、TUI /mesh）
// 与 remote_agent_status 工具。放在无 tag 包内，web/TUI 层不引 libp2p 也能用
type TaskStatus struct {
	TaskID     string    `json:"taskId"`
	Peer       string    `json:"peer,omitempty"`
	Status     string    `json:"status"` // running | recovered | completed | error | aborted
	Progress   string    `json:"progress,omitempty"`
	Dispatched time.Time `json:"dispatched,omitempty"`
}

// Mesh 暴露给 doge 其它部分的网络接口；默认（非 -tags mesh）构建提供禁用 stub，
// libp2p 实现在 internal/mesh/libp2p
type Mesh interface {
	Start(ctx context.Context) error
	Close() error

	ID() string
	Peers() []PeerInfo
	// Self 返回本节点自己的 AgentProfile（身份 + 能力），供调用方和 mesh_peers 工具报告本节点
	Self() AgentProfile
	Advertise(cap Capability) // 更新本地能力并重广播 profile

	// Send 向指定 peer 发送一个 Envelope（点对点）。只传小消息：
	// 大 payload 必须 gzip 压缩或走独立 stream
	Send(peerID string, env Envelope) error
	// Broadcast 向所有已连接且认证的 peer 扇出小 Envelope（advertise 等），
	// 禁止大 payload
	Broadcast(env Envelope) error
	Subscribe(h EnvelopeHandler) (unsubscribe func())

	// Forward 管理声明式端口转发规则（P2）
	ForwardReload() error
	ForwardAdd(rule ForwardRule) error
	ForwardRemove(name string) error
	ForwardSetEnabled(name string, enabled bool) error
	ForwardList() []ForwardRule
}

// Options 配置 mesh 构造；ConfigPath 为空时用 agent 目录下的 mesh.json
type Options struct {
	// ConfigPath mesh.json 路径，为空时用包默认（~/.doge/agent/mesh.json）
	ConfigPath string
	// Config 预加载的配置，为 nil 时从 ConfigPath 加载
	Config *Config

	// Version、Skills、Tools、Model 写入广播的 AgentProfile，
	// 让远端 peer 看到本节点能力
	Version string
	Skills  []string
	Tools   []string
	Model   string

	// Probe 关闭 forwards 启动与热载轮询，用于只发现/查看网络的临时命令
	// （`doge mesh peers`/`doge mesh profile`）
	Probe bool

	// Uploads 上传暂存区（/doge/file/0.1 收发的落点）。nil = 本节点不参与
	// mesh 文件传输（mesh_file 工具返回错误）
	Uploads *uploads.Store
}
