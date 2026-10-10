package permissions

import (
	"path/filepath"
	"strings"
)

// rm target categories, strictest first.
const (
	rmAllow    int = iota
	rmAsk          // home subdirectory: destructive but user-scoped
	rmHardDeny     // filesystem root, whole home, top-level system dir
)

// parseRmTargets extracts the target paths and recursive/force semantics of
// a rm (or sudo rm) command, however the flags are written (rm -r -f /,
// rm --recursive --force ~, rm -rf "/", rm -fr /etc).
func parseRmTargets(cmd string) (targets []string, recursive, force bool) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return nil, false, false
	}
	if fields[0] == "sudo" {
		fields = fields[1:]
	}
	if len(fields) == 0 || fields[0] != "rm" {
		return nil, false, false
	}
	for _, f := range fields[1:] {
		switch {
		case f == "-r" || f == "-R" || f == "--recursive":
			recursive = true
		case f == "-f" || f == "--force":
			force = true
		case strings.HasPrefix(f, "--recursive"):
			recursive = true
		case strings.HasPrefix(f, "--force"):
			force = true
		case strings.HasPrefix(f, "-") && f != "-" && !strings.HasPrefix(f, "--"):
			// Combined short flags: -rf, -fr, -fR ... parse each character.
			for _, ch := range f[1:] {
				switch ch {
				case 'r', 'R':
					recursive = true
				case 'f':
					force = true
				}
			}
		case f == "--":
			// everything after -- is a target
		case strings.HasPrefix(f, "-") && f != "-":
			// other flags: ignore
		default:
			targets = append(targets, f)
		}
	}
	return targets, recursive, force
}

// rmTargetCategory classifies a single rm target:
//   - rmHardDeny: the filesystem root, the whole home directory, or a
//     top-level system directory (rm -rf /, rm -rf /var, rm -rf ~).
//   - rmAsk: a specific home subdirectory (rm -rf ~/proj, rm -rf ~/.config).
//   - rmAllow: a deeper absolute path (rm -rf /tmp/proj, rm -rf /root/data/x).
func rmTargetCategory(t string) int {
	t = stripOuterQuotes(t)
	cleaned := filepath.Clean(t)
	switch {
	case t == "~" || cleaned == "~":
		// The whole home directory (including "~/", "~/.", "~/x/..").
		return rmHardDeny
	case strings.HasPrefix(t, "~/"):
		// A home-relative target: if it resolves back inside home it's a
		// user-scoped subdirectory (ask); traversal like "~/.." that leaves
		// home is treated as escaping and hard-denied.
		if strings.HasPrefix(cleaned, "~/") {
			return rmAsk
		}
		return rmHardDeny
	}
	if !strings.HasPrefix(cleaned, "/") {
		return rmAllow
	}
	// Root and top-level directories (/tmp, /var, /etc, …) are destructive
	// to remove recursively with force. Deeper paths are project-local.
	if cleaned == "/" || !strings.Contains(strings.Trim(cleaned, "/"), "/") {
		return rmHardDeny
	}
	return rmAllow
}

func stripOuterQuotes(t string) string {
	if len(t) >= 2 && ((t[0] == '\'' && t[len(t)-1] == '\'') ||
		(t[0] == '"' && t[len(t)-1] == '"')) {
		return t[1 : len(t)-1]
	}
	return t
}

// destructiveRm reports whether a rm -r -f command targets the filesystem
// root, the whole home directory, or a top-level system directory — a hard
// deny that no configuration can override, regardless of flag/path spelling.
func destructiveRm(cmd string) bool {
	targets, recursive, force := parseRmTargets(cmd)
	if !recursive || !force {
		return false
	}
	for _, t := range targets {
		if rmTargetCategory(t) == rmHardDeny {
			return true
		}
	}
	return false
}

// destructiveRmAsk reports whether a rm -r -f command targets a specific
// home subdirectory (e.g. rm -rf ~/proj): destructive but user-scoped, so
// the user is asked to confirm instead of being silently denied. Hard-deny
// targets are excluded (they are handled by destructiveRm).
func destructiveRmAsk(cmd string) bool {
	targets, recursive, force := parseRmTargets(cmd)
	if !recursive || !force {
		return false
	}
	anyAsk := false
	for _, t := range targets {
		switch rmTargetCategory(t) {
		case rmHardDeny:
			return false // handled separately as a hard deny
		case rmAsk:
			anyAsk = true
		}
	}
	return anyAsk
}
