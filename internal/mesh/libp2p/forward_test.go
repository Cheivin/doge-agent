//go:build mesh

package libp2p

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doge/internal/mesh"
)

func TestParseAddr(t *testing.T) {
	network, addr := parseAddr("unix:///var/run/docker.sock")
	if network != "unix" || addr != "/var/run/docker.sock" {
		t.Errorf("unix parse = (%q, %q)", network, addr)
	}
	network, addr = parseAddr("127.0.0.1:8080")
	if network != "tcp" || addr != "127.0.0.1:8080" {
		t.Errorf("tcp parse = (%q, %q)", network, addr)
	}
}

// TestForwardTCPEcho: A exposes a local TCP echo server; B listens on a local
// port bridging to it. Connecting to B's port must round-trip through A's
// service (the acceptance flow "连 B 端口 === A 服务").
func TestForwardTCPEcho(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	connectAndWait(t, a, b)

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go acceptEcho(echoLn)
	echoPort := echoLn.Addr().(*net.TCPAddr).Port

	expose := mesh.ForwardRule{Name: "echo", Mode: "expose", Local: fmt.Sprintf("127.0.0.1:%d", echoPort), Enabled: true}
	if err := a.ForwardAdd(expose); err != nil {
		t.Fatalf("ForwardAdd expose: %v", err)
	}

	bPort := freeTCPPort(t)
	listen := mesh.ForwardRule{Name: "b-echo", Mode: "listen", Bind: "127.0.0.1", Port: bPort, Peer: a.host.ID().String(), Remote: "echo", Enabled: true}
	if err := b.ForwardAdd(listen); err != nil {
		t.Fatalf("ForwardAdd listen: %v", err)
	}

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", bPort))
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer conn.Close()
	msg := "ping-through-forward"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != msg {
		t.Errorf("echo = %q, want %q", buf, msg)
	}
}

// TestForwardUnixSocket: A exposes a unix socket; B bridges to it over TCP
// (unix <-> TCP bridging).
func TestForwardUnixSocket(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	connectAndWait(t, a, b)

	sockPath := filepath.Join(t.TempDir(), "svc.sock")
	uln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer uln.Close()
	go acceptEcho(uln)

	expose := mesh.ForwardRule{Name: "usvc", Mode: "expose", Local: "unix://" + sockPath, Enabled: true}
	if err := a.ForwardAdd(expose); err != nil {
		t.Fatalf("ForwardAdd unix expose: %v", err)
	}
	bPort := freeTCPPort(t)
	listen := mesh.ForwardRule{Name: "b-usvc", Mode: "listen", Bind: "127.0.0.1", Port: bPort, Peer: a.host.ID().String(), Remote: "usvc", Enabled: true}
	if err := b.ForwardAdd(listen); err != nil {
		t.Fatalf("ForwardAdd listen: %v", err)
	}

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", bPort))
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer conn.Close()
	msg := "unix-bridge"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo over unix bridge: %v", err)
	}
	if string(buf) != msg {
		t.Errorf("echo = %q, want %q", buf, msg)
	}
}

// TestForwardEnableDisable hot-stops and re-starts a single listen rule.
func TestForwardEnableDisable(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	b := newNode(t, tok, "node-b")
	connectAndWait(t, a, b)

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go acceptEcho(echoLn)
	if err := a.ForwardAdd(mesh.ForwardRule{Name: "echo", Mode: "expose", Local: fmt.Sprintf("127.0.0.1:%d", echoLn.Addr().(*net.TCPAddr).Port), Enabled: true}); err != nil {
		t.Fatal(err)
	}

	bPort := freeTCPPort(t)
	listen := mesh.ForwardRule{Name: "b-echo", Mode: "listen", Bind: "127.0.0.1", Port: bPort, Peer: a.host.ID().String(), Remote: "echo", Enabled: true}
	if err := b.ForwardAdd(listen); err != nil {
		t.Fatal(err)
	}
	// Accepts while enabled.
	if conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", bPort)); err != nil {
		t.Fatalf("dial while enabled: %v", err)
	} else {
		conn.Close()
	}

	// Disable: connections must be refused.
	if err := b.ForwardSetEnabled("b-echo", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", bPort), 400*time.Millisecond)
		if err != nil {
			return true
		}
		conn.Close()
		return false
	}, "listen port closed after disable")

	// Re-enable: accepts again.
	if err := b.ForwardSetEnabled("b-echo", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", bPort), 400*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, "listen port re-opened after enable")
}

// TestForwardReload reconciles forwards from mesh.json without restarting.
func TestForwardReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mesh.json")
	tok := testToken(t)
	cfg := &mesh.Config{}
	cfg.SetToken(tok)
	cfg.Forwards = []mesh.ForwardRule{{Name: "svc", Mode: "expose", Local: "127.0.0.1:1", Enabled: true}}
	if err := mesh.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	m, err := New(mesh.Options{ConfigPath: path, Config: cfg, Probe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := m.ForwardReload(); err != nil {
		t.Fatalf("ForwardReload: %v", err)
	}
	rules := m.ForwardList()
	if len(rules) != 1 || rules[0].Name != "svc" {
		t.Fatalf("rules after initial reload: %+v", rules)
	}

	// Swap svc -> other on disk, reload, verify the swap takes effect.
	cfg.Forwards = []mesh.ForwardRule{{Name: "other", Mode: "expose", Local: "127.0.0.1:2", Enabled: true}}
	if err := mesh.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.ForwardReload(); err != nil {
		t.Fatalf("ForwardReload 2: %v", err)
	}
	rules = m.ForwardList()
	if len(rules) != 1 || rules[0].Name != "other" {
		t.Fatalf("rules after reload swap: %+v", rules)
	}
	m.fwMu.Lock()
	_, hasSvc := m.forwards["svc"]
	_, hasOther := m.forwards["other"]
	m.fwMu.Unlock()
	if hasSvc {
		t.Error("svc handle should be gone after reload")
	}
	if !hasOther {
		t.Error("other handle should exist after reload")
	}
}

// TestForwardToolExecute drives the mesh_forward agent tool against a real
// mesh node.
func TestForwardToolExecute(t *testing.T) {
	tok := testToken(t)
	a := newNode(t, tok, "node-a")
	tool := ForwardTool(a)
	if tool.Name != "mesh_forward" {
		t.Fatalf("tool name = %q", tool.Name)
	}

	// list on an empty node.
	res, err := tool.Execute("id1", map[string]any{"action": "list"}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "No forward rules") {
		t.Errorf("list result = %+v", res.Content)
	}

	// add an expose rule via the tool.
	if _, err := tool.Execute("id2", map[string]any{
		"action": "add", "name": "echo", "mode": "expose", "local": "127.0.0.1:1",
	}, context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(a.ForwardList()) != 1 || a.ForwardList()[0].Name != "echo" {
		t.Fatalf("rules after tool add: %+v", a.ForwardList())
	}

	// remove it via the tool.
	if _, err := tool.Execute("id3", map[string]any{"action": "remove", "name": "echo"}, context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(a.ForwardList()) != 0 {
		t.Fatalf("rules after tool remove: %+v", a.ForwardList())
	}
}

func acceptEcho(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}(c)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
