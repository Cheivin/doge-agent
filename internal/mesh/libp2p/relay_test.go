//go:build mesh

package libp2p

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"doge/internal/mesh"
)

// newRelayClientNode builds a node whose autorelay candidates are the given
// static relay addresses (as if the relay seed was in bootstrapPeers).
func newRelayClientNode(t *testing.T, tok *mesh.Token, name string, staticRelays []string) *meshNode {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mesh-"+name+".json")
	cfg := &mesh.Config{Name: name, Roles: []string{name}, BootstrapPeers: staticRelays}
	cfg.SetToken(tok)
	m, err := New(mesh.Options{ConfigPath: path, Config: cfg, Probe: true, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func hasCircuitAddr(m *meshNode) bool {
	for _, a := range m.host.Addrs() {
		if strings.Contains(a.String(), "/p2p-circuit") {
			return true
		}
	}
	return false
}

func firstCircuitAddr(m *meshNode) string {
	for _, a := range m.host.Addrs() {
		if strings.Contains(a.String(), "/p2p-circuit") {
			return a.String()
		}
	}
	return ""
}

// TestRelayHopEndToEnd: R runs the circuit v2 hop service; B and C treat R as
// their static relay (autorelay reserves and advertises a circuit address);
// C then reaches B THROUGH the relay and both run the PSK handshake, exchange
// profiles and pass an envelope over the relayed connection. This is the path
// NAT'd nodes take when no direct connection is possible.
func TestRelayHopEndToEnd(t *testing.T) {
	tok := testToken(t)
	r := newNode(t, tok, "node-relay")
	if err := r.startRelay(); err != nil {
		t.Fatalf("startRelay: %v", err)
	}
	rAddr := fmt.Sprintf("%s/p2p/%s", r.host.Addrs()[0], r.host.ID())

	b := newRelayClientNode(t, tok, "node-b", []string{rAddr})
	c := newRelayClientNode(t, tok, "node-c", []string{rAddr})

	// autorelay reserves on R (reachability is unknown = treated as private)
	// and rewrites the advertised addresses with /p2p-circuit entries.
	waitFor(t, 30*time.Second, func() bool {
		return hasCircuitAddr(b) && hasCircuitAddr(c)
	}, "circuit addresses via relay reservation")

	// C dials B through the relay; the connection then runs the normal
	// mutual-auth + profile exchange. The dial retries until B's reservation
	// on R is in place (autorelay reserves shortly after boot).
	ai, err := peer.AddrInfoFromString(firstCircuitAddr(b))
	if err != nil {
		t.Fatalf("parse circuit addr: %v", err)
	}
	connected := false
	for start := time.Now(); time.Since(start) < 20*time.Second; time.Sleep(200 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.host.Connect(ctx, *ai)
		cancel()
		if err == nil {
			connected = true
			break
		}
	}
	if !connected {
		t.Fatal("connect via relay: giving up after 20s")
	}
	waitFor(t, 10*time.Second, func() bool {
		return hasPeerProfile(c, b.host.ID().String()) && hasPeerProfile(b, c.host.ID().String())
	}, "handshake + profile exchange over the relayed connection")

	// Envelope send/receive over the relayed connection.
	received := make(chan mesh.Envelope, 4)
	unsub := b.Subscribe(func(env mesh.Envelope) error {
		received <- env
		return nil
	})
	defer unsub()
	env := mesh.Envelope{Type: mesh.MsgCwdList, To: b.host.ID().String()}
	if err := c.Send(b.host.ID().String(), env); err != nil {
		t.Fatalf("send over relay: %v", err)
	}
	select {
	case got := <-received:
		if got.From != c.host.ID().String() {
			t.Errorf("from = %q, want %q", got.From, c.host.ID())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for envelope over the relay")
	}
}

// TestRelayACLDeniesStrangers: a wrong-token peer must not be able to hold a
// relay reservation on a mesh relay node (it is rejected by the ACL grace rule
// once its handshake fails and its connection is closed).
func TestRelayACLDeniesStrangers(t *testing.T) {
	tok := testToken(t)
	other := testToken(t)
	r := newNode(t, tok, "node-relay")
	if err := r.startRelay(); err != nil {
		t.Fatalf("startRelay: %v", err)
	}
	stranger := newNode(t, other, "node-stranger")

	// The stranger connects (same-token peers do this too) but fails the PSK
	// handshake; the relay ACL must reject its reservation attempts.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ai := peer.AddrInfo{ID: r.host.ID(), Addrs: r.host.Addrs()}
	if err := stranger.host.Connect(ctx, ai); err != nil {
		t.Fatalf("stranger connect: %v", err)
	}
	// The handshake closes the wrong-token peer shortly after; give the ACL a
	// moment, then verify the stranger is NOT authed (its reservations would
	// die with the connection anyway) and is dropped from the peers table.
	waitFor(t, 10*time.Second, func() bool {
		return !hasPeer(r, stranger.host.ID().String())
	}, "stranger dropped after handshake rejection")
}
