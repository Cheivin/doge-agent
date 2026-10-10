package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// refreshWindow drops and re-dials the window's WebSocket, simulating a page
// reload (the clientID stays the same, sessionStorage survives refresh).
func refreshWindow(t *testing.T, ts *httptest.Server, c *http.Client, conn *websocket.Conn, id string) *websocket.Conn {
	t.Helper()
	conn.Close(websocket.StatusNormalClosure, "refresh")
	time.Sleep(100 * time.Millisecond) // let the old connection tear down
	conn2, ctx2 := dialWSC(t, ts, c, id)
	readJSON(t, ctx2, conn2) // session snapshot
	return conn2
}

// TestOwnershipSurvivesRefresh_ForkedSession: a window that created its own
// session keeps write control across a refresh.
func TestOwnershipSurvivesRefresh_ForkedSession(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	conn, ctx := dialWSC(t, ts, c, "winA")
	readJSON(t, ctx, conn)

	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winA"); code != http.StatusOK {
		t.Fatalf("winA new = %d", code)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt (owner) = %d, want 202", code)
	}
	waitWindowIdle(s, "winA")

	conn = refreshWindow(t, ts, c, conn, "winA")

	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt after refresh = %d, want 202", code)
	}
	waitWindowIdle(s, "winA")
}

// TestOwnershipSurvivesRefresh_SharedSession: a window that owns the shared
// session keeps write control across a refresh.
func TestOwnershipSurvivesRefresh_SharedSession(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	conn, ctx := dialWSC(t, ts, c, "winA")
	readJSON(t, ctx, conn)

	// winA (first viewer) owns the shared session.
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt (owner) = %d, want 202", code)
	}
	waitPromptIdle(shared)

	conn = refreshWindow(t, ts, c, conn, "winA")

	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt on shared after refresh = %d, want 202", code)
	}
	waitWindowIdle(s, "winA")
}

// TestOwnershipSurvivesRefresh_TUIActive: with the TUI running, a window that
// forked its own session keeps write control across a refresh (the TUI owns
// the shared session, so the fork is the window's only writable view).
func TestOwnershipSurvivesRefresh_TUIActive(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, true) // TUI active
	c := login(t, ts, "tok-123")

	conn, ctx := dialWSC(t, ts, c, "winA")
	readJSON(t, ctx, conn)

	// winA forks its own session (the shared one is TUI-owned).
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winA"); code != http.StatusOK {
		t.Fatalf("winA new = %d, want 200", code)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt on fork = %d, want 202", code)
	}
	waitWindowIdle(s, "winA")

	conn = refreshWindow(t, ts, c, conn, "winA")

	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt after refresh (TUI active) = %d, want 202", code)
	}
	waitWindowIdle(s, "winA")
}

// TestRefreshAfterSoleOwnerPromotion reproduces the reported "refresh loses
// ownership" chain end-to-end: winA owns the shared session, winB observes
// read-only, winA leaves, winB (sole viewer) takes ownership, and winB's
// refresh must not lose it.
func TestRefreshAfterSoleOwnerPromotion(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	connA, ctxA := dialWSC(t, ts, c, "winA")
	connB, ctxB := dialWSC(t, ts, c, "winB")
	readJSON(t, ctxA, connA)
	readJSON(t, ctxB, connB)

	// winA owns the shared session; winB is a read-only observer.
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt = %d", code)
	}
	waitPromptIdle(shared)
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusForbidden {
		t.Fatalf("winB (reader) prompt = %d, want 403", code)
	}
	// winA leaves the shared session → winB (sole viewer) takes ownership.
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winA"); code != http.StatusOK {
		t.Fatalf("winA new = %d", code)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt after winA left = %d, want 202", code)
	}
	waitWindowIdle(s, "winB")

	// winB refreshes: must still own the shared session.
	connB = refreshWindow(t, ts, c, connB, "winB")
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt after refresh = %d, want 202", code)
	}
	waitWindowIdle(s, "winB")
}
