package services

import (
	"path/filepath"

	"doge/internal/coding"
	"doge/internal/mesh"
	"doge/internal/web"
)

type (
	// MeshService mesh 网络视图、运行时开关与权限配置（对应 /api/mesh*）
	MeshService struct {
		d Deps
	}
)

// view 取 mesh 视图（运行时启停后取 App 上的当前值），未启用返回 unavailable
func (s *MeshService) view() (web.MeshView, error) {
	v := s.d.Mesh
	if s.d.App != nil {
		v = s.d.App.MeshView
	}
	if v == nil {
		return nil, errUnavailable("mesh not enabled (build with -tags mesh)")
	}
	return v, nil
}

// meshConfigPath mesh.json 路径
func meshConfigPath() string {
	return filepath.Join(coding.AgentDir(), "mesh.json")
}

// Running mesh 运行状态
func (s *MeshService) Running() map[string]any {
	running := s.d.App != nil && s.d.App.MeshNode != nil
	return map[string]any{"running": running}
}

// Start 运行时启动 mesh 节点（mesh.json 里须有 token，-tags mesh 构建）
func (s *MeshService) Start() error {
	if s.d.App == nil {
		return errUnavailable("mesh not available")
	}
	if s.d.App.MeshNode != nil {
		return errBusy("mesh already running")
	}
	if err := s.d.App.StartMesh(s.d.Version, ""); err != nil {
		return errInvalid(err.Error())
	}
	return nil
}

// Stop 停用 mesh 节点
func (s *MeshService) Stop() error {
	if s.d.App == nil || s.d.App.MeshNode == nil {
		return errNotFound("mesh not running")
	}
	s.d.App.StopMesh()
	return nil
}

// ConfigGet 读 mesh.json（含 token，本地配置面板自用）
func (s *MeshService) ConfigGet() (map[string]any, error) {
	cfg, err := mesh.LoadConfig(meshConfigPath())
	if err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{"config": cfg, "path": meshConfigPath()}, nil
}

// ConfigPut 写 mesh.json 并热应用（运行中则重启节点）；token 留空保留现值
func (s *MeshService) ConfigPut(cfg mesh.Config) map[string]any {
	path := meshConfigPath()
	cur, err := mesh.LoadConfig(path)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	if cfg.Token == "" {
		cfg.Token = cur.Token
	}
	if err := mesh.SaveConfig(path, &cfg); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	restarted := false
	if s.d.App != nil && s.d.App.MeshNode != nil {
		s.d.App.StopMesh()
		if err := s.d.App.StartMesh(s.d.Version, ""); err != nil {
			return map[string]any{"ok": true, "restarted": false, "error": "配置已保存，节点重启失败: " + err.Error()}
		}
		restarted = true
	}
	return map[string]any{"ok": true, "restarted": restarted}
}

// Info 节点自身、地址、配置、peers、任务与转发规则
func (s *MeshService) Info() (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"self":     v.Self(),
		"addrs":    v.Addrs(),
		"config":   v.Config(),
		"peers":    v.Peers(),
		"tasks":    v.Tasks(),
		"forwards": v.ForwardList(),
	}, nil
}

// Forwards 转发规则列表
func (s *MeshService) Forwards() (map[string]any, error) {
	v, err := s.view()
	if err != nil {
		return nil, err
	}
	return map[string]any{"forwards": v.ForwardList()}, nil
}

// ForwardAdd 新增转发规则（校验 + 持久化 + 热应用）
func (s *MeshService) ForwardAdd(rule mesh.ForwardRule) error {
	v, err := s.view()
	if err != nil {
		return err
	}
	if err := v.ForwardAdd(rule); err != nil {
		return errInvalid(err.Error())
	}
	return nil
}

// ForwardRemove 删除转发规则
func (s *MeshService) ForwardRemove(name string) error {
	v, err := s.view()
	if err != nil {
		return err
	}
	if err := v.ForwardRemove(name); err != nil {
		return errNotFound(err.Error())
	}
	return nil
}

// ForwardSetEnabled 启停单条转发规则
func (s *MeshService) ForwardSetEnabled(name string, on bool) error {
	v, err := s.view()
	if err != nil {
		return err
	}
	if err := v.ForwardSetEnabled(name, on); err != nil {
		return errNotFound(err.Error())
	}
	return nil
}
