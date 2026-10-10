package permissions

import "strings"

// splitSegments splits a normalized shell command into its logical
// segments: everything joined by command separators (;, &&, ||, |),
// command substitution ($(...), backticks), and sub-shells. Segments are
// evaluated independently by the decider and the strictest outcome wins,
// so `ls ; rm -rf /` cannot bypass the rm rule.
func splitSegments(cmd string) []string {
	var segments []string
	var substitutions []string
	var current strings.Builder
	depth := 0
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
			current.WriteByte(c)
		case inDouble:
			if c == '"' {
				inDouble = false
			} else if c == '\\' && i+1 < len(cmd) {
				current.WriteByte(c)
				i++
				current.WriteByte(cmd[i])
				continue
			}
			current.WriteByte(c)
		case c == '\'':
			inSingle = true
			current.WriteByte(c)
		case c == '"':
			inDouble = true
			current.WriteByte(c)
		case c == '\\' && i+1 < len(cmd):
			current.WriteByte(c)
			i++
			current.WriteByte(cmd[i])
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '(':
			// $() command substitution: the inner command is evaluated as
			// its own segment (appended after the outer segments).
			inner, next := extractParen(cmd, i+2)
			substitutions = append(substitutions, strings.TrimSpace(inner))
			i = next
		case c == '`':
			// Backtick substitution: extract the inner command and skip it
			// in the outer segment.
			close := strings.IndexByte(cmd[i+1:], '`')
			if close >= 0 {
				inner := cmd[i+1 : i+1+close]
				substitutions = append(substitutions, strings.TrimSpace(inner))
				i += close + 1
			} else {
				current.WriteByte(c)
			}
		case c == ';' || c == '&' || c == '|':
			segments = append(segments, strings.TrimSpace(current.String()))
			current.Reset()
			// consume doubled separators (&&, ||)
			if i+1 < len(cmd) && cmd[i+1] == c {
				i++
			}
		case c == '(':
			depth++
			current.WriteByte(c)
		case c == ')':
			depth--
			current.WriteByte(c)
		default:
			current.WriteByte(c)
		}
	}
	segments = append(segments, strings.TrimSpace(current.String()))
	segments = append(segments, substitutions...)
	return filterEmpty(segments)
}

// extractParen returns the text inside a $() group starting at the open
// paren index, plus the index just past the closing paren.
func extractParen(cmd string, open int) (string, int) {
	depth := 1
	for i := open; i < len(cmd); i++ {
		switch cmd[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return cmd[open:i], i
			}
		}
	}
	return cmd[open:], len(cmd)
}

func filterEmpty(segments []string) []string {
	out := segments[:0]
	for _, s := range segments {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// shellWrappers are commands that execute their remaining arguments as a
// shell command; the inner command must be extracted for evaluation.
var shellWrappers = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"eval": true, "source": true, ".": true,
}

// unwrapShell extracts the inner command from a shell wrapper invocation
// ("sh -c 'rm -rf /'", "eval rm -rf /"). Returns (inner, true) when the
// command is a wrapper with a payload. The payload may span multiple
// fields (quotes were collapsed by normalizeCommand), so it is re-joined
// and surrounding quotes stripped.
func unwrapShell(cmd string) (string, bool) {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return "", false
	}
	verb := fields[0]
	if verb == "sudo" && len(fields) > 1 {
		fields = fields[1:]
		verb = fields[0]
	}
	if !shellWrappers[verb] {
		return "", false
	}
	rest := fields[1:]
	var payload []string
	switch verb {
	case "eval", "source", ".":
		payload = rest
	default:
		// sh -c "cmd": skip flags; -c may be separate or fused (-lc).
		for i := 0; i < len(rest); i++ {
			f := rest[i]
			if f == "-c" {
				payload = rest[i+1:]
				break
			}
			if strings.HasPrefix(f, "-") && strings.Contains(f, "c") && len(f) > 2 && !strings.Contains(f, "=") {
				payload = []string{strings.TrimPrefix(f, "-")}
				payload = append(payload, rest[i+1:]...)
				break
			}
			if strings.HasPrefix(f, "-") {
				continue
			}
			payload = rest[i:]
			break
		}
	}
	if len(payload) == 0 {
		return "", false
	}
	inner := strings.TrimSpace(strings.Join(payload, " "))
	if len(inner) >= 2 && ((inner[0] == '\'' && inner[len(inner)-1] == '\'') ||
		(inner[0] == '"' && inner[len(inner)-1] == '"')) {
		inner = inner[1 : len(inner)-1]
	}
	if inner == "" {
		return "", false
	}
	return inner, true
}
