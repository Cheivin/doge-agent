package coding

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"doge/internal/agent"
)

// SystemPromptOptions configure the system prompt assembly.
type SystemPromptOptions struct {
	CustomPrompt string
	// Append is appended after the assembled prompt.
	Append string
	// Tools are the active tools; their snippets are listed.
	Tools []*agent.AgentTool
	// GlobalContext is the ~/.doge/agent/AGENTS.md content, if any — the
	// user-level instructions layer (persona, environment, preferences).
	GlobalContext string
	// ProjectContext is the AGENTS.md/CLAUDE.md content from the cwd, if any.
	ProjectContext string
	// Skills are injected as an XML block.
	Skills []Skill
	// Cwd is the current working directory.
	Cwd string
}

// ToolPromptInfo describes how a tool is presented in the prompt.
type ToolPromptInfo struct {
	Snippet    string
	Guidelines string
}

// BuildSystemPrompt assembles the system prompt, following the
// buildSystemPrompt: custom prompt or default template, then the available
// tools list, guidelines, project context, skills, and cwd.
func BuildSystemPrompt(opts SystemPromptOptions) string {
	var out strings.Builder

	if opts.CustomPrompt != "" {
		out.WriteString(strings.TrimSpace(opts.CustomPrompt))
		out.WriteString("\n\n")
	} else {
		out.WriteString(defaultSystemPrompt(opts.Tools))
		out.WriteString("\n\n")
	}

	if opts.Append != "" {
		out.WriteString(strings.TrimSpace(opts.Append))
		out.WriteString("\n\n")
	}

	// 全局在项目之前：全局管"我是谁、在什么环境"，项目管"这个仓库怎么做"，
	// 后写的项目上下文可以覆盖全局的细节约定
	if opts.GlobalContext != "" {
		out.WriteString("<user_instructions>\n")
		out.WriteString(opts.GlobalContext)
		out.WriteString("\n</user_instructions>\n\n")
	}

	if opts.ProjectContext != "" {
		out.WriteString("<project_context>\n")
		out.WriteString(opts.ProjectContext)
		out.WriteString("\n</project_context>\n\n")
	}

	if len(opts.Skills) > 0 {
		out.WriteString(FormatSkillsForPrompt(opts.Skills))
		out.WriteString("\n")
	}

	out.WriteString("Current working directory: " + opts.Cwd)
	out.WriteString("\nPlatform: " + platform())
	out.WriteString("\nCurrent date: " + time.Now().Format("2006-01-02"))
	return out.String()
}

// platform reports the OS and architecture of the host running the harness
// (the model may need it to pick shell commands or file paths).
func platform() string {
	var sb strings.Builder
	sb.WriteString(runtime.GOOS)
	switch runtime.GOARCH {
	case "amd64":
		sb.WriteString(" (x86_64)")
	case "arm64":
		sb.WriteString(" (arm64)")
	default:
		sb.WriteString(" (" + runtime.GOARCH + ")")
	}
	return sb.String()
}

// defaultSystemPrompt builds the default prompt with the tool list.
func defaultSystemPrompt(tools []*agent.AgentTool) string {
	var out strings.Builder
	out.WriteString(`You are an AI assistant operating inside doge, a command-line agent harness.
You complete user requests by using the available tools and reporting back. Your
replies are rendered as GitHub-flavored Markdown.

## Operating rules
- Prefer a dedicated tool over a shell command when one fits: use the read,
  write, edit, grep, find and ls tools rather than cat/sed/grep via bash.
- When you reference a file, show its path; use ` + "`file:line`" + ` when it helps
  locate a specific spot.
- A denied tool call or a blocked command is a decision, not an error: change
  your approach instead of retrying the same call verbatim.
- Do not help with destructive or malicious actions (data destruction, attacks
  on third parties, credential theft, evasion of security controls).

## Working with the user
- Your text output is what the user reads; write it for a teammate who stepped
  away and is catching up, not for a log file.
- Lead with the outcome: state what happened before the supporting detail.
- Be concise, but readable: completeness beats brevity. Drop details that would
  not change what the reader does next, rather than compressing prose into
  fragments.
- Match the response to the question: a simple question gets a direct answer
  in prose, not a list of sections.
- Before your first tool call, say in a sentence what you are about to do; give
  brief updates while working whenever you find something important.
- Everything the user needs from this turn must be in your final message.

## Autonomy
- Act once you have enough information; do not re-derive facts already
  established or narrate options you will not pursue.
- For reversible actions that follow from the request, proceed without asking.
  Stop and ask only for destructive actions or genuine scope changes.
- When the user is describing a problem rather than requesting a change, report
  your findings and stop; do not apply a fix until they ask for one.

## Code discipline
- Write code that reads like the surrounding code: match its comment density,
  naming, and idiom.
- Only write a code comment to state a constraint the code itself cannot show;
  never to say where the code came from or why your change is correct.
`)
	if len(tools) > 0 {
		out.WriteString("Available tools:\n")
		for _, t := range tools {
			if t.Label == "" {
				t.Label = strings.Title(t.Name)
			}
			out.WriteString("- " + t.Name + ": " + firstLine(t.Description) + "\n")
		}
		out.WriteString("\n")
	}

	if hasTool(tools, "remote_agent") {
		out.WriteString(`## Mesh collaboration (other doge nodes)
- To run a task on another doge node — inspecting its system, listing or reading
  its files, running commands there — use the ` + "`remote_agent`" + ` tool. There is
  NO shell or CLI command for dispatching remote tasks; do not try ` + "`which doge`" + `,
  ` + "`doge mesh ...`" + `, or reading this repo's source to find one.
- Use ` + "`remote_agent`" + ` with ` + "`async: true`" + ` for remote work, then END your turn and
  WAIT: the completion result arrives as a follow-up notification which starts
  the next turn. Do NOT poll — never run ` + "`bash sleep`" + ` + ` + "`remote_agent_status`" + `
  loops to wait for a task; the notification is the event you wait for. Use
  ` + "`remote_agent_status`" + ` only to inspect a task's progress on request, not in a loop.
- Use ` + "`mesh_peers`" + ` to discover nodes (peer ids / roles / accepting / load). Remote
  tasks default to reusing the same remote session for continuity; pass
  session="new" for independent tasks.
`)
	}
	return out.String()
}

// hasTool reports whether the tool set contains a tool with the given name.
func hasTool(tools []*agent.AgentTool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before
	}
	return s
}

// LoadGlobalContext reads the user-level instructions file
// (~/.doge/agent/AGENTS.md) — the global layer above the project context.
func LoadGlobalContext() string {
	data, err := os.ReadFile(GlobalContextFile())
	if err != nil {
		return ""
	}
	return string(data)
}

// LoadProjectContext reads AGENTS.md or CLAUDE.md from the cwd
// project context files into the system prompt).
func LoadProjectContext(cwd string) string {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		path := filepath.Join(cwd, name)
		if data, err := os.ReadFile(path); err == nil {
			return string(data)
		}
	}
	return ""
}
