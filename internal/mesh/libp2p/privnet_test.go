//go:build mesh

package libp2p

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// TestPrivateNetworkConnect verifies that two nodes in private/VPN mode (with
// the transport-level PSK protector) connect and exchange profiles.
func TestPrivateNetworkConnect(t *testing.T) {
	tok := testToken(t)
	a := newNodeOpts(t, tok, "a", true)
	b := newNodeOpts(t, tok, "b", true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ai := peer.AddrInfo{ID: b.host.ID(), Addrs: b.host.Addrs()}
	if err := a.host.Connect(ctx, ai); err != nil {
		t.Fatalf("private-network connect failed: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return hasPeerProfile(a, b.host.ID().String()) && hasPeerProfile(b, a.host.ID().String())
	}, "profile exchange over private network")
}
