package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"doge/internal/app"
	"doge/internal/coding"
	"doge/internal/web"
)

// runWebMode 启动 Web 控制台，决定 TUI 是否同时运行
func runWebMode(a *app.App, opts cliOptions, cfg *coding.WebSettings) {
	web.Version = versionString()

	// 解析 Web 设置：CLI 参数 > 配置文件 > 默认值
	host := "127.0.0.1"
	if opts.webHost != "" {
		host = opts.webHost
	} else if cfg != nil && cfg.Host != "" {
		host = cfg.Host
	}
	port := 8080
	if opts.webPort != 0 {
		port = opts.webPort
	} else if cfg != nil && cfg.Port != 0 {
		port = cfg.Port
	}
	if port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "doge: invalid web port %d\n", port)
		os.Exit(1)
	}
	token := opts.webToken
	if token == "" && cfg != nil {
		token = cfg.Token
	}
	noTUI := opts.noTUI
	if !noTUI && cfg != nil {
		noTUI = cfg.NoTui
	}
	corsOrigins := opts.webCors
	if len(corsOrigins) == 0 && cfg != nil {
		corsOrigins = cfg.CorsOrigins
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))

	// The TUI owns the shared session when it actually runs (web+TUI mode);
	// in headless web mode nothing else holds the original session, so the
	// TUI-protection guard is disabled.
	tuiActive := !noTUI && isTTY(os.Stdout)

	srv, err := web.NewServer(web.Options{
		Backend:     web.NewBackend(a.Session, version, tuiActive),
		Token:       token,
		Address:     address,
		Mesh:        a.MeshView,
		Mcp:         a.Mcp,
		CorsOrigins: corsOrigins,
		Uploads:     a.Uploads,
		// POST /api/shutdown: the same graceful path as Ctrl+C (headless mode
		// waits on sigCh; web+TUI quits via the TUI's SIGINT handling, then
		// falls through to shutdownWeb below).
		Shutdown: func() {
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		os.Exit(1)
	}

	token = srv.Token()
	displayWebAccess(host, port, token, tuiActive)
	if err := writeWebTokenFile(token); err != nil {
		fmt.Fprintf(os.Stderr, "doge: warning: could not write web token file: %v\n", err)
	}

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start() }()

	// 无头 Web 模式：等待信号，绑定失败则退出
	if noTUI || !isTTY(os.Stdout) {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		select {
		case err := <-startErr:
			if err != nil {
				fmt.Fprintf(os.Stderr, "doge: web server: %v\n", err)
				os.Exit(1)
			}
		case <-sigCh:
		}
		shutdownWeb(srv)
		return
	}

	// Web + TUI：TUI 前台运行，Web 绑定失败则继续 TUI
	go func() {
		if err := <-startErr; err != nil {
			fmt.Fprintf(os.Stderr, "doge: web server: %v (TUI continues)\n", err)
		}
	}()
	runInteractive(a, opts.message)
	shutdownWeb(srv)
}

func shutdownWeb(srv *web.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// displayWebAccess 打印控制台 URL 和令牌，并打开浏览器
func displayWebAccess(host string, port int, token string, tuiActive bool) {
	host = strings.Trim(host, "[]") // allow IPv6 literals to print cleanly
	urlHost := host
	if host == "0.0.0.0" || host == "::" {
		urlHost = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s/", net.JoinHostPort(urlHost, strconv.Itoa(port)))
	hint := "Press Ctrl+C to stop the server."
	if tuiActive {
		hint = "Press Ctrl+C on the TUI to quit (TUI+web mode)."
	}
	fmt.Fprintf(os.Stderr, `doge web console
  URL:         %s
  Access token: %s
  Token file:  ~/.doge/agent/web-token (0600)
%s
`, url, token, hint)
	// Best-effort auto-open with the token prefilled.
	openBrowser(url + "?token=" + token)
}

// writeWebTokenFile 持久化令牌供脚本/用户引用
func writeWebTokenFile(token string) error {
	path := filepath.Join(coding.AgentDir(), "web-token")
	return os.WriteFile(path, []byte(token+"\n"), 0o600)
}

// openBrowser 在默认浏览器中打开 URL，忽略错误
func openBrowser(url string) {
	if os.Getenv("DOGE_WEB_NO_OPEN") == "1" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		// Non-fatal: the printed URL still works.
	}
}
