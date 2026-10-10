package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"doge/internal/agent"
	"doge/internal/ai"
)

// ReadTool reads a text or image file. offset/limit are 1-indexed lines;
// output is truncated to MaxOutputLines/MaxOutputBytes with a hint to
// continue reading.
func ReadTool(ops *Operations) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "read",
		Label:       "Read",
		Description: "Read a text or image file. Use offset/limit to page through large files.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "Path to the file to read"},
				"offset": map[string]any{"type": "integer", "description": "1-indexed start line (default 1)"},
				"limit":  map[string]any{"type": "integer", "description": "Maximum lines to read"},
			},
			"required":             []any{"path"},
			"additionalProperties": false,
		},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			path, _ := params["path"].(string)
			if path == "" {
				return ToolError("read: path is required"), nil
			}
			offset := intParam(params, "offset", 1)
			limit := intParam(params, "limit", 0)
			if offset < 1 {
				offset = 1
			}

			data, err := ops.ReadFile(path)
			if err != nil {
				return ToolError(fmt.Sprintf("read: %v", err)), nil
			}

			// Image files are returned as image content blocks.
			if mime, ok := imageMime(path); ok {
				return &agent.AgentToolResult{
					Content: []ai.ContentBlock{ai.ImageBlock(data, mime)},
					Details: map[string]any{"mimeType": mime, "bytes": len(data)},
				}, nil
			}

			text := string(data)
			lines := strings.Split(text, "\n")

			// Select the requested window; `first` is the real file line
			// number of the first selected line.
			first := 1
			if limit > 0 && offset+limit-1 < len(lines) {
				lines = lines[offset-1 : offset+limit-1]
				first = offset
			} else if offset > 1 {
				lines = lines[offset-1:]
				first = offset
			}

			truncated := false
			if len(lines) > MaxOutputLines {
				lines = lines[:MaxOutputLines]
				truncated = true
			}

			if text == "" {
				return ToolResult(""), nil
			}

			// Number lines with their real file line numbers so the model
			// can cite `file:line` (the system prompt asks it to).
			numbered := make([]string, len(lines))
			for i, ln := range lines {
				numbered[i] = fmt.Sprintf("%d\t%s", first+i, ln)
			}
			out := strings.Join(numbered, "\n")
			if len(out) > MaxOutputBytes {
				// Trim on a line boundary so line numbers stay consistent.
				keep := 0
				size := 0
				for i, ln := range numbered {
					if size+len(ln)+1 > MaxOutputBytes {
						break
					}
					size += len(ln) + 1
					keep = i + 1
				}
				if keep == 0 {
					keep = 1
				}
				numbered = numbered[:keep]
				out = strings.Join(numbered, "\n")
				truncated = true
			}

			total := len(strings.Split(text, "\n"))
			endLine := first + len(numbered) - 1
			if truncated || endLine < total {
				out += fmt.Sprintf("\n[Showing lines %d-%d of %d. Use offset=%d to continue.]",
					first, endLine, total, endLine+1)
			}
			return ToolResult(out), nil
		},
	}
}

// WriteTool writes a file, creating parent directories.
func WriteTool(ops *Operations) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "write",
		Label:       "Write",
		Description: "Write a file, creating parent directories as needed. Overwrites existing content.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Path to the file to write"},
				"content": map[string]any{"type": "string", "description": "Full file content"},
			},
			"required":             []any{"path", "content"},
			"additionalProperties": false,
		},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			path, _ := params["path"].(string)
			content, _ := params["content"].(string)
			if path == "" {
				return ToolError("write: path is required"), nil
			}
			var writeErr error
			_ = WithPathLock(path, func() error {
				if dir := filepath.Dir(path); dir != "" && dir != "." {
					if err := ops.MkdirAll(dir, 0); err != nil {
						writeErr = err
						return nil
					}
				}
				writeErr = ops.WriteFile(path, []byte(content), 0)
				return nil
			})
			if writeErr != nil {
				return ToolError(fmt.Sprintf("write: %v", writeErr)), nil
			}
			return ToolResult(fmt.Sprintf("Successfully wrote %d bytes to %s", len(content), path)), nil
		},
	}
}

// LSTool lists directory entries: case-insensitive sort, directories get a
// trailing slash, dotfiles included.
func LSTool(ops *Operations) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "ls",
		Label:       "List",
		Description: "List files and directories in a path (default: current directory).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":  map[string]any{"type": "string", "description": "Directory to list"},
				"limit": map[string]any{"type": "integer", "description": "Maximum entries (default 500)"},
			},
			"additionalProperties": false,
		},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			path, _ := params["path"].(string)
			if path == "" {
				path = "."
			}
			limit := intParam(params, "limit", 500)

			entries, err := ops.ReadDir(path)
			if err != nil {
				return ToolError(fmt.Sprintf("ls: %v", err)), nil
			}
			names := make([]string, 0, len(entries))
			dirs := map[string]bool{}
			for _, e := range entries {
				names = append(names, e.Name())
				dirs[e.Name()] = e.IsDir()
			}
			sortStringsCaseInsensitive(names)

			var out strings.Builder
			shown := 0
			for _, name := range names {
				if dirs[name] {
					out.WriteString(name + "/\n")
				} else {
					out.WriteString(name + "\n")
				}
				shown++
				if shown >= limit {
					out.WriteString(fmt.Sprintf("... %d more entries\n", len(names)-shown))
					break
				}
			}
			return ToolResult(strings.TrimSuffix(out.String(), "\n")), nil
		},
	}
}

// imageMime detects image mime types from the file extension.
func imageMime(path string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	case ".bmp":
		return "image/bmp", true
	}
	return "", false
}

func intParam(params map[string]any, key string, def int) int {
	v, ok := params[key].(float64)
	if !ok {
		return def
	}
	return int(v)
}

func sortStringsCaseInsensitive(s []string) {
	// Insertion sort is fine for directory listings.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && strings.ToLower(s[j]) < strings.ToLower(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
