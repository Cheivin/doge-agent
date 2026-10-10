//go:build mesh

package libp2p

import (
	"context"
	"fmt"
	"os"
	"strings"

	"doge/internal/agent"
	"doge/internal/coding/tools"
	"doge/internal/coding/uploads"
	"doge/internal/mesh"
)

// MeshFileTool builds the mesh_file agent tool: send staged/local files to a
// peer's upload store, pull remote files back, and list what a peer shares.
// The store is this node's upload staging area (nil = tool still registers but
// reports why it cannot run).
func MeshFileTool(m mesh.Mesh) *agent.AgentTool {
	return &agent.AgentTool{
		Name:  "mesh_file",
		Label: "Transfer files over the mesh",
		Description: "Transfer files between this node and another doge node's staging area. " +
			"send puts a local file (or a staged upload id) into the peer's staging area and returns the remote upload id — pair with remote_agent(files=[...]) to hand files to a remote task. " +
			"pull fetches a file from the peer's staging area into this node's (the remote side must have staged it via mesh_file(action=\"stage\") or received it from this node). " +
			"list shows what the peer will share. Files land in staging areas only — the receiving agent reads them with the read tool and decides where they belong.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"send", "pull", "list", "stage"}, "description": "send = push a file to the peer; pull = fetch a remote upload id; list = list the peer's shareable files; stage = put a local file into THIS node's staging area (returns an upload id the peer can pull)"},
				"peer":   map[string]any{"type": "string", "description": "target peer id or node name (send/pull/list)"},
				// upload 可以是本地路径（send 时先 stage）或 uploadID
				"upload": map[string]any{"type": "string", "description": "send: a local file path or a staged upload id; pull: the remote upload id"},
			},
			"required": []string{"action"},
		},
		Execute: func(_ string, params map[string]any, ctx context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			action := strParam(params["action"])
			peerRef := strParam(params["peer"])
			upload := strings.TrimSpace(strParam(params["upload"]))
			node, ok := m.(*meshNode)
			if !ok {
				return tools.ToolError("mesh_file: mesh file transfer requires the libp2p node"), nil
			}
			store := node.uploadStore()
			if store == nil {
				return tools.ToolError("mesh_file: this node has no upload store (file transfer disabled)"), nil
			}

			switch action {
			case "stage":
				if upload == "" {
					return tools.ToolError("mesh_file: upload (local file path) is required for stage"), nil
				}
				meta, err := stageLocalFile(store, upload)
				if err != nil {
					return tools.ToolError("mesh_file: " + err.Error()), nil
				}
				return tools.ToolResult(fmt.Sprintf("Staged %s as upload %s (peer can pull it via mesh_file(action=\"pull\", upload=\"%s\"))", meta.Name, meta.ID, meta.ID)), nil

			case "send", "pull", "list":
				if peerRef == "" {
					return tools.ToolError("mesh_file: peer is required for " + action), nil
				}
				peerID, err := resolvePeerRef(m, peerRef)
				if err != nil {
					return tools.ToolError("mesh_file: " + err.Error()), nil
				}
				switch action {
				case "list":
					files, err := node.ListRemoteFiles(ctx, peerID)
					if err != nil {
						return tools.ToolError("mesh_file: " + err.Error()), nil
					}
					if len(files) == 0 {
						return tools.ToolResult("No shareable files on " + peerID), nil
					}
					var b strings.Builder
					fmt.Fprintf(&b, "Shareable files on %s:", peerID)
					for _, f := range files {
						fmt.Fprintf(&b, "\n- %s  size=%d sha256=%s upload=%s", f.Name, f.Size, shortSHA(f.SHA256), f.UploadID)
					}
					return tools.ToolResult(b.String()), nil
				case "send":
					if upload == "" {
						return tools.ToolError("mesh_file: upload (local path or staged upload id) is required for send"), nil
					}
					meta, err := stageLocalFile(store, upload)
					if err != nil {
						return tools.ToolError("mesh_file: " + err.Error()), nil
					}
					ref, ack, err := node.SendFile(ctx, peerID, meta)
					if err != nil {
						return tools.ToolError("mesh_file: send to " + peerID + " failed: " + err.Error()), nil
					}
					if !ack.OK {
						return tools.ToolError(fmt.Sprintf("mesh_file: send rejected by %s (%s): %s", peerID, ack.Code, ack.Message)), nil
					}
					return tools.ToolResult(fmt.Sprintf("Sent %s to %s staging area (remote upload=%s). Use remote_agent(files=[\"%s\"], peer=...) to hand it to a remote task.", meta.Name, peerID, ref.UploadID, ref.UploadID)), nil
				case "pull":
					if upload == "" {
						return tools.ToolError("mesh_file: upload (remote upload id) is required for pull"), nil
					}
					ref, err := node.PullFile(ctx, peerID, upload)
					if err != nil {
						return tools.ToolError("mesh_file: pull from " + peerID + " failed: " + err.Error()), nil
					}
					meta, err := store.Meta(ref.UploadID)
					if err != nil {
						return tools.ToolError("mesh_file: " + err.Error()), nil
					}
					return tools.ToolResult(fmt.Sprintf("Pulled %s from %s into local staging: %s (use the read tool on this path)", ref.Name, peerID, meta.Path)), nil
				}
			}
			return tools.ToolError("mesh_file: unknown action " + action), nil
		},
	}
}

// stageLocalFile 把本地路径文件放进暂存区；uploadID 直接复用既有暂存项。
func stageLocalFile(store *uploads.Store, upload string) (*uploads.Meta, error) {
	if meta, err := store.Meta(upload); err == nil {
		return meta, nil // 已是 uploadID
	}
	if upload == "" || upload[0] != '/' {
		return nil, fmt.Errorf("upload must be an absolute local path or an upload id")
	}
	f, err := os.Open(upload)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	meta, err := store.Save(upload, "agent", "", f)
	if err != nil {
		return nil, err
	}
	return meta, nil
}

// shortSHA 哈希截短展示
func shortSHA(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
