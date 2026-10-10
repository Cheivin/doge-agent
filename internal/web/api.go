package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"doge/internal/coding"
	"doge/internal/coding/permissions"
)

// handleLogin validates the presented token and issues a session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Trim before comparing: copying the token off a terminal or the token
	// file often drags in a trailing newline/space, which must not fail the
	// login.
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token required")
		return
	}
	if value, ok := s.auth.login(req.Token); ok {
		setSessionCookie(w, value)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeError(w, http.StatusUnauthorized, "invalid token")
}

// handleLogout revokes the session and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		s.auth.logout(c.Value)
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleStatus reports the live agent state of the request's window.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backendFor(r).Status())
}

// handleStats reports the current session's token usage, cache rate, and
// estimated cost.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backendFor(r).Stats())
}

// handlePrompt injects a user message. The run executes asynchronously (the
// agent loop is blocking) and agent events stream over the WebSocket. A 409
// is returned when a run is already in flight; a 403 when the window does
// not hold write control of the session (another window / the TUI is using
// it). Optional "attachments" reference staged uploads (POST /api/uploads):
// images go in as multimodal content, small texts inline, everything else as
// read-tool references.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text        string   `json:"text"`
		Attachments []string `json:"attachments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" && len(req.Attachments) == 0 {
		writeError(w, http.StatusBadRequest, "empty prompt")
		return
	}
	// Resolve attachments up front so an unknown id fails synchronously.
	var atts []coding.Attachment
	if len(req.Attachments) > 0 {
		if s.uploads == nil {
			writeError(w, http.StatusServiceUnavailable, "uploads not available")
			return
		}
		for _, id := range req.Attachments {
			meta, err := s.uploads.Meta(strings.TrimSpace(id))
			if err != nil {
				writeError(w, http.StatusBadRequest, "unknown attachment: "+id)
				return
			}
			atts = append(atts, coding.AttachmentFromMeta(meta))
		}
	}
	b := s.backendFor(r)
	if sb, ok := b.(*sessionBackend); ok && !sb.Writable() {
		writeError(w, http.StatusForbidden, errSessionReadOnly.Error())
		return
	}
	if b.Status().Streaming {
		writeError(w, http.StatusConflict, "agent is busy")
		return
	}
	// Prompt blocks for the whole run; hand it to a goroutine and acknowledge.
	// The agent's internal mutex guards against a racing second prompt.
	if len(atts) == 0 {
		go func() { _ = b.Prompt(req.Text) }()
	} else {
		go func() { _ = b.PromptWithAttachments(req.Text, atts) }()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// handleAbort cancels the current run.
func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	b := s.backendFor(r)
	if sb, ok := b.(*sessionBackend); ok && !sb.Writable() {
		writeError(w, http.StatusForbidden, errSessionReadOnly.Error())
		return
	}
	b.Abort()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleCompact triggers context compaction.
func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	b := s.backendFor(r)
	if err := b.Compact(); err != nil {
		writeReadOnlyOrError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReload re-reads models.json, auth.json and permission.json (global),
// and lands any pending per-session MCP toggle (see /api/session/mcp — the
// manual "刷新生效" step).
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	b := s.backendFor(r)
	mcpPending := b.McpInfo().Pending
	if err := b.ReloadConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mcpApplied": mcpPending})
}

// handleModels lists available models (resolvable key or keyless local
// endpoint; the active model always included). ?all=1 lists the full catalog
// instead — including models that currently lack credentials — for
// configuration UIs.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	var models []ModelInfo
	if r.URL.Query().Get("all") == "1" {
		models = s.backendFor(r).ListAllModels()
	} else {
		models = s.backendFor(r).ListModels()
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// handleModel switches the active model.
func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Spec string `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Spec) == "" {
		writeError(w, http.StatusBadRequest, "empty model spec")
		return
	}
	b := s.backendFor(r)
	if sb, ok := b.(*sessionBackend); ok && !sb.Writable() {
		writeError(w, http.StatusForbidden, errSessionReadOnly.Error())
		return
	}
	if err := b.SwitchModel(req.Spec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// thinkingLevels POST /api/thinking 接受的推理级别
var thinkingLevels = map[string]bool{"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true}

// handleThinking 调整推理级别（与 TUI /model spec:thinking 同一链路，会落
// thinking_level_change entry）。
func (s *Server) handleThinking(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Level = strings.TrimSpace(req.Level)
	if !thinkingLevels[req.Level] {
		writeError(w, http.StatusBadRequest, "invalid thinking level (want minimal|low|medium|high|xhigh)")
		return
	}
	if err := s.backendFor(r).SetThinkingLevel(req.Level); err != nil {
		writeReadOnlyOrError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSession returns the current session and its transcript.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backendFor(r).SessionInfo())
}

// handleSessionList lists persisted sessions (read only).
func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.backendFor(r).ListSessions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list sessions failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// FsDir 目录选择器用的目录项
type FsDir struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// handleFsList 列出指定目录下的子目录（新建会话的目录选择器数据源），
// path 为空时从用户主目录开始；隐藏目录（.开头）不展示。
func (s *Server) handleFsList(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		path = home
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dirs := []FsDir{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dirs = append(dirs, FsDir{Name: e.Name(), Path: filepath.Join(abs, e.Name())})
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		parent = "" // 已到根
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": abs, "parent": parent, "dirs": dirs})
}

// handleSessionNew starts a fresh session and resets this window's view only.
// Creating a session is an independent action: the window owns the new session
// regardless of who holds control of its current (possibly read-only) view —
// this is the escape hatch for a window observing a session another window or
// the TUI is using.
func (s *Server) handleSessionNew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cwd string `json:"cwd"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // body 可选，空 cwd 用当前目录
	}
	b := s.backendFor(r)
	if err := b.NewSessionIn(req.Cwd); err != nil {
		writeReadOnlyOrError(w, err)
		return
	}
	s.hub.broadcastTo(clientID(r), []byte(`{"type":"session_reset"}`))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSessionSwitch restores an existing session (by id or path) and resets
// this window's view only.
func (s *Server) handleSessionSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.backendFor(r).SwitchSession(req.ID); err != nil {
		writeReadOnlyOrError(w, err)
		return
	}
	s.hub.broadcastTo(clientID(r), []byte(`{"type":"session_reset"}`))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSessionClaim requests write control of the current session: an
// uncontended session is owned immediately; otherwise the window joins the
// FIFO wait queue and stays read-only until the current owner hands over.
func (s *Server) handleSessionClaim(w http.ResponseWriter, r *http.Request) {
	b := s.backendFor(r)
	sb, ok := b.(*sessionBackend)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"owned": true, "queued": false})
		return
	}
	owned, queued := sb.ClaimControl()
	writeJSON(w, http.StatusOK, map[string]any{"owned": owned, "queued": queued})
}

// handleSessionTakeover forcibly transfers write control of the current
// session to the requesting window, releasing the previous owner (whoever it
// is) without waiting for it to hand off. This is the explicit "强夺" path for
// a blocked/queued window that does not want to (or cannot) wait for the old
// owner to release. The response carries the previous owner's id so clients
// can report what happened.
func (s *Server) handleSessionTakeover(w http.ResponseWriter, r *http.Request) {
	b := s.backendFor(r)
	sb, ok := b.(*sessionBackend)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"owned": true, "prevOwner": ""})
		return
	}
	owned, prevOwner := sb.TakeoverControl()
	writeJSON(w, http.StatusOK, map[string]any{"owned": owned, "prevOwner": prevOwner})
}

// handleSessionDelete removes a persisted session; active sessions are
// rejected by the backend.
func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.backendFor(r).DeleteSession(req.ID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.hub.broadcast([]byte(`{"type":"session_list_changed"}`))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleTurnEdit rewinds the conversation to the messageSeq-th user message,
// replaces it with the edited text, and re-runs from there (asynchronously).
func (s *Server) handleTurnEdit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seq  int    `json:"seq"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "empty message")
		return
	}
	b := s.backendFor(r)
	if sb, ok := b.(*sessionBackend); ok && !sb.Writable() {
		writeError(w, http.StatusForbidden, errSessionReadOnly.Error())
		return
	}
	if err := b.RewriteTurn(req.Seq, req.Text); err != nil {
		writeReadOnlyOrError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// handleQuestionAnswer delivers a human's answer to a pending mesh question.
// The answer bypasses the agent loop entirely (see Backend.AnswerQuestion).
func (s *Server) handleQuestionAnswer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.Text = strings.TrimSpace(req.Text)
	if req.ID == "" || req.Text == "" {
		writeError(w, http.StatusBadRequest, "id and text are required")
		return
	}
	if !s.backendFor(r).AnswerQuestion(req.ID, req.Text) {
		writeError(w, http.StatusNotFound, "question not pending")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePermissionAnswer delivers a permission decision via REST — the same
// thing as the WS permission_response frame, for clients that do not keep a
// WebSocket open.
func (s *Server) handlePermissionAnswer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          string `json:"id"`
		AllowOnce   bool   `json:"allowOnce"`
		AllowAlways bool   `json:"allowAlways"`
		Deny        bool   `json:"deny"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	resp := permissions.AskResponse{
		AllowOnce:   req.AllowOnce,
		AllowAlways: req.AllowAlways,
		Deny:        req.Deny,
	}
	if !s.hub.answerPermission(req.ID, resp) {
		writeError(w, http.StatusNotFound, "permission ask not pending")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// writeReadOnlyOrError maps a read-only rejection to 403 and any other error
// to 409.
func writeReadOnlyOrError(w http.ResponseWriter, err error) {
	if errors.Is(err, errSessionReadOnly) {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	writeError(w, http.StatusConflict, err.Error())
}

// writeJSON writes a JSON response body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error body.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
