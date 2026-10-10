// Package mcp 实现 MCP（Model Context Protocol）服务器接入：社区标准 mcpServers
// 配置格式、懒连接管理、mcp 代理工具与 directTools 提升。默认构建内置，无
// mcp 配置时不初始化、不建缓存、不连任何服务器（懒加载哲学，见 docs/mcp-design.md）。
package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultIdleTimeoutSec 空闲回收时长（无调用则断开服务器进程/会话）
	DefaultIdleTimeoutSec = 600
	// DefaultRequestTimeoutMs 单次 MCP 请求超时
	DefaultRequestTimeoutMs = 30000
)

// serverNameRe 约束 server 名（同时用作缓存文件名，防路径穿越）
var serverNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Server 一个 MCP server 定义（mcp.json 的 mcpServers 项）。
// 与社区标准格式对齐：stdio 用 type:"stdio"+command+args，HTTP 用
// type:"http"+url；缺省 type 时按 url/command 推断（sse 视同 http）。
type Server struct {
	// Type 传输类型："stdio" | "http"（"sse" 兼容视为 http）。仅是标注，
	// 实际传输仍由 command/url 决定
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Disabled 是否禁用（/mcp enable|disable 只写此字段，不改源文件）。
	// 不带 omitempty：显式写 false 才能覆盖全局配置里的 disabled:true
	Disabled bool `json:"disabled"`
	// AutoStart 发现/reload 后自动启动并连接（默认懒连接，首次调用才 spawn）。
	// 不带 omitempty：API 关闭时需显式写 false 覆盖全局的 autoStart:true
	AutoStart bool `json:"autoStart"`
	// DirectTools 提升为一等工具：true=全部，或工具名数组（server__tool 命名）
	DirectTools any `json:"directTools,omitempty"`
	// IncludeTools / ExcludeTools 代理模式下的工具过滤（glob）
	IncludeTools []string `json:"includeTools,omitempty"`
	ExcludeTools []string `json:"excludeTools,omitempty"`
	// ApproveTools 审批白名单（glob）；配置后未命中的工具调用直接返回「需批准」，
	// 不再执行（headless fail-closed）
	ApproveTools []string `json:"approveTools,omitempty"`
	// SearchKeywords 搜索加权词（name -> 关键词）
	SearchKeywords map[string][]string `json:"searchKeywords,omitempty"`
}

// Settings 与 mcpServers 同文件共存的顶层设置
type Settings struct {
	// ToolPrefix direct 工具名前缀，默认 "server__tool"
	ToolPrefix string `json:"toolPrefix,omitempty"`
	// IdleTimeoutSec 空闲回收秒数（0 = 600）
	IdleTimeoutSec int `json:"idleTimeout,omitempty"`
	// RequestTimeoutMs 单次请求超时毫秒（0 = 30000）
	RequestTimeoutMs int `json:"requestTimeoutMs,omitempty"`
	// DirectTools 全局提升设置（server 级优先）
	DirectTools any `json:"directTools,omitempty"`
	// ApproveTools 全局审批白名单（server 级优先）
	ApproveTools []string `json:"approveTools,omitempty"`
	// Trace 调试：记录每次 MCP 请求/响应摘要到诊断日志
	Trace bool `json:"trace,omitempty"`
}

// Config 一份 mcp.json / .mcp.json 的内容
type Config struct {
	McpServers map[string]*Server `json:"mcpServers"`
	Settings   Settings           `json:"settings"`
}

// McpFilePaths 返回 MCP 配置发现路径（高优先级在后）：
// 全局 agentDir/mcp.json → 项目 .doge/mcp.json → 项目共享 .mcp.json。
// override 非空时替换全局路径（--mcp-config）。
func McpFilePaths(agentDir, cwd, override string) []string {
	paths := make([]string, 0, 3)
	if override != "" {
		paths = append(paths, override)
	} else {
		paths = append(paths, filepath.Join(agentDir, "mcp.json"))
	}
	if cwd != "" {
		paths = append(paths,
			filepath.Join(cwd, ".doge", "mcp.json"),
			filepath.Join(cwd, ".mcp.json"),
		)
	}
	return paths
}

// LoadConfigs 读取并合并全部 MCP 配置（低→高优先级依次覆盖）：
// server 同名时字段级合并；disabled / autoStart / directTools 三个「开关类」
// 字段不看零值、只看键是否存在——高优先级文件里写了键就以它为准（API 写入的
// 显式 false 必须能覆盖全局配置里的 true）。
func LoadConfigs(paths []string) (*Config, error) {
	cfg := &Config{McpServers: map[string]*Server{}}
	type entry struct {
		cfg      *Config
		presence map[string]map[string]bool
	}
	entries := make([]entry, 0, len(paths))
	for _, p := range paths {
		part, present, err := readConfigFile(p)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{part, present})
		mergeConfig(cfg, part)
	}
	// 开关类字段的最终值：按优先级从低到高，键存在的文件赢
	for _, e := range entries {
		for name, keys := range e.presence {
			s := cfg.McpServers[name]
			if s == nil {
				continue
			}
			src := e.cfg.McpServers[name]
			if src == nil {
				continue
			}
			if _, ok := keys["disabled"]; ok {
				s.Disabled = src.Disabled
			}
			if _, ok := keys["autoStart"]; ok {
				s.AutoStart = src.AutoStart
			}
			if _, ok := keys["directTools"]; ok {
				s.DirectTools = src.DirectTools
			}
		}
	}
	return cfg, nil
}

// readConfigFile 读取单个配置文件；不存在返回空配置（不报错）。
// 第二个返回值记录每个 server 条目显式写出的键（开关类字段合并用）。
func readConfigFile(path string) (*Config, map[string]map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{McpServers: map[string]*Server{}}, nil, nil
		}
		return nil, nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, nil, err
	}
	if c.McpServers == nil {
		c.McpServers = map[string]*Server{}
	}
	var raw struct {
		McpServers map[string]map[string]bool `json:"mcpServers"`
	}
	_ = json.Unmarshal(data, &raw)
	return &c, raw.McpServers, nil
}

// mergeConfig 把 higher 覆盖进 base（higher 优先级更高）：server 字段级合并
// （/mcp enable|disable 只写 disabled 到项目文件，不能整项替换丢命令），
// settings 非零字段覆盖。
func mergeConfig(base, higher *Config) {
	for name, s := range higher.McpServers {
		if cur, ok := base.McpServers[name]; ok {
			mergeServer(cur, s)
		} else {
			base.McpServers[name] = s
		}
	}
	h := higher.Settings
	if h.ToolPrefix != "" {
		base.Settings.ToolPrefix = h.ToolPrefix
	}
	if h.IdleTimeoutSec > 0 {
		base.Settings.IdleTimeoutSec = h.IdleTimeoutSec
	}
	if h.RequestTimeoutMs > 0 {
		base.Settings.RequestTimeoutMs = h.RequestTimeoutMs
	}
	if h.DirectTools != nil {
		base.Settings.DirectTools = h.DirectTools
	}
	if len(h.ApproveTools) > 0 {
		base.Settings.ApproveTools = h.ApproveTools
	}
	if h.Trace {
		base.Settings.Trace = h.Trace
	}
}

// mergeServer 字段级合并：higher 的非零字段覆盖 base。
// disabled / autoStart / directTools 不在这里合并——它们是「键存在才生效」的
// 开关类字段，由 LoadConfigs 的 presence 遍历处理（显式 false 必须能覆盖）。
func mergeServer(base, higher *Server) {
	if higher.Type != "" {
		base.Type = higher.Type
	}
	if higher.Command != "" {
		base.Command = higher.Command
	}
	if len(higher.Args) > 0 {
		base.Args = higher.Args
	}
	if higher.URL != "" {
		base.URL = higher.URL
	}
	if len(higher.Env) > 0 {
		base.Env = higher.Env
	}
	if len(higher.Headers) > 0 {
		base.Headers = higher.Headers
	}
	if len(higher.IncludeTools) > 0 {
		base.IncludeTools = higher.IncludeTools
	}
	if len(higher.ExcludeTools) > 0 {
		base.ExcludeTools = higher.ExcludeTools
	}
	if len(higher.ApproveTools) > 0 {
		base.ApproveTools = higher.ApproveTools
	}
	if len(higher.SearchKeywords) > 0 {
		base.SearchKeywords = higher.SearchKeywords
	}
}

// IdleTimeout 生效的空闲超时
func (c *Config) IdleTimeout() time.Duration {
	secs := c.Settings.IdleTimeoutSec
	if secs <= 0 {
		secs = DefaultIdleTimeoutSec
	}
	return time.Duration(secs) * time.Second
}

// RequestTimeout 生效的请求超时
func (c *Config) RequestTimeout() time.Duration {
	ms := c.Settings.RequestTimeoutMs
	if ms <= 0 {
		ms = DefaultRequestTimeoutMs
	}
	return time.Duration(ms) * time.Millisecond
}

// DirectToolPrefix 生效的 direct 工具名前缀（默认双下划线）
func (c *Config) DirectToolPrefix() string {
	if strings.TrimSpace(c.Settings.ToolPrefix) == "" {
		return "__"
	}
	return c.Settings.ToolPrefix
}

// ToolName 一个 MCP 工具提升后的一等工具全名
func (c *Config) ToolName(server, tool string) string {
	return server + c.DirectToolPrefix() + tool
}
