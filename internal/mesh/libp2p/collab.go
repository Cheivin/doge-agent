//go:build mesh

package libp2p

import (
	"errors"
	"time"

	"doge/internal/agent"
	"doge/internal/coding"
	"doge/internal/mesh"
)

// CollabOptions configures the agent collaboration layer (P3+P4).
type CollabOptions struct {
	// Session is the local agent session. The slave handler forks one
	// isolated session per remote task from it; the master bridge uses it for
	// context snapshots, question routing (InteractiveCount + answer channel)
	// and async result injection. A node with no session (a pure mesh daemon)
	// simply has no collaboration roles.
	Session *coding.AgentSession

	// MeshTools are the node-level mesh tools (mesh_forward / mesh_peers /
	// remote_agent_status). A remote task forked at the "full" permission
	// level carries them too, so a dispatched task can configure THIS node's
	// forwarding or inspect the mesh (e.g. the master asks the slave to
	// expose a port itself). Read-only/write tasks never get them: mesh
	// forwarding mutates node config, which those levels must not allow.
	MeshTools []*agent.AgentTool

	// QuestionTimeout bounds how long a remote question may wait for a human
	// or LLM answer before the master replies MsgError(timeout) (default 5m).
	QuestionTimeout time.Duration

	// LLMAnswerModel overrides the model used for unattended LLM self-answers
	// to remote questions (empty uses the session model).
	LLMAnswerModel string
}

// Collab wires the P3 slave handler and the P4 master bridge onto one mesh
// node, so any doge instance can both accept and dispatch remote tasks. It
// owns a single Envelope subscription that dispatches to both roles.
type Collab struct {
	node    *meshNode
	handler *agentHandler
	bridge  *Bridge
	tools   *remoteToolHandler
	unsub   func()
}

// NewCollab builds the collaboration layer for the mesh node m. It returns an
// error when m is not a libp2p node (the only Mesh implementation that can
// host remote task sessions).
func NewCollab(m mesh.Mesh, opts CollabOptions) (*Collab, error) {
	node, ok := m.(*meshNode)
	if !ok {
		return nil, errors.New("mesh: agent collaboration requires the libp2p node")
	}
	c := &Collab{node: node}
	if opts.Session != nil {
		c.handler = newAgentHandler(node, opts.Session, opts.QuestionTimeout, opts.MeshTools)
		c.bridge = NewBridge(m, opts.Session, opts.QuestionTimeout)
		c.bridge.SetLLMAnswerModel(opts.LLMAnswerModel)
	}
	// Direct tool calls are independent of the agent session: a headless
	// daemon (mesh daemon) accepts them too when MeshTools are provided.
	if len(opts.MeshTools) > 0 {
		c.tools = newRemoteToolHandler(node, opts.MeshTools)
	}
	c.unsub = m.Subscribe(func(env mesh.Envelope) error {
		if c.handler != nil {
			_ = c.handler.Handle(env)
		}
		if c.bridge != nil {
			_ = c.bridge.Handle(env)
		}
		if c.tools != nil {
			_ = c.tools.Handle(env)
		}
		return nil
	})
	return c, nil
}

// Bridge returns the master-side bridge (nil on a session-less node).
func (c *Collab) Bridge() *Bridge { return c.bridge }

// Close unsubscribes the collab from the envelope stream.
func (c *Collab) Close() {
	if c.unsub != nil {
		c.unsub()
		c.unsub = nil
	}
}
