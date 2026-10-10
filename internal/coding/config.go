package coding

import (
	"os"
	"path/filepath"

	"doge/internal/ai"
)

// Config paths: everything lives under ~/.doge/agent
// unless DOGE_CODING_AGENT_DIR overrides it.

const configDirName = ".doge"

// AgentDir returns ~/.doge/agent (or DOGE_CODING_AGENT_DIR).
func AgentDir() string {
	if v := os.Getenv("DOGE_CODING_AGENT_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".doge/agent"
	}
	return filepath.Join(home, configDirName, "agent")
}

// ProjectDir returns the project config dir (.doge).
func ProjectDir(cwd string) string {
	return filepath.Join(cwd, configDirName)
}

// SessionsDir returns the session storage directory.
func SessionsDir() string {
	if v := os.Getenv("DOGE_CODING_AGENT_SESSION_DIR"); v != "" {
		return v
	}
	return filepath.Join(AgentDir(), "sessions")
}

// AuthFile returns the credential store path.
func AuthFile() string {
	return filepath.Join(AgentDir(), "auth.json")
}

// SettingsFile returns the global settings path.
func SettingsFile() string {
	return filepath.Join(AgentDir(), "settings.json")
}

// ProjectSettingsFile returns the project settings path.
func ProjectSettingsFile(cwd string) string {
	return filepath.Join(ProjectDir(cwd), "settings.json")
}

// LogDir returns the diagnostic log directory (internal/logx sink).
func LogDir() string {
	return filepath.Join(AgentDir(), "logs")
}

// LogFile returns the process log path.
func LogFile() string {
	return filepath.Join(LogDir(), "doge.log")
}

// UploadsDir returns the upload staging directory.
func UploadsDir() string {
	return filepath.Join(AgentDir(), "uploads")
}

// ModelsFile returns the user models catalog path.
func ModelsFile() string {
	return filepath.Join(AgentDir(), "models.json")
}

// SkillsDirs returns the skill discovery directories (user, project).
func SkillsDirs(cwd string) []string {
	return []string{
		filepath.Join(AgentDir(), "skills"),
		filepath.Join(ProjectDir(cwd), "skills"),
	}
}

// PromptTemplateDirs returns the prompt template directories.
func PromptTemplateDirs(cwd string) []string {
	return []string{
		filepath.Join(AgentDir(), "prompts"),
		filepath.Join(ProjectDir(cwd), "prompts"),
	}
}

// GlobalContextFile returns the global instructions file path (AGENTS.md
// convention, the global layer above the project's AGENTS.md/CLAUDE.md).
func GlobalContextFile() string {
	return filepath.Join(AgentDir(), "AGENTS.md")
}

// EnsureAgentDir creates the agent directory tree.
func EnsureAgentDir() error {
	for _, d := range []string{
		AgentDir(),
		SessionsDir(),
		UploadsDir(),
		filepath.Join(AgentDir(), "skills"),
		filepath.Join(AgentDir(), "prompts"),
		LogDir(),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// ApiKey is resolved through ai.ResolveAPIKey; the auth store adapter lives
// in model_runtime.go.
var _ = ai.StopReasonStop
