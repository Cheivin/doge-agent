package web

import (
	"path/filepath"
	"sync"

	"doge/internal/coding"
	"doge/pkg/logx"
)

// sessionControl tracks the viewers of one session and who may write it.
type sessionControl struct {
	active  *coding.AgentSession // shared active instance (nil until first open)
	readers map[string]bool      // consumers viewing this session
	owner   string               // write-holder; "" = uncontended
	waiters []string             // FIFO queue for the next ownership
}

// controlTable arbitrates, per session, who may write when multiple
// consumers (the TUI and/or web windows) view the same session. Control is
// lazy: a session with a single consumer needs no arbitration at all — the
// first viewer simply becomes the owner. Sessions are keyed by their file
// path, and each holds one shared active AgentSession instance so all its
// viewers observe the same live stream and writes stay serialized.
type controlTable struct {
	mu         sync.Mutex
	sessions   map[string]*sessionControl // key = session file path
	onTransfer func(key, owner string)    // notified on forced takeover (server broadcasts)
	live       func(consumer string) bool // 消费者是否仍有活跃连接；nil 视为全部活跃
}

func newControlTable() *controlTable {
	return &controlTable{sessions: map[string]*sessionControl{}}
}

// SetOnTransfer installs the ownership-change notification hook. It is called
// with the session key and the new owner when ownership is forcibly
// transferred (takeover), so the server can notify the displaced owner and
// external clients. Graceful releases do not notify (ownership is discovered
// lazily by the next write).
func (t *controlTable) SetOnTransfer(fn func(key, owner string)) {
	t.mu.Lock()
	t.onTransfer = fn
	t.mu.Unlock()
}

// SetLiveness 注入消费者活跃度探针（窗口无活跃 WS 连接即视为放弃写权）。
// 仅服务端进程接线；单测不注入时全部视为活跃，保持原有语义。
func (t *controlTable) SetLiveness(fn func(consumer string) bool) {
	t.mu.Lock()
	t.live = fn
	t.mu.Unlock()
}

// dropStale 清理失去活跃连接的幽灵消费者：断开（刷新换 clientID、关闭标签页）
// 不会通知控制表，残留的 owner 会让后续查看者永远只读且 claim 只能排队。
// owner 让位后按 FIFO 提升仍活跃的排队者，waiters/readers 里的死项一并剔除。
func (t *controlTable) dropStale(key string, c *sessionControl) {
	if t.live == nil {
		return
	}
	for i := len(c.waiters) - 1; i >= 0; i-- {
		if !t.live(c.waiters[i]) {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
		}
	}
	for r := range c.readers {
		if !t.live(r) {
			delete(c.readers, r)
		}
	}
	if c.owner != "" && !t.live(c.owner) {
		logx.Infof("[web] stale owner %q of %s released (no live connection)", c.owner, filepath.Base(key))
		c.owner = ""
		for len(c.waiters) > 0 {
			w := c.waiters[0]
			if c.readers[w] {
				c.owner = w
				break
			}
			c.waiters = c.waiters[1:]
		}
		// 提升 owner 后广播一次：被提升窗口的 UI 停在"排队中"，
		// 不广播它不知道自己已拿到写权（与 takeover 的通知口径一致）
		if c.owner != "" && t.onTransfer != nil {
			t.onTransfer(key, c.owner)
		}
	}
}

// register seeds a session (e.g. the TUI's shared session) with its active
// instance. It is called once at startup.
func (t *controlTable) register(key string, active *coding.AgentSession) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.sessions[key]
	if c == nil {
		c = &sessionControl{readers: map[string]bool{}}
		t.sessions[key] = c
	}
	c.active = active
}

// open registers consumer as a viewer of the session keyed by key, creating
// the shared active instance via create if none exists yet. It returns the
// active instance and whether the consumer now holds write control (the
// first viewer of an uncontended session becomes the owner).
//
// create can be slow (it loads and replays the target session file), so it
// runs OUTSIDE the table lock — holding the global mutex across it would
// block every other window's control operations for the whole load. If two
// consumers create the same session concurrently, the first one to re-lock
// wins and the loser's instance is discarded (a freshly forked session has
// no resources to release).
func (t *controlTable) open(key, consumer string, create func() (*coding.AgentSession, error)) (*coding.AgentSession, bool, error) {
	t.mu.Lock()
	c := t.sessions[key]
	if c == nil {
		c = &sessionControl{readers: map[string]bool{}}
		t.sessions[key] = c
	}
	if c.active == nil {
		t.mu.Unlock()
		active, err := create()
		if err != nil {
			return nil, false, err
		}
		t.mu.Lock()
		if c.active == nil {
			c.active = active
		}
	}
	// 先清幽灵再注册自己：消费者此刻可能尚未建立 WS 连接（REST 先行），
	// 放在注册之后会把自己一并清掉
	t.dropStale(key, c)
	c.readers[consumer] = true
	if c.owner == "" {
		c.owner = consumer
	}
	owned := c.owner == consumer
	t.mu.Unlock()
	logx.Infof("[web] %s opened %s (owner=%v)", consumer, filepath.Base(key), owned)
	return c.active, owned, nil
}

// acquire requests write control of a session. An uncontended session is
// owned immediately; otherwise the consumer joins the FIFO waiter queue
// (idempotent) and stays read-only until the owner hands over.
func (t *controlTable) acquire(key, consumer string) (owned bool, queued bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.sessions[key]
	if c == nil {
		return false, false
	}
	t.dropStale(key, c)
	c.readers[consumer] = true // claim 即在看，补注册以防自己被 dropStale 清掉
	if c.owner == "" {
		c.owner = consumer
		return true, false
	}
	if c.owner == consumer {
		return true, false
	}
	for _, w := range c.waiters {
		if w == consumer {
			return false, true // already queued
		}
	}
	c.waiters = append(c.waiters, consumer)
	return false, true
}

// isOwner reports whether consumer holds write control of key.
func (t *controlTable) isOwner(key, consumer string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.sessions[key]
	return c != nil && c.owner == consumer
}

// release removes a consumer from a session, handing ownership to the next
// queued waiter (FIFO) when the owner leaves. Triggers onTransfer when the
// owner changes so the server can notify viewers.
func (t *controlTable) release(key, consumer string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.sessions[key]
	if c == nil {
		return
	}
	wasOwner := c.owner == consumer
	delete(c.readers, consumer)
	for i, w := range c.waiters {
		if w == consumer {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			break
		}
	}
	if !wasOwner {
		return
	}
	c.owner = ""
	for len(c.waiters) > 0 {
		w := c.waiters[0]
		c.waiters = c.waiters[1:]
		if c.readers[w] && (t.live == nil || t.live(w)) {
			c.owner = w
			break
		}
	}
	// Lazy control: an uncontended session (no waiters) is owned by its sole
	// remaining viewer, so a window that outlived the previous owner keeps
	// write control without an explicit claim.
	if c.owner == "" && len(c.readers) == 1 {
		for r := range c.readers {
			if t.live == nil || t.live(r) {
				c.owner = r
			}
		}
	}
	// Note: a graceful release does not broadcast an ownership change — the
	// displaced/new owner discovers control lazily on its next write (403 vs
	// accepted), preserving the per-window event stream's silence. Only a
	// forced takeover (takeover) notifies viewers, because the displaced owner
	// was not consulted.
}

// takeover forcibly transfers write control of a session to consumer,
// releasing the current owner (whoever it is — another web window or the TUI)
// without waiting for it to hand off. This is the explicit "强夺" path: a
// blocked window can deliberately take over instead of waiting in the FIFO
// queue. The taker's own queue position (if it was queued) is removed so it is
// never both owner and waiter; other windows' FIFO positions are preserved and
// take over when the new owner eventually releases. Triggers onTransfer when
// the owner changes. Returns whether the session exists and the previous
// owner's id ("" when uncontended).
func (t *controlTable) takeover(key, consumer string) (took bool, prevOwner string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.sessions[key]
	if c == nil {
		return false, ""
	}
	prev := c.owner
	logx.Warnf("[web] %s FORCE-TOOK %s from %q", consumer, filepath.Base(key), prev)
	for i, w := range c.waiters {
		if w == consumer {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			break
		}
	}
	c.owner = consumer
	c.readers[consumer] = true
	if t.onTransfer != nil && prev != consumer {
		t.onTransfer(key, consumer)
	}
	return true, prev
}

// Owner returns the current write-holder of a session and whether the session
// is known at all.
func (t *controlTable) Owner(key string) (owner string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.sessions[key]
	if c == nil {
		return "", false
	}
	return c.owner, true
}

// readersOf lists the consumers currently viewing key.
func (t *controlTable) readersOf(key string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.sessions[key]
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.readers))
	for r := range c.readers {
		out = append(out, r)
	}
	return out
}

// activeFor returns the shared active instance for key.

// actives returns every registered session's shared live instance — the
// target set for process-level redistribution (e.g. re-issuing the mcp
// toolset after a config reload).
func (t *controlTable) actives() []*coding.AgentSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*coding.AgentSession, 0, len(t.sessions))
	for _, c := range t.sessions {
		if c.active != nil {
			out = append(out, c.active)
		}
	}
	return out
}
