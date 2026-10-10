package coding

import (
	"path/filepath"
	"testing"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding/permissions"
)

// newToolSession builds an AgentSession over the mock LLM, with the given tool
// configuration, as a helper for fork-tool tests.
func newToolSession(t *testing.T, extraTools, customTools []*agent.AgentTool) *AgentSession {
	t.Helper()
	dir := t.TempDir()
	manager := NewSessionManager(filepath.Join(dir, "sessions"))
	auth := LoadAuthStore(filepath.Join(dir, "auth.json"))
	runtime := NewModelRuntime(auth)
	runtime.Models.AddProvider(&ai.Provider{ID: "mock", Name: "Mock", RequiresKey: false})
	runtime.Models.AddModel(&ai.Model{ID: "m", API: "openai-completions", Provider: "mock", ContextWindow: 100000})
	model, _, err := runtime.ResolveModel("mock/m")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := LoadSettings(filepath.Join(dir, "s.json"), filepath.Join(dir, "p", ".doge", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewAgentSession(AgentSessionOptions{
		Cwd: dir, Model: model, SessionManager: manager, Settings: settings, ModelRuntime: runtime,
		Tools: customTools, ExtraTools: extraTools,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func toolNames(s *AgentSession) map[string]bool {
	out := map[string]bool{}
	for _, tool := range s.Agent.State().Tools {
		out[tool.Name] = true
	}
	return out
}

// TestForkPreservesExtraTools: a forked session (web window new/switch) must
// inherit the source session's injected tools (mesh_forward), not fall back to
// the default set. Regression for "webui 里问 AI 没有 mesh_forward".
func TestForkPreservesExtraTools(t *testing.T) {
	base := newToolSession(t, []*agent.AgentTool{{Name: "mesh_forward"}}, nil)
	if !toolNames(base)["mesh_forward"] {
		t.Fatal("base session missing mesh_forward (setup bug)")
	}

	fork, err := base.ForkNew()
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(fork)
	if !names["mesh_forward"] {
		t.Fatalf("forked session lost mesh_forward; tools = %v", names)
	}
	// The default tools are rebuilt for the fork's own cwd.
	for _, want := range []string{"read", "bash", "write", "edit", "code"} {
		if !names[want] {
			t.Fatalf("forked session missing default tool %s; tools = %v", want, names)
		}
	}
}

// TestForkPreservesCustomTools: a session with a full Tools override keeps that
// override in its forks too.
func TestForkPreservesCustomTools(t *testing.T) {
	custom := []*agent.AgentTool{{Name: "custom_only"}}
	base := newToolSession(t, nil, custom)
	if len(base.Agent.State().Tools) != 1 || base.Agent.State().Tools[0].Name != "custom_only" {
		t.Fatalf("base custom tools = %v", toolNames(base))
	}
	fork, err := base.ForkNew()
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(fork)
	if !names["custom_only"] {
		t.Fatalf("fork lost custom Tools override; tools = %v", names)
	}
}

type stubAskHandler struct{}

func (stubAskHandler) Ask(_ permissions.AskRequest) (permissions.AskResponse, error) {
	return permissions.AskResponse{Deny: true}, nil
}

// TestRemoteSessionRefusesAskHandler: remote-task forks run unattended. An
// observer opening the fork (e.g. the slave's web console viewing a running
// remote task) must not be able to install an interactive ask handler — a
// bash Ask decision would then block the whole remote run on a prompt modal
// nobody answers (regression: remote task stalled after two tool calls).
func TestRemoteSessionRefusesAskHandler(t *testing.T) {
	sess := newToolSession(t, nil, nil)

	sess.SetPermissionAskHandler(stubAskHandler{})
	if !sess.permissions.AskHandlerInstalled() {
		t.Fatal("a local session must accept the ask handler")
	}
	sess.SetPermissionAskHandler(nil)
	if sess.permissions.AskHandlerInstalled() {
		t.Fatal("clearing the ask handler must work")
	}

	sess.CurrentSession().Header.Kind = SessionKindRemote
	sess.SetPermissionAskHandler(stubAskHandler{})
	if sess.permissions.AskHandlerInstalled() {
		t.Fatal("a remote-task session must refuse interactive ask handlers (unattended)")
	}
}
