//go:build mesh

package libp2p

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"

	"doge/internal/mesh"
)

// forwardHandle is the runtime state of one forwarding rule.
type forwardHandle struct {
	rule mesh.ForwardRule
	// proto is the registered /doge/forward/<name>/0.1 protocol (expose
	// rules), used to unregister on stop.
	proto protocol.ID
	// listener is the local listener (listen rules).
	listener net.Listener
	stop     chan struct{}
	stopOnce sync.Once
}

// forwardProtocol returns the protocol id for an exposed service name.
func forwardProtocol(name string) protocol.ID {
	return protocol.ID("/doge/forward/" + name + "/0.1")
}

// startForwards applies the configured forwards at startup.
func (m *meshNode) startForwards() {
	cfg, err := mesh.LoadConfig(m.configPath)
	if err != nil {
		meshLogf("load forwards: %v", err)
		return
	}
	if err := m.applyForwards(cfg.Forwards); err != nil {
		meshLogf("apply forwards: %v", err)
	}
}

// ForwardReload re-reads mesh.json and reconciles the running forwards without
// restarting the host or touching peer connections.
func (m *meshNode) ForwardReload() error {
	cfg, err := mesh.LoadConfig(m.configPath)
	if err != nil {
		return err
	}
	return m.applyForwards(cfg.Forwards)
}

// ForwardAdd validates and starts a rule, persisting it to mesh.json.
func (m *meshNode) ForwardAdd(rule mesh.ForwardRule) error {
	if err := mesh.ValidateRule(rule); err != nil {
		return err
	}
	return m.forwardStart(rule)
}

// ForwardRemove stops and removes a rule, persisting the change.
func (m *meshNode) ForwardRemove(name string) error {
	m.fwMu.Lock()
	fw := m.forwards[name]
	m.fwMu.Unlock()
	if fw == nil {
		return fmt.Errorf("forward %q not found", name)
	}
	m.forwardStop(fw)
	m.fwMu.Lock()
	delete(m.forwards, name)
	m.fwMu.Unlock()
	m.persistForwards()
	return nil
}

// ForwardSetEnabled hot-starts/stops a single rule without touching others.
func (m *meshNode) ForwardSetEnabled(name string, enabled bool) error {
	m.fwMu.Lock()
	fw := m.forwards[name]
	m.fwMu.Unlock()
	if fw == nil {
		return fmt.Errorf("forward %q not found", name)
	}
	if fw.rule.Enabled == enabled {
		return nil
	}
	if enabled {
		rule := fw.rule
		rule.Enabled = true
		return m.forwardStart(rule)
	}
	m.forwardStop(fw)
	m.fwMu.Lock()
	if cur := m.forwards[name]; cur != nil {
		cur.rule.Enabled = false
	}
	m.fwMu.Unlock()
	m.persistForwards()
	return nil
}

// ForwardList returns the current rules (including disabled ones).
func (m *meshNode) ForwardList() []mesh.ForwardRule {
	m.fwMu.Lock()
	defer m.fwMu.Unlock()
	out := make([]mesh.ForwardRule, 0, len(m.forwards))
	for _, fw := range m.forwards {
		out = append(out, fw.rule)
	}
	return out
}

// forwardStart registers/starts a rule and stores its handle.
func (m *meshNode) forwardStart(rule mesh.ForwardRule) error {
	fw := &forwardHandle{rule: rule, stop: make(chan struct{})}
	switch rule.Mode {
	case "expose":
		fw.proto = forwardProtocol(rule.Name)
		if !rule.Enabled {
			break // registered but stopped: no handler
		}
		m.host.SetStreamHandler(fw.proto, m.handleForwardStream(rule))
	case "listen":
		if err := m.startListenRule(fw); err != nil {
			return fmt.Errorf("forward %s: %w", rule.Name, err)
		}
	}
	m.fwMu.Lock()
	m.forwards[rule.Name] = fw
	m.fwMu.Unlock()
	m.persistForwards()
	return nil
}

// forwardStop stops a rule: unregisters the expose handler or closes the
// listener.
func (m *meshNode) forwardStop(fw *forwardHandle) {
	switch fw.rule.Mode {
	case "expose":
		m.host.SetStreamHandler(fw.proto, nil)
	case "listen":
		fw.stopOnce.Do(func() {
			close(fw.stop)
			if fw.listener != nil {
				_ = fw.listener.Close()
			}
		})
	}
}

// applyForwards reconciles the running rules against a target set (from
// config): stops removed/changed rules, starts new/changed ones.
func (m *meshNode) applyForwards(rules []mesh.ForwardRule) error {
	m.fwMu.Lock()
	current := make(map[string]*forwardHandle, len(m.forwards))
	for k, v := range m.forwards {
		current[k] = v
	}
	m.fwMu.Unlock()

	target := make(map[string]mesh.ForwardRule, len(rules))
	for _, r := range rules {
		target[r.Name] = r
	}

	// Stop rules that are gone or changed.
	for name, fw := range current {
		tr, ok := target[name]
		if ok && ruleEqual(fw.rule, tr) {
			continue
		}
		m.forwardStop(fw)
		m.fwMu.Lock()
		delete(m.forwards, name)
		m.fwMu.Unlock()
	}

	// Start rules that are new or changed.
	for name, r := range target {
		m.fwMu.Lock()
		_, exists := m.forwards[name]
		m.fwMu.Unlock()
		if exists {
			continue
		}
		if err := m.forwardStart(r); err != nil {
			meshLogf("forward %s start: %v", name, err)
		}
	}
	return nil
}

// handleForwardStream serves one expose stream: dial the local service and
// bridge bytes both ways. 未完成互验的 peer 不放行——expose 的本地服务往往
// 是 docker.sock 这类高权限端点，转发流必须等互验完成（同 agent/file 门禁），
// 不给重连刷宽限的余地。
func (m *meshNode) handleForwardStream(rule mesh.ForwardRule) network.StreamHandler {
	return func(s network.Stream) {
		defer s.Close()
		if !m.waitPeerAdmitted(s.Conn().RemotePeer(), m.authWait) {
			meshLogf("forward %s stream from %s rejected: mutual auth not complete", rule.Name, s.Conn().RemotePeer())
			_ = s.Reset()
			return
		}
		networkName, addr := parseAddr(rule.Local)
		conn, err := net.DialTimeout(networkName, addr, 15*time.Second)
		if err != nil {
			meshLogf("forward %s dial %s: %v", rule.Name, rule.Local, err)
			return
		}
		defer conn.Close()
		bridge(s, conn)
	}
}

// startListenRule binds a local listener and starts accepting.
func (m *meshNode) startListenRule(fw *forwardHandle) error {
	if !fw.rule.Enabled {
		return nil // registered but stopped
	}
	networkName, addr := parseAddr(fw.rule.Bind)
	if networkName == "tcp" {
		addr = net.JoinHostPort(fw.rule.Bind, strconv.Itoa(fw.rule.Port))
	}
	ln, err := net.Listen(networkName, addr)
	if err != nil {
		return err
	}
	fw.listener = ln
	go m.acceptLoop(fw, ln)
	return nil
}

// acceptLoop accepts local connections and bridges each to the remote service.
func (m *meshNode) acceptLoop(fw *forwardHandle, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-fw.stop:
				return
			default:
			}
			meshLogDbg("forward %s accept: %v", fw.rule.Name, err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go m.bridgeAccepted(fw, conn)
	}
}

// bridgeAccepted bridges one accepted connection to the remote exposed
// service (peer + remote name), opening a /doge/forward/<remote>/0.1 stream.
func (m *meshNode) bridgeAccepted(fw *forwardHandle, conn net.Conn) {
	defer conn.Close()
	// fw.rule.Peer is the base58 form; decode it back to a peer.ID.
	pid, err := peer.Decode(fw.rule.Peer)
	if err != nil {
		meshLogf("forward %s: invalid peer %q: %v", fw.rule.Name, fw.rule.Peer, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := m.host.NewStream(ctx, pid, forwardProtocol(fw.rule.Remote))
	if err != nil {
		meshLogf("forward %s stream to %s/%s: %v", fw.rule.Name, fw.rule.Peer, fw.rule.Remote, err)
		return
	}
	defer s.Close()
	bridge(s, conn)
}

// bridge copies bytes both directions. When one side closes, the other side
// is closed too, unblocking the reverse copy.
func bridge(s network.Stream, conn net.Conn) {
	go func() {
		_, _ = io.Copy(conn, s)
		_ = conn.Close()
	}()
	_, _ = io.Copy(s, conn)
	_ = s.Close()
}

// startForwardPoller watches mesh.json and hot-reloads forwards when the file
// changes externally (e.g. via `doge mesh forward ...` from another process).
func (m *meshNode) startForwardPoller(ctx context.Context) {
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.pollForwards()
			}
		}
	}()
}

func (m *meshNode) pollForwards() {
	cfg, err := mesh.LoadConfig(m.configPath)
	if err != nil {
		meshLogDbg("poll forwards: %v", err)
		return
	}
	m.fwMu.Lock()
	hash := forwardsHash(cfg.Forwards)
	sig := m.forwardsSig
	m.fwMu.Unlock()
	if hash == sig {
		return
	}
	if err := m.applyForwards(cfg.Forwards); err != nil {
		meshLogf("reload forwards: %v", err)
	}
	m.fwMu.Lock()
	m.forwardsSig = hash
	m.fwMu.Unlock()
}

// persistForwards writes the in-memory rules back to mesh.json and records
// the signature so the poller does not re-apply its own writes.
func (m *meshNode) persistForwards() {
	cfg, err := mesh.LoadConfig(m.configPath)
	if err != nil {
		meshLogf("persist forwards: %v", err)
		return
	}
	cfg.Forwards = m.ForwardList()
	if err := mesh.SaveConfig(m.configPath, cfg); err != nil {
		meshLogf("persist forwards: %v", err)
		return
	}
	m.fwMu.Lock()
	m.forwardsSig = forwardsHash(cfg.Forwards)
	m.fwMu.Unlock()
}

func forwardsHash(rules []mesh.ForwardRule) string {
	data, _ := json.Marshal(rules)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func ruleEqual(a, b mesh.ForwardRule) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

// parseAddr resolves "unix:///path" (network=unix) vs "host:port" (tcp).
func parseAddr(a string) (networkName, addr string) {
	if strings.HasPrefix(a, "unix://") {
		return "unix", strings.TrimPrefix(a, "unix://")
	}
	return "tcp", a
}
