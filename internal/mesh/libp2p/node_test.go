//go:build mesh

package libp2p

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"doge/internal/mesh"
)

func TestNodeMutualAuthAndProfileExchange(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	connectAndWait(t, a, b)

	prof := peerProfile(a, b.host.ID().String())
	if prof == nil {
		t.Fatal("a has no profile for b")
	}
	if prof.Name != "node-b" || len(prof.Roles) != 1 || prof.Roles[0] != "node-b" {
		t.Errorf("b profile seen from a: %+v", prof)
	}
	if prof.PeerID != b.host.ID().String() {
		t.Errorf("profile peer id = %q, want %q", prof.PeerID, b.host.ID())
	}
	if prof.Version != "test" {
		t.Errorf("version = %q, want test", prof.Version)
	}
	// Peers list the addrs and (optionally) latency.
	for _, p := range a.Peers() {
		if p.ID == b.host.ID().String() && len(p.Addrs) == 0 {
			t.Error("peer has no addrs")
		}
	}
}

func TestNodeSendSubscribe(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	connectAndWait(t, a, b)

	received := make(chan mesh.Envelope, 8)
	unsub := a.Subscribe(func(env mesh.Envelope) error {
		received <- env
		return nil
	})
	defer unsub()

	payload, _ := json.Marshal(map[string]string{"task": "hello"})
	env := mesh.Envelope{Type: mesh.MsgTask, To: a.host.ID().String(), Payload: payload}
	if err := b.Send(a.host.ID().String(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case got := <-received:
		if got.Type != mesh.MsgTask {
			t.Errorf("type = %q, want task", got.Type)
		}
		if got.From != b.host.ID().String() {
			t.Errorf("from = %q, want %q", got.From, b.host.ID())
		}
		var p map[string]string
		if err := json.Unmarshal(got.Payload, &p); err != nil || p["task"] != "hello" {
			t.Errorf("payload = %s, err %v", got.Payload, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for envelope")
	}

	// After unsubscribe, no further deliveries.
	unsub()
	if err := b.Send(a.host.ID().String(), env); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	select {
	case <-received:
		t.Fatal("received after unsubscribe")
	case <-time.After(400 * time.Millisecond):
	}
}

func TestNodeBroadcast(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	c := newNode(t, tok, "node-c")
	connectAndWait(t, a, b)
	connectAndWait(t, a, c)

	gotB := make(chan mesh.Envelope, 4)
	gotC := make(chan mesh.Envelope, 4)
	b.Subscribe(func(env mesh.Envelope) error { gotB <- env; return nil })
	c.Subscribe(func(env mesh.Envelope) error { gotC <- env; return nil })

	env := mesh.Envelope{Type: mesh.MsgCwdList} // empty To = broadcast
	if err := a.Broadcast(env); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	for i, ch := range []chan mesh.Envelope{gotB, gotC} {
		select {
		case got := <-ch:
			if got.Type != mesh.MsgCwdList || got.From != a.host.ID().String() {
				t.Errorf("peer %d got %+v", i, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("peer %d did not receive the broadcast", i)
		}
	}
}

func TestNodeRejectsWrongToken(t *testing.T) {
	tokA := testToken(t)
	tokC := testToken(t)
	a := newNode(t, tokA, "node-a")
	c := newNode(t, tokC, "node-c")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ai := peer.AddrInfo{ID: c.host.ID(), Addrs: c.host.Addrs()}
	_ = a.host.Connect(ctx, ai) // may succeed; the handshake must reject

	time.Sleep(1500 * time.Millisecond) // let the handshake fail and close
	for _, p := range a.Peers() {
		if p.ID == c.host.ID().String() {
			t.Fatalf("wrong-token peer %s became an authenticated peer", p.ID)
		}
	}
}

// TestNodeReconnect verifies that after a forced connection drop, the node's
// reconnect loop re-dials the peer and both sides re-authenticate and re-run
// the profile exchange automatically.
func TestNodeReconnect(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	connectAndWait(t, a, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.reconnectInterval = 500 * time.Millisecond
	go a.startReconnectLoop(ctx)

	// Force the connection down from A's side.
	if err := a.host.Network().ClosePeer(b.host.ID()); err != nil {
		t.Logf("ClosePeer: %v", err)
	}
	// Both sides should drop the peer from Peers().
	waitFor(t, 5*time.Second, func() bool {
		return !hasPeer(a, b.host.ID().String()) && !hasPeer(b, a.host.ID().String())
	}, "both sides drop the peer after disconnect")

	// A's reconnect loop must re-dial B; both re-authenticate + re-exchange
	// profiles without any explicit Connect call.
	waitFor(t, 15*time.Second, func() bool {
		return hasPeerProfile(a, b.host.ID().String()) && hasPeerProfile(b, a.host.ID().String())
	}, "peers re-connect after forced drop")
}
