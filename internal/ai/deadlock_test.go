package ai

import (
	"encoding/json"
	"testing"
	"time"
)

// TestCompletionsHandlerPushDoesNotHoldLock is a regression guard for a real
// production deadlock: the SSE handler used to hold the partial message's
// write lock across EventStream.Push. With the stream buffer full, the
// producer blocked holding the lock, while the consumer (the agent loop,
// marshaling the partial for a web listener) blocked on the same lock and
// stopped draining — a lock-order inversion that neither the stall watchdog
// (no pending read) nor abort (nothing cancels the channel send) could break.
//
// The invariant: pushing into a full stream buffer must never keep the
// message lock held, so a concurrent MarshalJSON always completes.
func TestCompletionsHandlerPushDoesNotHoldLock(t *testing.T) {
	h := &completionsHandler{
		opts:    &StreamOptions{},
		stream:  NewEventStream(1), // tiny buffer so the next Push blocks
		partial: NewAssistantMessage(APIOpenAICompletions, "p", "m"),
		toolIdx: map[int]int{}, argsJSON: map[int]string{},
		accums: map[int]*textAccum{},
	}
	// Fill the only buffer slot so the handler's next Push blocks.
	h.stream.Push(AssistantMessageEvent{Type: EventStart})

	// Producer: a data chunk appends text and pushes text_start + text_delta
	// into the full buffer — it must block WITHOUT holding partial.mu.
	go func() {
		_ = h.handle(SSEEvent{Data: []byte(`{"choices":[{"delta":{"content":"hi"}}]}`)})
	}()
	time.Sleep(50 * time.Millisecond) // let the producer reach the blocked Push

	// Consumer: MarshalJSON must complete even while the producer is blocked
	// pushing. If the lock were held across Push this would hang forever.
	done := make(chan struct{})
	go func() {
		_, _ = json.Marshal(h.partial)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MarshalJSON blocked while the producer held the lock across Push (deadlock)")
	}
}

// TestAnthropicHandlerPushDoesNotHoldLock mirrors the completions guard for
// the anthropic adapter's handler.
func TestAnthropicHandlerPushDoesNotHoldLock(t *testing.T) {
	h := &anthropicHandler{
		opts:    &StreamOptions{},
		stream:  NewEventStream(1),
		partial: NewAssistantMessage(APIAnthropicMessages, "p", "m"),
		accums:  map[int]*textAccum{},
	}
	h.stream.Push(AssistantMessageEvent{Type: EventStart})

	go func() {
		_ = h.handle(SSEEvent{Event: "content_block_start", Data: []byte(`{"index":0,"content_block":{"type":"text"}}`)})
	}()
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		_, _ = json.Marshal(h.partial)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("anthropic: MarshalJSON blocked while the producer held the lock across Push")
	}
}
