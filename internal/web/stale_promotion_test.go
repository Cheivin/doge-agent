package web

import (
	"net/http"
	"testing"

	"github.com/coder/websocket"
)

// TestStaleOwnerPromotesOldestLiveViewer 复现"幽灵持有者释放后在场窗口永远
// 只读"：owner 连接消失，第二个窗口以非 owner 连入并被 403；stale 清扫触发
// 时最老的活跃只读窗口必须被自动提升为 owner（而非把写权留给后连入的窗口），
// 且收到 session_control 通知翻转可写，写操作随即成功。
func TestStaleOwnerPromotesOldestLiveViewer(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	// winA 持有共享会话写权
	connA, ctxA := dialWSC(t, ts, c, "winA")
	readJSON(t, ctxA, connA)
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt (owner) = %d, want 202", code)
	}
	waitWindowIdle(s, "winA")

	// winB 连入（owner 仍在 → 只读，prompt 403）
	connB, ctxB := dialWSC(t, ts, c, "winB")
	readJSON(t, ctxB, connB)
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusForbidden {
		t.Fatalf("winB prompt (reader) = %d, want 403", code)
	}

	// winA 连接消失（等价于无 close 帧死连接的最终落点：hub 已无活跃连接）
	connA.Close(websocket.StatusNormalClosure, "gone")
	waitFor(t, "winA connection drained", func() bool { return s.hub.windowConns("winA") == 0 })

	// winC 连入触发 stale 清扫：winB（最老活跃读者）应被提升，而非 winC。
	// 清扫广播先于 winC 的注册快照入队，逐帧读到 session_info 为止。
	connC, ctxC := dialWSC(t, ts, c, "winC")
	var snapC map[string]any
	for i := 0; i < 3; i++ {
		snapC = readJSON(t, ctxC, connC)
		if snapC["type"] == "session_info" {
			break
		}
	}
	if snapC["type"] != "session_info" {
		t.Fatalf("winC never got session_info, last = %#v", snapC)
	}

	// winB 收到提升通知（与 takeover 同口径的 session_control 帧）
	m := readJSON(t, ctxB, connB)
	if m["type"] != "session_control" {
		t.Fatalf("winB frame = %#v, want session_control", m)
	}
	if m["owner"] != "winB" {
		t.Fatalf("session_control owner = %#v, want winB", m["owner"])
	}

	// 提升后 winB 可写（修复前此处一直 403）
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt after promotion = %d, want 202", code)
	}
	waitWindowIdle(s, "winB")

	// winC 仍是只读观察者
	st := getStatusFor(t, c, ts, "winC")
	if st["writable"] != false || st["owner"] != "winB" {
		t.Fatalf("winC status = writable=%v owner=%v, want false/winB", st["writable"], st["owner"])
	}
}

// TestStaleOwnerPromotesOldestWaiterFirst FIFO 排队者仍优先于普通读者：
// 幽灵 owner 释放时，先提升队列里最早的活跃排队者。
func TestStaleOwnerPromotesOldestWaiterFirst(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	connA, ctxA := dialWSC(t, ts, c, "winA")
	readJSON(t, ctxA, connA)
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt (owner) = %d, want 202", code)
	}
	waitWindowIdle(s, "winA")

	// winB 排队等待写权，winC 仅观察
	connB, ctxB := dialWSC(t, ts, c, "winB")
	readJSON(t, ctxB, connB)
	if b := doWindowClaim(t, c, ts, "winB"); b["queued"] != true {
		t.Fatalf("winB claim = %#v, want queued", b)
	}
	connC, ctxC := dialWSC(t, ts, c, "winC")
	readJSON(t, ctxC, connC)

	// winA 连接消失；winC 的 claim（acquire → dropStale）触发清扫
	connA.Close(websocket.StatusNormalClosure, "gone")
	waitFor(t, "winA connection drained", func() bool { return s.hub.windowConns("winA") == 0 })
	if b := doWindowClaim(t, c, ts, "winC"); b["owned"] != false {
		t.Fatalf("winC claim = %#v, want owned=false (winB promoted first)", b)
	}

	// FIFO 队头 winB 被提升并收到通知
	m := readJSON(t, ctxB, connB)
	if m["type"] != "session_control" || m["owner"] != "winB" {
		t.Fatalf("winB frame = %#v, want session_control owner=winB", m)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt after promotion = %d, want 202", code)
	}
	waitWindowIdle(s, "winB")
}
