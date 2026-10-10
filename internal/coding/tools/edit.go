package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"doge/internal/agent"
	"doge/internal/ai"
)

// EditOp is one replacement in an edit call.
type EditOp struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// EditTool applies a set of non-overlapping replacements to a file. All
// oldText values must match uniquely against the original file content.
// Matching falls back to fuzzy matching (normalized text) when exact
// matches fail, preserving the unchanged bytes of untouched lines.
func EditTool(ops *Operations) *agent.AgentTool {
	return &agent.AgentTool{
		Name:        "edit",
		Label:       "Edit",
		Description: "Apply replacements to a file. Each oldText must match exactly once; use write for whole-file rewrites.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
				"edits": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"oldText": map[string]any{"type": "string"},
							"newText": map[string]any{"type": "string"},
						},
						"required": []any{"oldText", "newText"},
					},
				},
			},
			"required":             []any{"path", "edits"},
			"additionalProperties": false,
		},
		PrepareArguments: func(args any) (map[string]any, error) {
			m, ok := args.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("expected an object")
			}
			// Some models pass edits as a JSON string or use legacy
			// oldText/newText top-level fields.
			if _, ok := m["edits"]; !ok {
				if _, hasOld := m["oldText"]; hasOld {
					m["edits"] = []any{map[string]any{
						"oldText": m["oldText"], "newText": m["newText"],
					}}
				}
			} else if s, ok := m["edits"].(string); ok {
				var ops []any
				if err := json.Unmarshal([]byte(s), &ops); err != nil {
					return nil, fmt.Errorf("edits must be a JSON array: %w", err)
				}
				m["edits"] = ops
			}
			return m, nil
		},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			path, _ := params["path"].(string)
			if path == "" {
				return ToolError("edit: path is required"), nil
			}
			rawEdits, _ := params["edits"].([]any)
			edits := make([]EditOp, 0, len(rawEdits))
			for _, re := range rawEdits {
				m, ok := re.(map[string]any)
				if !ok {
					return ToolError("edit: each edit must be an object"), nil
				}
				oldText, _ := m["oldText"].(string)
				newText, _ := m["newText"].(string)
				edits = append(edits, EditOp{OldText: oldText, NewText: newText})
			}
			if len(edits) == 0 {
				return ToolError("edit: no edits provided"), nil
			}

			data, err := ops.ReadFile(path)
			if err != nil {
				return ToolError(fmt.Sprintf("edit: %v", err)), nil
			}
			content := stripBOM(string(data))
			hadBOM := content != string(data)

			var newContent string
			var diff string
			_ = WithPathLock(path, func() error {
				newContent, diff, err = applyEdits(content, edits, ops)
				return nil
			})
			if err != nil {
				return ToolError(fmt.Sprintf("edit: %v", err)), nil
			}
			if hadBOM {
				newContent = "\ufeff" + newContent
			}
			if err := ops.WriteFile(path, []byte(newContent), 0); err != nil {
				return ToolError(fmt.Sprintf("edit: %v", err)), nil
			}
			return &agent.AgentToolResult{
				Content: []ai.ContentBlock{ai.TextBlock(fmt.Sprintf("Edited %s", path))},
				Details: map[string]any{"diff": diff},
			}, nil
		},
	}
}

// applyEdits applies edits against the ORIGINAL content (not incrementally),
// checking that each oldText matches exactly once and edits don't overlap.
// editMatch is a located edit against the original content.
type editMatch struct {
	op    EditOp
	start int
	end   int
	fuzzy bool
}

func applyEdits(content string, edits []EditOp, ops *Operations) (string, string, error) {
	matches := make([]editMatch, 0, len(edits))

	for _, op := range edits {
		if op.OldText == "" {
			return "", "", fmt.Errorf("oldText must not be empty")
		}
		start := strings.Index(content, op.OldText)
		if start < 0 {
			// Fuzzy fallback: match in normalized space.
			if m, ok := fuzzyMatch(content, op.OldText); ok {
				matches = append(matches, editMatch{op: op, start: m.start, end: m.end, fuzzy: true})
				continue
			}
			return "", "", fmt.Errorf("oldText %q not found in file", truncateForError(op.OldText))
		}
		end := start + len(op.OldText)
		if strings.Index(content[start+1:], op.OldText) >= 0 {
			return "", "", fmt.Errorf("oldText %q is not unique in file", truncateForError(op.OldText))
		}
		matches = append(matches, editMatch{op: op, start: start, end: end})
	}

	// Overlap check.
	sort.Slice(matches, func(i, j int) bool { return matches[i].start < matches[j].start })
	for i := 1; i < len(matches); i++ {
		if matches[i].start < matches[i-1].end {
			return "", "", fmt.Errorf("edits overlap near byte %d", matches[i].start)
		}
	}

	// Build the new content and a unified diff (4 lines of context).
	var out strings.Builder
	last := 0
	for _, m := range matches {
		out.WriteString(content[last:m.start])
		out.WriteString(m.op.NewText)
		last = m.end
	}
	out.WriteString(content[last:])

	diff := unifiedDiff(content, matches)
	return out.String(), diff, nil
}

// fuzzyMatch finds oldText in content in a normalized space where smart
// quotes/dashes/special spaces are mapped to ASCII and trailing whitespace
// is ignored, mapping the match back to byte offsets in the original.
func fuzzyMatch(content, oldText string) (struct{ start, end int }, bool) {
	normContent := normalizeFuzzy(content)
	normOld := normalizeFuzzy(oldText)
	idx := strings.Index(normContent, normOld)
	if idx < 0 {
		return struct{ start, end int }{}, false
	}
	// normContent is a rune-for-rune transformation of content (see
	// normalizeRune): byte offsets differ only when runes differ in width.
	// Map the normalized byte index back to the content byte index.
	start := mapNormOffset(content, idx)
	end := mapNormOffset(content, idx+len(normOld))
	return struct{ start, end int }{start, end}, true
}

// mapNormOffset maps a byte offset in the normalized string back to the
// original string.
func mapNormOffset(content string, normOffset int) int {
	var normIdx int
	origIdx := 0
	for origIdx < len(content) {
		if normIdx >= normOffset {
			return origIdx
		}
		r := runeAt(content, origIdx)
		origIdx += runeLen(r)
		normIdx += runeLen(normalizeRune(r))
	}
	return origIdx
}

func runeAt(s string, i int) rune {
	for _, r := range s[i:] {
		return r
	}
	return 0
}

func runeLen(r rune) int {
	if r < 0x80 {
		return 1
	}
	if r < 0x800 {
		return 2
	}
	if r < 0x10000 {
		return 3
	}
	return 4
}

// normalizeFuzzy maps special characters to their ASCII equivalents,
// following the normalizeForFuzzyMatch. The transformation is rune-for-rune
// (no characters are removed) so byte offsets map back to the original.
func normalizeFuzzy(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); {
		r := runeAt(s, i)
		out.WriteRune(normalizeRune(r))
		i += runeLen(r)
	}
	return out.String()
}

// normalizeRune maps a rune to its ASCII equivalent for fuzzy matching.
func normalizeRune(r rune) rune {
	switch r {
	case '\u201c', '\u201d', '\u201e', '\u201f', '\u00ab', '\u00bb':
		return '"'
	case '\u2018', '\u2019', '\u201a', '\u201b', '\u2039', '\u203a':
		return '\''
	case '\u2013', '\u2014':
		return '-'
	case '\u00a0', '\u2007', '\u202f':
		return ' '
	case '\u2026':
		return '.'
	}
	return r
}

// unifiedDiff renders a standard unified diff with 4 lines of context.
func unifiedDiff(content string, matches []editMatch) string {
	if len(matches) == 0 {
		return ""
	}
	var out strings.Builder
	contextLines := 4
	lines := strings.Split(content, "\n")
	lineStarts := make([]int, len(lines)+1)
	pos := 0
	for i, l := range lines {
		lineStarts[i] = pos
		pos += len(l) + 1 // +1 for \n
	}
	lineStarts[len(lines)] = pos

	for _, m := range matches {
		startLine := lineIndex(lineStarts, m.start)
		endLine := lineIndex(lineStarts, m.end)
		from := startLine - contextLines
		if from < 0 {
			from = 0
		}
		to := endLine + contextLines
		if to > len(lines) {
			to = len(lines)
		}
		for i := from; i < to; i++ {
			if i >= startLine && i <= endLine {
				out.WriteString("-" + lines[i] + "\n")
			} else {
				out.WriteString(" " + lines[i] + "\n")
			}
		}
		// Replaced lines.
		for _, l := range strings.Split(strings.TrimSuffix(m.op.NewText, "\n"), "\n") {
			out.WriteString("+" + l + "\n")
		}
	}
	return strings.TrimSuffix(out.String(), "\n")
}

func lineIndex(lineStarts []int, offset int) int {
	// Binary search for the last lineStart <= offset.
	lo, hi := 0, len(lineStarts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if lineStarts[mid] <= offset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\ufeff")
}

func truncateForError(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
