// Package app 装配可复用的 agent 运行时：CLI 与桌面客户端共用同一套
// 设置→日志→模型→会话→技能→上传→mesh→MCP 启动序列，入口只做各自的
// 参数解析与运行模式分派。
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/uploads"
	"doge/internal/guide"
	"doge/internal/mcp"
	"doge/internal/mesh"
	"doge/internal/web"
	"doge/pkg/logx"
)

type (
	// Env 启动环境：工作目录与设置，装配前由 Prepare 准备
	Env struct {
		Cwd      string
		Settings *coding.SettingsManager
	}

	// Options 装配选项，语义对齐原 CLI 参数
	Options struct {
		Version      string // 构建版本，mesh/MCP 上报用
		Model        string // provider/model[:thinking]，空则用设置默认
		APIKey       string // API 密钥，覆盖存储/环境变量
		SystemPrompt string // 自定义系统提示词
		SessionID    string // 会话 id 或路径；--continue 时在最近会话里二次筛选
		Continue     bool   // 继续最近会话
		NoSession    bool   // 不持久化会话
		Skills       string // 额外技能目录
		Prompts      string // 额外提示词模板目录
		NoMcp        bool   // 禁用 MCP 服务器
		McpConfig    string // MCP 配置文件路径
		McpTrace     bool   // 追踪 MCP 请求响应
		Mesh         bool   // 启动 P2P mesh（需 -tags mesh 构建）
		MeshToken    string // mesh 网络令牌，覆盖 mesh.json
	}

	// App 装配产物：会话与各运行时组件
	App struct {
		Settings *coding.SettingsManager
		Runtime  *coding.ModelRuntime
		Session  *coding.AgentSession
		MeshNode mesh.Mesh    // nil = 未启用
		MeshView web.MeshView // nil = 未启用
		Mcp      *mcp.Manager
		Uploads  *uploads.Store

		sweepCancel context.CancelFunc
		meshCancel  context.CancelFunc
	}
)

// Prepare 准备 agent 目录与设置（全局 + 项目）
func Prepare(cwd string) (*Env, error) {
	if err := coding.EnsureAgentDir(); err != nil {
		return nil, err
	}
	settings, err := coding.LoadSettings(coding.SettingsFile(), coding.ProjectSettingsFile(cwd))
	if err != nil {
		return nil, err
	}
	// 流式超时检测，进程级生效
	if secs := settings.Settings.StreamIdleSeconds; secs > 0 {
		ai.SetStreamIdleTimeout(time.Duration(secs) * time.Second)
	}
	return &Env{Cwd: cwd, Settings: settings}, nil
}

// SetupLog 初始化诊断日志；logLevel 为空时用设置里的 logLevel
func SetupLog(e *Env, version, logLevel string, consoleLog bool) error {
	if logLevel == "" {
		logLevel = e.Settings.Settings.LogLevel
	}
	lvl, err := logx.ParseLevel(logLevel)
	if err != nil {
		return err
	}
	return logx.Setup(logx.Options{
		Path:   coding.LogFile(),
		Level:  lvl,
		Stderr: consoleLog,
		Banner: fmt.Sprintf("doge %s pid %d level %s", version, os.Getpid(), lvl),
	})
}

// Assemble 装配模型运行时、会话、技能、上传暂存区、mesh 与 MCP
func Assemble(e *Env, opts Options) (*App, error) {
	// 模型运行时（认证 + 目录）
	auth := coding.LoadAuthStore(coding.AuthFile())
	runtime := coding.NewModelRuntime(auth)
	if err := runtime.AddCustomModels(coding.ModelsFile()); err != nil {
		return nil, err
	}

	// 解析模型
	var model *ai.Model
	var thinking string
	var err error
	switch {
	case opts.Model != "":
		model, thinking, err = runtime.ResolveModel(opts.Model)
	case e.Settings.Settings.DefaultProvider != "" && e.Settings.Settings.DefaultModel != "":
		model, thinking, err = runtime.ResolveModel(e.Settings.Settings.DefaultProvider + "/" + e.Settings.Settings.DefaultModel)
	default:
		model, thinking, err = runtime.DefaultModel()
	}
	if err != nil {
		return nil, err
	}
	if thinking == "" {
		thinking = e.Settings.Settings.DefaultThinkingLevel
	}

	// 会话选择
	manager := coding.NewSessionManager(coding.SessionsDir())
	var session *coding.Session
	switch {
	case opts.NoSession:
		session, _ = manager.CreateSession(e.Cwd)
	case opts.Continue:
		summaries, listErr := manager.ListSessions()
		if listErr != nil || len(summaries) == 0 {
			return nil, errors.New("no previous sessions found")
		}
		target := summaries[0]
		if opts.SessionID != "" {
			for _, s := range summaries {
				if s.ID == opts.SessionID || s.FilePath == opts.SessionID {
					target = s
					break
				}
			}
		}
		session, err = manager.LoadSession(target.FilePath)
		if err != nil {
			return nil, err
		}
	case opts.SessionID != "":
		if filepath.IsAbs(opts.SessionID) {
			session, err = manager.LoadSession(opts.SessionID)
		} else {
			session, err = manager.LoadSession(filepath.Join(coding.SessionsDir(), opts.SessionID))
		}
		if err != nil {
			return nil, err
		}
	default:
		session, _ = manager.CreateSession(e.Cwd)
	}

	// 技能和提示词模板
	skillDirs := coding.SkillsDirs(e.Cwd)
	if opts.Skills != "" {
		skillDirs = []string{opts.Skills}
	}
	skills := coding.LoadSkills(skillDirs...)
	templates := coding.LoadPromptTemplates(coding.PromptTemplateDirs(e.Cwd)...)
	if opts.Prompts != "" {
		templates = coding.LoadPromptTemplates(opts.Prompts)
	}

	// 上传暂存区：web 与 mesh 共用（settings.uploads 节配置，未配置取默认值）。
	// 周期清理挂在这里而不是 web server：TUI-only / mesh-only 同样生效
	uploadStore := uploads.NewStore(coding.UploadsDir(), e.Settings.UploadsConfig())
	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	uploadStore.StartSweep(sweepCtx)

	// Mesh：会话前构建节点（后台），以便注入工具
	var meshNode mesh.Mesh
	var meshCancel context.CancelFunc
	var meshTools []*agent.AgentTool
	if opts.Mesh || opts.MeshToken != "" {
		meshNode, meshCancel, meshTools = startMesh(opts, skills, model, uploadStore)
	}

	sess, err := coding.NewAgentSession(coding.AgentSessionOptions{
		Cwd:             e.Cwd,
		Model:           model,
		ThinkingLevel:   thinking,
		CustomPrompt:    opts.SystemPrompt,
		APIKey:          opts.APIKey,
		SessionManager:  manager,
		Session:         session,
		Settings:        e.Settings,
		ModelRuntime:    runtime,
		Skills:          skills,
		SkillsDirs:      skillDirs,
		PromptTemplates: templates,
		// 内置 guide 工具：用法/配置/功能问题按模块查询内置指南（恒可用，
		// 独立部署无仓库文档也能答）；mesh 工具随构建与会话注入
		ExtraTools: append([]*agent.AgentTool{guide.Tool()}, meshTools...),
	})
	if err != nil {
		closeMesh(meshNode, meshCancel)
		sweepCancel()
		return nil, err
	}

	// P3/P4：挂载 agent 协作层，注入远程工具，暴露 mesh 视图
	var meshView web.MeshView
	if meshNode != nil {
		meshView = attachMeshCollab(meshNode, sess, meshTools, uploadStore)
	}

	// MCP：发现配置，构建代理+直接工具，挂载到会话
	mcpMgr := mcp.New(mcp.Options{
		AgentDir:   coding.AgentDir(),
		Cwd:        e.Cwd,
		ConfigPath: opts.McpConfig,
		NoMcp:      opts.NoMcp,
		Trace:      opts.McpTrace,
		Version:    opts.Version,
	})
	if err := mcpMgr.Discover(); err != nil {
		fmt.Fprintf(os.Stderr, "doge: warning: mcp config: %v\n", err)
	}
	sess.SetMcpTools(mcpMgr.BuildTools())

	return &App{
		Settings:    e.Settings,
		Runtime:     runtime,
		Session:     sess,
		MeshNode:    meshNode,
		MeshView:    meshView,
		Mcp:         mcpMgr,
		Uploads:     uploadStore,
		sweepCancel: sweepCancel,
		meshCancel:  meshCancel,
	}, nil
}

// Close 收尾：停 mesh、关 MCP、停上传清理，幂等
func (a *App) Close() {
	closeMesh(a.MeshNode, a.meshCancel)
	a.meshCancel = nil
	a.MeshNode = nil
	if a.sweepCancel != nil {
		a.sweepCancel()
		a.sweepCancel = nil
	}
	if a.Mcp != nil {
		a.Mcp.Close()
		a.Mcp = nil
	}
}
