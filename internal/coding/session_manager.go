package coding

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// SessionSummary is the lightweight metadata used for session listing.
type SessionSummary struct {
	ID           string
	FilePath     string
	Cwd          string
	Timestamp    time.Time
	Name         string
	MessageCount int
	// Kind classifies the session ("" = user session, "remote" = a slave's
	// remote-task fork). The UIs use it to keep remote sessions separate.
	Kind string
	// SourceName / SourcePeerID identify the node that dispatched a remote
	// task (Kind=remote); the UIs label the message cards with them.
	SourceName   string
	SourcePeerID string
	// FirstMessage is a truncated preview of the first user message, used to
	// tell sessions apart in lists.
	FirstMessage string
}

// SessionManager owns the session directory and session lifecycle.
type SessionManager struct {
	dir string
	mu  sync.Mutex

	// sumCache caches per-file session summaries keyed by file path,
	// invalidated when the file's size or mtime changes. Summaries read every
	// byte of a session file (message count + first-message preview), and the
	// UIs re-list sessions on every switch/reset — without the cache a switch
	// re-reads the whole session tree each time.
	sumMu    sync.Mutex
	sumCache map[string]sumCacheEntry

	// active tracks live AgentSession instances by session file path (the mesh
	// slave's remote-task forks register here). The web console resolves a
	// switched-to session to its ACTIVE instance when one exists, so viewing a
	// running remote task streams its events live instead of a detached fork.
	activeMu sync.Mutex
	active   map[string]*AgentSession
}

// sumCacheEntry is a cached summary plus the file identity it was built from.
type sumCacheEntry struct {
	size    int64
	modTime time.Time
	sum     SessionSummary
}

// NewSessionManager creates a manager rooted at dir (e.g.
// ~/.doge/agent/sessions).
func NewSessionManager(dir string) *SessionManager {
	return &SessionManager{dir: dir}
}

// RegisterActive records a live AgentSession instance by its session file path
// (the mesh slave registers each remote-task fork). UnregisterActive removes
// it when the task ends.
func (m *SessionManager) RegisterActive(s *AgentSession) {
	if s == nil || s.CurrentSession() == nil {
		return
	}
	m.activeMu.Lock()
	if m.active == nil {
		m.active = map[string]*AgentSession{}
	}
	m.active[s.CurrentSession().FilePath] = s
	m.activeMu.Unlock()
}

// UnregisterActive removes a live instance (call when its run finishes).
func (m *SessionManager) UnregisterActive(s *AgentSession) {
	if s == nil || s.CurrentSession() == nil {
		return
	}
	m.activeMu.Lock()
	delete(m.active, s.CurrentSession().FilePath)
	m.activeMu.Unlock()
}

// ActiveFor returns the live AgentSession instance for a session file path, if
// one is currently registered (e.g. a running remote-task fork).
func (m *SessionManager) ActiveFor(filePath string) *AgentSession {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	return m.active[filePath]
}

// Dir returns the session root directory.
func (m *SessionManager) Dir() string {
	return m.dir
}

// CreateSession creates a new in-memory session for cwd. Nothing touches
// disk — neither the per-cwd directory nor the session file — until the
// first assistant message is persisted (lazy creation in ensureFileLocked).
func (m *SessionManager) CreateSession(cwd string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	id := newSessionID()
	dir := filepath.Join(m.dir, EncodeCwd(cwd))
	s := &Session{
		Header: SessionHeader{
			Type:      "session",
			Version:   sessionVersion,
			ID:        id,
			Timestamp: now.UnixMilli(),
			Cwd:       cwd,
		},
		FilePath: filepath.Join(dir, sessionFileName(now, id)),
		idIndex:  make(map[string]*SessionEntry),
		manager:  m,
	}
	return s, nil
}

// LoadSession loads a session from a session file path.
func (m *SessionManager) LoadSession(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := &Session{FilePath: path, created: true, manager: m, idIndex: make(map[string]*SessionEntry)}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)

	line := 0
	for scanner.Scan() {
		data := scanner.Bytes()
		if len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		if line == 0 {
			if err := json.Unmarshal(data, &s.Header); err != nil {
				return nil, fmt.Errorf("invalid session header: %w", err)
			}
			if s.Header.Type != "session" {
				return nil, fmt.Errorf("not a session file: %s", path)
			}
			line++
			continue
		}
		var entry SessionEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			return nil, fmt.Errorf("invalid session entry at line %d: %w", line+1, err)
		}
		s.Entries = append(s.Entries, &entry)
		s.idIndex[entry.ID] = &entry
		s.LeafID = entry.ID
		if entry.Message != nil && entry.Message.Role == "assistant" {
			s.HasAssistant = true
		}
		line++
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if s.Header.ID == "" {
		return nil, fmt.Errorf("empty session file: %s", path)
	}
	return s, nil
}

// ListSessions scans the session directory and returns session summaries
// sorted by timestamp descending (newest first). Per-file summaries are cached
// on (size, mtime), so repeated listings only stat the files.
func (m *SessionManager) ListSessions() ([]SessionSummary, error) {
	var summaries []SessionSummary
	err := filepath.WalkDir(m.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if sum, ok := m.summaryFor(path, info); ok {
			summaries = append(summaries, sum)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Timestamp.After(summaries[j].Timestamp)
	})
	return summaries, nil
}

// summaryFor returns the summary for one session file, served from the cache
// when the file is unchanged since it was built. Files without a valid session
// header are skipped (ok=false) and never cached.
func (m *SessionManager) summaryFor(path string, info os.FileInfo) (SessionSummary, bool) {
	m.sumMu.Lock()
	if e, ok := m.sumCache[path]; ok && e.size == info.Size() && e.modTime.Equal(info.ModTime()) {
		m.sumMu.Unlock()
		return e.sum, true
	}
	m.sumMu.Unlock()

	hdr, ok, err := readSessionHeader(path)
	if err != nil || !ok {
		return SessionSummary{}, false
	}
	sum := SessionSummary{
		ID:           hdr.ID,
		FilePath:     path,
		Cwd:          hdr.Cwd,
		Timestamp:    time.UnixMilli(hdr.Timestamp),
		Name:         sessionDisplayName(path),
		MessageCount: countMessageLines(path),
		Kind:         hdr.Kind,
		SourceName:   hdr.SourceName,
		SourcePeerID: hdr.SourcePeerID,
		FirstMessage: firstUserMessageText(path),
	}
	m.sumMu.Lock()
	if m.sumCache == nil {
		m.sumCache = map[string]sumCacheEntry{}
	}
	m.sumCache[path] = sumCacheEntry{size: info.Size(), modTime: info.ModTime(), sum: sum}
	m.sumMu.Unlock()
	return sum, true
}

// FindSessionFile resolves a session id (or file basename) to its file path
// without scanning file contents: session files are named "<ts>_<id>.jsonl",
// so directory names are matched by id suffix / basename and confirmed by
// header. Returns "" when no session file matches.
func (m *SessionManager) FindSessionFile(idOrBasename string) string {
	idOrBasename = strings.TrimSpace(idOrBasename)
	if idOrBasename == "" || strings.Contains(idOrBasename, string(os.PathSeparator)) {
		return ""
	}
	wantFile := strings.HasSuffix(idOrBasename, ".jsonl")
	idSuffix := "_" + idOrBasename + ".jsonl"
	var match string
	filepath.WalkDir(m.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if match != "" {
			return filepath.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base != idOrBasename && !(!wantFile && strings.HasSuffix(base, idSuffix)) {
			return nil
		}
		// The header is authoritative: a file named after another session's
		// id must not resolve.
		if hdr, ok, err := readSessionHeader(path); err == nil && ok && hdr.ID != "" {
			if hdr.ID == idOrBasename || base == idOrBasename {
				match = path
				return filepath.SkipAll
			}
		}
		return nil
	})
	return match
}

// readSessionHeader reads only the first line of a session file (bounded).
func readSessionHeader(path string) (SessionHeader, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionHeader{}, false, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return SessionHeader{}, false, err
	}
	var hdr SessionHeader
	if err := json.Unmarshal(line, &hdr); err != nil {
		return SessionHeader{}, false, nil
	}
	return hdr, hdr.Type == "session", nil
}

// countMessageLines counts message entries without loading the whole file.
func countMessageLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		line := strings.TrimSpace(scanner.Text())
		if strings.Contains(line, `"type":"message"`) || strings.Contains(line, `"type": "message"`) {
			count++
		}
	}
	return count
}

// sessionDisplayName derives a display name from the file name:
// "<timestamp>_<id>.jsonl" → "<timestamp>".
func sessionDisplayName(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".jsonl")
	if idx := strings.LastIndex(base, "_"); idx > 0 {
		return strings.ReplaceAll(base[:idx], "T", " ")[:19]
	}
	return base
}

// firstMessagePreviewLen bounds the first-message preview (in runes).
const firstMessagePreviewLen = 80

// firstUserMessageText scans a session file from the top and returns a
// truncated preview of the first user message ("" when absent). The first
// user message is normally among the first lines, so the scan exits early.
func firstUserMessageText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	first := true
	for scanner.Scan() {
		if first { // skip the session header line
			first = false
			continue
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Type != "message" || entry.Message == nil || entry.Message.Role != "user" {
			continue
		}
		text := userContentText(entry.Message.Content)
		return truncateRunes(text, firstMessagePreviewLen)
	}
	return ""
}

// userContentText extracts text from a user message content field, which
// serializes either as a plain string or an array of content blocks.
func userContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// truncateRunes shortens s to at most n runes with an ellipsis.
func truncateRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return strings.TrimSpace(string(runes[:n])) + "…"
}

// DeleteSession removes a session file from disk (and prunes its cwd
// directory when it becomes empty). Deleting a session that is active
// anywhere must be prevented by the caller.
func (m *SessionManager) DeleteSession(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	// Best-effort cleanup of the now-empty per-cwd directory.
	_ = os.Remove(filepath.Dir(path))
	return nil
}
