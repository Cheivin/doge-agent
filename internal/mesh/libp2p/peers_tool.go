//go:build mesh

package libp2p

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"doge/internal/agent"
	"doge/internal/coding/tools"
	"doge/internal/mesh"
)

// PeersTool builds the mesh_peers agent tool: it lists the doge nodes
// currently connected on the mesh (same token) with their AgentProfile
// capabilities, so the agent can DISCOVER other agents and pick a target for
// remote_agent (by peer id or role) instead of guessing. This is the LLM-facing
// half of §4.3 "agent 发现与能力感知" — the profile broadcast is already
// maintained by the mesh; this tool surfaces it to the model.
func PeersTool(m mesh.Mesh) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "mesh_peers",
		Label:       "List mesh nodes and their capabilities",
		Description: "Discover the doge nodes on the mesh (same token). With self=true, show THIS node's own identity and profile (peer id, name, roles, model, accepting, load, cwds) — use it to answer questions like \"which node am I\". Without self, list the connected remote nodes with their capabilities: peer id, name, roles, model, allowed working directories (cwds), skills, whether they accept remote tasks (accepting), current load, latency. Pass peer=<id> to see one node's full profile. Use this FIRST to discover which nodes exist and which roles or peer ids to target with remote_agent. A node's name (name=) can be used as the peer argument of remote_agent / remote_tool / mesh_forward instead of the full peer id — but only while it is unique among connected nodes; duplicate names are rejected and you must use the exact peer id.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"self": map[string]any{"type": "boolean", "description": "true = show this node's own identity/profile instead of the peer list"},
				"peer": map[string]any{"type": "string", "description": "optional peer id to inspect in full"},
			},
		},
		Execute: func(_ string, params map[string]any, ctx context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			if boolParam(params["self"]) {
				return tools.ToolResult(formatPeerProfile(mesh.PeerInfo{ID: m.ID(), Profile: m.Self()})), nil
			}
			if pid := strParam(params["peer"]); pid != "" {
				for _, p := range m.Peers() {
					if p.ID == pid {
						return tools.ToolResult(formatPeerProfile(p)), nil
					}
				}
				return tools.ToolResult("mesh_peers: no such peer " + pid + " connected (use mesh_peers without peer= to list all)"), nil
			}
			peers := m.Peers()
			if len(peers) == 0 {
				return tools.ToolResult("No mesh peers connected. Run another doge --mesh node with the same token (see `doge mesh peers`)."), nil
			}
			sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
			var b strings.Builder
			fmt.Fprintf(&b, "Mesh peers (%d):\n", len(peers))
			for _, p := range peers {
				acc := "no"
				if p.Profile.Accepting {
					acc = "yes"
				}
				fmt.Fprintf(&b, "  %s\n", p.ID)
				fmt.Fprintf(&b, "    name=%s roles=[%s] accepting=%s load=%d/%d model=%s\n",
					orEmpty(p.Profile.Name), strings.Join(p.Profile.Roles, ","), acc,
					p.Profile.Load, p.Profile.MaxLoad, orEmpty(p.Profile.Model))
				if len(p.Profile.Cwds) > 0 {
					fmt.Fprintf(&b, "    cwds=[%s]\n", strings.Join(p.Profile.Cwds, ","))
				}
				if len(p.Profile.Skills) > 0 {
					fmt.Fprintf(&b, "    skills=[%s]\n", strings.Join(p.Profile.Skills, ","))
				}
				fmt.Fprintf(&b, "    latency=%s\n", p.Latency.Round(time.Microsecond))
			}
			return tools.ToolResult(b.String()), nil
		},
	}
}

// resolvePeerRef resolves a peer reference to a peer ID: an exact peer ID
// passes through as-is; otherwise the reference is matched against connected
// peers' profile names. An ambiguous name is rejected immediately (listing
// the matching peer ids) so the caller can re-target by peer ID.
func resolvePeerRef(m mesh.Mesh, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("peer reference is empty")
	}
	peers := m.Peers()
	// 可解析成 peer ID：在已连列表里直接用；不在列表里也原样返回，
	// 让 Send 给出明确的 "not connected" 错误（保持精确 ID 语义）
	if pid, err := peer.Decode(ref); err == nil {
		id := pid.String()
		for _, p := range peers {
			if p.ID == id {
				return id, nil
			}
		}
		return ref, nil
	}
	// 按节点名（profile.Name）匹配
	var matches []mesh.PeerInfo
	for _, p := range peers {
		if p.Profile.Name == ref {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", fmt.Errorf("no connected peer named %q (use mesh_peers to list nodes and their names)", ref)
	default:
		ids := make([]string, 0, len(matches))
		for _, p := range matches {
			ids = append(ids, p.ID)
		}
		return "", fmt.Errorf("peer name %q is ambiguous (%s); target one by its exact peer id", ref, strings.Join(ids, ", "))
	}
}

// formatPeerProfile renders one peer's full AgentProfile (mirrors the
// `doge mesh profile <peer>` output).
func formatPeerProfile(p mesh.PeerInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Peer %s\n", p.ID)
	fmt.Fprintf(&b, "  Name:      %s\n", orEmpty(p.Profile.Name))
	fmt.Fprintf(&b, "  Version:   %s\n", orEmpty(p.Profile.Version))
	fmt.Fprintf(&b, "  OS/Arch:   %s/%s\n", orEmpty(p.Profile.OS), orEmpty(p.Profile.Arch))
	fmt.Fprintf(&b, "  Roles:     %s\n", joinOrDash(p.Profile.Roles))
	fmt.Fprintf(&b, "  Skills:    %s\n", joinOrDash(p.Profile.Skills))
	fmt.Fprintf(&b, "  Model:     %s\n", orEmpty(p.Profile.Model))
	fmt.Fprintf(&b, "  Cwds:      %s\n", joinOrDash(p.Profile.Cwds))
	fmt.Fprintf(&b, "  Accepting: %v\n", p.Profile.Accepting)
	remoteTools := "no"
	if p.Profile.Tags["remoteTools"] == "true" {
		remoteTools = "yes"
	}
	fmt.Fprintf(&b, "  DirectTools: %s (remote_tool on this peer; disable via allowRemoteTools:false)\n", remoteTools)
	fmt.Fprintf(&b, "  Load:      %d/%d\n", p.Profile.Load, p.Profile.MaxLoad)
	fmt.Fprintf(&b, "  Latency:   %s\n", p.Latency.Round(time.Microsecond))
	fmt.Fprintf(&b, "  Addrs:     %s\n", strings.Join(p.Addrs, ", "))
	return b.String()
}

func orEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func joinOrDash(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ", ")
}
