package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"doge/internal/agent"
	"doge/internal/ai"
	"doge/internal/coding/permissions"
)

// newWSClient spins up the hub behind an httptest server and dials it.
func newWSClient(t *testing.T, h *Hub) (*websocket.Conn, *httptest.Server, context.Context) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(h.serveWS))
	t.Cleanup(ts.Close)
	wsURL := "ws" + ts.URL[4:] + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn, ts, ctx
}

func readJSON(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return mustJSON(t, data)
}

func mustJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
	return m
}

func TestBroadcastToClient(t *testing.T) {
	h := newHub()
	h.SetSnapshot(func(string) ([]byte, error) { return []byte(`{"type":"hi"}`), nil })
	conn, _, ctx := newWSClient(t, h)

	// Wait for the registration snapshot to guarantee the client is in.
	if m := readJSON(t, ctx, conn); m["type"] != "hi" {
		t.Fatalf("first frame = %#v", m)
	}

	ev := agent.AgentEvent{
		Type:    agent.EventMessageUpdate,
		Message: &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{ai.TextBlock("hey")}},
	}
	if err := h.AgentListener(ev); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, ctx, conn)
	if m["type"] != "message_update" {
		t.Fatalf("broadcast type = %#v", m)
	}
}

func TestAskFlow(t *testing.T) {
	h := newHub()
	h.SetSnapshot(func(string) ([]byte, error) { return []byte(`{"type":"hi"}`), nil })
	conn, _, ctx := newWSClient(t, h)
	readJSON(t, ctx, conn) // registration snapshot

	req := permissions.AskRequest{Kind: "bash", ToolName: "bash", Command: "rm -rf /tmp/x"}
	type result struct {
		resp permissions.AskResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := h.Ask(req)
		ch <- result{resp, err}
	}()

	m := readJSON(t, ctx, conn)
	if m["type"] != "permission_ask" {
		t.Fatalf("ask frame = %#v", m)
	}
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("no ask id: %#v", m)
	}

	err := conn.Write(ctx, websocket.MessageText, []byte(
		`{"type":"permission_response","id":"`+id+`","response":{"allowOnce":true}}`))
	if err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("Ask error: %v", r.err)
		}
		if !r.resp.AllowOnce {
			t.Fatalf("response = %+v", r.resp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not return")
	}
}

func TestAskNoClientsDenies(t *testing.T) {
	h := newHub()
	resp, err := h.Ask(permissions.AskRequest{Kind: "bash", ToolName: "bash"})
	if err == nil {
		t.Fatal("expected error with zero clients")
	}
	if !resp.Deny {
		t.Fatalf("expected deny, got %+v", resp)
	}
}

// TestSlowClientDisconnected ensures a client that cannot keep up is dropped
// (never blocking the event source), exercising the white-box path.
func TestSlowClientDisconnected(t *testing.T) {
	h := newHub()
	c := &client{send: make(chan []byte, clientBuffer)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	// Overflow the send buffer: more frames than the channel holds.
	for i := 0; i < clientBuffer+10; i++ {
		h.broadcast([]byte(`{"type":"x"}`))
	}

	h.mu.Lock()
	_, ok := h.clients[c]
	h.mu.Unlock()
	if ok {
		t.Fatal("slow client was not disconnected")
	}
	// Drain the buffered frames, then the channel must be closed.
	discarded := 0
	for {
		if _, ok := <-c.send; !ok {
			break
		}
		discarded++
		if discarded > clientBuffer+20 {
			t.Fatal("channel never closed after drop")
		}
	}
}

// TestAgentListenerIsNonBlocking: a full client buffer must not stall
// AgentListener (used by the synchronous agent loop).
func TestAgentListenerNonBlocking(t *testing.T) {
	h := newHub()
	c := &client{send: make(chan []byte, 4)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_ = h.AgentListener(agent.AgentEvent{Type: agent.EventTurnStart})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AgentListener blocked on a full buffer")
	}
}
