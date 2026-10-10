//go:build mesh

package libp2p

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/tools"
	"doge/internal/mesh"
)

// ---------------------------------------------------------------------------
// Mock LLM: one scripted EventStream per API name. Each node's session uses a
// distinct API ("mesh-mock-a" for the master, "mesh-mock-b" for the slave), so
// the two nodes' model behavior is independent.
// ---------------------------------------------------------------------------

type mockLLM struct {
	mu      sync.Mutex
	scripts []func(call int, ctx *ai.Context) *ai.Message
	calls   int
}

var (
	mockRegMu  sync.Mutex
	mockStream = map[string]*mockLLM{}
)

// mockAPI registers (once) and scripts a mock stream API. A nil script result
// makes the stream block until the request context is canceled, then abort —
// used to test interrupt/timeout.
func mockAPI(t *testing.T, api string, fns ...func(call int, ctx *ai.Context) *ai.Message) {
	t.Helper()
	mockRegMu.Lock()
	defer mockRegMu.Unlock()
	m := mockStream[api]
	if m == nil {
		m = &mockLLM{}
		mockStream[api] = m
		ai.RegisterAPIStream(api, func(model *ai.Model, ctx *ai.Context, opts *ai.StreamOptions) *ai.EventStream {
			return m.stream(model, ctx, opts)
		})
	}
	m.mu.Lock()
	m.scripts = fns
	m.calls = 0
	m.mu.Unlock()
}

func (m *mockLLM) stream(model *ai.Model, ctx *ai.Context, opts *ai.StreamOptions) *ai.EventStream {
	m.mu.Lock()
	call := m.calls
	m.calls++
	scripts := m.scripts
	m.mu.Unlock()

	var final *ai.Message
	if call < len(scripts) {
		final = scripts[call](call, ctx)
	}
	s := ai.NewEventStream(16)
	if final == nil {
		// Block until the run is aborted.
		go func() {
			<-opts.Context.Done()
			m := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonAborted, ErrorMessage: "aborted by context", Content: []ai.ContentBlock{}}
			s.Push(ai.ErrorEvent(m))
			s.End(m)
		}()
		return s
	}
	go pushMessage(s, model, final)
	return s
}

func pushMessage(s *ai.EventStream, model *ai.Model, final *ai.Message) {
	partial := ai.NewAssistantMessage(model.API, model.Provider, model.ID)
	partial.Content = append([]ai.ContentBlock{}, final.Content...)
	partial.StopReason = final.StopReason
	s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: partial})
	for i := range final.Content {
		switch final.Content[i].Type {
		case ai.ContentTypeText:
			s.Push(ai.AssistantMessageEvent{Type: ai.EventTextStart, Partial: partial, ContentIndex: i})
			s.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, Partial: partial, ContentIndex: i, Delta: final.Content[i].Text})
			s.Push(ai.AssistantMessageEvent{Type: ai.EventTextEnd, Partial: partial, ContentIndex: i})
		case ai.ContentTypeToolCall:
			s.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallStart, Partial: partial, ContentIndex: i})
			call := final.Content[i]
			s.Push(ai.AssistantMessageEvent{Type: ai.EventToolCallEnd, Partial: partial, ContentIndex: i, ToolCall: &call})
		}
	}
	s.Push(ai.DoneEvent(final))
	s.End(final)
}

func textStop(text string) *ai.Message {
	return &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.ContentBlock{ai.TextBlock(text)}, Timestamp: time.Now().UnixMilli()}
}

// parseTaskID extracts the task id from an async dispatch ack like
// "... task=<id> You will be notified ...".
func parseTaskID(ack string) string {
	if idx := strings.Index(ack, "task="); idx >= 0 {
		rest := strings.TrimSpace(ack[idx+len("task="):])
		if sp := strings.IndexAny(rest, " \n\t"); sp >= 0 {
			return rest[:sp]
		}
		return rest
	}
	return ""
}

func toolCallStop(id, name string, args map[string]any) *ai.Message {
	return &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonToolUse,
		Content: []ai.ContentBlock{ai.ToolCallBlock(id, name, args)}, Timestamp: time.Now().UnixMilli()}
}

// newMockSession builds an AgentSession whose LLM is the named mock API.
func newMockSession(t *testing.T, api string) *coding.AgentSession {
	t.Helper()
	dir := t.TempDir()
	manager := coding.NewSessionManager(filepath.Join(dir, "sessions"))
	auth := coding.LoadAuthStore(filepath.Join(dir, "auth.json"))
	runtime := coding.NewModelRuntime(auth)
	runtime.Models.AddProvider(&ai.Provider{ID: api, Name: api, RequiresKey: false})
	runtime.Models.AddModel(&ai.Model{ID: "m", API: api, Provider: api, ContextWindow: 100000})
	model, _, err := runtime.ResolveModel(api + "/m")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := coding.LoadSettings(filepath.Join(dir, "s.json"), filepath.Join(dir, "p", ".doge", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := coding.NewAgentSession(coding.AgentSessionOptions{
		Cwd: dir, Model: model, SessionManager: manager, Settings: settings, ModelRuntime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// collabPair wires a master (a) and a slave (b) node with real mesh transport,
// an in-process handler on b and an in-process bridge on a.
type collabPair struct {
	a       *meshNode
	b       *meshNode
	handler *agentHandler
	bridge  *Bridge
	aSess   *coding.AgentSession
	bSess   *coding.AgentSession
	bCwd    string
}

func newCollabPair(t *testing.T, bLevel string) *collabPair {
	t.Helper()
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	connectAndWait(t, a, b)

	bCwd := t.TempDir()
	b.cfg.AllowRemoteTasks = true
	if bLevel != "" {
		b.cfg.RemoteTaskLevel = bLevel
	}
	b.cfg.Cwds = map[string]string{"work": bCwd}
	b.cfg.MaxConcurrentTasks = 2

	aSess := newMockSession(t, "mesh-mock-a")
	bSess := newMockSession(t, "mesh-mock-b")

	bridge := NewBridge(a, aSess, 0)
	handler := newAgentHandler(b, bSess, 0, nil)
	a.Subscribe(bridge.Handle)
	b.Subscribe(handler.Handle)

	return &collabPair{a: a, b: b, handler: handler, bridge: bridge, aSess: aSess, bSess: bSess, bCwd: bCwd}
}

// waitResult subscribes a result/error channel for a task id on the master.
func waitResult(t *testing.T, p *collabPair, taskID string, timeout time.Duration) mesh.ResultPayload {
	t.Helper()
	ch := make(chan mesh.ResultPayload, 1)
	unsub := p.a.Subscribe(func(env mesh.Envelope) error {
		switch env.Type {
		case mesh.MsgResult:
			if env.ReplyTo == taskID {
				var res mesh.ResultPayload
				if json.Unmarshal(env.Payload, &res) == nil {
					select {
					case ch <- res:
					default:
					}
				}
			}
		case mesh.MsgError:
			if env.ReplyTo == taskID {
				var ep mesh.ErrorPayload
				if json.Unmarshal(env.Payload, &ep) == nil {
					select {
					case ch <- mesh.ResultPayload{Status: "error", Error: ep.Code + ": " + ep.Message}:
					default:
					}
				}
			}
		}
		return nil
	})
	defer unsub()
	select {
	case res := <-ch:
		return res
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for result of %s", taskID)
		return mesh.ResultPayload{}
	}
}

// TestSlaveRunsTaskFresh: A dispatches a fresh-context task; B forks a session,
// runs it with the mock LLM, and returns the result (acceptance 1, sync).
func TestSlaveRunsTaskFresh(t *testing.T) {
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("task done: 42")
	})
	p := newCollabPair(t, "readonly")

	tp := mesh.TaskPayload{Task: "compute the answer", Context: "fresh", Cwd: "work"}
	data, _ := json.Marshal(tp)
	taskID := "task-fresh"
	if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgTask, ID: taskID, To: p.b.host.ID().String(), Payload: data}); err != nil {
		t.Fatal(err)
	}

	res := waitResult(t, p, taskID, 15*time.Second)
	if res.Status != "completed" {
		t.Fatalf("status = %q, error = %q", res.Status, res.Error)
	}
	if !strings.Contains(res.Output, "42") {
		t.Fatalf("output = %q, want task answer", res.Output)
	}
}

// TestSlaveMultipleToolCallsContinue: an assistant turn that issues TWO tool
// calls in one message must feed both results back and run another turn —
// a regression guard for the "run stops after the first tool round" bug
// reported on real deployments.
func TestSlaveMultipleToolCallsContinue(t *testing.T) {
	var probeCalls atomic.Int64
	probe := &agent.AgentTool{
		Name:       "mesh_probe",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(_ string, _ map[string]any, _ context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			return tools.ToolResult(fmt.Sprintf("probe %d ok", probeCalls.Add(1))), nil
		},
	}
	mockAPI(t, "mesh-mock-b",
		func(_ int, _ *ai.Context) *ai.Message {
			// Turn 1: text + two tool calls in ONE message (like the real
			// mesh_forward list + bash pairing).
			return &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonToolUse, Content: []ai.ContentBlock{
				ai.TextBlock("checking first"),
				ai.ToolCallBlock("c1", "mesh_probe", map[string]any{}),
				ai.ToolCallBlock("c2", "mesh_probe", map[string]any{}),
			}}
		},
		func(_ int, ctx *ai.Context) *ai.Message {
			// Turn 2 must see both tool results.
			seen := 0
			for _, m := range ctx.Messages {
				if m.Role == ai.RoleToolResult && m.ToolName == "mesh_probe" {
					seen++
				}
			}
			if seen != 2 {
				t.Errorf("turn 2 saw %d probe results, want 2", seen)
			}
			return textStop("AFTER-TOOLS")
		},
	)
	p := newCollabPair(t, "full")
	p.handler.meshTools = []*agent.AgentTool{probe}

	tp := mesh.TaskPayload{Task: "probe twice then summarize", Context: "fresh", Cwd: "work"}
	data, _ := json.Marshal(tp)
	taskID := "task-two-calls"
	if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgTask, ID: taskID, To: p.b.host.ID().String(), Payload: data}); err != nil {
		t.Fatal(err)
	}

	res := waitResult(t, p, taskID, 15*time.Second)
	if res.Status != "completed" {
		t.Fatalf("status = %q, error = %q", res.Status, res.Error)
	}
	if !strings.Contains(res.Output, "AFTER-TOOLS") {
		t.Fatalf("output = %q: the run ended after the first tool round instead of continuing", res.Output)
	}
	if got := probeCalls.Load(); got != 2 {
		t.Fatalf("probe executed %d times, want 2", got)
	}
}

// TestSlaveBadCwd: an out-of-whitelist cwd is rejected immediately with
// MsgError(bad-cwd) — the master does not wait for a timeout (acceptance 5).
func TestSlaveBadCwd(t *testing.T) {
	p := newCollabPair(t, "readonly")

	tp := mesh.TaskPayload{Task: "x", Context: "fresh", Cwd: "not-in-whitelist"}
	data, _ := json.Marshal(tp)
	taskID := "task-badcwd"
	if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgTask, ID: taskID, To: p.b.host.ID().String(), Payload: data}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res := waitResult(t, p, taskID, 15*time.Second)
	if res.Status != "error" || !strings.Contains(res.Error, "bad-cwd") {
		t.Fatalf("expected bad-cwd rejection, got %+v", res)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("bad-cwd rejection took too long: the master waited out the task")
	}
}

// TestSlaveUnauthorized: a node that does not allow remote tasks rejects with
// denied.
func TestSlaveUnauthorized(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	connectAndWait(t, a, b)
	aSess := newMockSession(t, "mesh-mock-a")
	bSess := newMockSession(t, "mesh-mock-b")
	bridge := NewBridge(a, aSess, 0)
	handler := newAgentHandler(b, bSess, 0, nil)
	a.Subscribe(bridge.Handle)
	b.Subscribe(handler.Handle)

	tp := mesh.TaskPayload{Task: "x", Context: "fresh"}
	data, _ := json.Marshal(tp)
	taskID := "task-denied"
	if err := a.Send(b.host.ID().String(), mesh.Envelope{Type: mesh.MsgTask, ID: taskID, To: b.host.ID().String(), Payload: data}); err != nil {
		t.Fatal(err)
	}

	ch := make(chan mesh.ErrorPayload, 1)
	unsub := a.Subscribe(func(env mesh.Envelope) error {
		if env.Type == mesh.MsgError && env.ReplyTo == taskID {
			var ep mesh.ErrorPayload
			if json.Unmarshal(env.Payload, &ep) == nil {
				select {
				case ch <- ep:
				default:
				}
			}
		}
		return nil
	})
	defer unsub()
	select {
	case ep := <-ch:
		if ep.Code != "denied" {
			t.Fatalf("error code = %q, want denied", ep.Code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for denied rejection")
	}
}

// TestSlaveInterrupt: a blocking task is aborted by MsgInterrupt and returns
// an aborted result (acceptance 6).
func TestSlaveInterrupt(t *testing.T) {
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return nil // block until aborted
	})
	p := newCollabPair(t, "readonly")

	tp := mesh.TaskPayload{Task: "long running", Context: "fresh"}
	data, _ := json.Marshal(tp)
	taskID := "task-interrupt"
	if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgTask, ID: taskID, To: p.b.host.ID().String(), Payload: data}); err != nil {
		t.Fatal(err)
	}

	// Wait until the task is registered on the slave, then interrupt.
	waitFor(t, 5*time.Second, func() bool {
		p.handler.mu.Lock()
		_, ok := p.handler.tasks[taskID]
		p.handler.mu.Unlock()
		return ok
	}, "slave task registered")

	if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgInterrupt, To: p.b.host.ID().String(), ReplyTo: taskID}); err != nil {
		t.Fatal(err)
	}

	res := waitResult(t, p, taskID, 15*time.Second)
	if res.Status != "aborted" {
		t.Fatalf("status = %q (error %q), want aborted", res.Status, res.Error)
	}
}

// TestSlaveBusy: with the concurrent-task cap reached, a further task is
// rejected immediately with MsgError(busy).
func TestSlaveBusy(t *testing.T) {
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return nil // block the first task so the cap stays held
	})
	p := newCollabPair(t, "readonly")
	p.b.cfg.MaxConcurrentTasks = 1
	p.handler.maxConcurrent = 1

	first := "task-busy-1"
	firstData, _ := json.Marshal(mesh.TaskPayload{Task: "long", Context: "fresh"})
	if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgTask, ID: first, To: p.b.host.ID().String(), Payload: firstData}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		p.handler.mu.Lock()
		defer p.handler.mu.Unlock()
		return p.handler.running == 1
	}, "first task occupies the single slot")

	second := "task-busy-2"
	secondData, _ := json.Marshal(mesh.TaskPayload{Task: "x", Context: "fresh"})
	if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgTask, ID: second, To: p.b.host.ID().String(), Payload: secondData}); err != nil {
		t.Fatal(err)
	}

	ch := make(chan mesh.ErrorPayload, 1)
	unsub := p.a.Subscribe(func(env mesh.Envelope) error {
		if env.Type == mesh.MsgError && env.ReplyTo == second {
			var ep mesh.ErrorPayload
			if json.Unmarshal(env.Payload, &ep) == nil {
				select {
				case ch <- ep:
				default:
				}
			}
		}
		return nil
	})
	defer unsub()
	select {
	case ep := <-ch:
		if ep.Code != "busy" {
			t.Fatalf("error code = %q, want busy", ep.Code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for busy rejection")
	}
}

// TestRemoteAgentTimeout: a synchronous dispatch that exceeds the tool timeout
// sends MsgInterrupt and reports a timeout; the slave aborts and returns an
// aborted result (acceptance 6, master-triggered).
func TestRemoteAgentTimeout(t *testing.T) {
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return nil // block until aborted
	})
	p := newCollabPair(t, "readonly")

	// Track the aborted result the slave sends after the interrupt.
	abortedCh := make(chan mesh.ResultPayload, 1)
	unsub := p.a.Subscribe(func(env mesh.Envelope) error {
		if env.Type == mesh.MsgResult {
			var res mesh.ResultPayload
			if json.Unmarshal(env.Payload, &res) == nil && res.Status == "aborted" {
				select {
				case abortedCh <- res:
				default:
				}
			}
		}
		return nil
	})
	defer unsub()

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	res, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "hang",
		"context": "fresh",
		"timeout": 1, // seconds
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content[0].Text, "timed out") {
		t.Fatalf("tool output = %q, want timeout report", res.Content[0].Text)
	}
	select {
	case <-abortedCh:
		// the slave aborted after the interrupt
	case <-time.After(10 * time.Second):
		t.Fatal("slave did not return an aborted result after the interrupt")
	}
}

// TestRemoteAgentSync: drive the master's remote_agent tool end-to-end — A's
// tool dispatches to B, B runs, A's tool returns the result (acceptance 1).
func TestRemoteAgentSync(t *testing.T) {
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("the meaning of life")
	})
	p := newCollabPair(t, "readonly")

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	res, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "tell me the meaning of life",
		"context": "fresh",
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].Text
	if !strings.Contains(out, "the meaning of life") {
		t.Fatalf("tool output = %q", out)
	}
}

// TestRemoteAgentSyncBadCwd: a bad cwd is returned by the tool quickly (the
// bridge routes MsgError to the waiter) — no timeout.
func TestRemoteAgentSyncBadCwd(t *testing.T) {
	p := newCollabPair(t, "readonly")
	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)

	start := time.Now()
	res, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "x",
		"context": "fresh",
		"cwd":     "bad-path",
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content[0].Text, "bad-cwd") {
		t.Fatalf("tool output = %q, want bad-cwd", res.Content[0].Text)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("bad-cwd was not rejected promptly")
	}
}

// TestRemoteAgentAsync: async dispatch returns immediately with a task id; the
// slave runs it; the result is injected as a follow-up message into the master
// agent and the in-flight registry reports completion (acceptance 2, async).
func TestRemoteAgentAsync(t *testing.T) {
	mockAPI(t, "mesh-mock-a", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("ack followup")
	})
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("async result: 7")
	})
	p := newCollabPair(t, "readonly")

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	res, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "async job",
		"context": "fresh",
		"async":   true,
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].Text
	taskID := parseTaskID(out)
	if taskID == "" {
		t.Fatalf("async ack = %q, want task id", out)
	}

	// The in-flight registry should show it running, then completed.
	waitFor(t, 10*time.Second, func() bool {
		infos := p.bridge.ListInflight()
		for _, inf := range infos {
			if inf.TaskID == taskID && inf.Status == "completed" {
				return true
			}
		}
		return false
	}, "async task completed in registry")

	// The follow-up message was injected into the master agent transcript.
	waitFor(t, 10*time.Second, func() bool {
		for _, m := range p.aSess.Agent.State().Messages {
			if m.Role == ai.RoleUser && strings.Contains(m.Text(), taskID) {
				return true
			}
		}
		return false
	}, "follow-up injected into master transcript")
}

// TestStatusTool: remote_agent_status lists in-flight async tasks.
func TestStatusTool(t *testing.T) {
	mockAPI(t, "mesh-mock-a", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("ack")
	})
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("done")
	})
	p := newCollabPair(t, "readonly")

	statusTool := RemoteAgentStatusTool(p.bridge)
	res, err := statusTool.Execute("c1", map[string]any{}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content[0].Text, "No in-flight remote tasks") {
		t.Fatalf("empty status = %q", res.Content[0].Text)
	}

	// Dispatch an async task, then check the status lists it.
	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	ares, _ := tool.Execute("c2", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "job",
		"context": "fresh",
		"async":   true,
	}, context.Background(), nil)
	taskID := parseTaskID(ares.Content[0].Text)
	if taskID == "" {
		t.Fatalf("async ack = %q, want task id", ares.Content[0].Text)
	}

	waitFor(t, 10*time.Second, func() bool {
		r, _ := statusTool.Execute("c3", map[string]any{"task": taskID}, context.Background(), nil)
		return strings.Contains(r.Content[0].Text, "completed")
	}, "status tool reports completed task")
}

// TestQuestionLLMAnswer: a slave agent asking a question (contact_supervisor)
// gets an unattended LLM answer from the master (acceptance 3, headless).
func TestQuestionLLMAnswer(t *testing.T) {
	mockAPI(t, "mesh-mock-a", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("use port 8080")
	})
	mockAPI(t, "mesh-mock-b",
		func(_ int, _ *ai.Context) *ai.Message {
			return toolCallStop("c1", "contact_supervisor", map[string]any{"message": "Which port should I bind?"})
		},
		func(_ int, _ *ai.Context) *ai.Message {
			return textStop("I will bind port 8080.")
		},
	)
	p := newCollabPair(t, "readonly")

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	res, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "decide on a port and tell me",
		"context": "fresh",
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].Text
	if !strings.Contains(out, "8080") {
		t.Fatalf("tool output = %q, want the port the slave decided", out)
	}
}

// TestQuestionHumanAnswer: with an interactive consumer online, the question is
// routed through the session answer channel and the human's answer is returned
// (acceptance 3, human online).
func TestQuestionHumanAnswer(t *testing.T) {
	mockAPI(t, "mesh-mock-b",
		func(_ int, _ *ai.Context) *ai.Message {
			return toolCallStop("c1", "contact_supervisor", map[string]any{"message": "Which color?"})
		},
		func(_ int, _ *ai.Context) *ai.Message {
			return textStop("I will use blue.")
		},
	)
	p := newCollabPair(t, "readonly")

	// Register a human consumer; answer every question in a goroutine.
	qCh := make(chan agent.Question, 1)
	p.aSess.Subscribe(func(ev agent.AgentEvent) error {
		if ev.Type == agent.EventQuestion && ev.Question != nil {
			select {
			case qCh <- *ev.Question:
			default:
			}
		}
		return nil
	}, coding.ConsumerInteractive)
	go func() {
		q := <-qCh
		p.aSess.AnswerQuestion(q.ID, "use blue")
	}()

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	res, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "decide on a color",
		"context": "fresh",
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].Text
	if !strings.Contains(out, "blue") {
		t.Fatalf("tool output = %q, want the human-chosen color", out)
	}
}

// TestRemoteAgentSummarySnapshot: the tool prepares a summary snapshot (via an
// LLM summarize call) and the slave receives it as initial context.
func TestRemoteAgentSummarySnapshot(t *testing.T) {
	// call 0 on the master: the summarize LLM call.
	mockAPI(t, "mesh-mock-a", func(_ int, ctx *ai.Context) *ai.Message {
		return textStop("session summary: working on the fibonacci CLI")
	})
	// slave: assert the summary was injected as the initial context.
	mockAPI(t, "mesh-mock-b", func(_ int, ctx *ai.Context) *ai.Message {
		found := false
		for _, m := range ctx.Messages {
			if strings.Contains(m.Text(), "fibonacci CLI") {
				found = true
			}
		}
		if !found {
			t.Fatalf("slave did not receive summary context; messages = %v", ctx.Messages)
		}
		return textStop("context received")
	})
	p := newCollabPair(t, "readonly")

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	res, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "continue the work",
		"context": "summary",
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content[0].Text, "context received") {
		t.Fatalf("tool output = %q", res.Content[0].Text)
	}
}

// TestRemoteAgentRoleRouting: with peer empty, the tool routes to an accepting
// peer by role.
func TestRemoteAgentRoleRouting(t *testing.T) {
	mockAPI(t, "mesh-mock-a", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("ack")
	})
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("routed by role")
	})
	p := newCollabPair(t, "readonly")

	// B's profile role is "node-b" (from newNode); give it a matching role for
	// the requested "researcher".
	p.b.cfg.Roles = []string{"researcher"}
	// Re-advertise so A sees the profile.
	waitFor(t, 5*time.Second, func() bool {
		// The handler profile is built from cfg at send time; force a
		// rebroadcast by sending a fresh profile.
		p.b.sendProfile(p.a.host.ID())
		for _, peer := range p.a.Peers() {
			for _, r := range peer.Profile.Roles {
				if r == "researcher" {
					return true
				}
			}
		}
		return false
	}, "master sees researcher role on slave")

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	res, err := tool.Execute("c1", map[string]any{
		"roles":   []any{"researcher"},
		"task":    "do research",
		"context": "fresh",
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content[0].Text, "routed by role") {
		t.Fatalf("tool output = %q", res.Content[0].Text)
	}
}

// TestRemoteAgentForkSnapshot: the tool prepares a fork snapshot (recent
// transcript messages) and the slave loads them as its initial transcript.
func TestRemoteAgentForkSnapshot(t *testing.T) {
	// master: no LLM calls (fresh dispatch with a fork snapshot).
	mockAPI(t, "mesh-mock-a")
	// slave: assert the fork snapshot was loaded as the initial transcript.
	mockAPI(t, "mesh-mock-b", func(_ int, ctx *ai.Context) *ai.Message {
		found := false
		for _, m := range ctx.Messages {
			if m.Role == ai.RoleUser && strings.Contains(m.Text(), "original master context") {
				found = true
			}
		}
		if !found {
			t.Fatalf("slave did not receive fork transcript; messages = %v", ctx.Messages)
		}
		return textStop("fork context received")
	})
	p := newCollabPair(t, "readonly")

	// Persist a transcript message into the master session so RecentMessages
	// sees it.
	if err := p.aSess.Session.AppendMessage(ai.NewUserTextMessage("original master context line")); err != nil {
		t.Fatal(err)
	}

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	res, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "continue the work",
		"context": "fork",
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content[0].Text, "fork context received") {
		t.Fatalf("tool output = %q", res.Content[0].Text)
	}
}

// TestPeersTool: the mesh_peers tool surfaces the connected nodes and their
// capabilities to the agent, so it can discover remote_agent targets (the
// "agent 发现与能力感知" half exposed to the LLM).
func TestPeersTool(t *testing.T) {
	mockAPI(t, "mesh-mock-a")
	mockAPI(t, "mesh-mock-b")
	p := newCollabPair(t, "readonly")
	// The pair's profile was exchanged before the slave config was set; refresh
	// B's profile on A so the tool sees the real roles/accepting/load.
	p.b.cfg.Roles = []string{"worker", "researcher"}
	p.b.sendProfile(p.a.host.ID())
	waitFor(t, 5*time.Second, func() bool {
		prof := peerProfile(p.a, p.b.host.ID().String())
		return prof != nil && prof.Accepting && len(prof.Roles) == 2
	}, "master sees refreshed node-b profile")

	tool := PeersTool(p.a)

	// List all peers: node-b is shown with its roles and accepting status.
	res, err := tool.Execute("c1", map[string]any{}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].Text
	if !strings.Contains(out, p.b.host.ID().String()) {
		t.Fatalf("list output missing node-b id:\n%s", out)
	}
	for _, want := range []string{"worker", "researcher", "accepting=yes", "load=0/2", "model="} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}

	// Full profile of one peer.
	full, err := tool.Execute("c2", map[string]any{"peer": p.b.host.ID().String()}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	fo := full.Content[0].Text
	if !strings.Contains(fo, "Accepting: true") || !strings.Contains(fo, "Cwds:") {
		t.Fatalf("profile output = %q", fo)
	}

	// Unknown peer id → a helpful message, not an error.
	miss, err := tool.Execute("c3", map[string]any{"peer": "12D3KooXnone"}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(miss.Content[0].Text, "no such peer") {
		t.Fatalf("missing-peer output = %q", miss.Content[0].Text)
	}
}

// TestPeersToolSelf: mesh_peers(self=true) reports the local node's own
// identity — answering "which node am I" without touching the shell.
func TestPeersToolSelf(t *testing.T) {
	mockAPI(t, "mesh-mock-a")
	mockAPI(t, "mesh-mock-b")
	p := newCollabPair(t, "readonly")
	p.a.cfg.Roles = []string{"master", "researcher"}

	tool := PeersTool(p.a)
	res, err := tool.Execute("c1", map[string]any{"self": true}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].Text
	if !strings.Contains(out, p.a.host.ID().String()) {
		t.Fatalf("self output missing local peer id:\n%s", out)
	}
	for _, want := range []string{"Peer ", "Name:      node-a", "Roles:     master", "Accepting: false"} {
		if !strings.Contains(out, want) {
			t.Fatalf("self output missing %q:\n%s", want, out)
		}
	}
}

// TestPeersLoadReflectsRunningTasks: the slave's advertised Load tracks its
// in-flight remote tasks (1/2 while a task runs), so role routing is
// load-aware.
func TestPeersLoadReflectsRunningTasks(t *testing.T) {
	mockAPI(t, "mesh-mock-a")
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return nil // block, holding the task slot
	})
	p := newCollabPair(t, "readonly")

	// Start a blocking task on B.
	data, _ := json.Marshal(mesh.TaskPayload{Task: "long", Context: "fresh"})
	taskID := "task-load"
	if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgTask, ID: taskID, To: p.b.host.ID().String(), Payload: data}); err != nil {
		t.Fatal(err)
	}

	// A's cached profile of B should show Load 1 (was 0 after connect).
	waitFor(t, 5*time.Second, func() bool {
		if prof := peerProfile(p.a, p.b.host.ID().String()); prof != nil {
			return prof.Load == 1
		}
		return false
	}, "master sees slave load=1 while a task runs")
}

// parseResultSession extracts "session=<id>" from a sync tool result.
func parseResultSession(out string) string {
	if idx := strings.Index(out, "session="); idx >= 0 {
		rest := out[idx+len("session="):]
		if sp := strings.IndexAny(rest, " \n\t)"); sp >= 0 {
			return rest[:sp]
		}
		return rest
	}
	return ""
}

// TestRemoteAgentContinueSession: a dispatch with session=<id> resumes the
// previous remote-task session on the slave, so long back-and-forth tasks keep
// their conversation continuity (acceptance for the continuity design).
func TestRemoteAgentContinueSession(t *testing.T) {
	mockAPI(t, "mesh-mock-a")
	mockAPI(t, "mesh-mock-b",
		func(_ int, _ *ai.Context) *ai.Message {
			return textStop("first result")
		},
		func(_ int, ctx *ai.Context) *ai.Message {
			found := false
			for _, m := range ctx.Messages {
				if m.Role == ai.RoleUser && strings.Contains(m.Text(), "task one") {
					found = true
				}
			}
			if !found {
				t.Fatalf("continued task did not receive the previous task's transcript; messages = %v", ctx.Messages)
			}
			return textStop("second result continued")
		},
	)
	p := newCollabPair(t, "readonly")

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)

	// Task one: fresh session.
	res1, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "task one",
		"context": "fresh",
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sessID := parseResultSession(res1.Content[0].Text)
	if sessID == "" {
		t.Fatalf("first result missing session id: %q", res1.Content[0].Text)
	}

	// Task two: continue the same remote session.
	res2, err := tool.Execute("c2", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "task two",
		"context": "fresh",
		"session": sessID,
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res2.Content[0].Text, "second result continued") {
		t.Fatalf("continued result = %q", res2.Content[0].Text)
	}
	if got := parseResultSession(res2.Content[0].Text); got != sessID {
		t.Fatalf("continued task session = %q, want same %q", got, sessID)
	}
}

// TestRemoteAgentBadSession: continuing a session that does not exist (or is
// not a remote-task session) is rejected immediately with bad-session — the
// master does not wait out a timeout.
func TestRemoteAgentBadSession(t *testing.T) {
	p := newCollabPair(t, "readonly")

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	res, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "x",
		"context": "fresh",
		"session": "no-such-session",
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content[0].Text, "bad-session") {
		t.Fatalf("tool output = %q, want bad-session", res.Content[0].Text)
	}
}

// TestRemoteAgentRejectsUserSession: a remote client cannot continue a user's
// local session (Kind != remote) — only sessions the slave forked for remote
// tasks may be resumed.
func TestRemoteAgentRejectsUserSession(t *testing.T) {
	p := newCollabPair(t, "readonly")

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	res, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "x",
		"context": "fresh",
		"session": p.bSess.CurrentSession().Header.ID, // a user session
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content[0].Text, "bad-session") {
		t.Fatalf("tool output = %q, want bad-session (user session must not be continuable)", res.Content[0].Text)
	}
}

// TestRemoteAgentAsyncFollowsCallingSession: an async task dispatched from a
// web-style fork must have its result injected back into the FORK's session
// (the one that initiated it), never the node's shared session — and the
// in-flight entry is persisted to the fork's session file.
func TestRemoteAgentAsyncFollowsCallingSession(t *testing.T) {
	mockAPI(t, "mesh-mock-a", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("ack followup")
	})
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("fork callback result")
	})
	p := newCollabPair(t, "readonly")

	// Install the per-session builder exactly like attachMeshCollab does.
	p.aSess.SetExtraToolsBuilder(func(fork *coding.AgentSession) []*agent.AgentTool {
		return []*agent.AgentTool{RemoteAgentTool(p.a, p.bridge, fork)}
	})
	hasTool := func(sess *coding.AgentSession, name string) bool {
		for _, tl := range sess.Agent.State().Tools {
			if tl.Name == name {
				return true
			}
		}
		return false
	}
	if !hasTool(p.aSess, "remote_agent") {
		t.Fatal("shared session missing its own remote_agent")
	}

	// A web-style fork of the shared session gets its own remote_agent bound
	// to the fork.
	fork, err := p.aSess.ForkNew()
	if err != nil {
		t.Fatal(err)
	}
	if !hasTool(fork, "remote_agent") {
		t.Fatal("fork missing remote_agent")
	}

	var tool *agent.AgentTool
	for _, tl := range fork.Agent.State().Tools {
		if tl.Name == "remote_agent" {
			tool = tl
			break
		}
	}
	res, err := tool.Execute("c1", map[string]any{
		"peer":    p.b.host.ID().String(),
		"task":    "fork dispatched",
		"context": "fresh",
		"async":   true,
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	taskID := parseTaskID(res.Content[0].Text)
	if taskID == "" {
		t.Fatalf("async ack = %q, want task id", res.Content[0].Text)
	}

	// The follow-up must land in the FORK's agent transcript...
	waitFor(t, 10*time.Second, func() bool {
		for _, m := range fork.Agent.State().Messages {
			if m.Role == ai.RoleUser && strings.Contains(m.Text(), taskID) {
				return true
			}
		}
		return false
	}, "follow-up injected into the fork that dispatched")

	// ...and never leak into the shared session's transcript.
	for _, m := range p.aSess.Agent.State().Messages {
		if m.Role == ai.RoleUser && strings.Contains(m.Text(), taskID) {
			t.Fatalf("follow-up leaked into the shared session")
		}
	}

	// The in-flight entry was persisted to the FORK's session file.
	waitFor(t, 10*time.Second, func() bool {
		entries := fork.CurrentSession().ContextEntries()
		for _, e := range entries {
			if e.CustomType == "mesh_inflight" {
				if d, ok := e.Data.(map[string]any); ok && d["taskId"] == taskID {
					return true
				}
			}
		}
		return false
	}, "mesh_inflight persisted to the fork session")
}

// TestRemoteAgentSyncAbort: aborting the local run (cancel the tool's context,
// as a webui Abort does) unblocks a synchronous remote_agent immediately and
// sends MsgInterrupt so the slave stops the hung task too — instead of leaving
// the master waiting until the timeout.
func TestRemoteAgentSyncAbort(t *testing.T) {
	mockAPI(t, "mesh-mock-a")
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return nil // block the slave task
	})
	p := newCollabPair(t, "readonly")

	// Watch for the slave's aborted result (proves MsgInterrupt reached it).
	abortedCh := make(chan mesh.ResultPayload, 1)
	unsub := p.a.Subscribe(func(env mesh.Envelope) error {
		if env.Type == mesh.MsgResult {
			var res mesh.ResultPayload
			if json.Unmarshal(env.Payload, &res) == nil && res.Status == "aborted" {
				select {
				case abortedCh <- res:
				default:
				}
			}
		}
		return nil
	})
	defer unsub()

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var toolErr error
	go func() {
		_, toolErr = tool.Execute("c1", map[string]any{
			"peer":    p.b.host.ID().String(),
			"task":    "hang",
			"context": "fresh",
		}, ctx, nil)
		close(done)
	}()

	// Wait until the slave is running the task, then abort the local run.
	waitFor(t, 5*time.Second, func() bool {
		p.handler.mu.Lock()
		defer p.handler.mu.Unlock()
		return p.handler.running == 1
	}, "slave running the task")
	cancel()

	select {
	case <-done:
		if toolErr != nil {
			t.Fatalf("tool error after abort: %v", toolErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sync remote_agent did not return after the run was aborted")
	}

	// The slave received MsgInterrupt and returned an aborted result.
	select {
	case <-abortedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("slave did not return aborted result after master abort")
	}
}

// TestInjectFollowUpDoesNotInterruptBusyAgent: an async result is delivered as
// a queued FOLLOW-UP that never interrupts the main session. If the agent is
// mid-run, the result waits until the run naturally ends, then surfaces —
// the agent is expected to finish its turn and wait for the notification, not
// to be steered/interrupted.
func TestInjectFollowUpDoesNotInterruptBusyAgent(t *testing.T) {
	mockAPI(t, "mesh-mock-a",
		func(_ int, _ *ai.Context) *ai.Message {
			return toolCallStop("tc1", "mesh_blocker", map[string]any{})
		},
		func(_ int, _ *ai.Context) *ai.Message {
			return textStop("ack turn")
		},
		func(_ int, _ *ai.Context) *ai.Message {
			return textStop("done after result")
		},
	)
	mockAPI(t, "mesh-mock-b")
	p := newCollabPair(t, "readonly")

	// A tool the busy agent executes; it blocks until the test releases it.
	entered := make(chan struct{})
	released := make(chan struct{})
	p.aSess.AppendExtraTools([]*agent.AgentTool{{
		Name:       "mesh_blocker",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(_ string, _ map[string]any, ctx context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			select {
			case <-entered:
			default:
				close(entered)
			}
			select {
			case <-released:
				return tools.ToolResult("blocker done"), nil
			case <-ctx.Done():
				return tools.ToolResult("blocker interrupted"), nil
			}
		},
	}})

	runDone := make(chan error, 1)
	go func() { runDone <- p.aSess.Agent.Prompt("busy turn") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("busy agent never entered the blocker tool")
	}

	// The async result arrives while the agent is busy: it is QUEUED, not
	// interrupting the running turn.
	p.bridge.RegisterInflight("t1", p.b.host.ID().String(), "s", p.aSess, time.Minute)
	p.bridge.injectFollowUp("t1", p.b.host.ID().String(), mesh.ResultPayload{Status: "completed", Output: "ASYNC-RESULT-BODY"})

	select {
	case <-runDone:
		t.Fatal("busy run was interrupted by the follow-up")
	case <-time.After(300 * time.Millisecond):
	}
	for _, m := range p.aSess.Agent.State().Messages {
		if m.Role == ai.RoleUser && strings.Contains(m.Text(), "ASYNC-RESULT-BODY") {
			t.Fatal("result surfaced before the busy run ended")
		}
	}

	// Release the blocker: the run ends and the queued follow-up is processed.
	close(released)
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("busy run did not end after release")
	}
	waitFor(t, 10*time.Second, func() bool {
		for _, m := range p.aSess.Agent.State().Messages {
			if m.Role == ai.RoleUser && strings.Contains(m.Text(), "ASYNC-RESULT-BODY") {
				return true
			}
		}
		return false
	}, "queued result surfaced after the busy run ended")
}

// TestRemoteTaskRecordsSourceAndActive: while a remote task runs, the slave
// registers the fork as the live active instance (so a web window opening that
// session subscribes to its events) and stamps the session header with the
// dispatching node's identity; both are cleaned up on completion.
func TestRemoteTaskRecordsSourceAndActive(t *testing.T) {
	mockAPI(t, "mesh-mock-a")
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return nil // block so the fork stays registered
	})
	p := newCollabPair(t, "readonly")

	data, _ := json.Marshal(mesh.TaskPayload{Task: "hold", Context: "fresh", Cwd: "work"})
	taskID := "task-source"
	if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgTask, ID: taskID, To: p.b.host.ID().String(), Payload: data}); err != nil {
		t.Fatal(err)
	}

	// Wait for the fork to be live, then inspect it.
	var fork *coding.AgentSession
	waitFor(t, 5*time.Second, func() bool {
		p.handler.mu.Lock()
		defer p.handler.mu.Unlock()
		if rec := p.handler.tasks[taskID]; rec != nil && rec.sess != nil {
			fork = rec.sess
			return true
		}
		return false
	}, "task fork live")
	if fork == nil {
		t.Fatal("no fork session")
	}

	path := fork.CurrentSession().FilePath
	if fork.CurrentSession().Header.Kind != coding.SessionKindRemote {
		t.Fatalf("fork kind = %q, want remote", fork.CurrentSession().Header.Kind)
	}
	if fork.CurrentSession().Header.SourcePeerID != p.a.host.ID().String() {
		t.Fatalf("source peer = %q, want master %q", fork.CurrentSession().Header.SourcePeerID, p.a.host.ID().String())
	}
	if fork.CurrentSession().Header.SourceName != "node-a" {
		t.Fatalf("source name = %q, want node-a", fork.CurrentSession().Header.SourceName)
	}
	if fork.SessionManager.ActiveFor(path) != fork {
		t.Fatal("fork not registered as the live active instance")
	}

	// Abort the task; the fork is unregistered on completion.
	if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgInterrupt, To: p.b.host.ID().String(), ReplyTo: taskID}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return fork.SessionManager.ActiveFor(path) == nil
	}, "fork unregistered after completion")
}

// TestRemoteTaskMeshToolsByLevel: full-level remote-task forks carry the node's
// mesh tools (mesh_forward & co) alongside contact_supervisor, while lower
// levels never do — forwarding mutates node config, which read-only/write
// tasks must not be able to do.
func TestRemoteTaskMeshToolsByLevel(t *testing.T) {
	probe := &agent.AgentTool{
		Name:       "mesh_probe",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(_ string, _ map[string]any, _ context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			return tools.ToolResult("probe ok"), nil
		},
	}
	// run dispatches a blocking task at the given level and returns the fork's
	// tool names once the fork is live.
	run := func(level string) map[string]bool {
		mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
			return nil // block so the fork stays inspectable
		})
		p := newCollabPair(t, level)
		p.handler.meshTools = []*agent.AgentTool{probe}

		taskID := "task-tools-" + level
		data, _ := json.Marshal(mesh.TaskPayload{Task: "hold", Context: "fresh"})
		if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgTask, ID: taskID, To: p.b.host.ID().String(), Payload: data}); err != nil {
			t.Fatal(err)
		}

		names := map[string]bool{}
		waitFor(t, 5*time.Second, func() bool {
			p.handler.mu.Lock()
			defer p.handler.mu.Unlock()
			rec := p.handler.tasks[taskID]
			if rec == nil || rec.sess == nil {
				return false
			}
			for _, tl := range rec.sess.Agent.State().Tools {
				names[tl.Name] = true
			}
			return true
		}, level+" task fork live")

		// Abort the task and wait for the record to clear.
		if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgInterrupt, To: p.b.host.ID().String(), ReplyTo: taskID}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 5*time.Second, func() bool {
			p.handler.mu.Lock()
			defer p.handler.mu.Unlock()
			_, still := p.handler.tasks[taskID]
			return !still
		}, level+" task cleaned up")
		return names
	}

	full := run("full")
	if !full["mesh_probe"] || !full["contact_supervisor"] {
		t.Fatalf("full-level fork missing tools: %v", full)
	}
	ro := run("readonly")
	if ro["mesh_probe"] {
		t.Fatalf("readonly fork must not carry mesh tools: %v", ro)
	}
	if !ro["contact_supervisor"] {
		t.Fatalf("readonly fork missing contact_supervisor: %v", ro)
	}
}

// TestInflightWatchdogMarksTimeout: when a dispatched task never reports back
// (connection dropped or the slave died mid-run), the master-side watchdog
// marks the in-flight entry timed out and sends MsgInterrupt, so the mesh
// panel cannot show a dead task as "running" forever.
func TestInflightWatchdogMarksTimeout(t *testing.T) {
	p := newCollabPair(t, "readonly")

	interrupts := make(chan string, 1)
	unsub := p.b.Subscribe(func(env mesh.Envelope) error {
		if env.Type == mesh.MsgInterrupt {
			select {
			case interrupts <- env.ReplyTo:
			default:
			}
		}
		return nil
	})
	defer unsub()

	defer func(d time.Duration) { asyncResultGrace = d }(asyncResultGrace)
	asyncResultGrace = 40 * time.Millisecond

	peer := p.b.host.ID().String()
	p.bridge.RegisterInflight("wd1", peer, "summary", p.aSess, 20*time.Millisecond)

	waitFor(t, 5*time.Second, func() bool {
		st, ok := p.bridge.Status("wd1")
		return ok && st.Status == "timeout" && strings.Contains(st.Progress, "超时")
	}, "inflight entry marked timed out")
	select {
	case id := <-interrupts:
		if id != "wd1" {
			t.Fatalf("interrupt replyTo = %q, want wd1", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not send MsgInterrupt")
	}
}

// TestInjectFollowUpMarksRemoteTask: async results are injected as user
// messages carrying a structured Details marker (kind=remote-task), so the
// UIs render them as dedicated result cards instead of plain user bubbles.
func TestInjectFollowUpMarksRemoteTask(t *testing.T) {
	mockAPI(t, "mesh-mock-a", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("ack")
	})
	p := newCollabPair(t, "readonly")

	peer := p.b.host.ID().String()
	p.bridge.RegisterInflight("t9", peer, "s", p.aSess, time.Minute)
	p.bridge.injectFollowUp("t9", peer, mesh.ResultPayload{Status: "completed", Output: "BODY-XYZ", Session: "sess-1"})

	waitFor(t, 5*time.Second, func() bool {
		for _, m := range p.aSess.Agent.State().Messages {
			if m.Role != ai.RoleUser || !strings.Contains(m.Text(), "BODY-XYZ") {
				continue
			}
			d, _ := m.Details.(map[string]any)
			return d["kind"] == "remote-task" && d["taskId"] == "t9" && d["peer"] == peer &&
				d["status"] == "completed" && d["session"] == "sess-1"
		}
		return false
	}, "follow-up carries the remote-task marker")
}

// TestRemoteAgentReusesSessionByDefault: remote_agent defaults to REUSING the
// most recent remote session with the target peer (so related dispatches stay
// in one conversation), only opening a fresh one when session="new" is passed.
func TestRemoteAgentReusesSessionByDefault(t *testing.T) {
	mockAPI(t, "mesh-mock-a")
	mockAPI(t, "mesh-mock-b",
		func(_ int, _ *ai.Context) *ai.Message {
			return textStop("task one done")
		},
		func(_ int, ctx *ai.Context) *ai.Message {
			// Continued session must contain the first task's transcript.
			for _, m := range ctx.Messages {
				if m.Role == ai.RoleUser && strings.Contains(m.Text(), "task one") {
					return textStop("task two done (continued)")
				}
			}
			t.Fatalf("task two did not continue session one; messages = %v", ctx.Messages)
			return textStop("x")
		},
		func(_ int, _ *ai.Context) *ai.Message {
			return textStop("task three done (fresh)")
		},
	)
	p := newCollabPair(t, "readonly")

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	exec := func(task string, session any) string {
		params := map[string]any{"peer": p.b.host.ID().String(), "task": task, "context": "fresh"}
		if session != nil {
			params["session"] = session
		}
		res, err := tool.Execute("c", params, context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].Text
	}

	// Task 1: no prior session → fresh.
	first := exec("task one", nil)
	sid1 := parseResultSession(first)
	if sid1 == "" {
		t.Fatalf("task one result missing session: %q", first)
	}

	// Task 2: session empty → REUSES sid1 (continuity).
	second := exec("task two", nil)
	if sid2 := parseResultSession(second); sid2 != sid1 {
		t.Fatalf("task two session = %q, want reuse %q", sid2, sid1)
	}
	if !strings.Contains(second, "task two done (continued)") {
		t.Fatalf("task two output = %q", second)
	}

	// Task 3: session="new" → a fresh session.
	third := exec("task three", "new")
	if sid3 := parseResultSession(third); sid3 == sid1 {
		t.Fatalf("session=new did not open a fresh session (%q)", sid3)
	}
	if !strings.Contains(third, "task three done (fresh)") {
		t.Fatalf("task three output = %q", third)
	}
}

// TestRemoteSessionReusePerMasterSession: two different master sessions
// dispatching to the SAME peer each reuse their OWN remote session id — one
// never steals/reuses the other's.
func TestRemoteSessionReusePerMasterSession(t *testing.T) {
	mockAPI(t, "mesh-mock-a")
	mockAPI(t, "mesh-mock-b",
		func(_ int, _ *ai.Context) *ai.Message { return textStop("shared one done") },
		func(_ int, _ *ai.Context) *ai.Message { return textStop("fork one done") },
		func(_ int, _ *ai.Context) *ai.Message { return textStop("shared two done") },
		func(_ int, _ *ai.Context) *ai.Message { return textStop("fork two done") },
	)
	p := newCollabPair(t, "readonly")
	p.aSess.SetExtraToolsBuilder(func(fork *coding.AgentSession) []*agent.AgentTool {
		return []*agent.AgentTool{RemoteAgentTool(p.a, p.bridge, fork)}
	})
	fork, err := p.aSess.ForkNew()
	if err != nil {
		t.Fatal(err)
	}
	findTool := func(sess *coding.AgentSession) *agent.AgentTool {
		for _, tl := range sess.Agent.State().Tools {
			if tl.Name == "remote_agent" {
				return tl
			}
		}
		return nil
	}
	sharedTool := findTool(p.aSess)
	forkTool := findTool(fork)
	if sharedTool == nil || forkTool == nil {
		t.Fatal("sessions missing remote_agent")
	}
	exec := func(tool *agent.AgentTool, task string) string {
		res, err := tool.Execute("c", map[string]any{
			"peer": p.b.host.ID().String(), "task": task, "context": "fresh",
		}, context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].Text
	}

	shared1 := exec(sharedTool, "shared task one")
	fork1 := exec(forkTool, "fork task one")
	if parseResultSession(fork1) == parseResultSession(shared1) {
		t.Fatalf("two master sessions must not share a remote session id")
	}

	// Each session's second dispatch reuses ITS OWN first session.
	shared2 := exec(sharedTool, "shared task two")
	fork2 := exec(forkTool, "fork task two")
	if got := parseResultSession(shared2); got != parseResultSession(shared1) {
		t.Fatalf("shared did not reuse its own session: %q vs %q", got, parseResultSession(shared1))
	}
	if got := parseResultSession(fork2); got != parseResultSession(fork1) {
		t.Fatalf("fork did not reuse its own session: %q vs %q", got, parseResultSession(fork1))
	}
}

// TestSyncQuestionRoutedToDispatchingSession: a sync remote_agent dispatched
// from a web-forked session; the slave asks a permission question; the question
// is routed to THE FORK (not the shared session), and answering from the fork
// unblocks everything — the master is never stuck in its own turn.
func TestSyncQuestionRoutedToDispatchingSession(t *testing.T) {
	var p *collabPair
	mockAPI(t, "mesh-mock-a",
		func(_ int, _ *ai.Context) *ai.Message {
			peer := ""
			if p != nil {
				peer = p.b.host.ID().String()
			}
			return toolCallStop("ra1", "remote_agent", map[string]any{
				"peer": peer, "task": "ask permission then act", "context": "fresh",
			})
		},
		func(_ int, _ *ai.Context) *ai.Message {
			return textStop("master wrap-up")
		},
	)
	mockAPI(t, "mesh-mock-b",
		func(_ int, _ *ai.Context) *ai.Message {
			return toolCallStop("q1", "contact_supervisor", map[string]any{"message": "may I proceed?"})
		},
		func(_ int, _ *ai.Context) *ai.Message {
			return textStop("acted after approval")
		},
	)
	p = newCollabPair(t, "readonly")
	p.aSess.SetExtraToolsBuilder(func(fork *coding.AgentSession) []*agent.AgentTool {
		return []*agent.AgentTool{RemoteAgentTool(p.a, p.bridge, fork)}
	})
	// The user views a web fork (which is what dispatched), NOT the shared session.
	fork, err := p.aSess.ForkNew()
	if err != nil {
		t.Fatal(err)
	}
	// Only the fork has an interactive consumer (the user is on it). The shared
	// session watches too, to prove the question is NOT leaked to it.
	sharedSaw := make(chan agent.Question, 1)
	p.aSess.Subscribe(func(ev agent.AgentEvent) error {
		if ev.Type == agent.EventQuestion && ev.Question != nil {
			select {
			case sharedSaw <- *ev.Question:
			default:
			}
		}
		return nil
	}, coding.ConsumerInteractive)
	qCh := make(chan agent.Question, 1)
	fork.Subscribe(func(ev agent.AgentEvent) error {
		if ev.Type == agent.EventQuestion && ev.Question != nil {
			select {
			case qCh <- *ev.Question:
			default:
			}
		}
		return nil
	}, coding.ConsumerInteractive)
	go func() {
		q := <-qCh
		fork.AnswerQuestion(q.ID, "approved")
	}()

	done := make(chan error, 1)
	go func() { done <- fork.Agent.Prompt("dispatch a task to the worker") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("master run error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("DEADLOCK: master did not complete while answering the slave question")
	}

	// The slave's approved result reached the master transcript.
	found := false
	for _, m := range fork.Agent.State().Messages {
		if strings.Contains(m.Text(), "acted after approval") {
			found = true
		}
	}
	if !found {
		t.Fatalf("master transcript missing the slave's result: %v", fork.Agent.State().Messages)
	}

	// The question must NOT have leaked to the shared session.
	select {
	case q := <-sharedSaw:
		t.Fatalf("question leaked to the shared session: %+v", q)
	default:
	}
}

// TestTaskRunErrorSurfaces: a failed Prompt (persistence error, recovered
// panic, …) must report as an error result instead of a hollow "completed"
// built from whatever text the run produced before dying.
func TestTaskRunErrorSurfaces(t *testing.T) {
	p := newCollabPair(t, "readonly")
	res := p.handler.resultFromSession(p.bSess, false, errors.New("boom: persistence failed"))
	if res.Status != "error" || !strings.Contains(res.Error, "boom") {
		t.Fatalf("result = %+v, want error carrying the run error", res)
	}
	// Aborted still wins over the run error (the abort caused it).
	res = p.handler.resultFromSession(p.bSess, true, errors.New("boom"))
	if res.Status != "aborted" {
		t.Fatalf("aborted result = %+v, want status aborted", res)
	}
}

// TestRemoteAgentPeerNameAlias: remote_agent accepts a node NAME as the peer
// reference and resolves it to the connected peer before dispatching.
func TestRemoteAgentPeerNameAlias(t *testing.T) {
	mockAPI(t, "mesh-mock-b", func(_ int, _ *ai.Context) *ai.Message {
		return textStop("alias dispatch ok")
	})
	p := newCollabPair(t, "readonly")

	tool := RemoteAgentTool(p.a, p.bridge, p.aSess)
	res, err := tool.Execute("call", map[string]any{
		"peer": "node-b", "task": "say hi", "context": "fresh", "async": false,
	}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := renderToolText(res)
	if !strings.Contains(out, "alias dispatch ok") {
		t.Fatalf("output = %q, want the remote task result", out)
	}
}
