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
	"doge/internal/coding"
)

// newAuthedWS dials the real /ws endpoint with the logged-in client's cookie
// jar (token auth applies, unlike the raw-hub helper in hub_test.go).
func newAuthedWS(t *testing.T, ts *httptest.Server, client *http.Client) (*websocket.Conn, context.Context) {
	t.Helper()
	wsURL := "ws" + ts.URL[4:] + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn, ctx
}

// waitFor polls cond until true or times out (WS events arrive via goroutines).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// TestServerWSMultiWindowIsolation: multiple browser windows share the live
// event stream; closing one window never affects the others, and a reconnect
// gets a fresh snapshot plus subsequent events.
func TestServerWSMultiWindowIsolation(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := login(t, ts, "tok-123")

	conn1, ctx1 := newAuthedWS(t, ts, c)
	conn2, ctx2 := newAuthedWS(t, ts, c)
	// Every new connection first receives its session_info snapshot.
	if m := readJSON(t, ctx1, conn1); m["type"] != "session_info" {
		t.Fatalf("conn1 first frame = %#v", m)
	}
	if m := readJSON(t, ctx2, conn2); m["type"] != "session_info" {
		t.Fatalf("conn2 first frame = %#v", m)
	}

	// Both windows receive the same broadcast, in sync.
	f.Push(agent.AgentEvent{Type: agent.EventTurnStart})
	m1 := readJSON(t, ctx1, conn1)
	m2 := readJSON(t, ctx2, conn2)
	b1, _ := json.Marshal(m1)
	b2, _ := json.Marshal(m2)
	if string(b1) != string(b2) {
		t.Fatalf("windows out of sync: %s vs %s", b1, b2)
	}

	// One window closes; the other keeps receiving without stalls or panics.
	conn2.Close(websocket.StatusNormalClosure, "bye")
	f.Push(agent.AgentEvent{Type: agent.EventTurnEnd})
	if m := readJSON(t, ctx1, conn1); m["type"] != string(agent.EventTurnEnd) {
		t.Fatalf("conn1 after window close = %#v", m)
	}

	// Reconnect: snapshot again, then live events flow.
	conn3, ctx3 := newAuthedWS(t, ts, c)
	if m := readJSON(t, ctx3, conn3); m["type"] != "session_info" {
		t.Fatalf("reconnect first frame = %#v", m)
	}
	f.Push(agent.AgentEvent{Type: agent.EventMessageUpdate, Message: &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{ai.TextBlock("after reconnect")}}})
	if m := readJSON(t, ctx3, conn3); m["type"] != string(agent.EventMessageUpdate) {
		t.Fatalf("reconnect event = %#v", m)
	}
}

// TestServerWSSessionSwitchNoCrossTalk: pushing events from a different
// session source (the web console switching/forking sessions) delivers a
// clean new stream to every window — no stale frames, no mixing.
func TestServerWSSessionSwitchNoCrossTalk(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := login(t, ts, "tok-123")

	conn1, ctx1 := newAuthedWS(t, ts, c)
	conn2, ctx2 := newAuthedWS(t, ts, c)
	readJSON(t, ctx1, conn1) // snapshots first
	readJSON(t, ctx2, conn2)

	// Session A stream reaches both windows identically.
	f.Push(agent.AgentEvent{Type: agent.EventTurnStart})
	m1 := readJSON(t, ctx1, conn1)
	m2 := readJSON(t, ctx2, conn2)
	b1, _ := json.Marshal(m1)
	b2, _ := json.Marshal(m2)
	if string(b1) != string(b2) {
		t.Fatalf("session A desync: %s vs %s", b1, b2)
	}

	// Session B stream (new source) follows cleanly for both windows.
	f.Push(agent.AgentEvent{Type: agent.EventMessageUpdate, Message: &ai.Message{Role: ai.RoleAssistant, Content: []ai.ContentBlock{ai.TextBlock("from B")}}})
	m3 := readJSON(t, ctx1, conn1)
	m4 := readJSON(t, ctx2, conn2)
	if m3["type"] != string(agent.EventMessageUpdate) || m4["type"] != string(agent.EventMessageUpdate) {
		t.Fatalf("session B frames: %#v / %#v", m3, m4)
	}
	b3, _ := json.Marshal(m3)
	b4, _ := json.Marshal(m4)
	if string(b3) != string(b4) {
		t.Fatalf("session B desync: %s vs %s", b3, b4)
	}
}

// TestServerWSClientKindTracksConnections: browser connect/disconnect drives
// the backend consumer kind (headless → interactive → headless), which is the
// input mesh question routing keys on.
func TestServerWSClientKindTracksConnections(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	// Installing SetClientsChange fires once immediately: zero connections →
	// headless.
	waitFor(t, "initial headless kind", func() bool {
		k := f.Kinds()
		return len(k) == 1 && k[0] == coding.ConsumerHeadless
	})

	c := login(t, ts, "tok-123")
	conn, _ := newAuthedWS(t, ts, c)
	waitFor(t, "interactive after connect", func() bool {
		k := f.Kinds()
		return len(k) >= 2 && k[len(k)-1] == coding.ConsumerInteractive
	})

	conn.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "headless after disconnect", func() bool {
		k := f.Kinds()
		return len(k) >= 3 && k[len(k)-1] == coding.ConsumerHeadless
	})
}
