// doge 编码智能体 CLI，Pi agent harness 的 Go 移植版
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"doge/internal/ai"
	"doge/internal/app"
	"doge/internal/coding"
	"doge/internal/ui"
)

// version 和 gitCommit 由 Makefile 在构建时通过 -ldflags 注入
var (
	version   = "0.9.0"
	gitCommit = "" // 短 git 哈希（dirty 构建会带 -dirty 后缀）
)

// versionString 返回版本字符串，构建时有 git commit 则附加显示
func versionString() string {
	if gitCommit == "" {
		return version
	}
	return version + " (" + gitCommit + ")"
}

type cliOptions struct {
	mode         string // 输出模式：print/json
	model        string // 模型标识：provider/model[:thinking]
	apiKey       string // API 密钥（覆盖存储/环境变量）
	noSession    bool   // 不持久化会话
	resume       bool   // 恢复最近会话
	cont         bool   // 继续最近会话
	fork         bool   // 分叉会话
	sessionPath  string // 会话 ID 或路径
	systemPrompt string // 自定义系统提示词
	approve      bool   // 自动批准项目信任
	skills       string // 额外技能目录
	prompts      string // 额外提示词模板目录
	showModels   bool   // 列出可用模型
	export       string // 导出会话为 HTML
	message      string // 初始消息

	importZCode    string // 导入 ZCode 会话的 SQLite 库路径
	importZCodeSet bool   // 是否显式传入 -import-zcode

	web      bool     // 启动 Web 控制台
	webPort  int      // Web 端口
	webHost  string   // Web 绑定地址
	webToken string   // Web 访问令牌
	webCors  []string // CORS 允许的源
	noTUI    bool     // 无头 Web 模式
	noWeb    bool     // 禁用 Web 控制台

	noMcp     bool   // 禁用 MCP 服务器
	mcpConfig string // MCP 配置文件路径
	mcpTrace  bool   // 追踪 MCP 请求响应

	mesh      bool   // 启动 P2P mesh
	meshToken string // mesh 网络令牌

	logLevel string // 日志级别

	// args 位置参数；第一个是消息内容，或 "mesh" 子命令
	args []string
}

func main() {
	// 注入构建版本到 User-Agent
	ai.Version = version

	opts := parseArgs(os.Args[1:])
	if opts.showModels {
		listModels()
		return
	}

	// mesh 子命令：doge mesh new|peers|profile|forward ...
	if len(opts.args) > 0 && opts.args[0] == "mesh" {
		os.Exit(runMeshCommand(opts.args[1:]))
	}

	// 非交互模式：stdout 非 TTY 或指定 -p
	mode := coding.PrintModeText
	if opts.mode == "json" {
		mode = coding.PrintModeJSON
	}
	interactive := false
	if opts.mode == "" {
		interactive = isTTY(os.Stdout) && isTTY(os.Stdin)
	}

	// --fork 需要指定已有会话（--no-session 优先，沿用原会话选择顺序）
	if opts.fork && !opts.noSession {
		fmt.Fprintln(os.Stderr, "doge: --fork requires an existing session id")
		os.Exit(1)
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	// agent 目录与设置（全局 + 项目）
	env, err := app.Prepare(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		os.Exit(1)
	}

	// Web 控制台开关：--web 或 --no-tui 启用，--no-web 优先级最高
	webConfig := env.Settings.WebConfig()
	webEnabled := ((opts.web || opts.noTUI) || (webConfig != nil && webConfig.Enabled)) && !opts.noWeb

	// 诊断日志：agent/ai/permission/mesh/web 事件写入同一时序文件
	consoleLog := os.Getenv("DOGE_DEBUG") != ""
	if !consoleLog && webEnabled {
		noTuiRequested := opts.noTUI || (webConfig != nil && webConfig.NoTui)
		if noTuiRequested || !isTTY(os.Stdout) {
			consoleLog = true
		}
	}
	if err := app.SetupLog(env, versionString(), opts.logLevel, consoleLog); err != nil {
		fmt.Fprintf(os.Stderr, "doge: log setup failed: %v\n", err)
	}

	// 导入 ZCode 会话后直接退出，不进入 TUI/Web
	if opts.importZCodeSet {
		dbPath := opts.importZCode
		if dbPath == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				fmt.Fprintf(os.Stderr, "doge: %v\n", err)
				os.Exit(1)
			}
			dbPath = filepath.Join(home, ".zcode", "cli", "db", "db.sqlite")
		}
		mgr := coding.NewSessionManager(coding.SessionsDir())
		imported, skipped, err := coding.ImportZCodeSessions(dbPath, mgr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "doge: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("导入完成：新增 %d 个 ZCode 会话，跳过 %d 个\n", imported, skipped)
		return
	}

	// 装配：模型运行时、会话、技能、上传暂存区、mesh、MCP
	a, err := app.Assemble(env, app.Options{
		Version:      versionString(),
		Model:        opts.model,
		APIKey:       opts.apiKey,
		SystemPrompt: opts.systemPrompt,
		SessionID:    opts.sessionPath,
		Continue:     opts.cont || opts.resume,
		NoSession:    opts.noSession,
		Skills:       opts.skills,
		Prompts:      opts.prompts,
		NoMcp:        opts.noMcp,
		McpConfig:    opts.mcpConfig,
		McpTrace:     opts.mcpTrace,
		Mesh:         opts.mesh,
		MeshToken:    opts.meshToken,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		os.Exit(1)
	}

	if opts.export != "" {
		manager := coding.NewSessionManager(coding.SessionsDir())
		var target *coding.Session
		if filepath.IsAbs(opts.export) {
			target, err = manager.LoadSession(opts.export)
		} else {
			summaries, _ := manager.ListSessions()
			for _, s := range summaries {
				if s.ID == opts.export {
					target, err = manager.LoadSession(s.FilePath)
					break
				}
			}
			if target == nil {
				target, err = manager.LoadSession(filepath.Join(coding.SessionsDir(), opts.export))
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "doge: %v\n", err)
			os.Exit(1)
		}
		outPath := strings.TrimSuffix(opts.export, ".jsonl") + ".html"
		if err := coding.ExportToHTML(target, outPath); err != nil {
			fmt.Fprintf(os.Stderr, "doge: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Exported session to %s\n", outPath)
		return
	}

	if webEnabled {
		runWebMode(a, opts, webConfig)
		return
	}

	if interactive {
		runInteractive(a, opts.message)
		return
	}
	code := coding.RunPrintMode(a.Session, opts.message, mode)
	// os.Exit 会跳过 defer，手动收尾
	a.Close()
	os.Exit(code)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `doge %s - coding agent (Go port of the Pi agent harness)

Usage:
  doge [options] [message...]

Options:
  -p, --mode print|json   non-interactive mode
      --model <model>     provider/model or provider/model:thinking
      --session <id>      resume a specific session
      --continue          continue the most recent session
      --no-session        run without session persistence
      --export <path>     export a session to HTML
      --import-zcode <db> 导入 ZCode 会话（SQLite 库路径，留空用 ~/.zcode/cli/db/db.sqlite）
      --list-models       list available models
  -y, --approve           auto-approve project trust
      --web               start the Web console (shares the TUI session)
      --no-web            disable the Web console even if enabled in settings
      --port <n>          Web console port (default from settings or 8080)
      --web-host <host>   Web console bind address (default from settings or 127.0.0.1)
      --no-tui            headless web console (implies --web); logs print to the console too
      --web-token <tok>   fixed access token (default from settings or random)
      --web-cors <origin> allow cross-origin API/browser access (repeatable, or '*')
      --no-mcp            disable MCP servers even if configured
      --mcp-config <file> MCP config file to use instead of ~/.doge/agent/mcp.json
      --mcp-trace         trace MCP requests/responses in the diagnostic log
      --mesh              start the P2P mesh (join the mesh.json network)
      --mesh-token <tok>  mesh token to join (overrides the mesh.json token)
      --version           print version
      --help              show this help

Mesh subcommands (see 'doge mesh --help'):
  doge mesh new            create a new network token
  doge mesh peers          list connected nodes
  doge mesh forward ...    manage port forwarding rules

Interactive mode is started when stdin is a TTY.
Web console requires the access token shown on startup (or a file
~/.doge/agent/web-token). Web host/port/token can be set in
settings.json under "web": {"enabled":true,"host":"127.0.0.1","port":8080,
"token":"...","noTui":false,"corsOrigins":["https://ui.example.com"]}.
API clients authenticate with "Authorization: Bearer <token>" instead of
the browser login cookie; /ws also accepts ?token=.

Full usage/config guide is BUILT IN: ask the agent any usage question and it
queries the embedded "guide" tool (module list + per-module content) — no
external docs needed.
`, version)
}

func parseArgs(args []string) cliOptions {
	opts := cliOptions{}
	fs := flag.NewFlagSet("doge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.mode, "mode", "", "output mode: print (default), json")
	pFlag := fs.Bool("p", false, "print mode (non-interactive)")
	fs.StringVar(&opts.model, "model", "", "provider/model[:thinking]")
	fs.StringVar(&opts.apiKey, "api-key", "", "API key (overrides stored/ambient)")
	fs.BoolVar(&opts.noSession, "no-session", false, "run without session persistence")
	fs.BoolVar(&opts.resume, "resume", false, "resume the most recent session")
	fs.BoolVar(&opts.cont, "continue", false, "continue the most recent session")
	fs.BoolVar(&opts.fork, "fork", false, "fork a session")
	fs.StringVar(&opts.sessionPath, "session", "", "session id or path to resume")
	fs.StringVar(&opts.systemPrompt, "system-prompt", "", "custom system prompt")
	fs.StringVar(&opts.skills, "skill", "", "additional skill directory")
	fs.StringVar(&opts.prompts, "prompts", "", "additional prompt template directory")
	fs.BoolVar(&opts.approve, "approve", false, "auto-approve project trust")
	fs.BoolVar(&opts.showModels, "list-models", false, "list available models and exit")
	fs.StringVar(&opts.export, "export", "", "export a session to an HTML file (path or session id)")
	fs.StringVar(&opts.importZCode, "import-zcode", "", "import ZCode desktop sessions from a SQLite db (empty value: ~/.zcode/cli/db/db.sqlite)")
	fs.BoolVar(&opts.showModels, "models", false, "list available models and exit")
	fs.BoolVar(&opts.web, "web", false, "start the Web console")
	fs.IntVar(&opts.webPort, "port", 0, "Web console port (default from settings or 8080)")
	fs.StringVar(&opts.webHost, "web-host", "", "Web console bind address (default from settings or 127.0.0.1)")
	fs.StringVar(&opts.webToken, "web-token", "", "fixed Web access token (default from settings or random per start)")
	var cors multiFlag
	fs.Var(&cors, "web-cors", "origin allowed to call the API cross-origin, e.g. https://ui.example.com ('*' for any; repeatable)")
	fs.BoolVar(&opts.noTUI, "no-tui", false, "headless web console (implies --web); logs also print to the console")
	fs.BoolVar(&opts.noWeb, "no-web", false, "disable the Web console even if enabled in settings")
	fs.BoolVar(&opts.mesh, "mesh", false, "start the P2P mesh (join the mesh.json network)")
	fs.StringVar(&opts.logLevel, "log-level", "", "diagnostic log level: debug|info|warn|error|off (default info; settings logLevel)")
	fs.StringVar(&opts.meshToken, "mesh-token", "", "mesh token to join (overrides the mesh.json token)")
	fs.BoolVar(&opts.noMcp, "no-mcp", false, "disable MCP servers even if configured")
	fs.StringVar(&opts.mcpConfig, "mcp-config", "", "MCP config file to use instead of ~/.doge/agent/mcp.json (project files still merge)")
	fs.BoolVar(&opts.mcpTrace, "mcp-trace", false, "trace MCP requests/responses in the diagnostic log")
	versionFlag := fs.Bool("version", false, "print version")
	help := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if *versionFlag {
		fmt.Printf("doge %s\n", versionString())
		os.Exit(0)
	}
	if *help {
		printUsage()
		os.Exit(0)
	}
	if *pFlag && opts.mode == "" {
		opts.mode = "print"
	}
	// 标记 -import-zcode 是否被显式传入（值为空表示用默认库路径）
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "import-zcode" {
			opts.importZCodeSet = true
		}
	})
	opts.webToken = strings.TrimSpace(opts.webToken)
	opts.args = fs.Args()
	opts.message = strings.Join(opts.args, " ")
	return opts
}

func listModels() {
	auth := coding.LoadAuthStore(coding.AuthFile())
	runtime := coding.NewModelRuntime(auth)
	if err := runtime.AddCustomModels(coding.ModelsFile()); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Available models:")
	for _, m := range runtime.Models.AllModels() {
		label := m.Name
		if label == "" {
			label = m.ID
		}
		fmt.Printf("  %s/%s  (%s, %s)\n", m.Provider, m.ID, label, m.API)
	}
}

func isTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// runInteractive 启动 bubbletea 交互模式
func runInteractive(a *app.App, initialMessage string) {
	ui.Run(a.Session, a.MeshView, a.Mcp, version, initialMessage)
}
