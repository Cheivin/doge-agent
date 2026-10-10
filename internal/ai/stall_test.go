package ai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStallTimeoutTerminatesSilentStream: a provider that stops sending data
// (connection stays open) must not hang the stream — the stall watchdog aborts
// it with a clear stall error instead of blocking the run forever.
func TestStallTimeoutTerminatesSilentStream(t *testing.T) {
	// The handler blocks until the request is aborted or teardown unblocks it,
	// so srv.Close() never hangs on the still-open connection.
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
	defer func() { close(stop); srv.Close() }()

	old := stallTimeout()
	SetStreamIdleTimeout(200 * time.Millisecond)
	defer SetStreamIdleTimeout(old)

	model := &Model{ID: "m", API: APIOpenAICompletions, Provider: "p", BaseURL: srv.URL}
	stream := openAICompletionsStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}},
		&StreamOptions{APIKey: "sk-test"})
	done := make(chan struct{})
	var final *Message
	go func() {
		defer close(done)
		_ = stream.Range(func(AssistantMessageEvent) error { return nil })
		var err error
		final, err = stream.Result()
		if err != nil {
			t.Errorf("result: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not terminate on stall")
	}
	if final == nil || final.StopReason != StopReasonError || !strings.Contains(final.ErrorMessage, "stalled") {
		t.Errorf("stall not reported: %+v", final)
	}
}

// TestHeaderTimeoutTerminatesUnresponsiveProvider: a provider that accepts the
// connection but never sends response headers must not hang the stream — the
// transport's response-header timeout aborts the request.
func TestHeaderTimeoutTerminatesUnresponsiveProvider(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the connection forever without writing response headers.
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer func() { close(stop); srv.Close() }()

	old := stallTimeout()
	SetStreamIdleTimeout(300 * time.Millisecond)
	defer SetStreamIdleTimeout(old)

	model := &Model{ID: "m", API: APIOpenAICompletions, Provider: "p", BaseURL: srv.URL}
	stream := openAICompletionsStream(model, &Context{Messages: []*Message{NewUserTextMessage("hi")}},
		&StreamOptions{APIKey: "sk-test"})
	done := make(chan struct{})
	var final *Message
	go func() {
		defer close(done)
		_ = stream.Range(func(AssistantMessageEvent) error { return nil })
		var err error
		final, err = stream.Result()
		if err != nil {
			t.Errorf("result: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not terminate on header timeout")
	}
	if final == nil || final.StopReason != StopReasonError {
		t.Errorf("header timeout not reported: %+v", final)
	}
}
