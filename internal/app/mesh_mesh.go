//go:build mesh

package app

import (
	"fmt"
	"os"

	"doge/internal/agent"
	"doge/internal/coding"
	"doge/internal/coding/uploads"
	"doge/internal/mesh"
	"doge/internal/mesh/libp2p"
	"doge/internal/web"
)

// newMesh builds the libp2p-backed mesh node and the mesh tools injected into
// the agent session (mesh_forward). Returns a nil Mesh on failure.
func newMesh(opts mesh.Options) (mesh.Mesh, []*agent.AgentTool) {
	m, err := libp2p.New(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: mesh: %v\n", err)
		return nil, nil
	}
	return m, []*agent.AgentTool{libp2p.ForwardTool(m)}
}

// attachMeshCollab wires the P3/P4 agent collaboration layer onto the mesh
// node: the slave task handler and the master bridge share one envelope
// subscription, and the remote_agent / remote_agent_status / mesh_peers /
// mesh_file tools are injected into the session (so current and forked
// sessions can dispatch remote tasks and transfer files). The bridge is only
// constructed after the session exists, which is why the tools are attached
// here rather than in newMesh. meshTools (from newMesh, already in the session
// as extra tools) also ride along on full-level remote-task forks together
// with mesh_peers, so a dispatched task can configure this node's own
// forwarding. It returns a MeshView for the web console / TUI mesh panel
// (nil when no bridge).
func attachMeshCollab(m mesh.Mesh, sess *coding.AgentSession, meshTools []*agent.AgentTool, uploadStore *uploads.Store) web.MeshView {
	if m == nil || sess == nil {
		return nil
	}
	// Tools a full-level remote-task fork carries: the node's mesh tools
	// (mesh_forward) plus peers discovery and file staging (a task stages its
	// output files so the master can pull them). remote_agent_status needs the
	// bridge (created below), so it stays on local sessions only.
	slaveTools := append(append([]*agent.AgentTool{}, meshTools...), libp2p.PeersTool(m))
	if uploadStore != nil {
		slaveTools = append(slaveTools, libp2p.MeshFileTool(m))
	}
	collab, err := libp2p.NewCollab(m, libp2p.CollabOptions{Session: sess, MeshTools: slaveTools})
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: mesh collab: %v\n", err)
		return nil
	}
	// profile 上报会话当前工具集：远端任务 fork 携带同一工具集，peer 据此
	// 知道本节点能干什么。回调取实时状态（含 MCP/extra tools），每 30 秒随
	// profile 重广播刷新。
	if tn, ok := m.(interface{ SetToolNames(func() []string) }); ok {
		tn.SetToolNames(func() []string {
			names := make([]string, 0, 16)
			for _, tl := range sess.Agent.State().Tools {
				names = append(names, tl.Name)
			}
			return names
		})
	}
	if bridge := collab.Bridge(); bridge != nil {
		// Session-agnostic mesh tools: shared across the current session and
		// every fork (inherited via extraTools). meshTools themselves are
		// already on the session (main passes them as ExtraTools).
		localTools := []*agent.AgentTool{
			libp2p.PeersTool(m),
			libp2p.RemoteAgentStatusTool(bridge),
			// remote_tool calls the PEER's node-level tools directly (no remote
			// agent run) — the master-side half of the MsgToolCall protocol.
			libp2p.RemoteTool(m, bridge),
		}
		if uploadStore != nil {
			localTools = append(localTools, libp2p.MeshFileTool(m))
		}
		sess.AppendExtraTools(localTools)
		// remote_agent is PER-SESSION: each session (and every web fork) gets
		// its own instance bound to itself, so async results are injected back
		// into the session that dispatched the task, never the shared one.
		sess.SetExtraToolsBuilder(func(fork *coding.AgentSession) []*agent.AgentTool {
			return []*agent.AgentTool{libp2p.RemoteAgentTool(m, bridge, fork)}
		})
		return &meshViewAdapter{m: m, bridge: bridge}
	}
	return nil
}

// meshViewAdapter adapts the mesh node + bridge to the web console's MeshView
// (this node, connected peers, in-flight remote tasks, mesh.json state and
// forward rules).
type meshViewAdapter struct {
	m      mesh.Mesh
	bridge *libp2p.Bridge
}

func (a *meshViewAdapter) Self() mesh.AgentProfile  { return a.m.Self() }
func (a *meshViewAdapter) Peers() []mesh.PeerInfo   { return a.m.Peers() }
func (a *meshViewAdapter) Tasks() []mesh.TaskStatus { return a.bridge.ListInflight() }

// Addrs/Config reach for optional capabilities: the concrete libp2p node has
// them, and any other Mesh implementation (test doubles) degrades to empty.
func (a *meshViewAdapter) Addrs() []string {
	if n, ok := a.m.(interface{ Addrs() []string }); ok {
		return n.Addrs()
	}
	return nil
}

func (a *meshViewAdapter) Config() mesh.Config {
	if n, ok := a.m.(interface{ Config() mesh.Config }); ok {
		return n.Config()
	}
	return mesh.Config{}
}

func (a *meshViewAdapter) ForwardList() []mesh.ForwardRule { return a.m.ForwardList() }
func (a *meshViewAdapter) ForwardAdd(rule mesh.ForwardRule) error {
	return a.m.ForwardAdd(rule)
}
func (a *meshViewAdapter) ForwardRemove(name string) error { return a.m.ForwardRemove(name) }
func (a *meshViewAdapter) ForwardSetEnabled(name string, enabled bool) error {
	return a.m.ForwardSetEnabled(name, enabled)
}
