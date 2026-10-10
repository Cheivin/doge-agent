package web

import (
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// 同 key 切换（切到当前已在看的会话）不得抹掉所有权：此前 SwitchSession
// 以 release(oldKey) 收尾，oldKey==key 时把刚 open 注册的 reader 与 owner
// 一起清掉，窗口无端变只读。前端快照未就绪时点击会话列表即触发此路径。
func TestSameKeySwitchKeepsOwnership(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	conn, ctx := dialWSC(t, ts, c, "winA")
	readJSON(t, ctx, conn)

	// winA 是共享会话第一个查看者 → 持有写权
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt (owner) = %d, want 202", code)
	}
	waitWindowIdle(s, "winA")

	sharedID := s.shared.CurrentSession().Header.ID
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/switch", `{"id":"`+sharedID+`"}`, "winA"); code != http.StatusOK {
		t.Fatalf("switch to current = %d", code)
	}
	time.Sleep(50 * time.Millisecond)

	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("prompt after same-key switch = %d, want 202", code)
	}
	waitWindowIdle(s, "winA")
}

// 幽灵持有者自愈：断开（刷新换 clientID、关闭标签页）不释放所有权是历史
// 缺陷——残留 owner 让新窗口永远只读且 claim 只能排队。控制表以活跃连接
// 数为探针，断开的持有者视为放弃，新窗口 claim 立即生效。
func TestStaleOwnerReleasedForNewViewer(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	connA, ctxA := dialWSC(t, ts, c, "winA")
	readJSON(t, ctxA, connA)
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt (owner) = %d", code)
	}
	waitWindowIdle(s, "winA")

	// winA 消失（页面关闭后 clientID 丢失，重开变成 winB）
	connA.Close(websocket.StatusNormalClosure, "gone")
	time.Sleep(100 * time.Millisecond)

	connB, ctxB := dialWSC(t, ts, c, "winB")
	readJSON(t, ctxB, connB)

	res := doWindowClaim(t, c, ts, "winB")
	if queued, _ := res["queued"].(bool); queued {
		t.Fatalf("claim queued behind stale owner, res=%v", res)
	}
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winB"); code != http.StatusAccepted {
		t.Fatalf("winB prompt after stale owner healed = %d, want 202", code)
	}
	waitWindowIdle(s, "winB")
}

// 同 clientID 刷新的间隙（旧连接已断、新连接未建）不能丢写权：claim 先于
// WS 重连到达时，dropStale 会清掉自己的旧记录，acquire 必须把 claim 者
// 补注册为 reader 并立即重新持有。
func TestClaimDuringRefreshGapReacquires(t *testing.T) {
	llm := startMockLLM(t)
	t.Cleanup(llm.Close)
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	connA, ctxA := dialWSC(t, ts, c, "winA")
	readJSON(t, ctxA, connA)
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("winA prompt (owner) = %d", code)
	}
	waitWindowIdle(s, "winA")

	// 旧连接断开，新连接未建立前先 claim（前端 claim 先于 WS 重连的时序）
	connA.Close(websocket.StatusNormalClosure, "refresh")
	time.Sleep(100 * time.Millisecond)
	res := doWindowClaim(t, c, ts, "winA")
	if owned, _ := res["owned"].(bool); !owned {
		t.Fatalf("re-claim during refresh gap lost ownership, res=%v", res)
	}

	// WS 重连后写权依旧
	connA2, ctxA2 := dialWSC(t, ts, c, "winA")
	readJSON(t, ctxA2, connA2)
	if code := doWindow(t, c, "POST", ts.URL+"/api/prompt", `{"text":"hi"}`, "winA"); code != http.StatusAccepted {
		t.Fatalf("prompt after reconnect = %d, want 202", code)
	}
	waitWindowIdle(s, "winA")
}
