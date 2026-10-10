package coding

import (
	"path/filepath"
	"testing"

	"doge/internal/ai"
)

func assistantMsg(text string) *ai.Message {
	return &ai.Message{
		Role:       ai.RoleAssistant,
		StopReason: ai.StopReasonStop,
		Content:    []ai.ContentBlock{ai.TextBlock(text)},
	}
}

func userMsg(text string) *ai.Message { return ai.NewUserTextMessage(text) }

func TestRewritePlan(t *testing.T) {
	entries := []*SessionEntry{
		{ID: "e1", Type: EntryMessage, Message: userMsg("q1")},
		{ID: "e2", Type: EntryMessage, Message: assistantMsg("a1")},
		{ID: "e3", Type: EntryMessage, Message: userMsg("q2")},
		{ID: "e4", Type: EntryMessage, Message: assistantMsg("a2")},
		{ID: "e5", Type: EntryMessage, Message: userMsg("q3")},
	}
	// Edit the 2nd user message (seq 1): keep everything before it.
	keep, keepEnd, err := rewritePlan(entries, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(keep) != 2 || keep[0].Role != ai.RoleUser || keep[1].Role != ai.RoleAssistant {
		t.Fatalf("keep = %d messages", len(keep))
	}
	if keepEnd != 2 {
		t.Fatalf("keepEnd = %d (want 2 entries)", keepEnd)
	}
	// Edit the FIRST user message: nothing kept, empty leaf.
	keep, keepEnd, err = rewritePlan(entries, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(keep) != 0 || keepEnd != 0 {
		t.Fatalf("first-message rewrite: keep=%d end=%d", len(keep), keepEnd)
	}
	// Out of range.
	if _, _, err := rewritePlan(entries, 5); err == nil {
		t.Fatal("expected out-of-range error")
	}
}

func TestRewriteTurnRebasesSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	manager := NewSessionManager(filepath.Join(dir, "sessions"))
	sess, err := manager.CreateSession("/proj")
	if err != nil {
		t.Fatal(err)
	}
	as, err := NewAgentSession(AgentSessionOptions{
		Cwd:            "/proj",
		SessionManager: manager,
		Session:        sess,
		ModelRuntime:   NewModelRuntime(LoadAuthStore(filepath.Join(dir, "auth.json"))),
	})
	if err != nil {
		t.Fatal(err)
	}

	q1, a1 := userMsg("q1"), assistantMsg("a1")
	q2, a2 := userMsg("q2"), assistantMsg("a2")
	for _, m := range []*ai.Message{q1, a1, q2, a2} {
		if err := sess.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	as.Agent.SetMessages([]*ai.Message{q1, a1, q2, a2})

	// Rewrite the 2nd user message (q2, seq 1) with edited text.
	keep, err := as.RewriteTurn(1, "q2-edited")
	if err != nil {
		t.Fatal(err)
	}
	if len(keep) != 2 {
		t.Fatalf("kept %d messages", len(keep))
	}

	// Simulate the re-run: the agent appends the edited user message and a
	// new assistant reply (as the loop would via message_end events).
	if err := sess.AppendMessage(userMsg("q2-edited")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(assistantMsg("a2-edited")); err != nil {
		t.Fatal(err)
	}

	// The context must now read the NEW branch only: the old q2/a2 are off
	// the leaf path.
	msgs := ContextEntriesToMessages(sess.ContextEntries())
	var texts []string
	for _, m := range msgs {
		texts = append(texts, m.Text())
	}
	want := []string{"q1", "a1", "q2-edited", "a2-edited"}
	if len(texts) != len(want) {
		t.Fatalf("context = %v", texts)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Fatalf("context[%d] = %q (want %q); full: %v", i, texts[i], want[i], texts)
		}
	}
}
