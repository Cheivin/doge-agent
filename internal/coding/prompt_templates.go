package coding

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// PromptTemplate is a prompt template loaded from a .md file with
// frontmatter (description/argument-hint).
type PromptTemplate struct {
	Name        string
	Description string
	FilePath    string
	Content     string
}

var argRe = regexp.MustCompile(`\$\{([^}]+)\}|\$(\d+)|(\$@)`)

// LoadPromptTemplates loads .md templates from the given directories.
func LoadPromptTemplates(dirs ...string) []PromptTemplate {
	var out []PromptTemplate
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			t := PromptTemplate{
				Name:     strings.TrimSuffix(e.Name(), ".md"),
				FilePath: path,
				Content:  string(data),
			}
			if strings.HasPrefix(t.Content, "---") {
				if end := strings.Index(t.Content[4:], "---"); end >= 0 {
					fm := t.Content[4 : 4+end]
					start := 4 + end + 3
					if start < len(t.Content) {
						t.Content = strings.TrimPrefix(t.Content[start:], "\n")
					}
					for line := range strings.SplitSeq(fm, "\n") {
						line = strings.TrimSpace(line)
						if after, ok := strings.CutPrefix(line, "description:"); ok {
							t.Description = strings.Trim(strings.TrimSpace(after), `"'`)
						}
					}
				}
			}
			out = append(out, t)
		}
	}
	return out
}

// ExpandPromptTemplate expands a "/name args" invocation with bash-style
// placeholders: $1, $@, ${N:-default}, ${@:N:L}.
func ExpandPromptTemplate(t PromptTemplate, args string) string {
	parts := strings.Fields(args)
	return substituteArgs(t.Content, parts)
}

func substituteArgs(content string, args []string) string {
	return argRe.ReplaceAllStringFunc(content, func(m string) string {
		switch {
		case strings.HasPrefix(m, "${"):
			inner := m[2 : len(m)-1]
			if before, after, ok := strings.Cut(inner, ":-"); ok {
				name := before
				def := after
				if v, ok := argValue(name, args); ok {
					return v
				}
				return def
			}
			if v, ok := argValue(inner, args); ok {
				return v
			}
			return m
		case m == "$@":
			if len(args) == 0 {
				return ""
			}
			return strings.Join(args, " ")
		default: // $N
			n := 0
			for _, c := range m[1:] {
				if c < '0' || c > '9' {
					n = -1
					break
				}
				n = n*10 + int(c-'0')
			}
			if n < 1 || n > len(args) {
				return ""
			}
			return args[n-1]
		}
	})
}

// argValue resolves ${@:N:L} or $N style references.
func argValue(name string, args []string) (string, bool) {
	if strings.HasPrefix(name, "@:") {
		// ${@:N:L} slice
		rest := name[2:]
		parts := strings.SplitN(rest, ":", 2)
		n := atoi(parts[0])
		if n < 1 {
			return "", false
		}
		if len(parts) == 2 {
			l := atoi(parts[1])
			if n > len(args) {
				return "", false
			}
			end := min(n-1+l, len(args))
			return strings.Join(args[n-1:end], " "), true
		}
		if n > len(args) {
			return "", false
		}
		return strings.Join(args[n-1:], " "), true
	}
	n := atoi(name)
	if n < 1 || n > len(args) {
		return "", false
	}
	return args[n-1], true
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
