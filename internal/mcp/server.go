package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"doge/pkg/logx"
)

// server 一个 MCP server 的运行时：懒连接（首次用时才 spawn 子进程/建立 HTTP
// 会话）、空闲回收、工具元数据。天然串行：单 server 加锁串行调用（协议就是
// request/response，多 server 之间天然并行）。
type server struct {
	name string
	cfg  *Server
	mgr  *Manager

	mu      sync.Mutex
	sess    *mcp.ClientSession
	client  *mcp.Client
	connAt  time.Time
	lastUse time.Time
	tools   []*mcp.Tool // 元数据（连接成功时刷新；无连接时来自磁盘缓存）
	lastErr string
}

// connect 建立连接（幂等：已连接则直接返回）
func (s *server) connect(ctx context.Context) error {
	s.mu.Lock()
	if s.sess != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	s.mgr.logf("[mcp] %s connecting", s.name)
	client := mcp.NewClient(&mcp.Implementation{Name: "doge", Version: s.mgr.version}, nil)
	transport, err := s.transport()
	if err != nil {
		return err
	}
	// 初始化握手由 SDK 在 Connect 内完成
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		s.setErr(err)
		return fmt.Errorf("mcp server %s: %w", s.name, err)
	}

	s.mu.Lock()
	s.sess, s.client, s.connAt, s.lastUse = sess, client, time.Now(), time.Now()
	s.lastErr = ""
	s.mu.Unlock()
	s.mgr.logf("[mcp] %s connected", s.name)
	return nil
}

// transport 按配置构造传输：url 走 Streamable HTTP（配置的 headers 经
// RoundTripper 注入每个请求——鉴权头就靠它），否则 stdio 子进程
func (s *server) transport() (mcp.Transport, error) {
	url := strings.TrimSpace(s.cfg.URL)
	if url != "" {
		client := &http.Client{}
		if len(s.cfg.Headers) > 0 {
			hdrs := make(map[string]string, len(s.cfg.Headers))
			for k, v := range s.cfg.Headers {
				hdrs[k] = expandEnv(v)
			}
			client.Transport = headerRoundTripper{hdrs: hdrs, next: http.DefaultTransport}
		}
		return &mcp.StreamableClientTransport{
			Endpoint:   url,
			HTTPClient: client,
		}, nil
	}
	if strings.TrimSpace(s.cfg.Command) == "" {
		return nil, fmt.Errorf("mcp server %s: command or url required", s.name)
	}
	cmd := exec.Command(s.cfg.Command, s.cfg.Args...)
	cmd.Env = os.Environ()
	for k, v := range s.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+expandEnv(v))
	}
	return &mcp.CommandTransport{Command: cmd}, nil
}

// headerRoundTripper 给每个出站请求注入配置的 headers（Authorization 等）
type headerRoundTripper struct {
	hdrs map[string]string
	next http.RoundTripper
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range h.hdrs {
		clone.Header.Set(k, v)
	}
	return h.next.RoundTrip(clone)
}

// expandEnv 展开 ${VAR} 插值；未定义变量展开为空串（与 pi-mcp-adapter 一致）
func expandEnv(v string) string {
	return os.Expand(v, func(key string) string { return os.Getenv(key) })
}

// disconnect 断开连接并释放子进程
func (s *server) disconnect() {
	s.mu.Lock()
	if s.sess == nil {
		s.mu.Unlock()
		return
	}
	_ = s.sess.Close()
	s.sess, s.client = nil, nil
	s.mu.Unlock()
	s.mgr.logf("[mcp] %s disconnected", s.name)
}

// ensureConn 懒连接；返回时必已在线
func (s *server) ensureConn(ctx context.Context) error {
	s.mu.Lock()
	online := s.sess != nil
	s.mu.Unlock()
	if online {
		s.touch()
		return nil
	}
	return s.connect(ctx)
}

func (s *server) touch() {
	s.mu.Lock()
	s.lastUse = time.Now()
	s.mu.Unlock()
}

// listTools 拉取工具清单并刷新磁盘缓存
func (s *server) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	if err := s.ensureConn(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	start := time.Now()
	res, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		s.setErr(err)
		return nil, err
	}
	s.mu.Lock()
	s.tools = res.Tools
	s.mu.Unlock()
	s.mgr.cache.save(s.name, res.Tools)
	s.mgr.logf("[mcp] %s tools/list: %d tool(s) in %s", s.name, len(res.Tools), time.Since(start).Round(time.Millisecond))
	return res.Tools, nil
}

// callTool 调用一个工具（懒连接 + 串行执行）
func (s *server) callTool(ctx context.Context, tool string, args any) (*mcp.CallToolResult, error) {
	if err := s.ensureConn(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	start := time.Now()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		s.setErr(err)
		return nil, err
	}
	s.mgr.logf("[mcp] %s tools/call %s: %s, %d content block(s)", s.name, tool, time.Since(start).Round(time.Millisecond), len(res.Content))
	return res, nil
}

func (s *server) setErr(err error) {
	s.mu.Lock()
	s.lastErr = fmt.Sprint(err)
	s.mu.Unlock()
	// 连接/调用失败必须落日志（warn 级别，默认可见）——面板只显示摘要，
	// 完整报错以诊断日志为准
	logx.Warnf("[mcp] %s: %v", s.name, err)
}

// connected 是否在线
func (s *server) connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess != nil
}

// idle 距上次使用的时间（未用过则距连接时间）
func (s *server) idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess == nil {
		return 0
	}
	ref := s.lastUse
	if ref.IsZero() {
		ref = s.connAt
	}
	return time.Since(ref)
}

// toolInfos 当前元数据（来自缓存或活连接）
func (s *server) toolInfos() []ToolInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ToolInfo, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, ToolInfo{Name: t.Name, Description: t.Description})
	}
	return out
}
