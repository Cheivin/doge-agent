package permissions

import "strings"

// RiskLevel classifies a shell command for automatic decision-making.
type RiskLevel int

const (
	// RiskUnknown is the fallback for unrecognized commands: treated as
	// high risk (LLM approval or deny when unavailable).
	RiskUnknown RiskLevel = iota
	// RiskReadOnly commands only inspect state.
	RiskReadOnly
	// RiskProjectLocal commands mutate the local repo/project but are
	// recoverable (git add/commit, local builds).
	RiskProjectLocal
	// RiskSystem commands change system state (services, packages,
	// system files).
	RiskSystem
	// RiskNetwork commands push or pull data to/from remote hosts.
	RiskNetwork
)

func (r RiskLevel) String() string {
	switch r {
	case RiskReadOnly:
		return "readOnly"
	case RiskProjectLocal:
		return "projectLocal"
	case RiskSystem:
		return "system"
	case RiskNetwork:
		return "network"
	}
	return "unknown"
}

// classifyCommand splits a normalized command into its main verb and the
// first argument/subcommand, handling git/docker-style subcommands.
func classifyCommand(cmd string) (verb, arg string) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", ""
	}
	verb = fields[0]
	if len(fields) > 1 {
		arg = fields[1]
	}
	// sudo wraps the real command.
	if verb == "sudo" && len(fields) > 1 {
		verb = fields[1]
		if len(fields) > 2 {
			arg = fields[2]
		}
	}
	return verb, arg
}

// classifyRisk determines the risk level of a command.
func classifyRisk(cmd string) RiskLevel {
	verb, arg := classifyCommand(cmd)
	switch verb {
	case "ls", "cat", "head", "tail", "grep", "find", "wc", "pwd", "which",
		"whoami", "date", "env", "df", "du", "free", "ps", "top", "uptime",
		"stat", "file", "tree", "diff", "sort", "uniq", "echo", "ping", "dig",
		"nslookup", "journalctl":
		return RiskReadOnly
	case "curl", "wget":
		// Remote interactions are network-level, not read-only: POST/DELETE
		// mutate remote state. Piping into a shell is handled by the
		// builtin rules.
		return RiskNetwork
	case "git":
		switch arg {
		case "status", "log", "diff", "show", "branch", "tag", "remote",
			"rev-parse", "config", "ls-files", "blame":
			return RiskReadOnly
		case "add", "commit", "stash", "checkout", "switch", "restore",
			"reset", "merge", "rebase", "cherry-pick", "clean", "init":
			return RiskProjectLocal
		case "push", "pull", "fetch", "clone":
			return RiskNetwork
		}
		return RiskUnknown
	case "docker":
		switch arg {
		case "ps", "images", "logs", "inspect", "stats", "version", "network", "volume", "info":
			return RiskReadOnly
		case "build", "compose":
			return RiskProjectLocal
		case "run", "exec", "stop", "start", "restart", "rm", "rmi",
			"pull", "push", "save", "load", "tag", "commit":
			return RiskSystem
		}
		return RiskUnknown
	case "kubectl":
		switch arg {
		case "get", "describe", "logs", "explain", "top", "version":
			return RiskReadOnly
		case "apply", "delete", "scale", "rollout", "edit", "exec":
			return RiskSystem
		}
		return RiskUnknown
	case "systemctl", "service":
		switch arg {
		case "status", "is-active", "is-enabled", "list-units", "show":
			return RiskReadOnly
		case "start", "stop", "restart", "reload", "enable", "disable",
			"daemon-reload", "mask", "unmask":
			return RiskSystem
		}
		return RiskUnknown
	case "nginx", "caddy":
		switch arg {
		case "-t", "validate", "config":
			return RiskReadOnly
		case "-s", "reload", "restart", "stop":
			return RiskSystem
		}
		return RiskSystem
	case "apt", "apt-get", "yum", "dnf", "brew", "pip", "pip3", "npm", "pnpm",
		"yarn", "cargo":
		switch arg {
		case "search", "list", "show", "info", "view":
			return RiskReadOnly
		}
		return RiskSystem
	case "go", "make", "python3", "python", "node", "sh", "bash", "zsh",
		"gcc", "clang", "source":
		// Build/test/run commands are project-local by default; anything
		// clearly destructive falls to the builtin rules.
		return RiskProjectLocal
	case "ssh", "scp", "rsync":
		return RiskNetwork
	case "git-credential":
		return RiskNetwork
	case "mkdir", "touch", "cp", "mv", "chmod", "chown", "tar", "zip", "unzip":
		return RiskProjectLocal
	case "export", "cd":
		return RiskReadOnly
	case "crontab":
		return RiskSystem
	}
	return RiskUnknown
}
