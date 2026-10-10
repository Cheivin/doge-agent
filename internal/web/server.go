// Package web implements the optional Web console: a token-protected HTTP
// server plus a WebSocket broadcaster that shares the same AgentSession as
// the TUI (or runs standalone with --no-tui).
package web

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/uploads"
	"doge/internal/mcp"
	"doge/internal/mesh"
)

// Version Web 控制台报告的构建版本，main.go 注入
var Version = "dev"

// Options 配置 Web Server
type Options struct {
	Backend     Backend        // 后端实现
	Token       string         // 访问令牌，空则自动生成
	Address     string         // 监听地址，如 "127.0.0.1:8080"
	Mesh        MeshView       // mesh 网络视图，nil 则隐藏面板
	Mcp         McpView        // MCP 管理器视图
	CorsOrigins []string       // CORS 允许的源，"*" 允许所有
	Shutdown    func()         // 关闭回调，nil 则 503
	Uploads     *uploads.Store // 上传暂存区，nil 则上传接口 503
}

// MeshView exposes the mesh network (connected nodes + remote-task status) to
// the web console, decoupled from the libp2p implementation. A non-mesh build
// never sets it (nil = the mesh panel is hidden).
type MeshView interface {
	Self() mesh.AgentProfile
	Peers() []mesh.PeerInfo
	Tasks() []mesh.TaskStatus

	// Addrs lists this node's listen addresses (multiaddrs without the /p2p
	// suffix); Config returns the mesh.json snapshot the node started with.
	// The token is included: API callers already hold the access token, which
	// grants far more (bash) than mesh membership ever could.
	Addrs() []string
	Config() mesh.Config

	// Forward rules are declarative mesh.json entries; mutations validate,
	// persist and hot-apply on the running node.
	ForwardList() []mesh.ForwardRule
	ForwardAdd(rule mesh.ForwardRule) error
	ForwardRemove(name string) error
	ForwardSetEnabled(name string, enabled bool) error
}

// McpView 向 Web 控制台暴露 MCP 管理器
type McpView interface {
	Enabled() bool
	Servers() []mcp.ServerInfo
	Reconnect(name string) error
	SetEnabled(name string, enabled bool) error
	Setup() error
	Reload() error
	UpsertServers(servers map[string]*mcp.Server) error
	RemoveServer(name string) error
	// SetServerDirect / SetServerAutoStart 切换 server 的默认一等工具提升与
	// 自启连接（内存同步 + 项目文件持久化）。
	SetServerDirect(name string, on bool) error
	SetServerAutoStart(name string, on bool) error
	// BuildTools 产出当前配置下的 mcp 工具组（代理 + direct），供 reload 后
	// 向活动会话重发。
	BuildTools() []*agent.AgentTool
	// WritePath 返回 API/面板写入配置的主文件绝对路径（供 UI 展示定位问题用）。
	WritePath() string
}

// Server 令牌保护的 HTTP + WebSocket 控制台
type Server struct {
	auth      *authState
	hub       *Hub
	ctrl      *controlTable
	shared    *coding.AgentSession // TUI shared instance (nil when none)
	tuiActive bool
	version   string
	addr      string
	httpSrv   *http.Server

	// meshView backs the mesh panel (nil = mesh disabled/not compiled in).
	meshView MeshView

	// mcpView backs the MCP endpoints (nil = MCP disabled/not initialized).
	mcpView McpView

	// shutdown backs POST /api/shutdown (nil = endpoint answers 503).
	shutdown func()

	// uploads backs the upload endpoints (nil = uploads answer 503).
	// 周期清理不在这里——由 main 的 uploadStore.StartSweep 统一挂载，
	// TUI/mesh-only 模式同样生效
	uploads *uploads.Store

	// applyMu 串行化 place/apply 的写盘段：并发 confirm 会互相覆盖 .bak
	// 备份并竞争写同一目标文件
	applyMu sync.Mutex

	// corsOrigins backs the CORS middleware (empty = no CORS headers).
	corsOrigins []string

	// defaultBackend is the shared view served to requests without a clientID
	// (curl/scripts, older clients).
	defaultBackend Backend

	windowsMu sync.Mutex
	windows   map[string]*window

	listener net.Listener
}

// window 单个浏览器窗口的会话视图
type window struct {
	id      string
	backend Backend
	unsub   func()
	setKind func(coding.ConsumerKind)
}

// clientID 从请求头获取窗口 ID，默认回退到共享窗口
func clientID(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get("X-Doge-Client"))
	if id == "" {
		id = defaultWindowID
	}
	return id
}

// NewServer 构建控制台，初始化共享会话和控制表
func NewServer(opts Options) (*Server, error) {
	if opts.Backend == nil {
		return nil, errors.New("web: nil backend")
	}
	auth, err := newAuthState(opts.Token)
	if err != nil {
		return nil, err
	}
	addr := opts.Address
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	s := &Server{
		auth:           auth,
		hub:            newHub(),
		ctrl:           newControlTable(),
		defaultBackend: opts.Backend,
		windows:        map[string]*window{},
		addr:           addr,
		meshView:       opts.Mesh,
		mcpView:        opts.Mcp,
		shutdown:       opts.Shutdown,
		uploads:        opts.Uploads,
		corsOrigins:    opts.CorsOrigins,
	}
	if seed, ok := opts.Backend.(*sessionBackend); ok {
		s.shared = seed.currentAgent()
		s.tuiActive = seed.shared != nil
	}
	if s.shared != nil {
		s.ctrl.register(s.shared.CurrentSession().FilePath, s.shared)
		if s.tuiActive {
			s.ctrl.acquire(s.shared.CurrentSession().FilePath, "tui")
		}
	}

	s.hub.SetSnapshot(s.windowSnapshot)
	s.hub.SetWindowChange(s.setWindowKind)
	s.ctrl.SetOnTransfer(s.onControlTransfer)
	s.ctrl.SetLiveness(s.consumerLive)
	s.windows[defaultWindowID] = s.newDefaultWindow()
	s.setWindowKind(defaultWindowID, 0) // seed the default window's kind
	opts.Backend.InstallPermissionHandler(s.hub)

	httpSrv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.httpSrv = httpSrv
	return s, nil
}

// newDefaultWindow 订阅共享后端事件，推送到默认窗口
func (s *Server) newDefaultWindow() *window {
	seed := s.defaultBackend
	unsub, setKind := seed.Subscribe(func(ev agent.AgentEvent) error {
		data, err := marshalAgentEvent(ev)
		if err != nil {
			return err
		}
		s.hub.broadcastTo(defaultWindowID, data)
		return nil
	})
	return &window{id: defaultWindowID, backend: seed, unsub: unsub, setKind: setKind}
}

// newWindow 构建窗口级会话视图，绑定控制表
func (s *Server) newWindow(id string) *window {
	b := newWindowBackend(s.ctrl, s.shared, id, s.version, s.tuiActive)
	b.ask = s.hub
	unsub, setKind := b.Subscribe(func(ev agent.AgentEvent) error {
		// Marshal a snapshot, never the live streaming partial: the adapter
		// mutates it under the message lock while streaming, and marshaling
		// under that lock inside the agent's emit chain must not block the
		// loop (a blocked listener stops draining the event stream and
		// deadlocks the producer).
		if ev.Message != nil {
			ev.Message = ev.Message.Clone()
		}
		data, err := marshalAgentEvent(ev)
		if err != nil {
			return err
		}
		s.hub.broadcastTo(id, data)
		return nil
	})
	return &window{id: id, backend: b, unsub: unsub, setKind: setKind}
}

// backendForID 根据窗口 ID 解析后端，懒创建窗口视图
func (s *Server) backendForID(id string) Backend {
	s.windowsMu.Lock()
	defer s.windowsMu.Unlock()
	if w := s.windows[id]; w != nil {
		return w.backend
	}
	if s.shared == nil {
		// No shared session (e.g. test stubs): no per-window views possible.
		return s.defaultBackend
	}
	w := s.newWindow(id)
	s.windows[id] = w
	return w.backend
}

// backendFor 根据请求解析后端
func (s *Server) backendFor(r *http.Request) Backend {
	return s.backendForID(clientID(r))
}

// setWindowKind 同步窗口的消费者类型（连接数决定是否有浏览器）
func (s *Server) setWindowKind(id string, n int) {
	s.windowsMu.Lock()
	w := s.windows[id]
	s.windowsMu.Unlock()
	if w == nil || w.setKind == nil {
		return
	}
	if n > 0 {
		w.setKind(coding.ConsumerInteractive)
	} else {
		w.setKind(coding.ConsumerHeadless)
	}
}

// consumerLive 报告消费者是否仍活跃，控制表据此自愈幽灵持有者：窗口断开
// （刷新换 clientID、关闭标签页）后残留的写权视为放弃。TUI 无 WebSocket
// 连接，进程内存活即活跃。
func (s *Server) consumerLive(consumer string) bool {
	if consumer == "tui" {
		return true
	}
	return s.hub.windowConns(consumer) > 0
}

// onControlTransfer broadcasts ownership changes (release or force takeover)
// to every window, so the displaced owner turns read-only immediately and
// external clients can track who holds write control.
func (s *Server) onControlTransfer(key, owner string) {
	data, err := json.Marshal(map[string]any{"type": "session_control", "key": key, "owner": owner})
	if err != nil {
		return
	}
	s.hub.broadcast(data)
}

// handleMesh 返回 mesh 网络状态（节点、peers、任务、转发规则）
func (s *Server) handleMesh(w http.ResponseWriter, r *http.Request) {
	if s.meshView == nil {
		writeError(w, http.StatusNotFound, "mesh not enabled (build with -tags mesh)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"self":     s.meshView.Self(),
		"addrs":    s.meshView.Addrs(),
		"config":   s.meshView.Config(),
		"peers":    s.meshView.Peers(),
		"tasks":    s.meshView.Tasks(),
		"forwards": s.meshView.ForwardList(),
	})
}

// Token 返回当前访问令牌
func (s *Server) Token() string { return s.auth.Token() }

// AuthEnabled 报告令牌认证是否启用

// Address 返回配置的监听地址

// AdvertisedAddr 返回绑定地址（Start 后解析 :0 端口）

// Start 监听并服务，Shutdown 后返回 http.ErrServerClosed
func (s *Server) Start() error {
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.listener = l
	err = s.httpSrv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 优雅关闭服务器、WebSocket 客户端和窗口订阅
func (s *Server) Shutdown(ctx context.Context) error {
	s.hub.shutdown()
	s.windowsMu.Lock()
	for id, w := range s.windows {
		if w.unsub != nil {
			w.unsub()
		}
		delete(s.windows, id)
	}
	s.windowsMu.Unlock()
	if s.httpSrv != nil {
		return s.httpSrv.Shutdown(ctx)
	}
	return nil
}

// handler 构建路由表，静态资源服务 SPA，/api/* 和 /ws 需要令牌认证
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.Handle("POST /api/auth/logout", s.requireAuth(s.handleLogout))
	mux.Handle("GET /api/auth/whoami", s.requireAuth(s.handleWhoami))

	mux.Handle("GET /api/status", s.requireAuth(s.handleStatus))
	mux.Handle("GET /api/stats", s.requireAuth(s.handleStats))
	mux.Handle("POST /api/reload", s.requireAuth(s.handleReload))
	mux.Handle("POST /api/prompt", s.requireAuth(s.handlePrompt))
	mux.Handle("POST /api/abort", s.requireAuth(s.handleAbort))
	mux.Handle("POST /api/compact", s.requireAuth(s.handleCompact))

	// Uploads: staging store + attachment-aware prompt (files enter the
	// conversation as multimodal content or read-tool references).
	mux.Handle("POST /api/uploads", s.requireAuth(s.handleUploadsCreate))
	mux.Handle("GET /api/uploads", s.requireAuth(s.handleUploadsList))
	mux.Handle("GET /api/uploads/{id}", s.requireAuth(s.handleUploadsGet))
	mux.Handle("GET /api/uploads/{id}/content", s.requireAuth(s.handleUploadsContent))
	mux.Handle("DELETE /api/uploads/{id}", s.requireAuth(s.handleUploadsDelete))
	// place / apply: deterministic file placement and config application
	// (two-phase dry-run → confirm; P2 of the file-transfer design).
	mux.Handle("POST /api/uploads/{id}/place", s.requireAuth(s.handleUploadPlace))
	mux.Handle("POST /api/uploads/{id}/apply", s.requireAuth(s.handleUploadApply))
	mux.Handle("GET /api/models", s.requireAuth(s.handleModels))
	mux.Handle("POST /api/model", s.requireAuth(s.handleModel))
	mux.Handle("POST /api/thinking", s.requireAuth(s.handleThinking))
	mux.Handle("GET /api/session", s.requireAuth(s.handleSession))
	mux.Handle("GET /api/sessions", s.requireAuth(s.handleSessionList))
	mux.Handle("POST /api/session/new", s.requireAuth(s.handleSessionNew))
	mux.Handle("POST /api/session/switch", s.requireAuth(s.handleSessionSwitch))
	mux.Handle("POST /api/session/delete", s.requireAuth(s.handleSessionDelete))
	mux.Handle("POST /api/session/claim", s.requireAuth(s.handleSessionClaim))
	mux.Handle("POST /api/session/takeover", s.requireAuth(s.handleSessionTakeover))
	mux.Handle("POST /api/turn/edit", s.requireAuth(s.handleTurnEdit))
	mux.Handle("POST /api/question/answer", s.requireAuth(s.handleQuestionAnswer))
	mux.Handle("POST /api/permission/answer", s.requireAuth(s.handlePermissionAnswer))
	mux.Handle("GET /api/mesh", s.requireAuth(s.handleMesh))
	mux.Handle("GET /api/mesh/forwards", s.requireAuth(s.handleMeshForwardsList))
	mux.Handle("POST /api/mesh/forwards", s.requireAuth(s.handleMeshForwardsAdd))
	mux.Handle("POST /api/mesh/forwards/{name}/enable", s.requireAuth(s.handleMeshForwardsSetEnabled(true)))
	mux.Handle("POST /api/mesh/forwards/{name}/disable", s.requireAuth(s.handleMeshForwardsSetEnabled(false)))
	mux.Handle("DELETE /api/mesh/forwards/{name}", s.requireAuth(s.handleMeshForwardsRemove))

	// MCP: server status + tools, reconnect, enable/disable, setup scaffold.
	mux.Handle("GET /api/mcp", s.requireAuth(s.handleMcpStatus))
	mux.Handle("POST /api/mcp/reconnect", s.requireAuth(s.handleMcpReconnect))
	mux.Handle("POST /api/mcp/enable", s.requireAuth(s.handleMcpSetEnabled(true)))
	mux.Handle("POST /api/mcp/disable", s.requireAuth(s.handleMcpSetEnabled(false)))
	mux.Handle("POST /api/mcp/reload", s.requireAuth(s.handleMcpReload))
	mux.Handle("POST /api/mcp/servers", s.requireAuth(s.handleMcpServersUpsert))
	mux.Handle("DELETE /api/mcp/servers/{name}", s.requireAuth(s.handleMcpServersRemove))
	mux.Handle("POST /api/mcp/servers/{name}/direct", s.requireAuth(s.handleMcpServerDirect))
	mux.Handle("POST /api/mcp/servers/{name}/autostart", s.requireAuth(s.handleMcpServerAutoStart))

	// Per-session MCP toggle (normal sessions only; applied on reload).
	mux.Handle("GET /api/session/mcp", s.requireAuth(s.handleSessionMcpGet))
	mux.Handle("POST /api/session/mcp", s.requireAuth(s.handleSessionMcpSet))

	// Skills: installed-skill list/detail (fresh scan of the discovery dirs)
	// plus the active session's loaded set.
	mux.Handle("GET /api/skills", s.requireAuth(s.handleSkillsList))
	mux.Handle("GET /api/skills/{name}", s.requireAuth(s.handleSkillsGet))
	mux.Handle("POST /api/skills/{name}/enabled", s.requireAuth(s.handleSkillsSetEnabled))
	mux.Handle("DELETE /api/skills/{name}", s.requireAuth(s.handleSkillsDelete))
	mux.Handle("GET /api/session/skills", s.requireAuth(s.handleSessionSkills))

	// agent 配置与诊断
	mux.Handle("GET /api/fs/ls", s.requireAuth(s.handleFsList))
	mux.Handle("GET /api/config", s.requireAuth(s.handleConfigOverview))
	mux.Handle("GET /api/config/settings", s.requireAuth(s.handleSettingsGet))
	mux.Handle("PUT /api/config/settings", s.requireAuth(s.handleSettingsPut))
	mux.Handle("GET /api/config/auth", s.requireAuth(s.handleAuthGet))
	mux.Handle("POST /api/config/auth", s.requireAuth(s.handleAuthSet))
	mux.Handle("DELETE /api/config/auth/{provider}", s.requireAuth(s.handleAuthDelete))
	mux.Handle("GET /api/config/models", s.requireAuth(s.handleConfigModelsGet))
	mux.Handle("POST /api/config/models", s.requireAuth(s.handleConfigModelsUpsert))
	mux.Handle("DELETE /api/config/models/{provider}/{id}", s.requireAuth(s.handleConfigModelsDelete))
	mux.Handle("GET /api/logs", s.requireAuth(s.handleLogs))

	mux.Handle("POST /api/shutdown", s.requireAuth(s.handleShutdown))
	mux.Handle("GET /ws", s.requireAuth(s.hub.serveWS))

	mux.Handle("/", staticHandler())
	return compression(s.cors(mux))
}

// shutdownFlushDelay 关闭响应到达客户端的延迟
const shutdownFlushDelay = 300 * time.Millisecond

// handleWhoami 确认凭证并报告认证渠道
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	via, _ := s.auth.authorized(r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "method": via})
}

// handleShutdown 优雅停止 agent 进程
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if s.shutdown == nil {
		writeError(w, http.StatusServiceUnavailable, "shutdown not available")
		return
	}
	fn := s.shutdown
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	go func() {
		time.Sleep(shutdownFlushDelay)
		fn()
	}()
}

// cors 处理跨域请求，配置的源允许浏览器调用 API
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.ToLower(strings.TrimRight(r.Header.Get("Origin"), "/"))
		if origin != "" && origin != "null" {
			if wildcard, ok := s.originAllowed(origin); ok {
				h := w.Header()
				if wildcard {
					h.Set("Access-Control-Allow-Origin", "*")
				} else {
					h.Set("Access-Control-Allow-Origin", origin)
					h.Add("Vary", "Origin")
					h.Set("Access-Control-Allow-Credentials", "true")
				}
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Doge-Client")
				h.Set("Access-Control-Max-Age", "86400")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed 报告源是否允许跨域调用
func (s *Server) originAllowed(origin string) (wildcard, ok bool) {
	for _, o := range s.corsOrigins {
		switch o = strings.ToLower(strings.TrimRight(strings.TrimSpace(o), "/")); o {
		case "*":
			return true, true
		case origin:
			return false, true
		}
	}
	return false, false
}

// compression 启用透明 gzip 压缩
func compression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" || !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gz := &gzipWriter{ResponseWriter: w, gz: gzip.NewWriter(w)}
		defer gz.Close()
		next.ServeHTTP(gz, r)
	})
}

func acceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.TrimSpace(strings.SplitN(enc, ";", 2)[0]) == "gzip" {
			return true
		}
	}
	return false
}

// gzipWriter wraps a ResponseWriter with gzip. Content-Encoding is set on the
// first write, covering handlers that omit an explicit WriteHeader (static
// files, etc.).
type gzipWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	return g.gz.Write(p)
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	g.Header().Del("Content-Length")
	g.Header().Set("Content-Encoding", "gzip")
	g.Header().Set("Vary", "Accept-Encoding")
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Flush() {
	_ = g.gz.Flush()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) Close() { _ = g.gz.Close() }

// windowSnapshot builds the session_info frame a client needs to rebuild its
// view on (re)connect for a given window.
func (s *Server) windowSnapshot(id string) ([]byte, error) {
	b := s.backendForID(id)
	st := b.Status()
	info := b.SessionInfo()
	return json.Marshal(map[string]any{
		"type":             "session_info",
		"sessionId":        st.SessionID,
		"cwd":              st.Cwd,
		"model":            st.Model,
		"thinking":         st.Thinking,
		"streaming":        st.Streaming,
		"pendingTools":     st.PendingTools,
		"errorMessage":     st.ErrorMessage,
		"version":          st.Version,
		"messages":         info.Messages,
		"streamingMessage": streamingOrNil(st, b),
	})
}

func streamingOrNil(st Status, b Backend) *ai.Message {
	if !st.Streaming {
		return nil
	}
	return b.StreamingMessage()
}
