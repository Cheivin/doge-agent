//go:build mesh

// Package libp2p is the libp2p-backed implementation of the mesh.Mesh
// interface. It is only compiled with -tags mesh; the default build uses the
// disabled stub in internal/mesh.
package libp2p

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	"github.com/libp2p/go-libp2p/p2p/host/eventbus"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ma "github.com/multiformats/go-multiaddr"

	"doge/internal/mesh"
	"doge/pkg/logx"
)

// meshNode implements mesh.Mesh on top of a libp2p host.
type meshNode struct {
	opts  mesh.Options
	cfg   *mesh.Config
	token *mesh.Token
	psk   []byte

	host     host.Host
	mdnsSvc  io.Closer
	kdht     io.Closer
	relaySvc *relay.Relay

	// rd is the rendezvous discovery used to feed autorelay candidates when
	// no static seeds are configured (set by startDiscovery).
	rd *routing.RoutingDiscovery

	// seedRelayAddrs are the configured seed addresses with /p2p/<seedID>
	// appended, ready to be extended with /p2p-circuit/p2p/<selfID> by
	// relayAddrsFactory.
	seedRelayAddrs []ma.Multiaddr
	// selfID is the node's peer ID derived from the persisted key. It is set
	// before the libp2p host exists, so address factories can run during host
	// construction.
	selfID peer.ID

	configPath string

	mu        sync.Mutex
	peers     map[peer.ID]*peerState
	subs      map[uint64]*subEntry
	subSeq    uint64
	localCaps []mesh.Capability
	// taskLoad is the current number of in-flight remote tasks (set by the
	// collab handler as tasks start/finish); it is advertised in the profile's
	// Load so peers can do load-aware role routing.
	taskLoad int

	fwMu     sync.Mutex
	forwards map[string]*forwardHandle
	// forwardsSig is the content hash of the forwards last applied/seen; the
	// reload poller compares against it to detect external config changes.
	forwardsSig string

	closed    chan struct{}
	closeOnce sync.Once

	// toolNames 实时会话工具名（profile 上报用），attach 后注入
	toolNames func() []string

	// reconnectInterval controls how often known-but-disconnected peers are
	// re-dialed after a network drop. Configurable for tests.
	reconnectInterval time.Duration

	// authWait 数据流门禁等待互验完成的上限（默认 streamHandshakeGrace，
	// 测试可缩短）
	authWait time.Duration

	// foundActive single-flights the mDNS notifiee's per-peer retry loop so
	// repeated browse announcements don't pile up duplicate dial loops.
	foundActive map[peer.ID]bool
}

// peerState tracks one connected peer.
type peerState struct {
	addrs       []string
	profile     *mesh.AgentProfile
	caps        []mesh.Capability
	authed      bool
	connectedAt time.Time
	latency     time.Duration
}

// subEntry is one registered Envelope subscriber.
type subEntry struct {
	seq uint64
	h   mesh.EnvelopeHandler
}

// New builds a mesh node: it creates the libp2p host (with the optional
// PrivateNetwork PSK protector), registers the stream handlers and the
// connection notifier. Discovery and forwards start with Start.
func New(opts mesh.Options) (*meshNode, error) {
	cfg := opts.Config
	if cfg == nil {
		c, err := mesh.LoadConfig(opts.ConfigPath)
		if err != nil {
			return nil, err
		}
		cfg = c
	}
	path := opts.ConfigPath
	if path == "" {
		path = mesh.DefaultConfigPath()
	}
	tok, err := cfg.TokenObj()
	if err != nil {
		return nil, err
	}
	psk, err := pskBytes(tok.PSK)
	if err != nil {
		return nil, err
	}
	// Seed addresses embedded in the token (via `doge mesh new --bootstrap`
	// or `doge mesh join`) are honored when the config doesn't set its own, so
	// a node joining with a shared token automatically gets the network's
	// seed list.
	if len(cfg.BootstrapPeers) == 0 && len(tok.BootstrapPeers) > 0 {
		cfg.BootstrapPeers = tok.BootstrapPeers
	}

	// Persist the node identity (stable PeerID across restarts) and use the
	// configured fixed listen port when set (stable address for seeds).
	key, err := loadOrCreateKey(path)
	if err != nil {
		return nil, err
	}
	listenAddr := "/ip4/0.0.0.0/tcp/0"
	if cfg.ListenPort > 0 {
		listenAddr = fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", cfg.ListenPort)
	}
	m := &meshNode{
		opts:              opts,
		cfg:               cfg,
		token:             tok,
		psk:               psk,
		configPath:        path,
		peers:             map[peer.ID]*peerState{},
		subs:              map[uint64]*subEntry{},
		forwards:          map[string]*forwardHandle{},
		closed:            make(chan struct{}),
		reconnectInterval: 10 * time.Second,
	}
	if m.selfID, err = peer.IDFromPrivateKey(key); err != nil {
		return nil, err
	}
	hostOpts := []libp2p.Option{
		libp2p.Identity(key),
		libp2p.ListenAddrStrings(listenAddr),
		// TCP only. libp2p v0.45.0 pins quic-go v0.55.0, which panics on the
		// server side of any QUIC connection under Go 1.26 ("crypto/tls bug:
		// where's my session ticket?" — libp2p's TLS identity disables session
		// tickets; quic-go v0.56+ fixed it but dropped the logging/metrics
		// packages libp2p v0.45.0 needs). A newer libp2p could restore QUIC;
		// until then the mesh runs over TCP only.
		libp2p.Transport(tcp.NewTCPTransport),
		// NAT traversal: router port mapping (UPnP/NAT-PMP; a silent no-op
		// when the router doesn't support it) plus DCUtR hole punching, which
		// upgrades relayed connections to direct ones when possible.
		libp2p.NATPortMap(),
		libp2p.EnableHolePunching(),
	}
	if cfg.PrivateNetwork {
		// Transport-level PSK admission for the private/VPN deployment mode.
		// All nodes in the token network must be in the same mode.
		hostOpts = append(hostOpts, libp2p.PrivateNetwork(deriveKey(psk, "libp2p-private-network")))
	}
	// Auto relay: a NAT'd node reserves relay slots and advertises circuit
	// addresses so peers can reach it through the relay (hole punching then
	// tries to upgrade to a direct connection). Configured seeds are the
	// static candidates (the recommended topology — the seed runs relay:true);
	// without seeds, candidates come from the rendezvous discovery. The
	// library defaults pace relay discovery at a 3-minute boot delay and a
	// 30-second interval — far too slow next to the 10-second reconnect loop,
	// so both are tightened.
	relayOpts := []autorelay.Option{autorelay.WithBootDelay(3 * time.Second), autorelay.WithMinInterval(10 * time.Second)}
	if relays := m.staticRelays(); len(relays) > 0 {
		hostOpts = append(hostOpts, libp2p.EnableAutoRelayWithStaticRelays(relays, relayOpts...))
		// 种子的完整 multiaddr（含 /p2p/<id>），供 relayAddrsFactory 拼 circuit 地址
		for _, ai := range relays {
			for _, a := range ai.Addrs {
				m.seedRelayAddrs = append(m.seedRelayAddrs, a.Encapsulate(ma.StringCast(fmt.Sprintf("/p2p/%s", ai.ID))))
			}
		}
	} else {
		hostOpts = append(hostOpts, libp2p.EnableAutoRelayWithPeerSource(m.relayPeerSource, relayOpts...))
	}
	// autorelay only advertises circuit addresses built from the relay's
	// PUBLIC peerstore addrs (cleanupAddressSet drops private ones), which
	// breaks VPN-overlay seeds and leaves nothing to advertise in in-process
	// setups. The reservation still comes from autorelay; this factory makes
	// the advertisement deterministic by appending the seed relay's circuit
	// address explicitly.
	if len(m.seedRelayAddrs) > 0 {
		hostOpts = append(hostOpts, libp2p.AddrsFactory(m.relayAddrsFactory))
	}
	h, err := libp2p.New(hostOpts...)
	if err != nil {
		return nil, fmt.Errorf("libp2p host: %w", err)
	}
	m.host = h

	// autorelay's finder only starts after a reachability event, and we don't
	// run autonat (no event would ever arrive). Emit an initial Private event
	// (retained for late subscribers): the finder starts on Private/Unknown,
	// but the address manager only advertises relay addresses when the state
	// is Private — doge nodes are typically behind NAT, so claiming Private
	// without better knowledge is the conservative, relay-enabled stance.
	if em, err := m.host.EventBus().Emitter(new(event.EvtLocalReachabilityChanged), eventbus.Stateful); err == nil {
		_ = em.Emit(event.EvtLocalReachabilityChanged{Reachability: network.ReachabilityPrivate})
		_ = em.Close()
	}

	h.SetStreamHandler(protoHandshake, m.handleHandshakeStream)
	h.SetStreamHandler(protoAgent, m.handleAgentStream)
	if m.opts.Uploads != nil {
		h.SetStreamHandler(protoFile, m.handleFileStream)
	}
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF:    m.onConnected,
		DisconnectedF: m.onDisconnected,
	})
	return m, nil
}

// Start runs discovery, forwards and the background loops, then blocks until
// ctx is canceled.
func (m *meshNode) Start(ctx context.Context) error {
	if err := m.startDiscovery(ctx); err != nil {
		return err
	}
	if !m.opts.Probe {
		m.startForwards()
		m.startForwardPoller(ctx)
		if m.cfg.Relay {
			// 中继起不来只降级（少了中继路径），不影响 mesh 其余能力
			if err := m.startRelay(); err != nil {
				meshLogf("relay service: %v", err)
			}
		}
	}
	m.startProfilePusher(ctx)
	m.startReconnectLoop(ctx)
	<-ctx.Done()
	return nil
}

func (m *meshNode) Close() error {
	m.closeOnce.Do(func() { close(m.closed) })
	if m.relaySvc != nil {
		_ = m.relaySvc.Close()
	}
	if m.mdnsSvc != nil {
		_ = m.mdnsSvc.Close()
	}
	if m.kdht != nil {
		_ = m.kdht.Close()
	}
	return m.host.Close()
}

// ID returns this node's PeerID.
func (m *meshNode) ID() string { return m.host.ID().String() }

// Addrs returns this node's listen multiaddrs (without the /p2p/<id> suffix).
func (m *meshNode) Addrs() []string {
	addrs := m.host.Addrs()
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

// Config 返回节点启动时的 mesh.json 快照（含 token，供 web API 展示）。
// 转发规则的实时状态不在其中——走 ForwardList（热重载后是最新的）。
func (m *meshNode) Config() mesh.Config {
	return *m.cfg
}

// Peers returns the connected, authenticated peers with their cached profiles.
func (m *meshNode) Peers() []mesh.PeerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mesh.PeerInfo, 0, len(m.peers))
	for id, st := range m.peers {
		if !st.authed {
			continue
		}
		pi := mesh.PeerInfo{ID: id.String(), Addrs: st.addrs, Caps: st.caps, Latency: st.latency}
		if st.profile != nil {
			pi.Profile = *st.profile
		}
		out = append(out, pi)
	}
	return out
}

// Advertise records a local capability and re-broadcasts the profile so peers
// see the updated capability set.
func (m *meshNode) Advertise(cap mesh.Capability) {
	m.mu.Lock()
	for _, c := range m.localCaps {
		if c == cap {
			m.mu.Unlock()
			return
		}
	}
	m.localCaps = append(m.localCaps, cap)
	m.mu.Unlock()
	m.rebroadcastProfile()
}

// Send delivers one Envelope to a peer over a fresh point-to-point stream.
func (m *meshNode) Send(peerID string, env mesh.Envelope) error {
	// peerID is the base58 form (from PeerInfo.ID / peer.ID.String()); decode
	// it back to a peer.ID — peer.ID(x) alone would double-encode.
	pid, err := peer.Decode(peerID)
	if err != nil {
		return fmt.Errorf("invalid peer id %q: %w", peerID, err)
	}
	if m.host.Network().Connectedness(pid) != network.Connected {
		return mesh.ErrPeerNotConnected
	}
	if env.From == "" {
		env.From = m.host.ID().String()
	}
	if env.ID == "" {
		env.ID = newID()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := m.host.NewStream(ctx, pid, protoAgent)
	if err != nil {
		return fmt.Errorf("open stream to %s: %w", pid, err)
	}
	defer s.Close()
	return writeFrame(s, env)
}

// Broadcast fans a small Envelope out to every connected, authenticated peer.
func (m *meshNode) Broadcast(env mesh.Envelope) error {
	m.mu.Lock()
	ids := make([]string, 0, len(m.peers))
	for id, st := range m.peers {
		if st.authed {
			ids = append(ids, id.String())
		}
	}
	m.mu.Unlock()
	var firstErr error
	for _, id := range ids {
		if err := m.Send(id, env); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Subscribe registers an Envelope handler; the returned function unsubscribes.
func (m *meshNode) Subscribe(h mesh.EnvelopeHandler) func() {
	m.mu.Lock()
	m.subSeq++
	e := &subEntry{seq: m.subSeq, h: h}
	m.subs[e.seq] = e
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.subs, e.seq)
		m.mu.Unlock()
	}
}

// handleAgentStream reads Envelope frames until EOF and dispatches them.
// 未完成互验的 peer 不放行：等待互验（见 waitPeerAdmitted），等不到（错
// token / 静默攻击者，连接已被 establish 断开）即 reset，一帧都不会分派。
func (m *meshNode) handleAgentStream(s network.Stream) {
	defer s.Close()
	pid := s.Conn().RemotePeer()
	if !m.waitPeerAdmitted(pid, m.authWait) {
		meshLogf("agent stream from %s rejected: mutual auth not complete", pid)
		_ = s.Reset()
		return
	}
	from := pid.String()
	for {
		var env mesh.Envelope
		if err := readFrame(s, &env); err != nil {
			if err != io.EOF {
				meshLogf("agent stream from %s: %v", from, err)
			}
			return
		}
		if env.From == "" {
			env.From = from
		}
		m.dispatch(env)
	}
}

// handleHandshakeStream performs the inbound side of mutual authentication.
// The peer hashes our ID (HMAC(psk, ourID)); we verify against our own ID and
// reply with HMAC(psk, theirID), which they verify against their own ID.
func (m *meshNode) handleHandshakeStream(s network.Stream) {
	defer s.Close()
	pid := s.Conn().RemotePeer()
	// 首帧限期：挂着握手流不发的对端到点即断，不占 handler goroutine
	s.SetDeadline(time.Now().Add(handshakeTimeout))
	var msg handshakeMsg
	if err := readFrame(s, &msg); err != nil {
		meshLogf("handshake read from %s: %v", pid, err)
		return
	}
	if !hmacEqual(msg.Token, hmacToken(m.psk, m.host.ID().String())) {
		meshLogf("handshake rejected for %s", pid)
		_ = m.host.Network().ClosePeer(pid)
		return
	}
	_ = writeFrame(s, handshakeMsg{PeerID: m.host.ID().String(), Token: hmacToken(m.psk, pid.String())})
}

// dispatch fans an Envelope out to every subscriber. Advertise envelopes are
// also handled internally (profile cache update).
func (m *meshNode) dispatch(env mesh.Envelope) {
	if env.Type == mesh.MsgAdvertise {
		m.handleAdvertise(env)
	}
	m.mu.Lock()
	entries := make([]*subEntry, 0, len(m.subs))
	for _, e := range m.subs {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	for _, e := range entries {
		if err := e.h(env); err != nil {
			meshLogf("envelope handler: %v", err)
		}
	}
}

// handleAdvertise decrypts and caches a peer's AgentProfile.
func (m *meshNode) handleAdvertise(env mesh.Envelope) {
	plain, err := decryptRecord(m.psk, "advertise", []byte(env.Payload))
	if err != nil {
		meshLogf("decrypt advertise from %s: %v", env.From, err)
		return
	}
	var prof mesh.AgentProfile
	if err := json.Unmarshal(plain, &prof); err != nil {
		meshLogf("parse advertise from %s: %v", env.From, err)
		return
	}
	pid, err := peer.Decode(env.From)
	if err != nil {
		meshLogf("advertise from invalid peer %q", env.From)
		return
	}
	prof.PeerID = env.From // trust the stream identity over the payload
	m.mu.Lock()
	if st := m.peers[pid]; st != nil {
		cp := prof
		st.profile = &cp
		st.caps = capsFromProfile(cp)
	}
	m.mu.Unlock()
}

// onConnected starts the (async) mutual-auth + profile exchange.
func (m *meshNode) onConnected(_ network.Network, c network.Conn) {
	pid := c.RemotePeer()
	m.mu.Lock()
	st := m.peers[pid]
	if st == nil {
		st = &peerState{}
		m.peers[pid] = st
	}
	st.addrs = appendUnique(st.addrs, c.RemoteMultiaddr().String())
	// Refreshed on every connection: the relay ACL grants a short grace to
	// peers whose PSK handshake is still in flight, keyed on this timestamp.
	st.connectedAt = time.Now()
	m.mu.Unlock()
	go m.establish(pid)
}

func (m *meshNode) onDisconnected(_ network.Network, c network.Conn) {
	pid := c.RemotePeer()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.peers[pid]
	if st == nil {
		return
	}
	// 同一 peer 可有多条连接（双向同时重拨的竞态会留下两条）：只要还有存活
	// 连接就不动状态，否则单条断开会把仍在线的 peer 从 Peers() 隐藏且无法恢复
	// （Disconnected 回调时 swarm 已把该连接从连接表移除，ConnsToPeer 是准的）。
	if len(m.host.Network().ConnsToPeer(pid)) > 0 {
		return
	}
	if !st.authed {
		// Never authenticated (e.g. a wrong-token peer whose handshake was
		// rejected): don't retain it for reconnection.
		delete(m.peers, pid)
		return
	}
	// Keep the entry + addresses so the reconnect loop can re-dial it after a
	// network drop. It is hidden from Peers() until it authenticates again.
	st.authed = false
}

// startReconnectLoop periodically re-dials known-but-disconnected peers so a
// network drop recovers automatically. mDNS and bootstrap peers have no other
// re-discovery path (libp2p mDNS emits each peer once); DHT peers also get
// re-found by findLoop, but this loop covers every previously-authenticated
// peer uniformly.
func (m *meshNode) startReconnectLoop(ctx context.Context) {
	go func() {
		interval := m.reconnectInterval
		if interval <= 0 {
			interval = 10 * time.Second
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.reconnectKnownPeers(ctx)
			}
		}
	}()
}

// reconnectKnownPeers dials every known peer that is not currently connected.
// The recorded connection addresses are passed along explicitly: addresses
// learned passively (identify) live in the peerstore only 2–15 minutes, so an
// ID-only re-dial fails with "no addresses" once a network drop outlasts them.
// Connect re-absorbs the given addresses with a permanent TTL, then the
// connection notifier re-runs mutual auth + profile exchange automatically.
func (m *meshNode) reconnectKnownPeers(ctx context.Context) {
	type target struct {
		id    peer.ID
		addrs []string
	}
	m.mu.Lock()
	targets := make([]target, 0, len(m.peers))
	for id, st := range m.peers {
		targets = append(targets, target{id: id, addrs: st.addrs})
	}
	m.mu.Unlock()
	for _, t := range targets {
		if m.host.Network().Connectedness(t.id) == network.Connected {
			continue
		}
		addrs := make([]ma.Multiaddr, 0, len(t.addrs))
		for _, s := range t.addrs {
			if a, err := ma.NewMultiaddr(s); err == nil {
				addrs = append(addrs, a)
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := m.host.Connect(cctx, peer.AddrInfo{ID: t.id, Addrs: addrs})
		cancel()
		if err != nil {
			meshLogf("reconnect %s: %v", t.id, err)
		}
	}
}

// establish performs mutual auth, then exchanges profiles with the peer.
func (m *meshNode) establish(pid peer.ID) {
	select {
	case <-m.closed:
		return
	default:
	}
	if err := m.handshake(pid); err != nil {
		meshLogf("handshake with %s failed: %v", pid, err)
		_ = m.host.Network().ClosePeer(pid)
		return
	}
	m.mu.Lock()
	if st := m.peers[pid]; st != nil {
		st.authed = true
	}
	m.mu.Unlock()
	go m.measureLatency(pid)
	m.sendProfile(pid)
}

// handshake performs the outbound side of mutual auth.
func (m *meshNode) handshake(pid peer.ID) error {
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	s, err := m.host.NewStream(ctx, pid, protoHandshake)
	if err != nil {
		return err
	}
	defer s.Close()
	// 握手全程限期：静默不答的对端到点失败，establish 随即 ClosePeer，
	// 未互验连接不会常驻
	s.SetDeadline(time.Now().Add(handshakeTimeout))
	// We hash the remote's ID; the remote verifies against its own ID and vice
	// versa, proving both sides hold the same psk.
	if err := writeFrame(s, handshakeMsg{PeerID: m.host.ID().String(), Token: hmacToken(m.psk, pid.String())}); err != nil {
		return err
	}
	var msg handshakeMsg
	if err := readFrame(s, &msg); err != nil {
		return err
	}
	if !hmacEqual(msg.Token, hmacToken(m.psk, m.host.ID().String())) {
		return fmt.Errorf("peer %s failed mutual auth", pid)
	}
	return nil
}

// sendProfile sends this node's profile to one peer (AES-GCM encrypted).
func (m *meshNode) sendProfile(to peer.ID) {
	data, err := json.Marshal(m.localProfile())
	if err != nil {
		return
	}
	enc, err := encryptRecord(m.psk, "advertise", data)
	if err != nil {
		return
	}
	env := mesh.Envelope{
		Type:    mesh.MsgAdvertise,
		To:      to.String(),
		Payload: enc,
	}
	if err := m.Send(to.String(), env); err != nil {
		meshLogf("send profile to %s: %v", to, err)
	}
}

// rebroadcastProfile pushes the profile to every authenticated peer.
func (m *meshNode) rebroadcastProfile() {
	m.mu.Lock()
	ids := make([]peer.ID, 0, len(m.peers))
	for id, st := range m.peers {
		if st.authed {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.sendProfile(id)
	}
}

// startProfilePusher periodically re-broadcasts the profile so stale caches
// expire (profiles carry a timestamp-less snapshot; refresh keeps them fresh).
func (m *meshNode) startProfilePusher(ctx context.Context) {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.rebroadcastProfile()
			}
		}
	}()
}

// localProfile builds this node's AgentProfile from the config and options.
func (m *meshNode) localProfile() mesh.AgentProfile {
	m.mu.Lock()
	caps := append([]mesh.Capability(nil), m.localCaps...)
	load := m.taskLoad
	m.mu.Unlock()
	tags := map[string]string{}
	if len(caps) > 0 {
		parts := make([]string, 0, len(caps))
		for _, c := range caps {
			parts = append(parts, string(c))
		}
		tags["caps"] = strings.Join(parts, ",")
	}
	// 让 peer 知道本节点接受 remote_tool 直接调用（mesh_peers/profile 展示）
	if m.cfg.AllowRemoteTools {
		tags["remoteTools"] = "true"
	}
	tools := m.opts.Tools
	if m.toolNames != nil {
		if live := m.toolNames(); len(live) > 0 {
			tools = live
		}
	}
	return mesh.AgentProfile{
		PeerID:    m.host.ID().String(),
		Name:      m.cfg.Name,
		Roles:     m.cfg.Roles,
		Version:   m.opts.Version,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Model:     m.opts.Model,
		Tools:     tools,
		Skills:    m.opts.Skills,
		Accepting: m.cfg.AllowRemoteTasks,
		Load:      load,
		MaxLoad:   m.cfg.MaxConcurrentTasks,
		Cwds:      cwdAliases(m.cfg.Cwds),
		Tags:      tags,
	}
}

// Self implements mesh.Mesh: this node's own profile (identity + capability).
func (m *meshNode) Self() mesh.AgentProfile {
	return m.localProfile()
}

// SetToolNames 注入实时工具名回调，profile 广播时取会话当前工具集
func (m *meshNode) SetToolNames(fn func() []string) {
	m.toolNames = fn
}

// peerName returns a peer's advertised name from the cached profile ("" when
// unknown). The collab handler uses it to label remote-task sessions with the
// dispatching node.
func (m *meshNode) peerName(peerID string) string {
	pid, err := peer.Decode(peerID)
	if err != nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.peers[pid]; st != nil && st.profile != nil {
		return st.profile.Name
	}
	return ""
}

// SetTaskLoad records the current number of in-flight remote tasks and
// re-broadcasts the profile so peers see a live Load (used for load-aware role
// routing). The collab handler calls it as tasks start and finish.
func (m *meshNode) SetTaskLoad(n int) {
	m.mu.Lock()
	if m.taskLoad == n {
		m.mu.Unlock()
		return
	}
	m.taskLoad = n
	m.mu.Unlock()
	m.rebroadcastProfile()
}

// relayHandshakeGrace is how long a not-yet-authenticated peer may hold a
// relay reservation: the handshake closes wrong-token peers within a couple of
// round trips, which takes their reservation down with the connection.
const relayHandshakeGrace = 2 * time.Minute

// relayAddrsFactory appends the seed relays' circuit addresses
// (<relayAddr>/p2p/<relayID>/p2p-circuit/p2p/<selfID>) to the advertised set,
// so peers always learn how to reach this node through the relay regardless
// of whether the relay's own addresses are public. It uses m.selfID because
// libp2p invokes the factory while the host is still being constructed
// (hole punch service start), when m.host is not set yet.
func (m *meshNode) relayAddrsFactory(addrs []ma.Multiaddr) []ma.Multiaddr {
	tail := ma.StringCast(fmt.Sprintf("/p2p-circuit/p2p/%s", m.selfID))
	out := addrs
	for _, ra := range m.seedRelayAddrs {
		out = append(out, ra.Encapsulate(tail))
	}
	return out
}

// startRelay runs the circuit v2 hop service (cfg.Relay nodes — typically the
// public/VPN-IP seed), so NAT'd peers can reserve slots and reach each other
// through this node.
func (m *meshNode) startRelay() error {
	svc, err := relay.New(m.host,
		// The library default caps each relayed circuit at 2 minutes / 128KB,
		// which would cut mesh port forwarding mid-stream. Admission is
		// handled by the ACL below plus the PSK handshake, so the relay is
		// never open to strangers and the limits can go.
		relay.WithInfiniteLimits(),
		// By default a reservation only advertises the relay's public
		// addresses; VPN-overlay seeds (Tailscale/overlay IPs) must advertise
		// their private addresses too or clients get no usable circuit addr.
		relay.WithReservationAddressFilter(func(ma.Multiaddr) bool { return true }),
		relay.WithACL(m),
	)
	if err != nil {
		return err
	}
	m.relaySvc = svc
	return nil
}

// handshakeTimeout 单侧握手（首帧等待 + 回复）的整体期限；静默不答的对端
// 到点失败并被 establish 断开。
const handshakeTimeout = 10 * time.Second

// streamHandshakeGrace 数据流门禁等待互验完成的上限：合法 peer 的握手在
// 连接后秒级完成，等待几乎零成本；静默不答的连接最迟 handshakeTimeout 就被
// establish 断开，等待上限略高于它兜住调度余量。
const streamHandshakeGrace = 15 * time.Second

// peerAuthed 即时判定 peer 是否已完成互验
func (m *meshNode) peerAuthed(pid peer.ID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.peers[pid]
	return st != nil && st.authed
}

// waitPeerAdmitted 等待 peer 完成互验，最长 maxWait（零值取
// streamHandshakeGrace）。数据流（agent / file / forward）一律经此门禁，
// 刻意**不做"刚连接即放行"的宽限**——那会让错 token 的攻击者反复重连刷新
// 窗口，直达 forward/expose（docker.sock）等高危面。等待语义下，未完成互验
// 的流一帧都不会被处理；握手失败或超时的连接由 establish 断开，本等待也随
// 连接失效提前返回。
func (m *meshNode) waitPeerAdmitted(pid peer.ID, maxWait time.Duration) bool {
	if maxWait <= 0 {
		maxWait = streamHandshakeGrace
	}
	deadline := time.Now().Add(maxWait)
	for {
		if m.peerAuthed(pid) {
			return true
		}
		if m.host.Network().Connectedness(pid) != network.Connected {
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-m.closed:
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// knownPeer implements the relay admission rule: connected peers pass (the
// reservation lives on their connection), authenticated peers always pass,
// and unauthenticated peers only get the handshake grace — a wrong-token peer
// is closed by the handshake and its reservation dies with the connection.
func (m *meshNode) knownPeer(p peer.ID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.peers[p]
	if st == nil {
		return false
	}
	if st.authed {
		return true
	}
	return time.Since(st.connectedAt) < relayHandshakeGrace
}

// AllowReserve implements relay.ACLFilter: who may hold a reservation.
func (m *meshNode) AllowReserve(p peer.ID, _ ma.Multiaddr) bool {
	return m.knownPeer(p)
}

// AllowConnect implements relay.ACLFilter: who may dial through this relay.
func (m *meshNode) AllowConnect(src peer.ID, _ ma.Multiaddr, _ peer.ID) bool {
	return m.knownPeer(src)
}

// staticRelays parses the configured seed addresses into autorelay candidates
// (the seed is expected to run relay:true).
func (m *meshNode) staticRelays() []peer.AddrInfo {
	var out []peer.AddrInfo
	for _, b := range m.cfg.BootstrapPeers {
		ai, err := parseBootstrapAddr(b)
		if err != nil {
			meshLogf("bootstrap multiaddr %q: %v", b, err)
			continue
		}
		out = append(out, ai...)
	}
	return out
}

// relayPeerSource feeds autorelay candidates from the rendezvous (seed-less
// public-DHT mode). The finder calls it whenever it needs more candidates and
// rate-limits the calls; each invocation performs one rendezvous lookup.
func (m *meshNode) relayPeerSource(ctx context.Context, num int) <-chan peer.AddrInfo {
	ch := make(chan peer.AddrInfo)
	go func() {
		defer close(ch)
		m.mu.Lock()
		rd := m.rd
		m.mu.Unlock()
		if rd == nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		peers, err := rd.FindPeers(cctx, m.token.Rendezvous)
		if err != nil {
			return
		}
		sent := 0
		for ai := range peers {
			if ai.ID == m.host.ID() {
				continue
			}
			select {
			case ch <- ai:
				sent++
			case <-ctx.Done():
				return
			}
			if sent >= num {
				return
			}
		}
	}()
	return ch
}

func capsFromProfile(p mesh.AgentProfile) []mesh.Capability {
	raw := p.Tags["caps"]
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]mesh.Capability, 0, len(parts))
	for _, s := range parts {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, mesh.Capability(s))
		}
	}
	return out
}

func cwdAliases(cwds map[string]string) []string {
	if len(cwds) == 0 {
		return nil
	}
	out := make([]string, 0, len(cwds))
	for alias := range cwds {
		out = append(out, alias)
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

// measureLatency pings a peer once (best effort).
func (m *meshNode) measureLatency(pid peer.ID) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-m.closed:
		return
	default:
	}
	if res, ok := <-ping.Ping(ctx, m.host, pid); ok && res.Error == nil {
		m.mu.Lock()
		if st := m.peers[pid]; st != nil {
			st.latency = res.RTT
		}
		m.mu.Unlock()
	}
}

// newID returns a random hex id for Envelope.ID.
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// meshLogf 记录 mesh 节点操作到进程日志（internal/logx）
func meshLogf(format string, args ...any) {
	logx.Infof("[mesh] "+format, args...)
}

// meshLogDbg meshLogf 的 debug 级别，供热循环（accept/poll 重试）避免刷 info
func meshLogDbg(format string, args ...any) {
	logx.Debugf("[mesh] "+format, args...)
}
