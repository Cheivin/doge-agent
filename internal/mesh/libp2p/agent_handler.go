//go:build mesh

package libp2p

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/tools"
	"doge/internal/coding/uploads"
	"doge/internal/mesh"
	"doge/pkg/logx"
)

// Defaults for the slave side (overridable in tests).
const (
	// defaultTaskTimeout bounds a remote task when the payload sets no
	// explicit Timeout.
	defaultTaskTimeout = 30 * time.Minute
	// defaultQuestionTimeout bounds how long a contact_supervisor question
	// waits for the master before the slave decides on its own.
	defaultQuestionTimeout = 5 * time.Minute
	// defaultMaxConcurrentTasks caps concurrent remote tasks when the config
	// leaves MaxConcurrentTasks zero.
	defaultMaxConcurrentTasks = 1
)

// taskRecord tracks one in-flight remote task on the slave side.
type taskRecord struct {
	id     string
	from   string // master peer
	sess   *coding.AgentSession
	cancel context.CancelFunc

	// interrupt is closed once (by MsgInterrupt) to abort the running task.
	interrupt chan struct{}
	// answer carries MsgAnswer payloads routed by ReplyTo=taskID to the
	// agent's contact_supervisor tool.
	answer chan string
	// done is closed when the task is fully cleaned up.
	done chan struct{}
}

// agentHandler is the P3 slave side: it accepts MsgTask envelopes, forks an
// isolated AgentSession per task, executes it, and reports back
// progress/question/result. It also routes MsgInterrupt and MsgAnswer.
type agentHandler struct {
	m         *meshNode
	sess      *coding.AgentSession // local session; fork template for tasks
	cfg       *mesh.Config
	meshTools []*agent.AgentTool // node-level mesh tools for full-level tasks

	questionTimeout time.Duration
	defaultTimeout  time.Duration
	maxConcurrent   int

	mu      sync.Mutex
	tasks   map[string]*taskRecord
	running int
}

// newAgentHandler builds the slave handler for the mesh node, forking task
// sessions from the local agent session.
func newAgentHandler(m *meshNode, sess *coding.AgentSession, qTimeout time.Duration, meshTools []*agent.AgentTool) *agentHandler {
	cfg := m.cfg
	max := cfg.MaxConcurrentTasks
	if max <= 0 {
		max = defaultMaxConcurrentTasks
	}
	if qTimeout <= 0 {
		qTimeout = defaultQuestionTimeout
	}
	return &agentHandler{
		m:               m,
		sess:            sess,
		cfg:             cfg,
		meshTools:       meshTools,
		questionTimeout: qTimeout,
		defaultTimeout:  defaultTaskTimeout,
		maxConcurrent:   max,
		tasks:           map[string]*taskRecord{},
	}
}

// Handle processes envelopes the slave role cares about. It returns
// immediately: task execution runs in its own goroutine.
func (h *agentHandler) Handle(env mesh.Envelope) error {
	switch env.Type {
	case mesh.MsgTask:
		go h.handleTask(env)
	case mesh.MsgInterrupt:
		h.handleInterrupt(env)
	case mesh.MsgAnswer:
		h.handleAnswer(env)
	}
	return nil
}

// handleTask validates an incoming task and, if accepted, forks and runs it.
// Any rejection is an immediate MsgError tied to the task id (the master does
// not wait for a timeout).
func (h *agentHandler) handleTask(env mesh.Envelope) {
	var tp mesh.TaskPayload
	if err := json.Unmarshal(env.Payload, &tp); err != nil {
		h.sendError(env.From, env.ID, "bad-request", "invalid task payload: "+err.Error())
		return
	}
	if !h.authorized(env.From) {
		h.sendError(env.From, env.ID, "denied", "peer not authorized for remote tasks")
		return
	}
	// 随任务下发的文件有写副作用（任务会读写这些文件），只读任务拒绝。
	if len(tp.Files) > 0 && h.taskLevel() == string(coding.RemoteReadOnly) {
		h.sendError(env.From, env.ID, "denied", "task files require remoteTaskLevel write or full")
		return
	}
	cwd, err := h.resolveCwd(tp.Cwd)
	if err != nil {
		h.sendError(env.From, env.ID, "bad-cwd", err.Error())
		return
	}
	if !h.acquire() {
		h.sendError(env.From, env.ID, "busy", fmt.Sprintf("max concurrent tasks reached (%d)", h.maxConcurrent))
		return
	}
	model, err := h.resolveModel(tp.Model)
	if err != nil {
		h.release()
		h.sendError(env.From, env.ID, "denied", "unresolvable model: "+err.Error())
		return
	}
	h.runTask(env, tp, cwd, model)
}

// authorized reports whether the peer may dispatch remote tasks: the node
// must allow remote tasks, and when a trustedPeers whitelist is configured
// the sender must be on it (same rule as direct tool calls, see peerTrusted).
func (h *agentHandler) authorized(from string) bool {
	return peerTrusted(h.cfg.AllowRemoteTasks, h.cfg.TrustedPeers, from)
}

// resolveCwd maps a task cwd (empty / alias / absolute) to a local working
// directory. Aliases and absolute paths must be in the mesh.json cwds
// whitelist; empty uses the local default cwd.
func (h *agentHandler) resolveCwd(want string) (string, error) {
	if want == "" {
		return h.sess.Cwd, nil
	}
	if abs, ok := h.cfg.Cwds[want]; ok {
		return abs, nil
	}
	if filepath.IsAbs(want) {
		clean := filepath.Clean(want)
		for _, v := range h.cfg.Cwds {
			if filepath.Clean(v) == clean {
				return clean, nil
			}
		}
	}
	return "", fmt.Errorf("cwd %q not allowed (not in mesh.json cwds)", want)
}

// acquire reserves a task slot; release frees it. Both are called under the
// handler mutex and report the live load to the node's profile.
func (h *agentHandler) acquire() bool {
	h.mu.Lock()
	if h.running >= h.maxConcurrent {
		h.mu.Unlock()
		return false
	}
	h.running++
	n := h.running
	h.mu.Unlock()
	h.m.SetTaskLoad(n)
	return true
}

func (h *agentHandler) release() {
	h.mu.Lock()
	h.running--
	n := h.running
	h.mu.Unlock()
	h.m.SetTaskLoad(n)
}

// resolveModel resolves the task model hint ("" uses the local session's
// model). An unresolvable hint rejects the task.
func (h *agentHandler) resolveModel(hint string) (*ai.Model, error) {
	if strings.TrimSpace(hint) == "" {
		return h.sess.Agent.State().Model, nil
	}
	model, _, err := h.sess.ModelRuntime.ResolveModel(hint)
	if err != nil {
		return nil, err
	}
	return model, nil
}

// runTask forks the per-task session, applies the context snapshot, runs the
// task, and sends the result. One task = one isolated session, so concurrent
// tasks never interfere.
func (h *agentHandler) runTask(env mesh.Envelope, tp mesh.TaskPayload, cwd string, model *ai.Model) {
	taskID := env.ID
	rec := &taskRecord{
		id:        taskID,
		from:      env.From,
		interrupt: make(chan struct{}),
		answer:    make(chan string, 1),
		done:      make(chan struct{}),
	}
	h.mu.Lock()
	h.tasks[taskID] = rec
	h.mu.Unlock()
	logx.Infof("[mesh] task %s accepted from %s cwd=%s session=%q context=%s timeout=%ds",
		taskID, env.From, cwd, tp.Session, tp.Context, tp.Timeout)
	defer func() {
		h.mu.Lock()
		sess := rec.sess
		delete(h.tasks, taskID)
		h.mu.Unlock()
		if sess != nil {
			sess.SessionManager.UnregisterActive(sess)
		}
		h.release()
		close(rec.done)
	}()

	contact := []*agent.AgentTool{h.contactTool(rec)}
	level := coding.RemoteToolLevel(h.taskLevel())
	// Full-level tasks carry the node's mesh tools (mesh_forward / mesh_peers
	// / remote_agent_status): the master can ask this node to configure its
	// OWN forwarding or inspect the mesh. Lower levels never get them —
	// forwarding mutates node config, which read-only/write must not allow.
	if level == coding.RemoteFull {
		contact = append(contact, h.meshTools...)
	}
	var sess *coding.AgentSession
	if tp.Session != "" {
		// Task continuity: resume a previous remote-task session. Only sessions
		// the slave forked for remote tasks (Kind "remote") may be continued —
		// a remote client can never resume a user's local session.
		target, err := h.sess.ResolveSession(tp.Session)
		if err != nil || target.Header.Kind != coding.SessionKindRemote {
			h.sendError(env.From, taskID, "bad-session", "no prior remote-task session "+tp.Session)
			return
		}
		sess, err = h.sess.ForkRemoteTo(target, level, model, contact)
		if err != nil {
			h.sendResult(env.From, taskID, mesh.ResultPayload{Status: "error", Error: "session continue failed: " + err.Error()})
			return
		}
	} else {
		var err error
		sess, err = h.sess.ForkRemote(cwd, level, model, contact)
		if err != nil {
			h.sendResult(env.From, taskID, mesh.ResultPayload{Status: "error", Error: "session fork failed: " + err.Error()})
			return
		}
		if err := h.applyContext(sess, tp); err != nil {
			h.sendResult(env.From, taskID, mesh.ResultPayload{Status: "error", Error: "context setup failed: " + err.Error()})
			return
		}
	}
	// Record who dispatched this task (the UIs label the remote session's
	// message cards with it) and register the live instance so a web window
	// that opens this session subscribes to its events while it runs. rec.sess
	// is published under the handler mutex so observers (tests, web) never read
	// it half-written.
	masterName := h.m.peerName(env.From)
	sess.CurrentSession().Header.SourceName = masterName
	sess.CurrentSession().Header.SourcePeerID = env.From
	sess.SessionManager.RegisterActive(sess)
	h.mu.Lock()
	rec.sess = sess
	h.mu.Unlock()

	// 随任务下发的文件：在任务文本前注入引用行（文件已在本地暂存区），
	// agent 用 read 工具按需取用。
	taskText := tp.Task
	if refs := h.fileRefLines(env.From, tp.Files); refs != "" {
		taskText = refs + "\n\n" + tp.Task
	}

	// Forward coarse agent events as progress (never the transcript; the
	// master shows progress without injecting it into the conversation).
	unsub, _ := sess.Subscribe(func(ev agent.AgentEvent) error {
		h.maybeProgress(rec, ev)
		return nil
	}, coding.ConsumerHeadless)
	defer unsub()

	timeout := time.Duration(tp.Timeout) * time.Second
	if timeout <= 0 {
		timeout = h.defaultTimeout
	}
	runCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	rec.cancel = cancel

	done := make(chan error, 1)
	go func() { done <- sess.Prompt(taskText) }()

	aborted := false
	var runErr error
	select {
	case runErr = <-done:
	case <-rec.interrupt:
		aborted = true
		sess.Abort()
		runErr = <-done
	case <-runCtx.Done():
		aborted = true
		sess.Abort()
		runErr = <-done
	}

	// A failed Prompt (persistence error, recovered panic, …) used to be
	// swallowed, surfacing as a hollow "completed" with whatever text the
	// run happened to produce. Report it so the master knows the task died.
	res := h.resultFromSession(sess, aborted, runErr)
	res.TaskID = taskID
	logx.Infof("[mesh] task %s finished status=%s runErr=%v", taskID, res.Status, runErr)
	// Report the session id so the master can continue this task conversation
	// in a later dispatch (remote_agent(session=<id>)).
	res.Session = sess.CurrentSession().Header.ID
	h.sendResult(env.From, taskID, res)
}

// taskLevel returns the configured remote task permission level.
func (h *agentHandler) taskLevel() string {
	level := h.cfg.RemoteTaskLevel
	if level == "" {
		return string(coding.RemoteReadOnly)
	}
	return level
}

// fileRefLines 把随任务下发的文件渲染为引用行（与 web 附件引用行同格式），
// 文件在从侧暂存区；找不到的文件记日志并跳过（不让单个坏引用卡死任务），
// 序号与分母都按实际渲染的文件计。
func (h *agentHandler) fileRefLines(from string, refs []mesh.RemoteFileRef) string {
	if len(refs) == 0 {
		return ""
	}
	store := h.m.uploadStore()
	found := make([]*uploads.Meta, 0, len(refs))
	for _, ref := range refs {
		if store == nil {
			break
		}
		meta, err := store.Meta(ref.UploadID)
		if err != nil {
			meshLogf("task file %s (upload %s) from %s not found in local store", ref.Name, ref.UploadID, from)
			continue
		}
		found = append(found, meta)
	}
	var b strings.Builder
	for i, meta := range found {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[附件 %d/%d] %s\n  size=%d sha256=%s 可用 read 工具读取", i+1, len(found), meta.Path, meta.Size, shortSHA(meta.SHA256))
	}
	return strings.TrimSpace(b.String())
}

// applyContext injects the task's context snapshot into the fork session
// according to task.Context: fresh = empty, summary = a user message with the
// master's summary, fork = the recent transcript messages.
func (h *agentHandler) applyContext(sess *coding.AgentSession, tp mesh.TaskPayload) error {
	if len(tp.Snapshot) == 0 {
		return nil
	}
	raw, err := decompressSnapshot(tp.Snapshot)
	if err != nil {
		return err
	}
	switch tp.Context {
	case "summary":
		sess.Agent.SetMessages([]*ai.Message{ai.NewUserTextMessage("<remote-context-summary>\n" + string(raw) + "\n</remote-context-summary>")})
	case "fork":
		var msgs []*ai.Message
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return fmt.Errorf("fork snapshot: %w", err)
		}
		sess.Agent.SetMessages(msgs)
	}
	return nil
}

// resultFromSession derives the result status/output from the fork session
// transcript after the run (or abort) finished. runErr is the Prompt return
// value: a failed run reports as an error even when the transcript looks
// incomplete-but-fine (e.g. it died between tool calls).
func (h *agentHandler) resultFromSession(sess *coding.AgentSession, aborted bool, runErr error) mesh.ResultPayload {
	if aborted {
		return mesh.ResultPayload{Status: "aborted", Error: "task aborted"}
	}
	if runErr != nil {
		return mesh.ResultPayload{Status: "error", Error: "task run failed: " + runErr.Error()}
	}
	msgs := sess.CurrentMessages()
	var last *ai.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == ai.RoleAssistant {
			last = msgs[i]
			break
		}
	}
	if last == nil {
		// The run never produced an assistant message (e.g. the provider
		// failed before the first token): report it as an error instead of a
		// hollow "completed", so the master knows the task did nothing.
		return mesh.ResultPayload{Status: "error", Error: "task produced no assistant output"}
	}
	switch last.StopReason {
	case ai.StopReasonError:
		return mesh.ResultPayload{Status: "error", Error: last.ErrorMessage}
	case ai.StopReasonAborted:
		return mesh.ResultPayload{Status: "aborted", Error: "task aborted"}
	}
	out := last.Text()
	truncated := false
	if len(out) > tools.MaxOutputBytes {
		out = out[:tools.MaxOutputBytes]
		truncated = true
	}
	return mesh.ResultPayload{Status: "completed", Output: out, Truncated: truncated}
}

// contactTool builds the per-task tool the remote agent calls to ask the
// master for a decision (≈ pi-subagents contact_supervisor). It blocks until
// the master answers (MsgAnswer routed by task id), the run is interrupted,
// or the question times out.
func (h *agentHandler) contactTool(rec *taskRecord) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "contact_supervisor",
		Label:       "Ask the master for a decision",
		Description: "Ask the supervising node (the master that dispatched this task) a question and wait for its answer. Use when you need a decision, missing input, or authorization that only the master can provide. The answer does not enter the master's conversation.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string", "description": "the question to ask the master"},
			},
			"required": []string{"message"},
		},
		ExecutionMode: agent.ToolExecutionSequential,
		Execute: func(_ string, params map[string]any, ctx context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			msg, _ := params["message"].(string)
			msg = strings.TrimSpace(msg)
			if msg == "" {
				return tools.ToolError("contact_supervisor: message is required"), nil
			}
			if err := h.sendQuestion(rec, msg); err != nil {
				return tools.ToolError("contact_supervisor: " + err.Error()), nil
			}
			select {
			case ans := <-rec.answer:
				return tools.ToolResult("Supervisor answered: " + ans), nil
			case <-ctx.Done():
				return tools.ToolError("contact_supervisor: interrupted"), nil
			case <-time.After(h.questionTimeout):
				return tools.ToolError("contact_supervisor: the master did not answer in time; decide on your own or report the blocker"), nil
			}
		},
	}
}

// sendQuestion asks the master a question for a task.
func (h *agentHandler) sendQuestion(rec *taskRecord, q string) error {
	data, err := json.Marshal(mesh.QuestionPayload{TaskID: rec.id, Prompt: q})
	if err != nil {
		return err
	}
	return h.m.Send(rec.from, mesh.Envelope{Type: mesh.MsgQuestion, To: rec.from, ReplyTo: rec.id, Payload: data})
}

// handleAnswer routes an answer back to the waiting contact_supervisor tool.
func (h *agentHandler) handleAnswer(env mesh.Envelope) {
	var ap mesh.AnswerPayload
	if err := json.Unmarshal(env.Payload, &ap); err != nil {
		return
	}
	h.mu.Lock()
	rec := h.tasks[env.ReplyTo]
	h.mu.Unlock()
	if rec == nil {
		return
	}
	select {
	case rec.answer <- ap.Answer:
	default:
	}
}

// handleInterrupt aborts a running task on MsgInterrupt.
func (h *agentHandler) handleInterrupt(env mesh.Envelope) {
	h.mu.Lock()
	rec := h.tasks[env.ReplyTo]
	h.mu.Unlock()
	if rec == nil {
		return
	}
	select {
	case <-rec.interrupt:
	default:
		close(rec.interrupt)
	}
}

// maybeProgress forwards coarse agent events as MsgProgress (turn/tool
// lifecycle only — never the transcript).
func (h *agentHandler) maybeProgress(rec *taskRecord, ev agent.AgentEvent) {
	var msg string
	switch ev.Type {
	case agent.EventTurnStart:
		msg = "turn started"
	case agent.EventToolExecutionStart:
		msg = "running tool " + ev.ToolName
	case agent.EventToolExecutionEnd:
		msg = "finished tool " + ev.ToolName
	default:
		return
	}
	data, _ := json.Marshal(mesh.ProgressPayload{TaskID: rec.id, Message: msg})
	_ = h.m.Send(rec.from, mesh.Envelope{Type: mesh.MsgProgress, To: rec.from, ReplyTo: rec.id, Payload: data})
}

// sendResult reports a task outcome to the master.
func (h *agentHandler) sendResult(to, taskID string, res mesh.ResultPayload) {
	if res.Truncated {
		res.Output += "\n[output truncated; use session-snap to pull the full session]"
	}
	data, err := json.Marshal(res)
	if err != nil {
		return
	}
	_ = h.m.Send(to, mesh.Envelope{Type: mesh.MsgResult, To: to, ReplyTo: taskID, Payload: data})
}

// sendError sends an immediate rejection tied to the task id (master does not
// wait for a timeout).
func (h *agentHandler) sendError(to, taskID, code, msg string) {
	logx.Warnf("[mesh] task %s rejected: %s: %s", taskID, code, msg)
	data, _ := json.Marshal(mesh.ErrorPayload{Code: code, Message: msg})
	_ = h.m.Send(to, mesh.Envelope{Type: mesh.MsgError, To: to, ReplyTo: taskID, Payload: data})
}

// compressSnapshot gzip 压缩 MsgTask wire 字段的上下文快照
// （会话文本压缩率高；frame 上限远大于任何实际快照）
func compressSnapshot(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decompressSnapshot 解压 compressSnapshot 的结果
func decompressSnapshot(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	defer zr.Close()
	return io.ReadAll(zr)
}
