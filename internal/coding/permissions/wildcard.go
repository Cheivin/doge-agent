package permissions

import "strings"

// MatchPattern reports whether the glob pattern matches s. `*` matches any
// run sequence (including slashes and spaces), `?` matches a single rune.
// Exported for MCP approveTools reuse.
func MatchPattern(pattern, s string) bool {
	return matchAt(pattern, s, 0, 0)
}

func matchAt(p, s string, pIdx, si int) bool {
	for pIdx < len(p) {
		switch p[pIdx] {
		case '*':
			// Collapse consecutive stars, then try consuming 0..n runes.
			for pIdx < len(p) && p[pIdx] == '*' {
				pIdx++
			}
			if pIdx == len(p) {
				return true
			}
			for i := si; i <= len(s); i++ {
				if matchAt(p, s, pIdx, i) {
					return true
				}
			}
			return false
		case '?':
			if si >= len(s) {
				return false
			}
			pIdx++
			si++
		default:
			if si >= len(s) || p[pIdx] != s[si] {
				return false
			}
			pIdx++
			si++
		}
	}
	return si == len(s)
}

// normalizeCommand strips leading/trailing whitespace, collapses runs of
// whitespace, and normalizes newlines so rule patterns see a single-line
// command.
func normalizeCommand(cmd string) string {
	return strings.Join(strings.Fields(cmd), " ")
}
