package permissions

// builtinRules are high-risk command patterns that are denied by default.
// They are enforced by the decider before user rules and cannot be
// overridden to "allow" by configuration: a project policy must never be
// able to re-enable destructive commands.
var builtinRules = []struct {
	pattern string
	// hard denies system-destructive commands; soft rules deny by default
	// but may be explicitly overridden to "allow" by the user's own rules.
	hard bool
}{
	// Recursive deletes are handled semantically by destructiveRm (which
	// covers flag/path variants) before pattern rules run; only the
	// no-preserve-root form is kept here as a hard pattern.
	{pattern: "rm -rf --no-preserve-root /", hard: true},
	{pattern: "rm -fr --no-preserve-root /", hard: true},
	// Filesystem/device destruction.
	{pattern: "mkfs*", hard: true},
	{pattern: "mkfs.*", hard: true},
	{pattern: "dd if=* of=/dev/*", hard: true},
	{pattern: "dd if=/dev/zero of=/dev/*", hard: true},
	{pattern: "> /dev/sd*", hard: true},
	{pattern: "echo * > /dev/sd*", hard: true},
	{pattern: "fdisk *", hard: true},
	{pattern: "parted *", hard: true},
	// System state.
	{pattern: "shutdown*", hard: true},
	{pattern: "reboot*", hard: true},
	{pattern: "halt*", hard: true},
	{pattern: "poweroff*", hard: true},
	{pattern: "init 0", hard: true},
	{pattern: "init 6", hard: true},
	// Fork bomb.
	{pattern: ":(){ :|:& };:", hard: true},
	// chmod on root.
	{pattern: "chmod -R 777 /", hard: true},
	{pattern: "chmod 777 /", hard: true},
	// Piping remote content into a shell is destructive by default; the
	// user may explicitly allow specific instances.
	{pattern: "curl * | sh", hard: false},
	{pattern: "curl * | bash", hard: false},
	{pattern: "wget * | sh", hard: false},
	{pattern: "wget * | bash", hard: false},
	// Environment credentials.
	{pattern: "export *TOKEN*=*", hard: false},
	{pattern: "export *KEY*=*", hard: false},
	{pattern: "printenv *KEY*", hard: false},
	{pattern: "cat ~/.ssh/*", hard: false},
	{pattern: "cat */auth.json", hard: false},
	{pattern: "cat */.doge/agent/auth.json", hard: false},
}
