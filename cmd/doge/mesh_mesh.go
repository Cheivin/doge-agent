//go:build mesh

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"doge/internal/agent"
	"doge/internal/coding"
	"doge/internal/coding/uploads"
	"doge/internal/mesh"
	"doge/internal/mesh/libp2p"
)

// runMeshCommand handles `doge mesh ...` in the mesh build.
func runMeshCommand(args []string) int {
	if len(args) == 0 {
		meshUsage()
		return 2
	}
	switch args[0] {
	case "new":
		return meshNewCmd(args[1:])
	case "join":
		return meshJoinCmd(args[1:])
	case "daemon":
		return meshDaemonCmd(args[1:])
	case "peers":
		return meshPeersCmd(args[1:])
	case "profile":
		return meshProfileCmd(args[1:])
	case "forward":
		return meshForwardCmd(args[1:])
	case "help", "-h", "--help":
		meshUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "doge mesh: unknown subcommand %q\n", args[0])
		meshUsage()
		return 2
	}
}

// probeNode creates a transient mesh node (Probe: no forwards) for
// discovery-only commands.
func probeNode() (mesh.Mesh, *mesh.Config, error) {
	cfgPath := meshConfigPath()
	cfg, err := mesh.LoadConfig(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	if _, err := cfg.TokenObj(); err != nil {
		return nil, nil, fmt.Errorf("%w (run `doge mesh new` first)", err)
	}
	m, err := libp2p.New(mesh.Options{ConfigPath: cfgPath, Config: cfg, Version: versionString(), Probe: true})
	if err != nil {
		return nil, nil, err
	}
	return m, cfg, nil
}

// meshDaemonCmd runs a headless mesh node (no agent session) until the process
// receives a signal. This is how a machine joins a mesh without running the
// interactive agent — e.g. a bootstrap seed on a fixed-VPN-IP host.
func meshDaemonCmd(args []string) int {
	fs := flag.NewFlagSet("mesh daemon", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	port := fs.Int("port", 0, "fixed TCP listen port (0 = mesh.json listenPort or random)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfgPath := meshConfigPath()
	cfg, err := mesh.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	if _, err := cfg.TokenObj(); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v (run `doge mesh new` first)\n", err)
		return 1
	}
	if *port > 0 {
		cfg.ListenPort = *port
	}
	// 暂存区：headless 从侧同样可收文件（设计 §7.4），清理随进程
	settings, err := coding.LoadSettings(coding.SettingsFile(), "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	uploadStore := uploads.NewStore(coding.UploadsDir(), settings.UploadsConfig())
	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	defer sweepCancel()
	uploadStore.StartSweep(sweepCtx)
	node, err := libp2p.New(mesh.Options{ConfigPath: cfgPath, Config: cfg, Version: versionString(), Uploads: uploadStore})
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: mesh: %v\n", err)
		return 1
	}
	defer node.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Headless nodes accept direct tool calls too (mesh_forward/mesh_peers on
	// this node, when allowRemoteTools is on) — the agent session is not
	// involved, so a daemon can publish/adjust forwards on behalf of a peer.
	collab, err := libp2p.NewCollab(node, libp2p.CollabOptions{
		MeshTools: []*agent.AgentTool{libp2p.ForwardTool(node), libp2p.PeersTool(node)},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: mesh collab: %v\n", err)
	} else {
		defer collab.Close()
	}
	go func() {
		if err := node.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "doge: mesh: %v\n", err)
		}
	}()
	fmt.Printf("doge mesh node %s\n", node.ID())
	fmt.Printf("  Name:   %s\n", orDash(cfg.Name))
	fmt.Printf("  Addrs:\n")
	for _, a := range node.Addrs() {
		fmt.Printf("    %s/p2p/%s\n", a, node.ID())
	}
	fmt.Println("  Press Ctrl+C to stop. Share an addr above as a bootstrap seed with `doge mesh new --bootstrap <addr>`.")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	return 0
}

// meshPeersCmd discovers the network for a short window and prints connected
// peers with their profiles.
func meshPeersCmd(args []string) int {
	fs := flag.NewFlagSet("mesh peers", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	wait := fs.Int("wait", 4, "how long to discover before listing (seconds)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	m, _, err := probeNode()
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*wait)*time.Second+10*time.Second)
	defer cancel()
	go func() {
		if err := m.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "doge: mesh: %v\n", err)
		}
	}()
	time.Sleep(time.Duration(*wait) * time.Second)
	peers := m.Peers()
	if len(peers) == 0 {
		fmt.Println("No peers found. Run `doge --mesh` on another node with the same token.")
		return 0
	}
	for _, p := range peers {
		printPeer(p)
	}
	return 0
}

// meshProfileCmd prints this node's profile (no arg) or a discovered peer's
// full profile.
func meshProfileCmd(args []string) int {
	fs := flag.NewFlagSet("mesh profile", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	wait := fs.Int("wait", 4, "how long to discover before listing (seconds)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	m, cfg, err := probeNode()
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*wait)*time.Second+10*time.Second)
	defer cancel()
	go func() {
		if err := m.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "doge: mesh: %v\n", err)
		}
	}()
	if len(fs.Args()) == 0 {
		fmt.Printf("Local node %s\n", m.ID())
		fmt.Printf("  Name:      %s\n", orDash(cfg.Name))
		fmt.Printf("  Roles:     %s\n", joinOrNone(cfg.Roles))
		fmt.Printf("  Version:   %s\n", versionString())
		fmt.Printf("  OS/Arch:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
		fmt.Printf("  Cwds:      %s\n", joinOrNone(sortedKeys(cfg.Cwds)))
		fmt.Printf("  Accepting: %v\n", cfg.AllowRemoteTasks)
		return 0
	}
	target := fs.Args()[0]
	time.Sleep(time.Duration(*wait) * time.Second)
	for _, p := range m.Peers() {
		if p.ID == target {
			printPeer(p)
			return 0
		}
	}
	fmt.Fprintf(os.Stderr, "doge: peer %q not found\n", target)
	return 1
}

func meshForwardCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "doge mesh forward: missing subcommand (add|list|enable|disable|remove)")
		return 2
	}
	switch args[0] {
	case "add":
		return meshForwardAddCmd(args[1:])
	case "list":
		return meshForwardListCmd()
	case "enable":
		return meshForwardSetCmd(args[1:], true)
	case "disable":
		return meshForwardSetCmd(args[1:], false)
	case "remove":
		return meshForwardRemoveCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "doge mesh forward: unknown subcommand %q\n", args[0])
		return 2
	}
}

// meshForwardAddCmd writes a forward rule to mesh.json (declarative; a running
// `doge --mesh` picks it up via hot reload).
func meshForwardAddCmd(args []string) int {
	fs := flag.NewFlagSet("mesh forward add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	mode := fs.String("mode", "", "expose | listen")
	local := fs.String("local", "", "expose: local service (host:port or unix:///path)")
	bind := fs.String("bind", "127.0.0.1", "listen: bind interface (0.0.0.0, 127.0.0.1, IP, or unix:///path)")
	port := fs.Int("port", 0, "listen: TCP port")
	peer := fs.String("peer", "", "listen: target peer ID")
	remote := fs.String("remote", "", "listen: remote exposed service name")
	enabled := fs.Bool("enabled", true, "start the rule immediately")
	name, rest := splitFirstPositional(args)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "doge mesh forward add: rule name is required")
		return 2
	}
	rule := mesh.ForwardRule{
		Name:    name,
		Mode:    *mode,
		Local:   *local,
		Bind:    *bind,
		Port:    *port,
		Peer:    *peer,
		Remote:  *remote,
		Enabled: *enabled,
	}
	if err := mesh.ValidateRule(rule); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	path := meshConfigPath()
	cfg, err := mesh.LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	cfg.Forwards = append(cfg.Forwards, rule)
	if err := mesh.SaveConfig(path, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	fmt.Printf("Added forward rule %q to %s\n", name, path)
	fmt.Println("A running `doge --mesh` on this node applies it automatically (hot reload).")
	return 0
}

func meshForwardListCmd() int {
	cfg, err := mesh.LoadConfig(meshConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	printForwards(cfg.Forwards)
	return 0
}

func meshForwardSetCmd(args []string, enabled bool) int {
	name, _ := splitFirstPositional(args)
	if name == "" {
		fmt.Fprintln(os.Stderr, "doge mesh forward: rule name is required")
		return 2
	}
	path := meshConfigPath()
	cfg, err := mesh.LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	found := false
	for i := range cfg.Forwards {
		if cfg.Forwards[i].Name == name {
			cfg.Forwards[i].Enabled = enabled
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "doge: forward rule %q not found\n", name)
		return 1
	}
	if err := mesh.SaveConfig(path, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	verb := "Enabled"
	if !enabled {
		verb = "Disabled"
	}
	fmt.Printf("%s forward rule %q (a running `doge --mesh` applies it automatically)\n", verb, name)
	return 0
}

func meshForwardRemoveCmd(args []string) int {
	name, _ := splitFirstPositional(args)
	if name == "" {
		fmt.Fprintln(os.Stderr, "doge mesh forward: rule name is required")
		return 2
	}
	path := meshConfigPath()
	cfg, err := mesh.LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	found := false
	out := cfg.Forwards[:0]
	for _, r := range cfg.Forwards {
		if r.Name == name {
			found = true
			continue
		}
		out = append(out, r)
	}
	if !found {
		fmt.Fprintf(os.Stderr, "doge: forward rule %q not found\n", name)
		return 1
	}
	cfg.Forwards = out
	if err := mesh.SaveConfig(path, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	fmt.Printf("Removed forward rule %q (a running `doge --mesh` applies it automatically)\n", name)
	return 0
}

func printPeer(p mesh.PeerInfo) {
	fmt.Printf("Peer %s\n", p.ID)
	fmt.Printf("  Name:      %s\n", orDash(p.Profile.Name))
	fmt.Printf("  Version:   %s\n", orDash(p.Profile.Version))
	fmt.Printf("  OS/Arch:   %s/%s\n", orDash(p.Profile.OS), orDash(p.Profile.Arch))
	fmt.Printf("  Roles:     %s\n", joinOrNone(p.Profile.Roles))
	fmt.Printf("  Skills:    %s\n", joinOrNone(p.Profile.Skills))
	fmt.Printf("  Model:     %s\n", orDash(p.Profile.Model))
	fmt.Printf("  Cwds:      %s\n", joinOrNone(p.Profile.Cwds))
	fmt.Printf("  Accepting: %v\n", p.Profile.Accepting)
	fmt.Printf("  Load:      %d/%d\n", p.Profile.Load, p.Profile.MaxLoad)
	fmt.Printf("  Latency:   %s\n", p.Latency.Round(time.Microsecond))
	fmt.Printf("  Addrs:     %s\n", strings.Join(p.Addrs, ", "))
}

func printForwards(rules []mesh.ForwardRule) {
	if len(rules) == 0 {
		fmt.Println("No forward rules.")
		return
	}
	sorted := append([]mesh.ForwardRule(nil), rules...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, r := range sorted {
		state := "enabled"
		if !r.Enabled {
			state = "disabled"
		}
		switch r.Mode {
		case "expose":
			fmt.Printf("  %-16s expose  local=%s  [%s]\n", r.Name, r.Local, state)
		case "listen":
			fmt.Printf("  %-16s listen  bind=%s:%d -> %s/%s  [%s]\n", r.Name, r.Bind, r.Port, r.Peer, r.Remote, state)
		default:
			fmt.Printf("  %-16s %s  [%s]\n", r.Name, r.Mode, state)
		}
	}
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
