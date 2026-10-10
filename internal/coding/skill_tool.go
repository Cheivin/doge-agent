package coding

import (
	"context"
	"strings"

	"doge/internal/agent"
	"doge/internal/ai"
)

// SkillTool returns the "skill" tool, the model-initiated half of the skills
// loop. The system prompt advertises each skill's name + description
// (<available_skills>); calling this tool returns the skill's full
// instructions, which then guide the rest of the turn. Mirrors how other
// harnesses (Claude Code's Skill meta-tool) do progressive disclosure: only
// the metadata is always in context, the body loads on demand.
func SkillTool(skills []Skill) *agent.AgentTool {
	byName := make(map[string]Skill, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	list := strings.Join(names, ", ")
	return &agent.AgentTool{
		Name:        "skill",
		Label:       "Skill",
		Description: "Load a skill's full instructions. Call it with the name of a skill listed in <available_skills> to receive that skill's complete instructions to follow.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Name of the skill to load (must be one of the skills in <available_skills>)",
				},
			},
			"required":             []any{"name"},
			"additionalProperties": false,
		},
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, onUpdate agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			name, _ := params["name"].(string)
			s, ok := byName[name]
			if !ok {
				return skillError("skill: unknown skill \"" + name + "\". Available skills: " + list), nil
			}
			if s.DisableModelInvocation {
				return skillError("skill \"" + name + "\" is not invocable by the model; ask the user to invoke it with /skill:" + name), nil
			}
			var b strings.Builder
			b.WriteString("Skill \"" + s.Name + "\" instructions:\n")
			b.WriteString("Location: " + s.FilePath + "\n")
			b.WriteString("=== Skill: " + s.Name + " ===\n")
			b.WriteString(s.Content)
			if !strings.HasSuffix(s.Content, "\n") {
				b.WriteString("\n")
			}
			b.WriteString("=== End skill ===\n")
			b.WriteString("Reference files (references/, templates/, scripts/) live alongside SKILL.md; read them with the read tool as needed.\n")
			return &agent.AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(b.String())}}, nil
		},
	}
}

// skillError builds an error tool result for the skill tool.
func skillError(message string) *agent.AgentToolResult {
	return &agent.AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(message)}}
}
