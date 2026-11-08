package console

import (
	"encoding/json"

	"doge/internal/mcp"
)

// ParseMcpServersPayload 解析社区 mcpServers 粘贴格式（导入与 apply 复用）：
//
//	{"playwright": {"command":"npx","args":[...]}}               （内层 map）
//	{"mcpServers": {"playwright": {"command":"npx","args":[...]}}} （完整 .mcp.json 内容）
func ParseMcpServersPayload(body []byte) (map[string]*mcp.Server, *Error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) == 0 {
		return nil, Invalid(`invalid request body: want an mcpServers object like {"playwright":{"command":"npx","args":[]}}`)
	}
	if _, ok := raw["mcpServers"]; ok && len(raw) == 1 {
		var servers map[string]*mcp.Server
		if err := json.Unmarshal(raw["mcpServers"], &servers); err != nil || len(servers) == 0 {
			return nil, Invalid(`mcpServers 值必须是一个对象，如 {"playwright":{"command":"npx"}}`)
		}
		return servers, nil
	}
	servers := make(map[string]*mcp.Server, len(raw))
	for k, v := range raw {
		var srv mcp.Server
		if err := json.Unmarshal(v, &srv); err != nil {
			return nil, Invalid("server " + k + ": " + err.Error())
		}
		servers[k] = &srv
	}
	return servers, nil
}
