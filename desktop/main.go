// doge 桌面客户端：wails v3 原生外壳 + typed Service + 事件桥。
// 无 HTTP 监听、无 token、无登录：前端由资产协议托管，数据走绑定与 wails
// Events；UI 独立于 Web 控制台（webui 与其服务端零改动）。
package main

import (
	"embed"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"doge/desktop/services"
	"doge/internal/ai"
	"doge/internal/app"
	"doge/internal/coding"
	"doge/internal/coding/uploads"
	"doge/internal/web"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed frontend
var frontendFS embed.FS

//go:embed assets
var assetsFS embed.FS

// version 和 gitCommit 由 Makefile 在构建时通过 -ldflags 注入
var (
	version   = "0.11.0"
	gitCommit = ""
)

// versionString 返回版本字符串，构建时有 git commit 则附加显示
func versionString() string {
	if gitCommit == "" {
		return version
	}
	return version + " (" + gitCommit + ")"
}

func main() {
	// 注入构建版本到 User-Agent
	ai.Version = version

	var (
		meshFlag  bool
		meshToken string
		showVer   bool
	)
	fs := flag.NewFlagSet("doge-desktop", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.BoolVar(&meshFlag, "mesh", false, "start the P2P mesh (join the mesh.json network)")
	fs.StringVar(&meshToken, "mesh-token", "", "mesh token to join (overrides the mesh.json token)")
	fs.BoolVar(&showVer, "version", false, "print version")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}
	if showVer {
		fmt.Printf("doge-desktop %s\n", versionString())
		os.Exit(0)
	}

	// agent 目录与设置（全局 + 项目）；初始目录取最近会话
	env, err := app.Prepare(lastSessionCwd())
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		os.Exit(1)
	}
	// 诊断日志照常落盘；GUI 无 stderr 依赖，仅 DOGE_DEBUG 时输出
	if err := app.SetupLog(env, versionString(), "", os.Getenv("DOGE_DEBUG") != ""); err != nil {
		fmt.Fprintf(os.Stderr, "doge: log setup failed: %v\n", err)
	}

	// 装配：模型运行时、会话、技能、上传暂存区、mesh、MCP
	a, err := app.Assemble(env, app.Options{
		Version:   versionString(),
		Mesh:      meshFlag,
		MeshToken: meshToken,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		os.Exit(1)
	}
	defer a.Close()

	gui := application.New(application.Options{
		Name: "doge",
		Mac: application.MacOptions{
			// 关窗不退出：窗口销毁后驻留托盘，从托盘/菜单重建
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		Assets: application.AssetOptions{Handler: assetsHandler(a.Uploads)},
	})

	// 事件桥 + Service 层（emit 走 wails Events，前端 Events.On("console") 收帧）
	backend := web.NewBackend(a.Session, version, false)
	console := services.NewConsole(backend, func(frame any) {
		gui.Event.Emit("console", frame)
	})
	defer console.Start()()
	svcs := services.New(services.Deps{
		Backend: backend,
		Session: a.Session,
		Mcp:     a.Mcp,
		Mesh:    a.MeshView,
		Uploads: a.Uploads,
		Console: console,
		App:     a,
		Version: versionString(),
	})
	svcOpt := application.ServiceOptions{MarshalError: services.MarshalError}
	gui.RegisterService(application.NewServiceWithOptions(svcs.Session, svcOpt))
	gui.RegisterService(application.NewServiceWithOptions(svcs.Config, svcOpt))
	gui.RegisterService(application.NewServiceWithOptions(svcs.Mcp, svcOpt))
	gui.RegisterService(application.NewServiceWithOptions(svcs.Skills, svcOpt))
	gui.RegisterService(application.NewServiceWithOptions(svcs.Mesh, svcOpt))
	gui.RegisterService(application.NewServiceWithOptions(svcs.Uploads, svcOpt))

	// 前端启动诊断通道：JS 报错经 Events 回传落日志（排查黑屏类问题）
	gui.Event.On("js:err", func(e *application.CustomEvent) {
		fmt.Fprintf(os.Stderr, "[js:err] %v\n", e.Data)
	})

	gui.Window.NewWithOptions(mainWindow())
	buildMenu(gui)
	buildTray(gui)

	if err := gui.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		os.Exit(1)
	}
}

// mainWindow 主窗口选项（关窗销毁，托盘重建）
func mainWindow() application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Name:      "main",
		Title:     "doge",
		Width:     1280,
		Height:    832,
		MinWidth:  960,
		MinHeight: 600,
		URL:       "/",
	}
}

// assetsHandler 资产服务：静态前端 + 附件内容直出（转录图片直显与下载走
// 同一条 wails 资产通道，前端 <img>/<a> 无需改造）
func assetsHandler(up *uploads.Store) http.Handler {
	sub, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		panic(err)
	}
	static := application.BundledAssetFileServer(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if up != nil && strings.HasPrefix(r.URL.Path, "/api/uploads/") && strings.HasSuffix(r.URL.Path, "/content") {
			rest := strings.TrimPrefix(r.URL.Path, "/api/uploads/")
			serveUploadContent(up, strings.TrimSuffix(rest, "/content"), w, r)
			return
		}
		static.ServeHTTP(w, r)
	})
}

// serveUploadContent 直出附件字节；非 svg 图片内联展示，其余按附件下载
// （存储型 XSS 防护与 Web 控制台同策略）
func serveUploadContent(up *uploads.Store, id string, w http.ResponseWriter, r *http.Request) {
	meta, f, err := up.Get(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", meta.MIME)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	disp := "attachment"
	if strings.HasPrefix(meta.MIME, "image/") && meta.MIME != "image/svg+xml" {
		disp = "inline"
	}
	w.Header().Set("Content-Disposition", disp+"; filename*=UTF-8''"+meta.Name)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// showWindow 显示主窗口；已销毁则重建
func showWindow(gui *application.App) {
	if w, ok := gui.Window.GetByName("main"); ok {
		w.Show()
		if f, ok := w.(interface{ Focus() }); ok {
			f.Focus()
		}
		return
	}
	gui.Window.NewWithOptions(mainWindow()).Show()
}

// execJS 在主窗口执行 JS（菜单动作经前端暴露的 window.dogeMenu 钩子转接）
func execJS(gui *application.App, code string) {
	if w, ok := gui.Window.GetByName("main"); ok {
		w.ExecJS(code)
	}
}

// buildMenu 菜单栏：会话/设置动作走前端钩子，其余用系统 role
func buildMenu(gui *application.App) {
	menu := application.NewMenu()

	appMenu := menu.AddSubmenu("doge")
	appMenu.AddRole(application.About)
	appMenu.AddSeparator()
	appMenu.AddRole(application.Hide)
	appMenu.AddRole(application.HideOthers)
	appMenu.AddRole(application.ShowAll)
	appMenu.AddSeparator()
	appMenu.AddRole(application.Quit)

	sess := menu.AddSubmenu("会话")
	newItem := sess.Add("新建会话")
	newItem.SetAccelerator("cmd+n")
	newItem.OnClick(func(*application.Context) { execJS(gui, "window.dogeMenu.newTask()") })
	abortItem := sess.Add("中断运行")
	abortItem.SetAccelerator("cmd+.")
	abortItem.OnClick(func(*application.Context) { execJS(gui, "window.dogeMenu.abort()") })

	set := menu.AddSubmenu("设置")
	prefItem := set.Add("偏好设置")
	prefItem.SetAccelerator("cmd+,")
	prefItem.OnClick(func(*application.Context) { execJS(gui, "window.dogeMenu.openSettings()") })
	reloadItem := set.Add("重载配置")
	reloadItem.SetAccelerator("cmd+r")
	reloadItem.OnClick(func(*application.Context) { execJS(gui, "window.dogeMenu.reloadConfig()") })

	edit := menu.AddSubmenu("编辑")
	edit.AddRole(application.Undo)
	edit.AddRole(application.Redo)
	edit.AddSeparator()
	edit.AddRole(application.Cut)
	edit.AddRole(application.Copy)
	edit.AddRole(application.Paste)
	edit.AddRole(application.SelectAll)

	win := menu.AddSubmenu("窗口")
	win.AddRole(application.Minimise)
	win.AddRole(application.Zoom)
	win.AddRole(application.CloseWindow)

	gui.Menu.SetApplicationMenu(menu)
}

// buildTray 系统托盘：点击显隐主窗口，菜单提供新建会话/退出
func buildTray(gui *application.App) {
	icon, err := assetsFS.ReadFile("assets/tray.png")
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: tray icon: %v\n", err)
		return
	}
	tray := gui.SystemTray.New()
	tray.SetTemplateIcon(icon).SetTooltip("doge")

	menu := application.NewMenu()
	showItem := menu.Add("显示主窗口")
	showItem.OnClick(func(*application.Context) { showWindow(gui) })
	newItem := menu.Add("新建会话")
	newItem.OnClick(func(*application.Context) {
		showWindow(gui)
		execJS(gui, "window.dogeMenu.newTask()")
	})
	menu.AddSeparator()
	quitItem := menu.Add("退出 doge")
	quitItem.OnClick(func(*application.Context) { gui.Quit() })
	tray.SetMenu(menu)
	tray.OnClick(func() { showWindow(gui) })
}

// lastSessionCwd 返回最近一个会话的工作目录（目录须仍存在），没有历史则回退 $HOME
func lastSessionCwd() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	mgr := coding.NewSessionManager(coding.SessionsDir())
	summaries, err := mgr.ListSessions()
	if err != nil || len(summaries) == 0 {
		return home
	}
	if c := summaries[0].Cwd; c != "" {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return home
}
