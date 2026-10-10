package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"doge/internal/agent"
	"doge/pkg/logx"
)

// Options 构造 Manager 的配置
type Options struct {
	AgentDir string
	Cwd      string
	// ConfigPath 覆盖全局配置文件位置（--mcp-config）
	ConfigPath string
	// NoMcp 完全禁用 MCP（--no-mcp）
	NoMcp bool
	// Trace 请求级调试日志（--mcp-trace；默认 info 级别可见）
	Trace bool
	// Version 客户端标识版本
	Version string
}

// ServerInfo 一个 server 的状态快照（TUI /mcp、web /api/mcp 共用）
type ServerInfo struct {
	Name      string `json:"name"`
	Transport string `json:"transport"` // stdio | http
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	Cache     bool   `json:"cache"` // 工具来自磁盘缓存（未连接）
	// Direct 全部工具提升为一等工具（directTools）；AutoStart 随发现/reload 自启
	Direct    bool       `json:"direct"`
	AutoStart bool       `json:"autoStart"`
	Tools     []ToolInfo `json:"tools"`
	Error     string     `json:"error,omitempty"`
}

// ToolInfo 一个 MCP 工具的摘要
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Manager 进程级 MCP 管理：server 注册表、懒连接、空闲回收、磁盘缓存。
// 懒加载：New 不读配置；首次 Discover/BuildTools 才读。无配置或全禁用时
// Enabled() 为 false，不建缓存目录、不 spawn 任何进程。
type Manager struct {
	opts    Options
	version string

	mu      sync.Mutex
	cfg     *Config
	servers map[string]*server
	ready   bool // Discover 已完成
	trace   bool // 请求级调试日志
	// writePath 是 API/面板写入配置的主文件（默认 <cwd>/.doge/mcp.json，
	// 无 cwd 回退 <agentDir>/mcp.json；--mcp-config 可覆盖）。所有
	// enable/disable/direct/autostart/upsert 均落此处：开关类字段靠
	// LoadConfigs 的「键存在即生效」叠加到全局/共享定义上，删除也只影响
	// 本项目——与面板文案一致。
	writePath  string
	cache      cache
	stopReaper chan struct{}

	// toolsBuilt 缓存 BuildTools 结果（工具实例无状态，可跨会话共享）
	toolsBuilt bool
	tools      []*agent.AgentTool
}

// New 构造 manager（不读配置、不连任何 server）
func New(opts Options) *Manager {
	writePath := opts.ConfigPath
	if writePath == "" {
		if opts.Cwd != "" {
			writePath = filepath.Join(opts.Cwd, ".doge", "mcp.json")
		} else {
			writePath = filepath.Join(opts.AgentDir, "mcp.json")
		}
	}
	return &Manager{
		opts:      opts,
		version:   opts.Version,
		trace:     opts.Trace,
		servers:   map[string]*server{},
		writePath: writePath,
		cache:     cache{dir: opts.AgentDir},
	}
}

// Discover 读取并合并配置、加载磁盘缓存、启动空闲回收。可重入（/mcp setup
// 后重新发现）：旧连接全部断开。失败（配置损坏）返回错误但不崩溃：/mcp 面板
// 能看到 lastErr。
func (m *Manager) Discover() error {
	if m.opts.NoMcp {
		return nil
	}
	paths := McpFilePaths(m.opts.AgentDir, m.opts.Cwd, m.opts.ConfigPath)
	cfg, err := LoadConfigs(paths)
	if err != nil {
		logx.Warnf("[mcp] config load: %v", err)
		return err
	}
	m.mu.Lock()
	// 重入：断开旧连接、停旧回收、清空工具缓存
	for _, s := range m.servers {
		s.disconnect()
	}
	if m.stopReaper != nil {
		close(m.stopReaper)
		m.stopReaper = nil
	}
	m.cfg = cfg
	m.ready = true
	m.servers = map[string]*server{}
	var autoStarts []*server
	for name, s := range cfg.McpServers {
		svr := &server{name: name, cfg: s, mgr: m}
		// 禁用的 server 也进注册表（面板可见、可重新启用），但不预载缓存不连接
		if !s.Disabled {
			svr.tools = m.cache.load(name)
			if s.AutoStart {
				autoStarts = append(autoStarts, svr)
			}
		}
		m.servers[name] = svr
	}
	m.toolsBuilt = false
	m.tools = nil
	hasServer := len(m.servers) > 0
	// 通道建立放锁内（Discover 顶部已在锁内 close 旧通道并置 nil，这里必然
	// 是重建），与 Close / 并发 Discover 对 m.stopReaper 的读写不再竞争
	if hasServer {
		m.stopReaper = make(chan struct{})
	}
	m.mu.Unlock()

	if hasServer {
		go m.idleReaper()
	}
	// autoStart：发现/reload 后立即启动连接（异步，失败记入日志与面板错误）
	for _, svr := range autoStarts {
		go m.autoConnect(svr)
	}
	logx.Infof("[mcp] discovered %d server(s) from %d config file(s)", len(m.servers), len(paths))
	return nil
}

// autoConnect autoStart 服务器的启动连接（异步路径；失败由 setErr 记日志）
func (m *Manager) autoConnect(svr *server) {
	ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout())
	defer cancel()
	_, _ = svr.listTools(ctx)
}

// Enabled 是否有可用的 MCP server
func (m *Manager) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready && len(m.servers) > 0
}

// Config 当前生效配置（nil 表示未初始化/禁用）
func (m *Manager) Config() *Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// BuildTools 产出工具集：一个 mcp 代理工具 + 各 server 的 direct 提升工具。
// 幂等；结果可跨会话共享（工具实例无状态，权限在会话侧按名拦截）。
func (m *Manager) BuildTools() []*agent.AgentTool {
	m.mu.Lock()
	if m.toolsBuilt {
		m.mu.Unlock()
		return m.tools
	}
	m.mu.Unlock()

	if !m.Enabled() {
		return nil
	}
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

	tools := []*agent.AgentTool{proxyTool(m)}
	tools = append(tools, m.buildDirectTools(cfg)...)

	m.mu.Lock()
	m.toolsBuilt = true
	m.tools = tools
	m.mu.Unlock()
	return tools
}

// Servers 各 server 状态快照
func (m *Manager) Servers() []ServerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.cfg
	out := make([]ServerInfo, 0, len(m.servers))
	for _, s := range m.servers {
		infos := s.toolInfos()
		all, names := m.directToolNames(cfg, s.name, s.cfg)
		out = append(out, ServerInfo{
			Name:      s.name,
			Transport: transportOf(s.cfg),
			Enabled:   !s.cfg.Disabled,
			Connected: s.connected(),
			Cache:     !s.connected() && len(infos) > 0,
			Direct:    all || len(names) > 0,
			AutoStart: s.cfg.AutoStart,
			Tools:     infos,
			Error:     s.lastErr,
		})
	}
	return out
}

// Reconnect 立即断开并重连一个 server（或全部），刷新工具缓存
func (m *Manager) Reconnect(name string) error {
	targets := m.pick(name)
	if len(targets) == 0 {
		return fmt.Errorf("mcp server %q not found", name)
	}
	for _, s := range targets {
		s.disconnect()
		ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout())
		_, err := s.listTools(ctx)
		cancel()
		if err != nil {
			return err
		}
	}
	// 工具清单可能变化，direct 组缓存失效
	m.invalidateTools()
	return nil
}

// SetEnabled 启用/禁用 server：更新内存状态，并写 disabled 字段到项目
// .doge/mcp.json（不改源配置文件）。禁用的 server 留在注册表里（面板可见），
// 只是不连接、不被调用、不参与检索。
func (m *Manager) SetEnabled(name string, enabled bool) error {
	name = strings.TrimSpace(name)
	m.mu.Lock()
	s := m.servers[name]
	var known bool
	if m.cfg != nil && m.cfg.McpServers[name] != nil {
		known = true
	}
	m.mu.Unlock()
	if s == nil {
		if !known {
			return fmt.Errorf("mcp server %q not found", name)
		}
		return m.setDisabledOnFile(name, enabled)
	}
	s.cfg.Disabled = !enabled
	if !enabled {
		s.disconnect()
	}
	// 启停改变可用 server 集合，工具组缓存失效
	m.invalidateTools()
	// 先落盘再连接：服务器当前连不上也不影响启停意图持久化
	if err := m.setDisabledOnFile(name, enabled); err != nil {
		return err
	}
	if enabled {
		ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout())
		if _, err := s.listTools(ctx); err != nil {
			cancel()
			return err
		}
		cancel()
	}
	return nil
}

// SetServerDirect 切换某 server 是否把工具提升为一等工具（directTools=true=
// 全部提升）。同步内存状态并持久化到项目 .doge/mcp.json；关闭时写显式 false，
// 可覆盖全局/共享配置里的 true。工具组重建由调用方 redistribute。
func (m *Manager) SetServerDirect(name string, on bool) error {
	if err := m.setServerFlag(name, func(s *Server) {
		if on {
			s.DirectTools = true
		} else {
			s.DirectTools = false // 显式 false：覆盖高优先级文件里的提升设置
		}
	}); err != nil {
		return err
	}
	// 提升集合变了，已构建的工具组失效（下次 BuildTools/redistribute 重算）
	m.invalidateTools()
	logx.Infof("[mcp] %s direct tools = %v (project config updated)", name, on)
	return nil
}

// SetServerAutoStart 切换是否随发现/reload 自动启动并连接。开启立即连一次；
// 关闭断开现有连接（下次调用按需重连）。
func (m *Manager) SetServerAutoStart(name string, on bool) error {
	m.mu.Lock()
	s := m.servers[name]
	m.mu.Unlock()
	if err := m.setServerFlag(name, func(sv *Server) { sv.AutoStart = on }); err != nil {
		return err
	}
	logx.Infof("[mcp] %s autoStart = %v (project config updated)", name, on)
	if s == nil || s.cfg.Disabled {
		return nil
	}
	if on {
		go m.autoConnect(s)
	} else {
		s.disconnect()
	}
	return nil
}

// invalidateTools 让缓存的工具组失效（server 集合或提升配置变化后）
func (m *Manager) invalidateTools() {
	m.mu.Lock()
	m.toolsBuilt = false
	m.tools = nil
	m.mu.Unlock()
}

// setServerFlag 修改单个 server 的开关类字段：内存同步 + 项目文件持久化
// （只写该文件里已有或新建的条目，不动其它字段）
func (m *Manager) setServerFlag(name string, mutate func(*Server)) error {
	name = strings.TrimSpace(name)
	m.mu.Lock()
	s := m.servers[name]
	var known bool
	if m.cfg != nil && m.cfg.McpServers[name] != nil {
		known = true
	}
	m.mu.Unlock()
	if s == nil {
		// 不在运行注册表（比如已禁用且配置来自其它文件）：只落盘
		if !known {
			return fmt.Errorf("mcp server %q not found", name)
		}
		return m.mutateProjectFile(func(cfg *Config) {
			if e := cfg.McpServers[name]; e != nil {
				mutate(e)
			}
		})
	}
	mutate(s.cfg)
	return m.mutateProjectFile(func(cfg *Config) {
		if cfg.McpServers[name] == nil {
			cfg.McpServers[name] = &Server{}
		}
		mutate(cfg.McpServers[name])
	})
}

// WritePath 返回 API/面板写入配置的主文件路径
func (m *Manager) WritePath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writePath
}

// Reload 重新读取配置并重建注册表（断开全部现有连接）。全局/项目配置变更后
// 由 POST /api/mcp/reload 或 TUI 重新发现手动触发，不自动生效。
func (m *Manager) Reload() error { return m.Discover() }

// UpsertServers 把一批 server 定义写入项目 .doge/mcp.json（与社区工具的
// mcpServers 粘贴格式一致：{"name": {"type":"stdio","command":...}}，可多项；
// 已有项字段级合并）。不改动运行状态——需 POST /api/mcp/reload（或重启）
// 重新发现才生效。任一项非法则整体不写入。
func (m *Manager) UpsertServers(servers map[string]*Server) error {
	if len(servers) == 0 {
		return fmt.Errorf("empty mcpServers object")
	}
	// 先整体校验（合并到项目已有项之后必须能解析到 command 或 url）
	for name, srv := range servers {
		name = strings.TrimSpace(name)
		if !serverNameRe.MatchString(name) {
			return fmt.Errorf("invalid server name %q (want letters/digits/._-)", name)
		}
		merged := &Server{}
		if cur := m.projectServerEntry(name); cur != nil {
			mergeServer(merged, cur)
		}
		mergeServer(merged, srv)
		if err := validateTransport(merged); err != nil {
			logx.Warnf("[mcp] config import rejected: server %s: %v", name, err)
			return fmt.Errorf("server %s: %w", name, err)
		}
		logx.Debugf("[mcp] config import: %s type=%s command=%q url=%q", name, merged.Type, merged.Command, merged.URL)
	}
	clean := make(map[string]*Server, len(servers))
	for name, srv := range servers {
		clean[strings.TrimSpace(name)] = srv
	}
	if err := m.mutateProjectFile(func(cfg *Config) {
		for name, srv := range clean {
			if cur := cfg.McpServers[name]; cur != nil {
				mergeServer(cur, srv)
			} else {
				cfg.McpServers[name] = srv
			}
		}
	}); err != nil {
		return err
	}
	logx.Infof("[mcp] project config upsert %d server(s) (reload to apply)", len(clean))
	return nil
}

// validateTransport 合并后的定义必须能落到一种传输：url 或 command 至少其一
func validateTransport(s *Server) error {
	switch strings.ToLower(strings.TrimSpace(s.Type)) {
	case "", "stdio", "http", "sse":
	default:
		return fmt.Errorf("invalid type %q (want stdio|http)", s.Type)
	}
	if strings.TrimSpace(s.Command) == "" && strings.TrimSpace(s.URL) == "" {
		return fmt.Errorf("command or url required")
	}
	return nil
}

// projectServerEntry 读项目文件里某 server 的当前定义（无则 nil）
func (m *Manager) projectServerEntry(name string) *Server {
	data, err := os.ReadFile(m.writePath)
	if err != nil {
		return nil
	}
	var cfg Config
	if json.Unmarshal(data, &cfg) != nil || cfg.McpServers == nil {
		return nil
	}
	return cfg.McpServers[name]
}

// RemoveServer 从项目 .doge/mcp.json 删除一个 server 定义。定义在全局/共享
// 配置里的 server 无法从这里删除（返回错误，请改用 disable）。
func (m *Manager) RemoveServer(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("server name required")
	}
	inProject := false
	if err := m.mutateProjectFile(func(cfg *Config) {
		if _, ok := cfg.McpServers[name]; ok {
			delete(cfg.McpServers, name)
			inProject = true
		}
	}); err != nil {
		return err
	}
	if !inProject {
		return fmt.Errorf("server %q 不在项目 .doge/mcp.json 中（定义于全局/共享配置，请编辑源文件或使用禁用）", name)
	}
	logx.Infof("[mcp] project config removed server %s (reload to apply)", name)
	return nil
}

// mutateProjectFile 读改写项目配置文件（disabled 开关与 server 增删共用）
func (m *Manager) mutateProjectFile(fn func(cfg *Config)) error {
	cfg := &Config{McpServers: map[string]*Server{}}
	if data, err := os.ReadFile(m.writePath); err == nil {
		_ = json.Unmarshal(data, cfg)
		if cfg.McpServers == nil {
			cfg.McpServers = map[string]*Server{}
		}
	}
	fn(cfg)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.writePath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(m.writePath, append(data, '\n'), 0o600)
}

// setDisabledOnFile 把 disabled 字段写入项目配置文件
func (m *Manager) setDisabledOnFile(name string, enabled bool) error {
	return m.mutateProjectFile(func(cfg *Config) {
		if cfg.McpServers[name] == nil {
			cfg.McpServers[name] = &Server{}
		}
		cfg.McpServers[name].Disabled = !enabled
	})
}

// Setup 向写入主文件（默认 <cwd>/.doge/mcp.json；--mcp-config 覆盖）
// 写入一个 mcpServers 模板；若文件已存在则报错不覆盖。写完立即重新发现。
func (m *Manager) Setup() error {
	if _, err := os.Stat(m.writePath); err == nil {
		return fmt.Errorf("%s 已存在", m.writePath)
	}
	scaffold := `{
  "mcpServers": {
    "example": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "some-mcp-server"]
    }
  }
}
`
	if err := os.MkdirAll(filepath.Dir(m.writePath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.writePath, []byte(scaffold), 0o600); err != nil {
		return err
	}
	logx.Infof("[mcp] scaffold written: %s", m.writePath)
	return m.Discover()
}

// Close 断开全部连接并停止回收
func (m *Manager) Close() {
	m.mu.Lock()
	if m.stopReaper != nil {
		close(m.stopReaper)
		m.stopReaper = nil
	}
	servers := make([]*server, 0, len(m.servers))
	for _, s := range m.servers {
		servers = append(servers, s)
	}
	m.mu.Unlock()
	for _, s := range servers {
		s.disconnect()
	}
}

// idleReaper 定时断开空闲超时的 server
func (m *Manager) idleReaper() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	timeout := m.idleTimeout()
	// stop 通道在启动前由 Discover 持锁赋值，这里持锁捕获一次，
	// 避免 Close 置 nil 与循环里的无锁读竞争
	m.mu.Lock()
	stop := m.stopReaper
	m.mu.Unlock()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.mu.Lock()
			var idle []*server
			for _, s := range m.servers {
				if s.connected() && s.idle() >= timeout {
					idle = append(idle, s)
				}
			}
			m.mu.Unlock()
			for _, s := range idle {
				m.logf("[mcp] %s idle over %s, recycling", s.name, timeout)
				s.disconnect()
			}
		}
	}
}

// pick 按名取 server（空名=全部）；禁用的 server 不参与连接类操作
func (m *Manager) pick(name string) []*server {
	m.mu.Lock()
	defer m.mu.Unlock()
	if name == "" {
		out := make([]*server, 0, len(m.servers))
		for _, s := range m.servers {
			if !s.cfg.Disabled {
				out = append(out, s)
			}
		}
		return out
	}
	if s := m.servers[name]; s != nil && !s.cfg.Disabled {
		return []*server{s}
	}
	return nil
}

func (m *Manager) requestTimeout() time.Duration {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if cfg == nil {
		return DefaultRequestTimeoutMs * time.Millisecond
	}
	return cfg.RequestTimeout()
}

func (m *Manager) idleTimeout() time.Duration {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if cfg == nil {
		return DefaultIdleTimeoutSec * time.Second
	}
	return cfg.IdleTimeout()
}

// logf 调试日志：--mcp-trace 时请求级详情直接 info 可见，否则 debug 级
func (m *Manager) logf(format string, args ...any) {
	if m.trace {
		logx.Infof(format, args...)
		return
	}
	logx.Debugf(format, args...)
}

func transportOf(s *Server) string {
	if strings.TrimSpace(s.URL) != "" {
		return "http"
	}
	return "stdio"
}

// ---- 供代理工具使用的查询/调用 ----

// findTool 按 server__tool 定位一个工具的元数据
func (m *Manager) findTool(serverName, toolName string) *mcp.Tool {
	m.mu.Lock()
	s := m.servers[serverName]
	m.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.findTool(toolName)
}

func (s *server) findTool(name string) *mcp.Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// callTool 懒连接并调用；返回序列化文本
func (m *Manager) callTool(serverName, toolName string, args any) (string, error) {
	m.mu.Lock()
	s := m.servers[serverName]
	m.mu.Unlock()
	if s == nil {
		return "", fmt.Errorf("mcp server %q not found", serverName)
	}
	if s.cfg.Disabled {
		return "", fmt.Errorf("mcp server %q 已禁用（/mcp enable 或面板重新启用）", serverName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout())
	defer cancel()
	res, err := s.callTool(ctx, toolName, args)
	if err != nil {
		return "", err
	}
	return formatResult(res), nil
}

// lastError 某 server 最近一次连接/调用错误
func (m *Manager) lastError(name string) string {
	m.mu.Lock()
	s := m.servers[name]
	m.mu.Unlock()
	if s == nil {
		return "not found"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}
