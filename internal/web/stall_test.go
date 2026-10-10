package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"doge/internal/ai"
)

// startStallingLLM serves an OpenAI-compatible endpoint that sends exactly one
// delta then holds the connection open forever (no [DONE], no more bytes) —
// simulating a provider that stalls mid-stream. The handler also unblocks on
// teardown so srv.Close() never hangs on the still-open connection.
func startStallingLLM(t *testing.T) *httptest.Server {
	t.Helper()
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() {
		close(stop)
		srv.Close()
	})
	return srv
}

// withShortStallTimeout sets a short provider idle budget for the test and
// restores the previous value on cleanup, so a stalled stream self-recovers
// within the test window instead of the 2-minute default.
func withShortStallTimeout(t *testing.T) {
	t.Helper()
	old := ai.StreamIdleTimeout()
	ai.SetStreamIdleTimeout(300 * time.Millisecond)
	t.Cleanup(func() { ai.SetStreamIdleTimeout(old) })
}

// TestAbortInterruptsStalledStream: a provider that stalls mid-stream must be
// interruptible via /api/abort on the owning window — the run ends promptly and
// the session frees up.
func TestAbortInterruptsStalledStream(t *testing.T) {
	llm := startStallingLLM(t)
	shared := newRealShared(t, llm)
	_, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	conn, ctx := dialWSC(t, ts, c, "winA")
	readJSON(t, ctx, conn)

	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("prompt = %d, want 202", code)
	}
	waitFor(t, "run starts streaming", func() bool { return shared.Agent.State().IsStreaming })

	if code := doWindow(t, c, "POST", ts.URL+"/api/abort", "", "winA"); code != http.StatusOK {
		t.Fatalf("abort = %d, want 200", code)
	}
	waitFor(t, "run terminates after abort", func() bool { return !shared.Agent.State().IsStreaming })
}

// TestStalledRunSelfRecovers: with a stall budget, a silent provider ends the
// run on its own (error surfaced, not "running forever"), and a new prompt is
// accepted right away — the session is never permanently frozen.
func TestStalledRunSelfRecovers(t *testing.T) {
	withShortStallTimeout(t)
	llm := startStallingLLM(t)
	shared := newRealShared(t, llm)
	_, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	conn, ctx := dialWSC(t, ts, c, "winA")
	readJSON(t, ctx, conn)

	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("prompt = %d, want 202", code)
	}
	waitFor(t, "run starts streaming", func() bool { return shared.Agent.State().IsStreaming })

	// No abort: the stall watchdog must release the run by itself.
	waitFor(t, "stalled run self-terminates", func() bool { return !shared.Agent.State().IsStreaming })

	st := getStatusFor(t, c, ts, "winA")
	if em, _ := st["errorMessage"].(string); !strings.Contains(em, "stalled") {
		t.Fatalf("expected stall error, got errorMessage=%q", em)
	}

	// The freed session accepts a new prompt immediately.
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"again"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("prompt after stall recovery = %d, want 202", code)
	}
}

// TestOtherWindowSwitchTakeoverAbortStalledRun: a stuck run on a window's own
// forked session is locatable (persisted on first message), so a fresh window
// can switch to it, force-take control, and abort it — the cross-window
// interrupt path works even while the provider is silent.
func TestOtherWindowSwitchTakeoverAbortStalledRun(t *testing.T) {
	llm := startStallingLLM(t)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	conn, ctx := dialWSC(t, ts, c, "winA")
	readJSON(t, ctx, conn)
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", "", "winA"); code != http.StatusOK {
		t.Fatalf("winA new = %d", code)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt = %d", code)
	}
	backendA := s.windows["winA"].backend.(*sessionBackend)
	runAgent := backendA.current()
	forkKey := backendA.curKey
	waitFor(t, "fork run starts streaming", func() bool { return runAgent.Agent.State().IsStreaming })

	// winB (fresh window) switches to the running fork, takes over, aborts.
	connB, ctxB := dialWSC(t, ts, c, "winB")
	readJSON(t, ctxB, connB)
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/switch", `{"id":"`+forkKey+`"}`, "winB"); code != http.StatusOK {
		t.Fatalf("winB switch to running fork = %d, want 200", code)
	}
	if j := doWindowTakeover(t, c, ts, "winB"); j["owned"] != true {
		t.Fatalf("winB takeover = %#v, want owned=true", j)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/abort", "", "winB"); code != http.StatusOK {
		t.Fatalf("winB abort = %d, want 200", code)
	}
	waitFor(t, "fork run terminates after switch+takeover+abort", func() bool { return !runAgent.Agent.State().IsStreaming })
}
