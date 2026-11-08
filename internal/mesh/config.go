package mesh

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config mesh 节点配置，持久化在 ~/.doge/agent/mesh.json。
// forwards 可独立热重载（ForwardReload）；其余字段启动时读取或经 `doge mesh reload`
type Config struct {
	Token            string   `json:"token"`
	Name             string   `json:"name,omitempty"`
	Roles            []string `json:"roles,omitempty"`
	AllowRemoteTasks bool     `json:"allowRemoteTasks"`
	RemoteTaskLevel  string   `json:"remoteTaskLevel,omitempty"`
	// AllowRemoteTools 允许 peer 用 remote_tool 直接调用本节点级工具
	// （mesh_peers/remote_agent_status 与 mesh_forward 只读操作，不经远端
	// agent）。默认关（fail-closed）：token 只是准入边界，直接调用能触碰
	// 节点配置，须显式开启。序列化不带 omitempty：mesh new 显式写出该字段，
	// 开关可见
	AllowRemoteTools bool `json:"allowRemoteTools"`
	// AllowRemoteForwards 允许 peer 经 remote_tool 直接调用 mesh_forward 的
	// 写操作（add/remove/enable/disable/reload——可暴露本机服务、绑定监听、
	// 改动转发规则）。默认关；AllowRemoteTools 只放开只读操作
	AllowRemoteForwards bool     `json:"allowRemoteForwards"`
	TrustedPeers        []string `json:"trustedPeers,omitempty"`
	MaxConcurrentTasks int      `json:"maxConcurrentTasks,omitempty"`
	BootstrapPeers     []string `json:"bootstrapPeers,omitempty"`
	PrivateNetwork     bool     `json:"privateNetwork"`
	// ListenPort 本节点固定 TCP 监听端口（0=每次启动随机）。种子节点设置它
	// （配合持久化身份），保证跨重启地址稳定、token 内嵌 bootstrap 地址长期有效
	ListenPort int `json:"listenPort,omitempty"`
	// Relay 作为 circuit v2 hop 中继服务，供 NAT 后的节点经本节点中继互通；
	// 通常开在有公网 IP / 固定 VPN IP 的种子节点上。准入走中继 ACL + PSK 握手
	Relay bool `json:"relay"`
	// AllowRemoteFiles 允许 trusted peer 经 /doge/file/0.1 收发文件（进本节点
	// 上传暂存区，不直达文件系统）。默认关，与 allowRemoteTasks 同构
	AllowRemoteFiles bool `json:"allowRemoteFiles"`
	// FileMaxBytes 单文件传输上限（0 = 256MiB 默认值，在 libp2p 侧兜底）
	FileMaxBytes int64             `json:"fileMaxBytes,omitempty"`
	Cwds         map[string]string `json:"cwds,omitempty"`
	Forwards     []ForwardRule     `json:"forwards,omitempty"`
}

// DefaultConfigPath 返回 doge agent 目录下的 mesh.json 路径
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "mesh.json"
	}
	return filepath.Join(home, ".doge", "agent", "mesh.json")
}

// LoadConfig 读取指定路径的配置；文件不存在时返回默认配置（不报错），
// 让首次 `doge mesh new` 从默认值起步。allowRemoteTools/allowRemoteForwards
// 缺省视为 false（fail-closed）：直接调用能触碰节点配置，须显式开启
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SaveConfig 写配置（0600，与 agent 目录其它凭据文件一致）；临时文件 +
// rename 原子落盘，避免并发写或崩溃留下半截 JSON
func SaveConfig(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// TokenObj 解码嵌入的 token
func (c *Config) TokenObj() (*Token, error) {
	if c.Token == "" {
		return nil, ErrNoToken
	}
	return DecodeToken(c.Token)
}

// SetToken 把 token 嵌入配置
func (c *Config) SetToken(t *Token) {
	c.Token = t.Encode()
}
