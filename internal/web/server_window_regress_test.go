package web

import (
	"net/http"
	"testing"
)

// waitWindowIdle blocks until the given window's session is no longer
// streaming, so tests don't tear down TempDirs while an async prompt is still
// persisting.
func waitWindowIdle(s *Server, id string) {
	s.windowsMu.Lock()
	w := s.windows[id]
	s.windowsMu.Unlock()
	if w == nil {
		return
	}
	if sb, ok := w.backend.(*sessionBackend); ok {
		sb.current().WaitForIdle()
	}
}

// TestTwoWindowsEachOwnSession: two windows that each create their own session
// must both remain writable on their own sessions.
func TestTwoWindowsEachOwnSession(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	connA, ctxA := dialWSC(t, ts, c, "winA")
	connB, ctxB := dialWSC(t, ts, c, "winB")
	readJSON(t, ctxA, connA)
	readJSON(t, ctxB, connB)

	// winA creates its own session A.
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winA"); code != http.StatusOK {
		t.Fatalf("winA new = %d, want 200", code)
	}
	// winB creates its own session B.
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winB"); code != http.StatusOK {
		t.Fatalf("winB new = %d, want 200", code)
	}
	// Both own their own sessions → both can prompt.
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt = %d, want 202", code)
	}
	waitWindowIdle(s, "winA")
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt = %d, want 202", code)
	}
	waitWindowIdle(s, "winB")
	// And each can still open a NEW session afterwards (no lock-in).
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winB"); code != http.StatusOK {
		t.Fatalf("winB second new = %d, want 200", code)
	}
}

// TestNewSessionNotBlockedBySharedOwner: a window that is a read-only observer
// of the TUI/shared session must still be able to create its OWN session.
func TestNewSessionNotBlockedBySharedOwner(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	connA, ctxA := dialWSC(t, ts, c, "winA")
	connB, ctxB := dialWSC(t, ts, c, "winB")
	readJSON(t, ctxA, connA)
	readJSON(t, ctxB, connB)

	// winA owns the shared session and is mid-run.
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt = %d", code)
	}
	// winB is a read-only observer of the shared session, but must still be
	// allowed to start its OWN session (creating a new session is not writing
	// the shared one).
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winB"); code != http.StatusOK {
		t.Fatalf("winB new while shared owned by winA = %d, want 200", code)
	}
	// winB now owns its fresh session and can prompt it.
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt on own session = %d, want 202", code)
	}
	waitWindowIdle(s, "winB")
	waitPromptIdle(shared)
}

// TestSoleRemainingViewerTakesOwnership: when the owner leaves a session and
// only one viewer remains, that viewer becomes the owner without an explicit
// claim (lazy control).
func TestSoleRemainingViewerTakesOwnership(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	connA, ctxA := dialWSC(t, ts, c, "winA")
	connB, ctxB := dialWSC(t, ts, c, "winB")
	readJSON(t, ctxA, connA)
	readJSON(t, ctxB, connB)

	// winA owns the shared session (first viewer).
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt = %d", code)
	}
	waitPromptIdle(shared)
	// winB is read-only.
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusForbidden {
		t.Fatalf("winB prompt (read-only) = %d, want 403", code)
	}
	// winA switches away → releases the shared session → winB (sole viewer)
	// becomes the owner without an explicit claim.
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winA"); code != http.StatusOK {
		t.Fatalf("winA new session = %d", code)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt after winA left (sole viewer) = %d, want 202", code)
	}
	waitWindowIdle(s, "winB")
}
