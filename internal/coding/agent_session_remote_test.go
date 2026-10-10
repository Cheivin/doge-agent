package coding

import (
	"strings"
	"testing"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
)

// TestForkRemoteToolLevels: the remote task permission levels mount the right
// tool sets — readonly has no bash, write adds write/edit/code but still no
// bash, full mounts everything including bash (acceptance 4).
func TestForkRemoteToolLevels(t *testing.T) {
	base := newToolSession(t, nil, nil)
	target := t.TempDir()

	has := func(names map[string]bool, tool string) bool { return names[tool] }

	for _, level := range []RemoteToolLevel{RemoteReadOnly, RemoteWrite, RemoteFull} {
		fork, err := base.ForkRemote(target, level, nil, nil)
		if err != nil {
			t.Fatalf("ForkRemote(%s): %v", level, err)
		}
		names := map[string]bool{}
		for _, tool := range fork.Agent.State().Tools {
			names[tool.Name] = true
		}
		switch level {
		case RemoteReadOnly:
			for _, want := range []string{"read", "grep", "find", "ls"} {
				if !has(names, want) {
					t.Errorf("readonly fork missing %s: %v", want, names)
				}
			}
			for _, no := range []string{"bash", "write", "edit"} {
				if has(names, no) {
					t.Errorf("readonly fork must not have %s: %v", no, names)
				}
			}
		case RemoteWrite:
			for _, want := range []string{"read", "grep", "find", "ls", "write", "edit"} {
				if !has(names, want) {
					t.Errorf("write fork missing %s: %v", want, names)
				}
			}
			if has(names, "bash") {
				t.Errorf("write fork must not have bash: %v", names)
			}
		case RemoteFull:
			for _, want := range []string{"read", "bash", "write", "edit"} {
				if !has(names, want) {
					t.Errorf("full fork missing %s: %v", want, names)
				}
			}
		}
	}
}

// TestAskAnswerQuestion: AskQuestion surfaces a question to interactive
// consumers and AnswerQuestion delivers the reply out-of-band (acceptance 3,
// human answer channel).
func TestAskAnswerQuestion(t *testing.T) {
	sess := newToolSession(t, nil, nil)

	qCh := make(chan agent.Question, 1)
	sess.Subscribe(func(ev agent.AgentEvent) error {
		if ev.Type == agent.EventQuestion && ev.Question != nil {
			qCh <- *ev.Question
		}
		return nil
	}, ConsumerInteractive)

	q := agent.Question{Prompt: "which port?", Kind: "mesh", From: "peer-b", Task: "t1"}
	answerCh, err := sess.AskQuestion(q)
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	var got agent.Question
	select {
	case got = <-qCh:
	default:
		t.Fatal("question event not fanned out")
	}
	if got.Prompt != q.Prompt || got.Task != "t1" || got.ID == "" {
		t.Fatalf("question event = %+v", got)
	}
	if !sess.AnswerQuestion(got.ID, "use 8080") {
		t.Fatal("AnswerQuestion returned false for a pending question")
	}
	select {
	case ans := <-answerCh:
		if ans != "use 8080" {
			t.Fatalf("answer = %q", ans)
		}
	default:
		t.Fatal("answer not delivered to the channel")
	}
	// A second answer to the same id must be a no-op.
	if sess.AnswerQuestion(got.ID, "again") {
		t.Fatal("AnswerQuestion must reject unknown ids")
	}
}

// TestAskQuestionNoConsumer: without an interactive consumer AskQuestion fails
// (the mesh bridge then falls back to an unattended LLM answer).
func TestAskQuestionNoConsumer(t *testing.T) {
	sess := newToolSession(t, nil, nil)
	if _, err := sess.AskQuestion(agent.Question{Prompt: "hi"}); err == nil {
		t.Fatal("AskQuestion without consumers must fail")
	}
}

// TestAppendExtraToolsSurvivesFork: tools injected late (the mesh remote tools,
// which need the session to exist) must be inherited by forked sessions (P4
// §5.6 "forks keep mesh tools").
func TestAppendExtraToolsSurvivesFork(t *testing.T) {
	base := newToolSession(t, nil, nil)
	base.AppendExtraTools([]*agent.AgentTool{{Name: "remote_agent"}, {Name: "remote_agent_status"}})
	if !toolNames(base)["remote_agent"] {
		t.Fatal("AppendExtraTools did not add remote_agent to the live agent")
	}
	fork, err := base.ForkNew()
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(fork)
	if !names["remote_agent"] || !names["remote_agent_status"] {
		t.Fatalf("fork lost late-injected tools: %v", names)
	}
}

// TestForkRemoteMarksSessionRemote: a remote-task fork session is marked
// Kind=remote (and only that), so the UIs can keep it apart from the user's
// own sessions while keeping it reachable.
func TestForkRemoteMarksSessionRemote(t *testing.T) {
	base := newToolSession(t, nil, nil)
	fork, err := base.ForkRemote(t.TempDir(), RemoteReadOnly, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if k := fork.CurrentSession().Header.Kind; k != SessionKindRemote {
		t.Fatalf("remote fork kind = %q, want %q", k, SessionKindRemote)
	}
	if k := base.CurrentSession().Header.Kind; k != "" {
		t.Fatalf("user session kind = %q, want empty", k)
	}

	// Materialize the fork's session file (first assistant message), then the
	// session list must report it as a remote session.
	fork.Session.AppendMessage(&ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{ai.TextBlock("done")}, Timestamp: time.Now().UnixMilli(),
	})
	summaries, err := base.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range summaries {
		if s.ID == fork.CurrentSession().Header.ID {
			found = true
			if s.Kind != SessionKindRemote {
				t.Fatalf("listed kind = %q, want %q", s.Kind, SessionKindRemote)
			}
		}
	}
	if !found {
		t.Fatal("remote fork session not listed")
	}
}

// TestSystemPromptMeshGuidance: when the mesh remote_agent tool is mounted, the
// system prompt carries explicit guidance to use it (and not to hunt for a CLI),
// so the model dispatches remote tasks via the tool instead of going to bash.
func TestSystemPromptMeshGuidance(t *testing.T) {
	with := buildSystemPromptWithTools("", []*agent.AgentTool{{Name: "remote_agent"}}, t.TempDir(), nil)
	for _, want := range []string{"Mesh collaboration", "remote_agent", "shell or CLI command for dispatching remote tasks"} {
		if !strings.Contains(with, want) {
			t.Fatalf("mesh guidance missing %q:\n%s", want, with)
		}
	}
	without := buildSystemPromptWithTools("", []*agent.AgentTool{{Name: "read"}}, t.TempDir(), nil)
	if strings.Contains(without, "Mesh collaboration") {
		t.Fatal("mesh guidance must not appear without remote_agent")
	}
}

// TestSessionManagerActiveRegistry: a live remote-task fork registers in the
// session manager so the web can attach to it; it is removed when done.
func TestSessionManagerActiveRegistry(t *testing.T) {
	base := newToolSession(t, nil, nil)
	fork, err := base.ForkRemote(t.TempDir(), RemoteReadOnly, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := fork.CurrentSession().FilePath
	fork.SessionManager.RegisterActive(fork)
	if got := fork.SessionManager.ActiveFor(path); got != fork {
		t.Fatal("active instance not found after register")
	}
	fork.SessionManager.UnregisterActive(fork)
	if got := fork.SessionManager.ActiveFor(path); got != nil {
		t.Fatal("active instance still present after unregister")
	}
}

// TestRemoteSessionSourceFields: SourceName/SourcePeerID on a remote fork's
// header flow through the session list.
func TestRemoteSessionSourceFields(t *testing.T) {
	base := newToolSession(t, nil, nil)
	fork, err := base.ForkRemote(t.TempDir(), RemoteReadOnly, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fork.CurrentSession().Header.SourceName = "node-a"
	fork.CurrentSession().Header.SourcePeerID = "peer-a"
	fork.Session.AppendMessage(&ai.Message{
		Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{ai.TextBlock("done")}, Timestamp: time.Now().UnixMilli(),
	})
	summaries, err := base.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range summaries {
		if s.ID == fork.CurrentSession().Header.ID {
			if s.SourceName != "node-a" || s.SourcePeerID != "peer-a" {
				t.Fatalf("source = %q/%q, want node-a/peer-a", s.SourceName, s.SourcePeerID)
			}
			return
		}
	}
	t.Fatal("remote session not listed")
}

// TestForkOfForkKeepsSessionTools: the per-session tool builder must be
// INHERITED by forks, so a fork-of-a-fork (web window doing /new repeatedly)
// still gets its own remote_agent — it must never silently lose it while the
// shared mesh tools (mesh_peers etc.) remain.
func TestForkOfForkKeepsSessionTools(t *testing.T) {
	base := newToolSession(t, nil, nil)
	base.SetExtraToolsBuilder(func(s *AgentSession) []*agent.AgentTool {
		return []*agent.AgentTool{{Name: "remote_agent"}}
	})
	f1, err := base.ForkNew()
	if err != nil {
		t.Fatal(err)
	}
	f2, err := f1.ForkNew()
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range []*AgentSession{f1, f2} {
		names := toolNames(s)
		if !names["remote_agent"] {
			t.Fatalf("fork level %d lost remote_agent: %v", i+1, names)
		}
	}
}
