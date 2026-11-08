package web

import (
	"bytes"
	"testing"
	"time"

	"doge/internal/agent"
)

// shortenKeepalive 缩短保活参数（测试注入短超时），测试结束恢复原值
func shortenKeepalive(t *testing.T, write, ping time.Duration) {
	t.Helper()
	ow, op := wsWriteTimeout.Load(), wsPingInterval.Load()
	wsWriteTimeout.Store(int64(write))
	wsPingInterval.Store(int64(ping))
	t.Cleanup(func() {
		wsWriteTimeout.Store(ow)
		wsPingInterval.Store(op)
	})
}

// TestWriteTimeoutClosesStalledReader 复现"对端停止读"泄漏：客户端建立连接
// 后不再读取，服务端持续广播大帧灌满 TCP 缓冲，写循环必须在单帧超时内失败
// 并回收连接（此前 conn.Write 永久阻塞，goroutine 与连接双双泄漏，读循环
// 也因 ctx 永不取消而卡死）。
func TestWriteTimeoutClosesStalledReader(t *testing.T) {
	shortenKeepalive(t, 150*time.Millisecond, time.Hour)
	h := newHub()
	h.SetSnapshot(func(string) ([]byte, error) { return []byte(`{"type":"hi"}`), nil })
	conn, _, _ := newWSClient(t, h)
	_ = conn // 客户端从此不再读取任何帧

	// 大帧灌满对端 TCP 缓冲（客户端不读），写循环限时失败后回收连接
	big := bytes.Repeat([]byte("x"), 64<<10)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for i := 0; i < 4096; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.broadcast(big)
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// 服务端必须在时限内把该连接清扫干净（写失败 → 退出 → disconnect）
	waitFor(t, "stalled reader reclaimed", func() bool {
		return h.windowConns(defaultWindowID) == 0
	})
}

// TestPingTimeoutClosesSilentClient 无 close 帧的死连接：客户端不读也不回
// pong，服务端保活 ping 必须限时失败并回收连接。
func TestPingTimeoutClosesSilentClient(t *testing.T) {
	shortenKeepalive(t, 500*time.Millisecond, 100*time.Millisecond)
	h := newHub()
	h.SetSnapshot(func(string) ([]byte, error) { return []byte(`{"type":"hi"}`), nil })
	conn, _, _ := newWSClient(t, h)
	_ = conn // 客户端从不读 → 永不应答 ping

	// ping 周期 + ping 超时后，服务端应已把死连接清扫干净
	waitFor(t, "silent client reclaimed by ping keepalive", func() bool {
		return h.windowConns(defaultWindowID) == 0
	})
}

// TestKeepaliveHealthyClientUnaffected 正常客户端（持续读、自动回 pong）在
// 保活周期内必须保持连接，不被误杀。
func TestKeepaliveHealthyClientUnaffected(t *testing.T) {
	shortenKeepalive(t, time.Second, 50*time.Millisecond)
	h := newHub()
	h.SetSnapshot(func(string) ([]byte, error) { return []byte(`{"type":"hi"}`), nil })
	conn, _, ctx := newWSClient(t, h)
	readJSON(t, ctx, conn) // 注册快照

	// 跨过多个 ping 周期后仍能收到广播帧
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := h.AgentListener(agent.AgentEvent{Type: agent.EventTurnStart}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	m := readJSON(t, ctx, conn)
	if m["type"] != "turn_start" {
		t.Fatalf("healthy client frame = %#v", m)
	}
	// 连接仍在（未被保活误杀）
	if n := h.windowConns(defaultWindowID); n != 1 {
		t.Fatalf("healthy client window conns = %d, want 1", n)
	}
}
