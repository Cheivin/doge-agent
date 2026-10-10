package app

// Shared mesh wiring, no build tag: it only touches the no-tag internal/mesh
// package (config, token) and calls the per-build newMesh. The libp2p
// implementation lives in mesh_mesh.go (+build mesh); mesh_stub.go
// (+build !mesh) provides the disabled counterparts.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/uploads"
	"doge/internal/mesh"
)

func skillNames(skills []coding.Skill) []string {
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	return names
}

// startMeshNode 加载 mesh 配置、解析 token（meshToken 覆盖 mesh.json）、构建
// 节点并后台启动。未编译 mesh 或无 token 时返回错误。
func startMeshNode(opts Options, skillList []string, model *ai.Model, uploadStore *uploads.Store) (mesh.Mesh, context.CancelFunc, []*agent.AgentTool, error) {
	path := filepath.Join(coding.AgentDir(), "mesh.json")
	cfg, err := mesh.LoadConfig(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mesh config: %v", err)
	}
	var tok *mesh.Token
	if opts.MeshToken != "" {
		tok, err = mesh.DecodeToken(opts.MeshToken)
		if err == nil {
			// Join with the provided token for this run, without persisting it.
			cfg.SetToken(tok)
		}
	} else {
		tok, err = cfg.TokenObj()
	}
	if err != nil {
		// ErrNoToken 自带操作提示，不再包一层
		if errors.Is(err, mesh.ErrNoToken) {
			return nil, nil, nil, err
		}
		return nil, nil, nil, fmt.Errorf("%v (run `doge mesh new` first)", err)
	}
	modelID := ""
	if model != nil {
		modelID = model.Name
		if modelID == "" {
			modelID = model.ID
		}
	}
	m, remoteTools := newMesh(mesh.Options{
		ConfigPath: path,
		Config:     cfg,
		Version:    opts.Version,
		Skills:     skillList,
		Model:      modelID,
		Uploads:    uploadStore,
	})
	if m == nil {
		return nil, nil, nil, errors.New("mesh support not compiled in; build with -tags mesh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := m.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "doge: mesh: %v\n", err)
		}
	}()
	return m, cancel, remoteTools, nil
}

// startMesh Assemble 装配期入口：失败只告警不中断（沿用原行为）
func startMesh(opts Options, skills []coding.Skill, model *ai.Model, uploadStore *uploads.Store) (mesh.Mesh, context.CancelFunc, []*agent.AgentTool) {
	m, cancel, tools, err := startMeshNode(opts, skillNames(skills), model, uploadStore)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return nil, nil, nil
	}
	return m, cancel, tools
}

// closeMesh stops and closes the mesh node. Idempotent.
func closeMesh(m mesh.Mesh, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	if m != nil {
		_ = m.Close()
	}
}

// StartMesh 运行时启动 mesh 节点（桌面端开关；token 空用 mesh.json）。
// 节点工具注入当前会话，MeshView 随之可用。
func (a *App) StartMesh(version, meshToken string) error {
	if a.MeshNode != nil {
		return errors.New("mesh already running")
	}
	names := []string{}
	for _, s := range a.Session.SkillsInfo() {
		names = append(names, s.Name)
	}
	model := a.Session.Agent.State().Model
	node, cancel, tools, err := startMeshNode(Options{Version: version, MeshToken: meshToken}, names, model, a.Uploads)
	if err != nil {
		return err
	}
	a.Session.AppendExtraTools(tools)
	a.MeshNode = node
	a.MeshView = attachMeshCollab(node, a.Session, tools, a.Uploads)
	a.meshCancel = cancel
	return nil
}

// StopMesh 停用 mesh 节点（已注入的 mesh 工具保留到会话重建，调用时报节点不可用）
func (a *App) StopMesh() {
	closeMesh(a.MeshNode, a.meshCancel)
	a.MeshNode = nil
	a.MeshView = nil
	a.meshCancel = nil
}
