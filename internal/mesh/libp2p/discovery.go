//go:build mesh

package libp2p

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	ma "github.com/multiformats/go-multiaddr"
)

// startDiscovery wires up mDNS (LAN) + DHT rendezvous (WAN) + configured
// bootstrap seeds. The meshNode implements mdns.Notifee for HandlePeerFound.
func (m *meshNode) startDiscovery(ctx context.Context) error {
	// Connect to explicitly configured bootstrap seeds (the token-embedded
	// self-hosted seeds, or VPN seed addresses). Retries until ctx ends so a
	// temporarily-down seed recovers.
	if len(m.cfg.BootstrapPeers) > 0 {
		seeds := m.parseBootstrapPeers()
		for _, ai := range seeds {
			go m.bootstrapConnectLoop(ctx, ai)
		}
	}

	// mDNS on the LAN. The service tag derives from the rendezvous so only
	// nodes sharing the token discover each other.
	svc := mdns.NewMdnsService(m.host, mdnsTag(m.token.Rendezvous), m)
	if err := svc.Start(); err != nil {
		return err
	}
	m.mdnsSvc = svc

	// DHT rendezvous. With configured seeds we form a private DHT around them;
	// with no seeds in public mode the DHT falls back to the default public
	// bootstrap peers; in private-network mode we rely on mDNS + seeds only.
	dhtOpts := []dht.Option{dht.Mode(dht.ModeServer)}
	if len(m.cfg.BootstrapPeers) > 0 {
		dhtOpts = append(dhtOpts, dht.BootstrapPeers(m.parseBootstrapPeers()...))
	}
	kdht, err := dht.New(ctx, m.host, dhtOpts...)
	if err != nil {
		return err
	}
	m.kdht = kdht
	go func() {
		_ = kdht.Bootstrap(ctx)
	}()
	rd := routing.NewRoutingDiscovery(kdht)
	m.mu.Lock()
	m.rd = rd
	m.mu.Unlock()
	go m.advertiseLoop(ctx, rd)
	go m.findLoop(ctx, rd)
	return nil
}

// HandlePeerFound implements mdns.Notifee: connect to a LAN-discovered peer
// (the connection notifier then runs mutual auth + profile exchange). The
// zeroconf browse keeps re-firing for the same peer, so retry loops are
// single-flighted per peer — without the guard every repeated announcement
// would pile up another backoff goroutine dialing the same (possibly offline)
// peer forever.
func (m *meshNode) HandlePeerFound(ai peer.AddrInfo) {
	if ai.ID == m.host.ID() {
		return
	}
	m.mu.Lock()
	if m.foundActive == nil {
		m.foundActive = map[peer.ID]bool{}
	}
	if m.foundActive[ai.ID] {
		m.mu.Unlock()
		return
	}
	m.foundActive[ai.ID] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.foundActive, ai.ID)
		m.mu.Unlock()
	}()

	backoff := 1 * time.Second
	for {
		if m.host.Network().Connectedness(ai.ID) == network.Connected {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := m.host.Connect(ctx, ai)
		cancel()
		if err == nil {
			return
		}
		meshLogf("connect to discovered %s: %v (retrying)", ai.ID, err)
		select {
		case <-m.closed:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// parseBootstrapPeers converts configured bootstrap multiaddrs into AddrInfo.
func (m *meshNode) parseBootstrapPeers() []peer.AddrInfo {
	return m.staticRelays()
}

// parseBootstrapAddr parses a bootstrap multiaddr like
// /ip4/x.x.x.x/tcp/4001/p2p/<peerid> into AddrInfo.
func parseBootstrapAddr(s string) ([]peer.AddrInfo, error) {
	maddr, err := ma.NewMultiaddr(s)
	if err != nil {
		return nil, err
	}
	return peer.AddrInfosFromP2pAddrs(maddr)
}

// bootstrapConnectLoop keeps a seed connection alive for the lifetime of the
// node: it re-dials whenever the seed is not connected (the seed restarting on
// a new port, or a network drop), instead of giving up after the first
// success — that was the old behavior, which stranded nodes whenever their
// seed connection came back from an outage with a changed address.
func (m *meshNode) bootstrapConnectLoop(ctx context.Context, ai peer.AddrInfo) {
	for {
		if m.host.Network().Connectedness(ai.ID) != network.Connected {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := m.host.Connect(cctx, ai)
			cancel()
			if err == nil {
				meshLogf("connected to bootstrap seed %s", ai.ID)
			} else if ctx.Err() == nil {
				meshLogf("connect bootstrap seed %s: %v", ai.ID, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// advertiseLoop re-advertises the rendezvous so the DHT provider record stays
// fresh.
func (m *meshNode) advertiseLoop(ctx context.Context, rd *routing.RoutingDiscovery) {
	ttl := 30 * time.Minute
	for {
		t, err := rd.Advertise(ctx, m.token.Rendezvous)
		if err == nil && t > 0 {
			ttl = t
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ttl / 2):
		}
	}
}

// findLoop periodically searches the rendezvous and connects to found peers.
func (m *meshNode) findLoop(ctx context.Context, rd *routing.RoutingDiscovery) {
	search := func() {
		peers, err := rd.FindPeers(ctx, m.token.Rendezvous)
		if err != nil {
			meshLogf("find peers: %v", err)
			return
		}
		for ai := range peers {
			if ai.ID == m.host.ID() {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if err := m.host.Connect(cctx, ai); err != nil {
				meshLogf("connect to rendezvous peer %s: %v", ai.ID, err)
			}
			cancel()
		}
	}
	search()
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			search()
		}
	}
}

// mdnsTag derives a short mDNS service tag from the rendezvous string so the
// LAN discovery namespace is per-network.
func mdnsTag(rendezvous string) string {
	sum := sha256.Sum256([]byte(rendezvous))
	return "_doge" + hex.EncodeToString(sum[:3]) + "._udp"
}
