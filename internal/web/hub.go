package web

import (
	"context"
	"doge/internal/ai"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"doge/internal/agent"
	"doge/internal/coding/permissions"
)

const (
	askTimeout   = 5 * time.Minute // mirrors the TUI's generous prompt budget
	clientBuffer = 512             // max queued broadcast frames per client
	maxInbound   = 1 << 20         // max inbound WS message size (1 MiB)
)

// Hub manages WebSocket clients and routes agent events to them. Each client
// belongs to a window (its clientID); agent events are pushed only to the
// clients of the window whose session produced them (per-window session
// isolation). Session-list changes and permission asks remain global (any
// window may see/answer them).
//
// The event path must never stall the agent loop: listeners are invoked
// synchronously, so broadcast only enqueues to per-client buffered channels
// and disconnects clients that fall behind.
type Hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}
	windows map[string]map[*client]struct{} // windowID → clients

	askMu    sync.Mutex
	asks     map[string]chan permissions.AskResponse
	serialID int64

	// snapshotFn builds the session_info frame a new client of a window needs
	// to rebuild its view (per window, so it reflects that window's session).
	snapshotFn func(windowID string) ([]byte, error)

	// windowChange is invoked with a window's connection count after any
	// connect/disconnect, so the server can sync that window's interactivity
	// kind (a connected browser means a human may answer).
	windowChange func(windowID string, n int)
}

// defaultWindowID is used for WebSocket connections that carry no client id
// (older clients / scripts): they attach to the shared-session window.
const defaultWindowID = "default"

// client is one connected WebSocket peer, bound to a window.
type client struct {
	conn      *websocket.Conn
	windowID  string
	send      chan []byte
	done      chan struct{}
	closeSend sync.Once
	closeDone sync.Once
}

func (c *client) markClosed() { c.closeSend.Do(func() { close(c.send) }) }

func newHub() *Hub {
	return &Hub{
		clients: map[*client]struct{}{},
		windows: map[string]map[*client]struct{}{},
		asks:    map[string]chan permissions.AskResponse{},
	}
}

// SetSnapshot installs the per-window frame builder for new connections.
func (h *Hub) SetSnapshot(fn func(windowID string) ([]byte, error)) { h.snapshotFn = fn }

// SetWindowChange installs a callback invoked (with that window's connection
// count) after every connect/disconnect of any window.
func (h *Hub) SetWindowChange(fn func(windowID string, n int)) {
	h.mu.Lock()
	h.windowChange = fn
	windows := make(map[string]int, len(h.windows))
	for id, cs := range h.windows {
		windows[id] = len(cs)
	}
	h.mu.Unlock()
	if fn != nil {
		for id, n := range windows {
			fn(id, n)
		}
	}
}

// addClient registers a client under its window.
func (h *Hub) addClient(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	cs := h.windows[c.windowID]
	if cs == nil {
		cs = map[*client]struct{}{}
		h.windows[c.windowID] = cs
	}
	cs[c] = struct{}{}
	n := len(cs)
	h.mu.Unlock()
	h.onWindowChange(c.windowID, n)
}

// onWindowChange reports a window's connection count to the install callback.
func (h *Hub) onWindowChange(windowID string, n int) {
	h.mu.Lock()
	fn := h.windowChange
	h.mu.Unlock()
	if fn != nil {
		fn(windowID, n)
	}
}

// windowConns 返回窗口当前活跃的 WebSocket 连接数（写权活跃度探针用）
func (h *Hub) windowConns(windowID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.windows[windowID])
}

// AgentListener implements agent.AgentListener; it broadcasts to all
// connected clients without ever blocking the agent loop.
func (h *Hub) AgentListener(ev agent.AgentEvent) error {
	data, err := marshalAgentEvent(ev)
	if err != nil {
		return err
	}
	h.broadcast(data)
	return nil
}

// broadcast enqueues a frame to every client (session-list changes,
// permission asks); slow clients are disconnected rather than allowed to back
// up the queue.
func (h *Hub) broadcast(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- data:
		default:
			c.markClosed() // too slow to keep up; drop it
			delete(h.clients, c)
			delete(h.windows[c.windowID], c)
		}
	}
}

// broadcastTo enqueues a frame to the clients of one window only (per-window
// session events). Slow clients in that window are dropped, never blocking
// the agent loop.
func (h *Hub) broadcastTo(windowID string, data []byte) {
	h.mu.Lock()
	cs := h.windows[windowID]
	for c := range cs {
		select {
		case c.send <- data:
		default:
			c.markClosed()
			delete(h.clients, c)
			delete(cs, c)
		}
	}
	h.mu.Unlock()
	if len(cs) > 0 && h.windowChange != nil {
		h.onWindowChange(windowID, len(cs))
	}
}

// enqueue sends a frame to a single client without blocking.
func (h *Hub) enqueue(c *client, data []byte) {
	select {
	case c.send <- data:
	default:
	}
}

// Ask broadcasts a permission prompt and waits for the first response from
// any client. With no clients connected it denies immediately, keeping
// unattended runs from stalling.
func (h *Hub) Ask(req permissions.AskRequest) (permissions.AskResponse, error) {
	h.mu.Lock()
	if len(h.clients) == 0 {
		h.mu.Unlock()
		return permissions.AskResponse{Deny: true}, errors.New("no web clients connected")
	}
	h.mu.Unlock()

	id := h.nextAskID()
	reply := make(chan permissions.AskResponse, 1)
	h.askMu.Lock()
	h.asks[id] = reply
	h.askMu.Unlock()
	defer func() {
		h.askMu.Lock()
		delete(h.asks, id)
		h.askMu.Unlock()
	}()

	payload, _ := json.Marshal(map[string]any{
		"type":    "permission_ask",
		"id":      id,
		"request": req,
	})
	h.broadcast(payload)

	select {
	case resp := <-reply:
		return resp, nil
	case <-time.After(askTimeout):
		return permissions.AskResponse{}, errors.New("permission prompt timed out")
	}
}

func (h *Hub) nextAskID() string {
	h.askMu.Lock()
	h.serialID++
	id := h.serialID
	h.askMu.Unlock()
	return "ask-" + strconv.FormatInt(id, 10)
}

// handleInbound processes a JSON message from a client. Only
// permission_response is meaningful; anything else is ignored.
func (h *Hub) handleInbound(data []byte) {
	var msg struct {
		Type     string                  `json:"type"`
		ID       string                  `json:"id"`
		Response permissions.AskResponse `json:"response"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	if msg.Type != "permission_response" || msg.ID == "" {
		return
	}
	h.answerPermission(msg.ID, msg.Response)
}

// answerPermission delivers a permission decision to a pending ask — the REST
// path (POST /api/permission/answer) of the WS permission_response frame. It
// reports whether the ask was still pending.
func (h *Hub) answerPermission(id string, resp permissions.AskResponse) bool {
	h.askMu.Lock()
	reply := h.asks[id]
	h.askMu.Unlock()
	if reply == nil {
		return false
	}
	select {
	case reply <- resp:
	default:
	}
	return true
}

// serveWS upgrades a request to a WebSocket and runs the client loops until
// disconnect or server shutdown. The caller must have already run requireAuth.
func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"}, // token auth already gates access
	})
	if err != nil {
		return
	}
	// We read (permission responses) and write (broadcasts) ourselves, so no
	// CloseRead: its background reader would consume inbound frames and starve
	// the response path below.
	ctx, cancel := context.WithCancel(r.Context())

	windowID := strings.TrimSpace(r.URL.Query().Get("client"))
	if windowID == "" {
		windowID = defaultWindowID
	}

	// Inbound frames are tiny (permission responses); cap them so a stray
	// binary blob cannot balloon memory (the coder/websocket default is a
	// mere 32 KiB, which the protocol doc already promises as 1 MiB).
	conn.SetReadLimit(maxInbound)

	c := &client{
		conn:     conn,
		windowID: windowID,
		send:     make(chan []byte, clientBuffer),
		done:     make(chan struct{}),
	}
	h.addClient(c)

	// Push a full snapshot for this client's window, so the view is current
	// even if a run started while the page was loading or reconnecting.
	if h.snapshotFn != nil {
		if snap, err := h.snapshotFn(windowID); err == nil {
			h.enqueue(c, snap)
		}
	}

	go c.writeLoop(ctx, cancel)

	defer func() {
		h.disconnect(c)
		cancel()
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		h.handleInbound(data)
	}
}

// disconnect removes a client and closes its channels and socket (which
// also unblocks a pending Conn.Read during graceful shutdown).
func (h *Hub) disconnect(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		c.markClosed()
	}
	n := 0
	if cs := h.windows[c.windowID]; cs != nil {
		if _, ok := cs[c]; ok {
			delete(cs, c)
		}
		n = len(cs)
		if n == 0 {
			delete(h.windows, c.windowID)
		}
	}
	h.mu.Unlock()
	h.onWindowChange(c.windowID, n)
	c.closeDone.Do(func() { close(c.done) })
	_ = c.conn.Close(websocket.StatusGoingAway, "server shutdown")
}

// writeLoop drains the outbound queue to the socket.
func (c *client) writeLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	for {
		select {
		case <-c.done:
			return
		case data, ok := <-c.send:
			if !ok {
				_ = c.conn.Close(websocket.StatusGoingAway, "slow client")
				return
			}
			if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
	}
}

// shutdown closes all client connections (used on graceful server stop).
func (h *Hub) shutdown() {
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		h.disconnect(c)
	}
}

type AgentEvent struct {
	Type string `json:"type"`

	// agent_end
	Messages []*ai.Message `json:"messages,omitempty"`
	// turn_end
	ToolResults []*ai.Message `json:"toolResults,omitempty"`

	// message_start / message_update / message_end / turn_end
	Message *ai.Message `json:"message,omitempty"`

	// tool_execution_start / tool_execution_update / tool_execution_end
	ToolCallID    string `json:"toolCallId,omitempty"`
	ToolName      string `json:"toolName,omitempty"`
	Args          any    `json:"args,omitempty"`
	PartialResult any    `json:"partialResult,omitempty"`
	Result        any    `json:"result,omitempty"`
	IsError       bool   `json:"isError,omitempty"`

	// question (mesh remote-task decision request)
	Question *agent.Question `json:"question,omitempty"`
	// mesh_progress
	MeshProgress *agent.MeshProgress `json:"meshProgress,omitempty"`
}

// AgentEventFrom maps an agent.AgentEvent to its wire representation.
func AgentEventFrom(ev agent.AgentEvent) AgentEvent {
	out := AgentEvent{Type: ev.Type, Question: ev.Question, MeshProgress: ev.MeshProgress}
	switch {
	case ev.Message != nil:
		out.Message = ev.Message
	case ev.Messages != nil:
		out.Messages = ev.Messages
	case ev.ToolResults != nil:
		out.ToolResults = ev.ToolResults
	}
	if ev.ToolCallID != "" {
		out.ToolCallID = ev.ToolCallID
		out.ToolName = ev.ToolName
		out.Args = ev.Args
	}
	if ev.PartialResult != nil {
		out.PartialResult = ev.PartialResult
	}
	if ev.Result != nil {
		out.Result = map[string]any{
			"isError":    ev.IsError,
			"content":    ev.Result.Content,
			"details":    ev.Result.Details,
			"addedTools": ev.Result.AddedToolNames,
		}
		out.IsError = ev.IsError
	}
	return out
}

// marshalAgentEvent converts an event to JSON for broadcast.
func marshalAgentEvent(ev agent.AgentEvent) ([]byte, error) {
	return json.Marshal(AgentEventFrom(ev))
}
