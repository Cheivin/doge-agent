package mcp

import (
	"strings"
)

// searchResult 一次命中的工具（跨 server）
type searchResult struct {
	server string
	name   string
	desc   string
	score  int
}

// searchTools 按 query 加权检索全部启用 server 的缓存工具：命中 name /
// server / description / searchKeywords。hyphen/underscore 视为等价
// （context7_resolve_library_id 可命中 context7_resolve-library-id）。
// limit+offset 分页；空 query 返回前 limit 个（浏览模式）。
func (m *Manager) searchTools(query, serverFilter string, limit, offset int) []searchResult {
	m.mu.Lock()
	servers := make([]*server, 0, len(m.servers))
	for name, s := range m.servers {
		if s.cfg.Disabled {
			continue
		}
		if serverFilter != "" && name != serverFilter {
			continue
		}
		servers = append(servers, s)
	}
	m.mu.Unlock()

	norm := normalizeQuery(query)
	var hits []searchResult
	for _, s := range servers {
		s.mu.Lock()
		tools := s.tools
		keywords := s.cfg.SearchKeywords
		s.mu.Unlock()
		for _, t := range tools {
			score := scoreTool(s.name, t.Name, t.Description, keywords, norm)
			if score <= 0 {
				continue
			}
			hits = append(hits, searchResult{server: s.name, name: t.Name, desc: t.Description, score: score})
		}
	}
	// 按分数降序（同分按 server+name）
	sortSearch(hits)
	if offset > len(hits) {
		offset = len(hits)
	}
	hits = hits[offset:]
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// scoreTool 返回加权分数（<=0 表示不命中）。
// 权重：name 全词 100 / 子串 60，name 在 query 内 50，server 名 30，
// description 10，searchKeywords 40。
func scoreTool(server, name, desc string, keywords map[string][]string, query string) int {
	if query == "" {
		return 1 // 浏览模式：全部命中
	}
	q := normalizeQuery(query)
	if q == "" {
		return 1
	}
	full := normalizeQuery(server + "__" + name)
	nameNorm := normalizeQuery(name)
	if full == q {
		return 100
	}
	if strings.Contains(full, q) {
		return 60
	}
	// 查询带 server 前缀/全名记忆时（context7_resolve_library_id 命中
	// resolve_library_id），name 是 query 的子串
	if strings.Contains(q, nameNorm) {
		return 50
	}
	if normalizeQuery(server) == q || strings.Contains(normalizeQuery(server), q) {
		return 30
	}
	if strings.Contains(normalizeQuery(desc), q) {
		return 10
	}
	for _, kw := range keywords[name] {
		if normalizeQuery(kw) == q {
			return 40
		}
	}
	return 0
}

// normalizeQuery 小写化并把 hyphen/underscore 归一到连字符，做模糊等价
func normalizeQuery(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	return strings.ReplaceAll(s, ".", "-")
}

// sortSearch 按 score 降序，同分按 server+name 字典序
func sortSearch(hits []searchResult) {
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0; j-- {
			if hits[j].score > hits[j-1].score ||
				(hits[j].score == hits[j-1].score && hits[j].server+hits[j].name < hits[j-1].server+hits[j-1].name) {
				hits[j], hits[j-1] = hits[j-1], hits[j]
			} else {
				break
			}
		}
	}
}
