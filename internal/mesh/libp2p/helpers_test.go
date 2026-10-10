//go:build mesh

package libp2p

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"doge/internal/mesh"
)

// testToken returns a fresh token for a test.
func testToken(t *testing.T) *mesh.Token {
	t.Helper()
	tok, err := mesh.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	return tok
}

// newNodeOpts builds an isolated mesh node (per-test config file, Probe mode so
// no forwards are started automatically).
func newNodeOpts(t *testing.T, tok *mesh.Token, name string, private bool) *meshNode {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mesh-"+name+".json")
	cfg := &mesh.Config{Name: name, Roles: []string{name}, PrivateNetwork: private}
	cfg.SetToken(tok)
	m, err := New(mesh.Options{ConfigPath: path, Config: cfg, Probe: true, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func newNode(t *testing.T, tok *mesh.Token, name string) *meshNode {
	return newNodeOpts(t, tok, name, false)
}

// connectAndWait directly connects a to b (using b's listen addrs) and waits
// until both sides have authenticated and exchanged profiles.
func connectAndWait(t *testing.T, a, b *meshNode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ai := peer.AddrInfo{ID: b.host.ID(), Addrs: b.host.Addrs()}
	if err := a.host.Connect(ctx, ai); err != nil {
		t.Fatalf("connect %s -> %s: %v", a.host.ID(), b.host.ID(), err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return hasPeerProfile(a, b.host.ID().String()) && hasPeerProfile(b, a.host.ID().String())
	}, "mutual profile exchange")
}

func hasPeer(m *meshNode, id string) bool {
	for _, p := range m.Peers() {
		if p.ID == id {
			return true
		}
	}
	return false
}

func hasPeerProfile(m *meshNode, id string) bool {
	for _, p := range m.Peers() {
		if p.ID == id && p.Profile.Name != "" {
			return true
		}
	}
	return false
}

func peerProfile(m *meshNode, id string) *mesh.AgentProfile {
	for _, p := range m.Peers() {
		if p.ID == id {
			return &p.Profile
		}
	}
	return nil
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}
