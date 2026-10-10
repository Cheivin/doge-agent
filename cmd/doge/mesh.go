package main

// Mesh CLI subcommands, no build tag: `mesh new` / `mesh join` only touch the
// no-tag internal/mesh package (config, token) and work in both builds. The
// subcommands that need a live node live in mesh_mesh.go (+build mesh);
// mesh_stub.go (+build !mesh) provides the disabled counterparts. The shared
// node wiring used by the CLI/desktop entry points is in internal/app.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"doge/internal/coding"
	"doge/internal/mesh"
)

// meshConfigPath returns the mesh.json path inside the doge agent dir.
func meshConfigPath() string {
	return filepath.Join(coding.AgentDir(), "mesh.json")
}

// multiFlag collects repeated string flags (e.g. --bootstrap a --bootstrap b).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// splitFirstPositional pulls the first non-flag argument (a rule name, a token,
// ...) out of args so the standard flag package can parse the remaining flags
// even when they come after the positional (e.g. `mesh join <token> --name x`).
func splitFirstPositional(args []string) (name string, rest []string) {
	for i, a := range args {
		if a == "--" {
			break
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			name = a
			rest = append(append([]string{}, args[:i]...), args[i+1:]...)
			return
		}
	}
	return "", args
}

// meshNewCmd implements `doge mesh new`: generates a token, embeds any
// --bootstrap seed multiaddrs, and writes mesh.json. It works in both builds
// (pure crypto/config, no libp2p).
func meshNewCmd(args []string) int {
	fs := flag.NewFlagSet("mesh new", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var bootstrap multiFlag
	fs.Var(&bootstrap, "bootstrap", "seed node multiaddr to embed in the token (repeatable)")
	name := fs.String("name", "", "human-readable node name (default: hostname)")
	port := fs.Int("port", 0, "fixed TCP listen port for this node (0 = random per start)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tok, err := mesh.NewToken(bootstrap...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	path := meshConfigPath()
	cfg, err := mesh.LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	cfg.SetToken(tok)
	if *name != "" {
		cfg.Name = *name
	} else if cfg.Name == "" {
		if host, err := os.Hostname(); err == nil {
			cfg.Name = host
		}
	}
	if len(tok.BootstrapPeers) > 0 {
		cfg.BootstrapPeers = tok.BootstrapPeers
	}
	if *port > 0 {
		cfg.ListenPort = *port
	}
	if err := mesh.SaveConfig(path, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	fmt.Println("New mesh network created.")
	fmt.Printf("  Token: %s\n", tok.Encode())
	fmt.Printf("  Config: %s\n", path)
	fmt.Println("Share the token with other nodes, then join with:")
	fmt.Printf("  doge mesh join %s\n", tok.Encode())
	return 0
}

// meshJoinCmd implements `doge mesh join <token> [--bootstrap <addr>...]`:
// writes a config that uses an existing token (shared from another node),
// optionally embedding seed addresses into the token so a seed-restart keeps
// the network reachable by the same bootstrap addresses.
func meshJoinCmd(args []string) int {
	tokenArg, rest := splitFirstPositional(args)
	fs := flag.NewFlagSet("mesh join", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var bootstrap multiFlag
	fs.Var(&bootstrap, "bootstrap", "seed node multiaddr to embed in the token (repeatable)")
	name := fs.String("name", "", "human-readable node name (default: hostname)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if tokenArg == "" {
		fmt.Fprintln(os.Stderr, "doge mesh join: a token argument is required")
		return 2
	}
	tok, err := mesh.DecodeToken(tokenArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	for _, b := range bootstrap {
		if b = strings.TrimSpace(b); b == "" {
			continue
		}
		if !strings.HasPrefix(b, "/") {
			fmt.Fprintf(os.Stderr, "doge: invalid bootstrap multiaddr %q\n", b)
			return 1
		}
		tok.BootstrapPeers = append(tok.BootstrapPeers, b)
	}
	path := meshConfigPath()
	cfg, err := mesh.LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	cfg.SetToken(tok)
	if *name != "" {
		cfg.Name = *name
	} else if cfg.Name == "" {
		if host, err := os.Hostname(); err == nil {
			cfg.Name = host
		}
	}
	if len(tok.BootstrapPeers) > 0 {
		cfg.BootstrapPeers = tok.BootstrapPeers
	}
	if err := mesh.SaveConfig(path, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "doge: %v\n", err)
		return 1
	}
	fmt.Printf("Joined mesh network. Config written to %s\n", path)
	if len(tok.BootstrapPeers) > 0 {
		fmt.Printf("  Bootstrap seeds: %s\n", strings.Join(tok.BootstrapPeers, ", "))
	}
	return 0
}

func meshUsage() {
	fmt.Fprint(os.Stderr, `doge mesh — peer-to-peer mesh

Usage:
  doge mesh new [--name NAME] [--bootstrap <multiaddr>...] [--port N]   create a new network
  doge mesh join <token> [--bootstrap <multiaddr>...] [--name NAME]     join with a shared token
  doge mesh daemon [--port N]                                          run a headless mesh node
  doge mesh peers [--wait SECONDS]                                     list connected nodes
  doge mesh profile [peer]                                             show a node profile
  doge mesh forward add <name> --mode expose --local ADDR [--enabled]
  doge mesh forward add <name> --mode listen --bind HOST --port PORT --peer ID --remote SVC
  doge mesh forward enable <name>
  doge mesh forward disable <name>
  doge mesh forward remove <name>
  doge mesh forward list

A node's identity (PeerID) and listen port are persisted (mesh-key + listenPort), so
a seed keeps a stable address across restarts and token-embedded bootstrap addresses
stay valid. --bootstrap/--port seed addresses belong to the node, not the shared token.

The `+"`doge mesh forward`"+` commands write mesh.json; a running `+"`doge --mesh`"+`
node picks the change up automatically (hot reload). Forward rules are
declarative: mode=expose publishes a local service, mode=listen bridges a
local port to a remote exposed service. local/bind accept unix:///path.
`)
}
