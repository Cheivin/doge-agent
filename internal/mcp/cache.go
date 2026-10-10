package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolMeta 缓存到磁盘的工具元数据（与 SDK 类型解耦，可跨进程读）
type toolMeta struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// cache 工具元数据磁盘缓存：search/describe/status 无活连接也能用。
// 目录 <agentDir>/mcp-cache/<server>.json，懒创建（无 mcp 配置时不建目录）。
type cache struct {
	dir string
}

// cacheDir 返回缓存目录
func (c *cache) cacheDir() string {
	return filepath.Join(c.dir, "mcp-cache")
}

// path 某 server 的缓存文件
func (c *cache) path(server string) string {
	return filepath.Join(c.cacheDir(), server+".json")
}

// load 读某 server 的缓存工具；无缓存返回 nil
func (c *cache) load(server string) []*mcp.Tool {
	data, err := os.ReadFile(c.path(server))
	if err != nil {
		return nil
	}
	var metas []toolMeta
	if err := json.Unmarshal(data, &metas); err != nil {
		return nil
	}
	out := make([]*mcp.Tool, 0, len(metas))
	for _, meta := range metas {
		out = append(out, &mcp.Tool{
			Name:        meta.Name,
			Description: meta.Description,
			InputSchema: meta.InputSchema,
		})
	}
	return out
}

// save 写入某 server 的缓存
func (c *cache) save(server string, tools []*mcp.Tool) {
	if c.dir == "" {
		return
	}
	metas := make([]toolMeta, 0, len(tools))
	for _, t := range tools {
		schema, _ := t.InputSchema.(map[string]any)
		metas = append(metas, toolMeta{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	data, err := json.MarshalIndent(metas, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(c.cacheDir(), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(c.path(server), data, 0o600); err != nil {
		return
	}
}
