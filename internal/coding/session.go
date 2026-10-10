package coding

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"doge/internal/ai"
)

// Session entry types (following the session JSONL format).
const (
	EntryMessage             = "message"
	EntryThinkingLevelChange = "thinking_level_change"
	EntryModelChange         = "model_change"
	EntryCompaction          = "compaction"
	EntryBranchSummary       = "branch_summary"
	EntryCustom              = "custom"
	EntryCustomMessage       = "custom_message"
	EntryLabel               = "label"
	EntrySessionInfo         = "session_info"
)

// Session version written by this implementation.
const sessionVersion = 3

// SessionHeader is the first line of a session file.
type SessionHeader struct {
	Type          string `json:"type"`
	Version       int    `json:"version"`
	ID            string `json:"id"`
	Timestamp     int64  `json:"timestamp"`
	Cwd           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
	// Kind classifies the session: "" (default) = a user's own session,
	// SessionKindRemote = a session the slave forked for a remote mesh task.
	// The UIs filter remote sessions out of the normal list by default but keep
	// an entry to view them.
	Kind string `json:"kind,omitempty"`
	// SourceName / SourcePeerID identify the node that dispatched a remote task
	// (set on Kind=remote sessions). The UIs show this on the message cards of
	// a remote session instead of a plain "USER".
	SourceName   string `json:"sourceName,omitempty"`
	SourcePeerID string `json:"sourcePeerId,omitempty"`
}

// Session kinds.
const (
	// SessionKindRemote marks a session forked on the slave to execute a remote
	// mesh task (as opposed to a user's own interactive session).
	SessionKindRemote = "remote"
)

// SessionEntry is one line of a session file. Entries form a tree via
// ParentID; the active branch is the chain ending at the leaf entry.
type SessionEntry struct {
	ID        string `json:"id"`
	ParentID  string `json:"parentId,omitempty"`
	Timestamp int64  `json:"timestamp"`
	Type      string `json:"type"`

	// message
	Message *ai.Message `json:"message,omitempty"`

	// thinking_level_change
	ThinkingLevel string `json:"thinkingLevel,omitempty"`

	// model_change
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"modelId,omitempty"`

	// compaction
	Summary          string `json:"summary,omitempty"`
	FirstKeptEntryID string `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int    `json:"tokensBefore,omitempty"`
	Details          any    `json:"details,omitempty"`
	FromHook         bool   `json:"fromHook,omitempty"`

	// branch_summary
	FromID string `json:"fromId,omitempty"`

	// custom / custom_message
	CustomType string            `json:"customType,omitempty"`
	Data       any               `json:"data,omitempty"`
	Content    []ai.ContentBlock `json:"content,omitempty"`
	Display    string            `json:"display,omitempty"`

	// label
	TargetID string `json:"targetId,omitempty"`

	// session_info
	Name string `json:"name,omitempty"`
}

// Session is an in-memory session backed by an append-only JSONL file.
type Session struct {
	mu sync.Mutex

	Header       SessionHeader
	Entries      []*SessionEntry
	LeafID       string
	FilePath     string
	HasAssistant bool

	// idIndex maps entry ID → entry for O(1) lookups during leaf-path walks
	// (ContextEntries). Populated on load and append; entries are never
	// removed (append-only file), so no deletion is needed.
	idIndex map[string]*SessionEntry

	manager *SessionManager
	created bool // file exists on disk
}

// AppendMessage appends a message entry and persists it. The file is created
// on the FIRST message (user or assistant), so a session that is mid-run is
// already locatable by other windows (switch/takeover) instead of living only
// in the creating agent's memory.
func (s *Session) AppendMessage(msg *ai.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.Role == ai.RoleAssistant && msg.StopReason != "" {
		s.HasAssistant = true
	}
	entry := &SessionEntry{
		ID:        newEntryID(),
		ParentID:  s.LeafID,
		Timestamp: time.Now().UnixMilli(),
		Type:      EntryMessage,
		Message:   msg,
	}
	return s.appendEntryLocked(entry)
}

// AppendCustomEntry appends a non-LLM entry (custom state, model changes,
// thinking level changes, compaction markers).
func (s *Session) AppendCustomEntry(entry *SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.ID = newEntryID()
	entry.ParentID = s.LeafID
	if entry.Timestamp == 0 {
		entry.Timestamp = time.Now().UnixMilli()
	}
	return s.appendEntryLocked(entry)
}

func (s *Session) appendEntryLocked(entry *SessionEntry) error {
	if s.idIndex == nil {
		s.idIndex = make(map[string]*SessionEntry)
	}
	s.idIndex[entry.ID] = entry
	if err := s.ensureFileLocked(); err != nil {
		return err
	}
	if err := appendJSONLLine(s.FilePath, entry); err != nil {
		return err
	}
	s.Entries = append(s.Entries, entry)
	s.LeafID = entry.ID
	return nil
}

// ensureFileLocked creates the session file with the header on first write
// (the delayed creation: wx create then full write).
func (s *Session) ensureFileLocked() error {
	if s.created {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.FilePath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.FilePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			// File exists (e.g. loaded session); append mode.
			f, err = os.OpenFile(s.FilePath, os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(s.Header); err != nil {
		return err
	}
	s.created = true
	return nil
}

// Branch moves the active leaf to the entry with the given id; subsequent
// appends fork from there.
func (s *Session) Branch(entryID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LeafID = entryID
}

// Leaf returns the active leaf entry, if any.
func (s *Session) Leaf() *SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entryByIDLocked(s.LeafID)
}

func (s *Session) entryByIDLocked(id string) *SessionEntry {
	if s.idIndex != nil {
		return s.idIndex[id]
	}
	for _, e := range s.Entries {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// ContextEntries returns the entries on the path from the root to the leaf.
// If the path contains a compaction entry, the output is the compaction
// entry followed by the kept entries and everything after (
// summarized history is not included in the context).
func (s *Session) ContextEntries() []*SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Walk from leaf to root.
	var path []*SessionEntry
	cur := s.LeafID
	for cur != "" {
		e := s.entryByIDLocked(cur)
		if e == nil {
			break
		}
		path = append(path, e)
		cur = e.ParentID
	}
	// Reverse to root-first.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}

	// Find the last compaction entry on the path.
	var out []*SessionEntry
	cutIndex := -1
	for i, e := range path {
		if e.Type == EntryCompaction {
			cutIndex = i
		}
	}
	if cutIndex < 0 {
		return path
	}
	compaction := path[cutIndex]
	out = append(out, compaction)
	// Entries from firstKeptEntryID (inclusive) up to the compaction entry
	// are kept; everything before the kept mark is summarized away.
	if compaction.FirstKeptEntryID != "" {
		for i, e := range path {
			if e.ID == compaction.FirstKeptEntryID {
				out = append(out, path[i:cutIndex]...)
				break
			}
		}
	}
	out = append(out, path[cutIndex+1:]...)
	return out
}

// ToMessages converts context entries to LLM messages (compaction and branch
// summaries become special user messages).
func ContextEntriesToMessages(entries []*SessionEntry) []*ai.Message {
	var messages []*ai.Message
	for _, e := range entries {
		switch e.Type {
		case EntryMessage:
			if e.Message != nil {
				messages = append(messages, e.Message)
			}
		case EntryCompaction:
			messages = append(messages, &ai.Message{
				Role: ai.RoleUser,
				Content: []ai.ContentBlock{ai.TextBlock(
					"<compaction-summary>\n" + e.Summary + "\n</compaction-summary>")},
				Timestamp: e.Timestamp,
			})
		case EntryBranchSummary:
			messages = append(messages, &ai.Message{
				Role: ai.RoleUser,
				Content: []ai.ContentBlock{ai.TextBlock(
					"<branch-summary>\n" + e.Summary + "\n</branch-summary>")},
				Timestamp: e.Timestamp,
			})
		case EntryCustomMessage:
			messages = append(messages, &ai.Message{
				Role:      ai.RoleUser,
				Content:   e.Content,
				Details:   e.Data,
				Timestamp: e.Timestamp,
			})
		}
	}
	return messages
}

// appendJSONLLine appends one JSON object as a line to the file.
func appendJSONLLine(path string, v any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(v)
}

// newEntryID generates an 8-hex-digit entry id with collision checks
// (following the generateSessionEntryId).
func newEntryID() string {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		if id != "" {
			return id
		}
	}
}

// newSessionID generates a random session id.
func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// EncodeCwd encodes a working directory for the session folder name:
// "--" + path (with / : \ → -) + "--".
func EncodeCwd(cwd string) string {
	clean := filepath.Clean(cwd)
	clean = strings.TrimPrefix(clean, "/")
	clean = strings.ReplaceAll(clean, "/", "-")
	clean = strings.ReplaceAll(clean, "\\", "-")
	clean = strings.ReplaceAll(clean, ":", "-")
	return "--" + clean + "--"
}

// sessionFileName builds "<iso-timestamp>_<sessionId>.jsonl" with
// ':' and '.' replaced by '-'.
func sessionFileName(t time.Time, id string) string {
	name := t.Format("2006-01-02T15-04-05-0700")
	name = strings.ReplaceAll(name, ":", "-")
	name = strings.ReplaceAll(name, ".", "-")
	return name + "_" + id + ".jsonl"
}

var _ = fmt.Sprintf
