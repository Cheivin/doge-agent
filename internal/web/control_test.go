package web

import (
	"fmt"
	"sync"
	"testing"

	"doge/internal/coding"
)

// createSession is a no-op create for open() in ownership tests (only the
// ownership bookkeeping is exercised, never the active instance).
func createSession() (*coding.AgentSession, error) { return nil, nil }

// TestControlTakeover: a blocked/queued window can explicitly force-take
// write control from the current owner, releasing it immediately without the
// owner handing off.
func TestControlTakeover(t *testing.T) {
	tbl := newControlTable()
	key := "/sessions/s1.jsonl"

	// winA opens first → becomes the owner.
	tbl.open(key, "winA", createSession)
	if o, _ := tbl.Owner(key); o != "winA" {
		t.Fatalf("initial owner = %q, want winA", o)
	}
	// winB (read-only viewer) and winC queue for control (FIFO behind winA).
	tbl.open(key, "winB", createSession)
	tbl.acquire(key, "winB")
	tbl.open(key, "winC", createSession)
	tbl.acquire(key, "winC")

	// winB force-takes from winA. It was queued; its own queue slot must be
	// removed, and winC's (preserved) queue position stays for later.
	took, prev := tbl.takeover(key, "winB")
	if !took {
		t.Fatal("takeover reported not-taken on a known session")
	}
	if prev != "winA" {
		t.Fatalf("takeover prevOwner = %q, want winA", prev)
	}
	if o, _ := tbl.Owner(key); o != "winB" {
		t.Fatalf("owner after takeover = %q, want winB", o)
	}
	if !tbl.isOwner(key, "winB") {
		t.Fatal("winB should hold write control after takeover")
	}

	// When winB releases, the preserved waiter winC takes over (FIFO) — a
	// takeover does not destroy other windows' queue positions.
	tbl.release(key, "winB")
	if o, _ := tbl.Owner(key); o != "winC" {
		t.Fatalf("owner after release = %q, want winC (preserved waiter)", o)
	}
}

// TestControlTakeoverIdempotent: taking over a session you already own is a
// no-op success (no double bookkeeping, no transfer event).
func TestControlTakeoverIdempotent(t *testing.T) {
	tbl := newControlTable()
	key := "/sessions/s2.jsonl"
	tbl.open(key, "winA", createSession)

	var transfers []string
	tbl.SetOnTransfer(func(k, owner string) { transfers = append(transfers, owner) })

	took, prev := tbl.takeover(key, "winA")
	if !took || prev != "winA" {
		t.Fatalf("idempotent takeover = (%v, %q), want (true, winA)", took, prev)
	}
	if len(transfers) != 0 {
		t.Fatalf("idempotent takeover triggered %d transfer events", len(transfers))
	}
}

// TestControlTakeoverUnknownSession: taking over an unregistered session fails.
func TestControlTakeoverUnknownSession(t *testing.T) {
	tbl := newControlTable()
	if took, _ := tbl.takeover("/sessions/missing.jsonl", "winA"); took {
		t.Fatal("takeover of an unknown session must fail")
	}
}

// TestControlTakeoverFiresTransfer: a real takeover notifies the transfer hook
// with the new owner (the server broadcasts it to all windows).
func TestControlTakeoverFiresTransfer(t *testing.T) {
	tbl := newControlTable()
	key := "/sessions/s3.jsonl"
	tbl.open(key, "winA", createSession)

	var transfers []string
	tbl.SetOnTransfer(func(k, owner string) {
		if k == key {
			transfers = append(transfers, owner)
		}
	})
	tbl.open(key, "winB", createSession)
	tbl.takeover(key, "winB")

	if len(transfers) != 1 || transfers[0] != "winB" {
		t.Fatalf("transfer events = %v, want [winB]", transfers)
	}
}

// TestControlOpenConcurrentCreate: create() runs outside the table lock (it
// loads and replays a session file); concurrent opens of the same
// not-yet-loaded session must all end up sharing ONE active instance (first
// installer wins, losers' instances are discarded) with exactly one owner.
func TestControlOpenConcurrentCreate(t *testing.T) {
	tbl := newControlTable()
	key := "/sessions/conc.jsonl"

	const n = 8
	got := make([]*coding.AgentSession, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			active, _, err := tbl.open(key, fmt.Sprintf("win%d", i), func() (*coding.AgentSession, error) {
				inst := &coding.AgentSession{}
				return inst, nil
			})
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = active
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if got[i] == nil || got[i] != got[0] {
			t.Fatalf("consumer %d got instance %p, consumer 0 got %p (must share one)", i, got[i], got[0])
		}
	}
	owner, ok := tbl.Owner(key)
	if !ok || owner == "" {
		t.Fatal("no owner after concurrent open")
	}
	if readers := tbl.readersOf(key); len(readers) != n {
		t.Fatalf("readers = %d, want %d", len(readers), n)
	}
}

// TestDropStalePromotionBroadcast: 幽灵 owner 被清后 FIFO 提升新 owner 时必须
// 广播——被提升窗口的 UI 停在"排队中"，不广播它不知道自己已拿到写权
func TestDropStalePromotionBroadcast(t *testing.T) {
	tbl := newControlTable()
	key := "/sessions/s4.jsonl"
	tbl.open(key, "winA", createSession) // owner
	tbl.open(key, "winB", createSession)
	tbl.acquire(key, "winB")             // winB 排队
	tbl.open(key, "winC", createSession) // winC 只读

	var transferred []string
	tbl.SetOnTransfer(func(k, owner string) {
		if k == key {
			transferred = append(transferred, owner)
		}
	})
	// winA 失联（无活跃 WS 连接），winB/winC 在线
	tbl.SetLiveness(func(consumer string) bool { return consumer != "winA" })

	// 任意后续操作触发 dropStale：winA 让位、winB 提升、广播一次
	if owned, _ := tbl.acquire(key, "winC"); owned {
		t.Fatal("winC must not own ahead of queued winB")
	}
	if o, _ := tbl.Owner(key); o != "winB" {
		t.Fatalf("owner after stale drop = %q, want winB", o)
	}
	if len(transferred) != 1 || transferred[0] != "winB" {
		t.Fatalf("transfer events = %v, want [winB]", transferred)
	}
}
