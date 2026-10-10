package coding

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"doge/internal/agent"
	"doge/internal/ai"
)

// TestAgentSessionSubscribeFanout covers the §8.5.2 pub-sub contract: multiple
// consumers, interactive/headless kind bookkeeping, dynamic setKind, idempotent
// unsubscribe, and observational (non-aborting) consumer errors.
func TestAgentSessionSubscribeFanout(t *testing.T) {
	sess := &AgentSession{} // fanOut only needs the consumer list, no agent.

	var got []string
	unsub1, _ := sess.Subscribe(func(ev agent.AgentEvent) error {
		got = append(got, "c1:"+string(ev.Type))
		return nil
	}, ConsumerInteractive)
	_, set2 := sess.Subscribe(func(ev agent.AgentEvent) error {
		got = append(got, "c2:"+string(ev.Type))
		return nil
	}, ConsumerHeadless)

	if n := sess.ConsumerCount(); n != 2 {
		t.Fatalf("ConsumerCount = %d, want 2", n)
	}
	if n := sess.InteractiveCount(); n != 1 {
		t.Fatalf("InteractiveCount = %d, want 1", n)
	}

	// Both consumers receive the event.
	sess.fanOut(agent.AgentEvent{Type: agent.EventTurnStart})
	if len(got) != 2 {
		t.Fatalf("fanout delivered %d events, want 2 (%v)", len(got), got)
	}

	// A headless consumer upgrading to interactive counts as a human present.
	set2(ConsumerInteractive)
	if n := sess.InteractiveCount(); n != 2 {
		t.Fatalf("InteractiveCount after setKind = %d, want 2", n)
	}
	set2(ConsumerHeadless)
	if n := sess.InteractiveCount(); n != 1 {
		t.Fatalf("InteractiveCount after downgrade = %d, want 1", n)
	}

	// Unsubscribe removes by identity and is idempotent.
	unsub1()
	if n := sess.ConsumerCount(); n != 1 {
		t.Fatalf("ConsumerCount after unsub = %d, want 1", n)
	}
	unsub1() // idempotent: no double removal, no panic
	if n := sess.ConsumerCount(); n != 1 {
		t.Fatalf("ConsumerCount after double unsub = %d, want 1", n)
	}

	// A failing consumer must not abort or panic the fan-out loop.
	_, _ = sess.Subscribe(func(ev agent.AgentEvent) error { return errors.New("boom") }, ConsumerHeadless)
	sess.fanOut(agent.AgentEvent{Type: agent.EventTurnStart})
}

// TestHandleAgentEventFanoutOnPersistFailure: a persistence failure still
// broadcasts the event to consumers (observers must see it) while the error
// propagates so the run aborts.
func TestHandleAgentEventFanoutOnPersistFailure(t *testing.T) {
	// Gaslight AppendMessage into failing: the session file's parent path is a
	// regular file, so MkdirAll returns ENOTDIR.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess := &AgentSession{
		Session: &Session{FilePath: filepath.Join(blocker, "sess.jsonl"), HasAssistant: true},
	}

	var got []agent.AgentEvent
	unsub, _ := sess.Subscribe(func(ev agent.AgentEvent) error {
		got = append(got, ev)
		return nil
	}, ConsumerHeadless)
	defer unsub()

	err := sess.handleAgentEvent(agent.AgentEvent{
		Type:    agent.EventMessageEnd,
		Message: &ai.Message{Role: ai.RoleUser, Content: []ai.ContentBlock{ai.TextBlock("hi")}},
	})
	if err == nil {
		t.Fatal("expected persistence error, got nil")
	}
	if len(got) != 1 {
		t.Fatalf("fanout delivered %d events on persist failure, want 1", len(got))
	}
	if got[0].Type != agent.EventMessageEnd {
		t.Fatalf("event type = %q", got[0].Type)
	}
}
