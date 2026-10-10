//go:build mesh

package libp2p

import (
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// TestDiscoveryConnect verifies the mDNS notifee path deterministically: when
// HandlePeerFound is invoked with a discovered peer's AddrInfo, the node dials
// it and both sides complete mutual auth + profile exchange.
//
// It deliberately does not run two zeroconf instances in one process: on macOS
// that races for the shared multicast socket (224.0.0.0:5353) and is flaky.
// The actual mDNS multicast delivery is libp2p's code and is exercised by
// cross-process operation; this test covers our side of the handoff.
func TestDiscoveryConnect(t *testing.T) {
	tok := testToken(t)
	a := newNodeOpts(t, tok, "node-a", true)
	b := newNodeOpts(t, tok, "node-b", true)

	ai := peer.AddrInfo{ID: b.host.ID(), Addrs: b.host.Addrs()}
	done := make(chan struct{})
	go func() {
		a.HandlePeerFound(ai)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("HandlePeerFound did not connect to the discovered peer")
	}

	waitFor(t, 5*time.Second, func() bool {
		return hasPeerProfile(a, b.host.ID().String()) && hasPeerProfile(b, a.host.ID().String())
	}, "mutual auth + profile exchange after discovery")
}
