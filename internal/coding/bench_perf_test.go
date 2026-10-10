package coding

import (
	"fmt"
	"testing"

	"doge/internal/ai"
)

// Benchmarks for the session entry ID index (P0-4): ContextEntries walks the
// leaf path back to the root, one entryByID lookup per step. Indexed lookups
// are O(1); the pre-optimization linear scan made the walk O(n²).

// benchSession builds a session of n user/assistant turns with an idIndex
// (as CreateSession/LoadSession now do).
func benchSession(n int) *Session {
	s := &Session{idIndex: make(map[string]*SessionEntry)}
	var prev string
	for i := 0; i < n; i++ {
		for _, role := range []string{ai.RoleUser, ai.RoleAssistant} {
			e := &SessionEntry{
				ID:       fmt.Sprintf("id-%04d-%s", i, role),
				ParentID: prev,
				Type:     EntryMessage,
				Message:  &ai.Message{Role: role, Content: []ai.ContentBlock{{Type: ai.ContentTypeText, Text: "一段会话内容。"}}},
			}
			s.Entries = append(s.Entries, e)
			s.idIndex[e.ID] = e
			prev = e.ID
		}
	}
	s.LeafID = prev
	return s
}

func BenchmarkContextEntriesIndexed(b *testing.B) {
	s := benchSession(300)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := len(s.ContextEntries()); got < 2 {
			b.Fatal("bad context")
		}
	}
}

// benchmarkContextEntriesLinear reproduces the pre-optimization O(n²) walk:
// entryByIDLocked scanned s.Entries linearly at every step.
func benchmarkContextEntriesLinear(s *Session) []*SessionEntry {
	linearEntryByID := func(id string) *SessionEntry {
		for _, e := range s.Entries {
			if e.ID == id {
				return e
			}
		}
		return nil
	}
	var path []*SessionEntry
	cur := s.LeafID
	for cur != "" {
		e := linearEntryByID(cur)
		if e == nil {
			break
		}
		path = append(path, e)
		cur = e.ParentID
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

func BenchmarkContextEntriesLinear(b *testing.B) {
	s := benchSession(300)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := len(benchmarkContextEntriesLinear(s)); got < 2 {
			b.Fatal("bad context")
		}
	}
}
