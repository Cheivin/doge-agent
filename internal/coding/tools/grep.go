package tools

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"doge/internal/agent"
)

// GrepTool searches for a pattern in files, respecting .gitignore files.
// It is a self-contained ripgrep replacement (line-based output).
func GrepTool(ops *Operations) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "grep",
		Label:       "Grep",
		Description: "Search for a regex pattern in files. Results are path:line: text.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":    map[string]any{"type": "string", "description": "Regex pattern (RE2)"},
				"path":       map[string]any{"type": "string", "description": "Directory or file to search (default: current directory)"},
				"glob":       map[string]any{"type": "string", "description": "Glob filter, e.g. '*.go'"},
				"ignoreCase": map[string]any{"type": "boolean"},
				"literal":    map[string]any{"type": "boolean", "description": "Treat pattern as a literal string"},
				"limit":      map[string]any{"type": "integer", "description": "Max matches (default 100)"},
			},
			"required":             []any{"pattern"},
			"additionalProperties": false,
		},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			pattern, _ := params["pattern"].(string)
			if pattern == "" {
				return ToolError("grep: pattern is required"), nil
			}
			path, _ := params["path"].(string)
			if path == "" {
				path = "."
			}
			glob, _ := params["glob"].(string)
			ignoreCase, _ := params["ignoreCase"].(bool)
			literal, _ := params["literal"].(bool)
			limit := intParam(params, "limit", 100)

			var re *regexp.Regexp
			var err error
			if literal {
				re, err = regexp.Compile(regexp.QuoteMeta(pattern))
			} else {
				re, err = regexp.Compile(pattern)
			}
			if err != nil {
				return ToolError(fmt.Sprintf("grep: invalid pattern: %v", err)), nil
			}
			if ignoreCase {
				re = regexp.MustCompile("(?i)" + re.String())
			}

			info, err := ops.Stat(path)
			if err != nil {
				return ToolError(fmt.Sprintf("grep: %v", err)), nil
			}

			var globRe *regexp.Regexp
			if glob != "" {
				globRe = globToRegexp(glob)
			}

			ignorer := &ignoreMatcher{}
			if info.IsDir() {
				ignorer, err = loadIgnore(ops, path)
				if err != nil {
					return ToolError(fmt.Sprintf("grep: %v", err)), nil
				}
			}

			var out strings.Builder
			matches := 0
			stop := false

			searchFile := func(file string) {
				if stop || ctx.Err() != nil {
					return
				}
				if globRe != nil && !globRe.MatchString(file) {
					return
				}
				f, err := os.Open(file)
				if err != nil {
					return
				}
				defer f.Close()
				scanner := bufio.NewScanner(f)
				scanner.Buffer(make([]byte, 64<<10), 1<<20)
				lineNo := 0
				for scanner.Scan() {
					lineNo++
					line := scanner.Text()
					if re.MatchString(line) {
						fmt.Fprintf(&out, "%s:%d: %s\n", file, lineNo, truncateLine(line))
						matches++
						if matches >= limit {
							stop = true
							return
						}
					}
				}
			}

			if !info.IsDir() {
				searchFile(path)
			} else {
				_ = ops.WalkDir(path, func(file string, d fs.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if stop || ctx.Err() != nil {
						return fs.SkipAll
					}
					if d.IsDir() {
						if file != path && (ignorer.Ignored(file, true) || shouldSkipDir(file)) {
							return filepath.SkipDir
						}
						return nil
					}
					if !ignorer.Ignored(file, false) {
						searchFile(file)
					}
					return nil
				})
			}

			if matches == 0 {
				return ToolResult("No matches found"), nil
			}
			return ToolResult(strings.TrimSuffix(out.String(), "\n")), nil
		},
	}
}

// FindTool lists files matching a glob pattern, respecting .gitignore.
func FindTool(ops *Operations) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "find",
		Label:       "Find",
		Description: "List files matching a glob pattern (e.g. '**/*.go').",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Glob pattern"},
				"path":    map[string]any{"type": "string", "description": "Directory to search (default: current directory)"},
				"limit":   map[string]any{"type": "integer", "description": "Max results (default 1000)"},
			},
			"required":             []any{"pattern"},
			"additionalProperties": false,
		},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			pattern, _ := params["pattern"].(string)
			if pattern == "" {
				return ToolError("find: pattern is required"), nil
			}
			path, _ := params["path"].(string)
			if path == "" {
				path = "."
			}
			limit := intParam(params, "limit", 1000)

			re := globToRegexp(pattern)
			ignorer, err := loadIgnore(ops, path)
			if err != nil {
				return ToolError(fmt.Sprintf("find: %v", err)), nil
			}

			var out []string
			stop := false
			_ = ops.WalkDir(path, func(file string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if stop || ctx.Err() != nil {
					return fs.SkipAll
				}
				if d.IsDir() {
					if file != path && (ignorer.Ignored(file, true) || shouldSkipDir(file)) {
						return filepath.SkipDir
					}
					return nil
				}
				if ignorer.Ignored(file, false) {
					return nil
				}
				rel, _ := filepath.Rel(path, file)
				if re.MatchString(rel) {
					out = append(out, rel)
					if len(out) >= limit {
						stop = true
						return fs.SkipAll
					}
				}
				return nil
			})
			if len(out) == 0 {
				return ToolResult("No files found"), nil
			}
			return ToolResult(strings.Join(out, "\n")), nil
		},
	}
}

func shouldSkipDir(path string) bool {
	name := filepath.Base(path)
	switch name {
	case ".git", "node_modules", ".hg", ".svn", ".venv", "venv", "__pycache__":
		return true
	}
	return false
}

func truncateLine(s string) string {
	const maxLine = 1000
	if len(s) > maxLine {
		return s[:maxLine] + "…"
	}
	return s
}

// globToRegexp converts a simple glob ('*', '**', '?') to a regexp.
func globToRegexp(glob string) *regexp.Regexp {
	var out strings.Builder
	out.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				out.WriteString(".*")
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
				}
			} else {
				out.WriteString("[^/]*")
			}
		case '?':
			out.WriteString("[^/]")
		default:
			out.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	out.WriteString("$")
	return regexp.MustCompile(out.String())
}

// ignoreMatcher implements .gitignore-style matching for grep/find.
type ignoreMatcher struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	re       *regexp.Regexp
	negated  bool
	dirOnly  bool
	anchored bool // starts with /
}

// loadIgnore parses .gitignore files from path upward to the root.
func loadIgnore(ops *Operations, path string) (*ignoreMatcher, error) {
	m := &ignoreMatcher{}
	abs := path
	if ops.Abs != nil {
		if resolved, err := ops.Abs(path); err == nil {
			abs = resolved
		}
	} else if resolved, err := filepath.Abs(path); err == nil {
		abs = resolved
	}
	if info, err := ops.Stat(abs); err == nil && !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	dir := abs
	for {
		gitignore := filepath.Join(dir, ".gitignore")
		if data, err := ops.ReadFile(gitignore); err == nil {
			m.addGitignore(string(data))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return m, nil
}

func (m *ignoreMatcher) addGitignore(content string) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negated := strings.HasPrefix(line, "!")
		if negated {
			line = line[1:]
		}
		dirOnly := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		anchored := strings.HasPrefix(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		re := globToRegexp(line)
		// Unanchored patterns match at any level.
		if !anchored {
			re = regexp.MustCompile("(^|/)" + re.String()[1:len(re.String())-1] + "($|/)")
		}
		m.patterns = append(m.patterns, ignorePattern{re: re, negated: negated, dirOnly: dirOnly, anchored: anchored})
	}
}

// Ignored reports whether the path (relative or absolute) is ignored.
func (m *ignoreMatcher) Ignored(path string, isDir bool) bool {
	ignored := false
	for _, p := range m.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if p.re.MatchString(path) || p.re.MatchString(filepath.Base(path)) {
			ignored = !p.negated
		}
	}
	return ignored
}
