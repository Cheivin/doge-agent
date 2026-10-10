package coding

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Skill is a skill loaded from a SKILL.md file (agentskills.io format).
type Skill struct {
	Name                   string
	Description            string
	Content                string
	FilePath               string
	DisableModelInvocation bool
}

var skillNameRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// LoadSkills discovers skills from the given directories: a directory
// containing SKILL.md is a skill root; otherwise .md files directly inside
// the directory are treated as skills.
func LoadSkills(dirs ...string) []Skill {
	seen := map[string]string{} // name → file path (first wins)
	var skills []Skill
	for _, dir := range dirs {
		collectSkillsFromDir(dir, seen, &skills)
	}
	return skills
}

// filterDisabledSkills 剔除全局 settings（disabledSkills）禁用的技能
func filterDisabledSkills(skills []Skill, disabled map[string]bool) []Skill {
	if len(disabled) == 0 {
		return skills
	}
	var out []Skill
	for _, s := range skills {
		if !disabled[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

func collectSkillsFromDir(dir string, seen map[string]string, out *[]Skill) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// A directory containing SKILL.md is a skill root.
	skillFile := filepath.Join(dir, "SKILL.md")
	if data, err := os.ReadFile(skillFile); err == nil {
		if s, ok := parseSkill(skillFile, data); ok {
			addSkill(s, seen, out)
		}
		return
	}
	// Otherwise: .md files directly inside are skills, and subdirectories
	// are searched recursively for SKILL.md.
	for _, e := range entries {
		if e.IsDir() {
			collectSkillsFromDir(filepath.Join(dir, e.Name()), seen, out)
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if s, ok := parseSkill(path, data); ok {
			addSkill(s, seen, out)
		}
	}
}

func addSkill(s Skill, seen map[string]string, out *[]Skill) {
	if _, exists := seen[s.Name]; exists {
		return // first occurrence wins
	}
	seen[s.Name] = s.FilePath
	*out = append(*out, s)
}

// parseSkill parses a SKILL.md file: frontmatter (name/description/
// disable-model-invocation) + content.
func parseSkill(path string, data []byte) (Skill, bool) {
	s := Skill{
		Name:     strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		FilePath: path,
		Content:  string(data),
	}
	text := string(data)
	if strings.HasPrefix(text, "---") {
		end := strings.Index(text[4:], "---")
		if end >= 0 {
			fm := text[4 : 4+end]
			contentStart := 4 + end + 3
			if contentStart < len(text) {
				s.Content = strings.TrimPrefix(text[contentStart:], "\n")
			}
			for line := range strings.SplitSeq(fm, "\n") {
				line = strings.TrimSpace(line)
				switch {
				case strings.HasPrefix(line, "name:"):
					s.Name = strings.TrimSpace(strings.TrimPrefix(line, "name:"))
					s.Name = strings.Trim(s.Name, `"'`)
				case strings.HasPrefix(line, "description:"):
					s.Description = strings.TrimSpace(strings.TrimPrefix(line, "description:"))
					s.Description = strings.Trim(s.Description, `"'`)
				case strings.HasPrefix(line, "disable-model-invocation:"):
					s.DisableModelInvocation = strings.TrimSpace(strings.TrimPrefix(line, "disable-model-invocation:")) == "true"
				}
			}
		}
	}
	if s.Description == "" {
		return Skill{}, false // description is required
	}
	if !skillNameRe.MatchString(s.Name) {
		return Skill{}, false
	}
	return s, true
}

// FormatSkillsForPrompt renders the model-visible skills XML block. Only
// metadata (name + description + location) is always in context; the full
// body loads on demand via the "skill" tool (progressive disclosure).
func FormatSkillsForPrompt(skills []Skill) string {
	var out strings.Builder
	out.WriteString("<available_skills>\n")
	for _, s := range skills {
		if s.DisableModelInvocation {
			continue
		}
		out.WriteString("<skill><name>" + s.Name + "</name><description>" + s.Description + "</description><location>" + s.FilePath + "</location></skill>\n")
	}
	out.WriteString("To use a skill, call the \"skill\" tool with its name to load its full instructions, then follow them. Only skills listed above are available.\n")
	out.WriteString("</available_skills>")
	return out.String()
}

// SkillBlockRe matches `<skill name location>` invocation blocks.
var SkillBlockRe = regexp.MustCompile(`<skill\s+name="([^"]+)"\s+location="([^"]*)"\s*/>`)

// ExpandSkillCommand expands "/skill:name args" into a user message that
// carries the skill's full instructions inline, so an explicit user invocation
// works without any further model cooperation. A `<skill name location/>`
// marker is kept at the top so the model can reference the same skill again.
func ExpandSkillCommand(skills []Skill, line string) string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/skill:") {
		return line
	}
	rest := strings.TrimPrefix(line, "/skill:")
	name := rest
	args := ""
	if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		name = rest[:idx]
		args = strings.TrimSpace(rest[idx:])
	}
	for _, s := range skills {
		if s.Name == name {
			return renderSkillInvocation(s, args, true)
		}
	}
	return line
}

// ResolveSkillBlocks finds every `<skill name location/>` invocation block in
// text and returns the full instructions of the referenced skills, each once
// and in order of first appearance. Blocks whose skill is unknown, or marked
// DisableModelInvocation, are skipped (the user must invoke those with
// /skill:name). This is the model-emitted half of the skills loop: the
// harness reads the block and delivers the content back into the context.
func ResolveSkillBlocks(text string, skills []Skill) []string {
	byName := make(map[string]Skill, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range SkillBlockRe.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		s, ok := byName[name]
		if !ok || s.DisableModelInvocation {
			continue
		}
		out = append(out, renderSkillInvocation(s, "", false))
	}
	return out
}

// renderSkillInvocation builds the model-visible text for an invoked skill:
// the full instructions plus, for user-initiated calls, the arguments. The
// user-invoked form leads with the canonical `<skill .../>` marker so the
// model can re-invoke the same skill; the model-emitted form (from
// ResolveSkillBlocks) is the content only, to avoid re-triggering resolution.
func renderSkillInvocation(s Skill, args string, withMarker bool) string {
	var b strings.Builder
	if withMarker {
		b.WriteString(`<skill name="` + s.Name + `" location="` + s.FilePath + `"/>` + "\n")
		b.WriteString("The user invoked skill \"" + s.Name + "\". Follow its instructions to complete the task.\n")
	}
	b.WriteString("=== Skill: " + s.Name + " ===\n")
	b.WriteString(s.Content)
	if !strings.HasSuffix(s.Content, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("=== End skill ===\n")
	if args != "" {
		b.WriteString(args)
	}
	return b.String()
}

// SkillInfo is the API view of a skill without its body: enough to list what
// is installed and whether the model may load it on its own.
type SkillInfo struct {
	Name                   string `json:"name"`
	Description            string `json:"description"`
	Location               string `json:"location"`
	DisableModelInvocation bool   `json:"disableModelInvocation"`
	ContentLength          int    `json:"contentLength"`
	Disabled               bool   `json:"disabled"` // 是否被全局停用（settings.disabledSkills）
}

// SkillsInfo returns the session's loaded skills — the set behind its skill
// tool and <available_skills> block (mirrors the session-level MCP view).
func (s *AgentSession) SkillsInfo() []SkillInfo {
	disabled := DisabledSkillSet(s.Cwd)
	out := make([]SkillInfo, 0, len(s.skills))
	for _, sk := range s.skills {
		out = append(out, SkillInfo{
			Name:                   sk.Name,
			Description:            sk.Description,
			Location:               sk.FilePath,
			DisableModelInvocation: sk.DisableModelInvocation,
			ContentLength:          len(sk.Content),
			Disabled:               disabled[sk.Name],
		})
	}
	return out
}

// SkillFile is one file inside a skill bundle (the UI lists references,
// templates and scripts without a shell).
type SkillFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// SkillDetail is the full API view of a skill: metadata, the SKILL.md body,
// and the bundle file tree.
type SkillDetail struct {
	SkillInfo
	Content string      `json:"content"`
	Files   []SkillFile `json:"files"`
}

// SkillDetailView builds the detail view for a loaded skill. The file tree is
// the directory holding SKILL.md (references/scripts/templates/assets), so a
// UI can show what the skill ships without reading arbitrary paths.
func SkillDetailView(s Skill) SkillDetail {
	return SkillDetail{
		SkillInfo: SkillInfo{
			Name:                   s.Name,
			Description:            s.Description,
			Location:               s.FilePath,
			DisableModelInvocation: s.DisableModelInvocation,
			ContentLength:          len(s.Content),
		},
		Content: s.Content,
		Files:   skillFiles(s.FilePath),
	}
}

// skillFiles lists the skill bundle recursively, skipping hidden entries,
// sorted by path. A broken tree (missing directory) yields an empty list.
func skillFiles(skillPath string) []SkillFile {
	dir := filepath.Dir(skillPath)
	var out []SkillFile
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		out = append(out, SkillFile{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
