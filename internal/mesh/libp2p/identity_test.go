//go:build mesh

package libp2p

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doge/internal/mesh"
)

// TestIdentityPersistence verifies a node keeps a stable PeerID across process
// restarts by persisting its libp2p key next to mesh.json.
func TestIdentityPersistence(t *testing.T) {
	tok := testToken(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mesh.json")
	cfg := &mesh.Config{}
	cfg.SetToken(tok)

	m1, err := New(mesh.Options{ConfigPath: path, Config: cfg, Probe: true})
	if err != nil {
		t.Fatalf("New #1: %v", err)
	}
	id1 := m1.ID()
	if err := m1.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}

	// A second node on the same config path must reuse the persisted identity.
	m2, err := New(mesh.Options{ConfigPath: path, Config: cfg, Probe: true})
	if err != nil {
		t.Fatalf("New #2: %v", err)
	}
	defer m2.Close()
	if m2.ID() != id1 {
		t.Fatalf("identity not persisted: %s != %s", id1, m2.ID())
	}

	// The key file exists with strict permissions.
	info, err := os.Stat(filepath.Join(dir, "mesh-key"))
	if err != nil {
		t.Fatalf("mesh-key not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mesh-key mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestListenPort verifies a configured fixed port is actually bound.
func TestListenPort(t *testing.T) {
	tok := testToken(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	path := filepath.Join(t.TempDir(), "mesh.json")
	cfg := &mesh.Config{ListenPort: port}
	cfg.SetToken(tok)
	m, err := New(mesh.Options{ConfigPath: path, Config: cfg, Probe: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	want := fmt.Sprintf("/tcp/%d", port)
	for _, a := range m.Addrs() {
		if strings.Contains(a, want) {
			return
		}
	}
	t.Errorf("node not listening on fixed port %d: addrs = %v", port, m.Addrs())
}
