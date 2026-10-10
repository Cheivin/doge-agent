package permissions

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"doge/pkg/logx"
)

// CheckContext describes the tool call being checked.
type CheckContext struct {
	ToolName string
	// Args are the validated tool arguments.
	Args map[string]any
	// Cwd is the session working directory (for path guards).
	Cwd string
}

// AskRequest describes a permission prompt for the user.
type AskRequest struct {
	Kind     string `json:"kind"`     // "tool" or "bash"
	ToolName string `json:"toolName"` // tool name (for "tool" and "bash" kinds)
	Command  string `json:"command"`  // normalized command (kind "bash")
	Path     string `json:"path"`     // target path (path guard)
	Reason   string `json:"reason"`   // human-readable rule description
	Cwd      string `json:"cwd"`      // session working directory (context for approvers)
}

// AskResponse is the user's answer to an AskRequest.
type AskResponse struct {
	AllowOnce   bool `json:"allowOnce"`   // allow this single call
	AllowAlways bool `json:"allowAlways"` // allow for the rest of the session
	Deny        bool `json:"deny"`
}

// AskHandler resolves "ask" decisions. The interactive TUI implements it
// with a confirmation prompt; headless modes may deny unconditionally.
type AskHandler interface {
	Ask(req AskRequest) (AskResponse, error)
}

// Approver evaluates a command's risk in unattended mode. It is typically
// implemented by an LLM-based gate; nil means no approval available.
type Approver interface {
	// Approve returns whether the command may run and a reason.
	Approve(req AskRequest) (bool, string, error)
}

// Decider evaluates tool calls against a policy. Decisions are cached for
// "allow always" answers within a session.
type Decider struct {
	policyPtr atomic.Pointer[Policy]
	cwd       string

	mu            sync.RWMutex
	ask           AskHandler
	approver      Approver
	allowedAlways map[string]bool
}

// NewDecider builds a decider. ask may be nil: ask decisions then default
// to deny. SetAskHandler can install a handler later (e.g. once the TUI
// is running).
func NewDecider(policy *Policy, cwd string, ask AskHandler) *Decider {
	if policy == nil {
		policy = &Policy{}
	}
	d := &Decider{cwd: cwd, ask: ask, allowedAlways: map[string]bool{}}
	d.policyPtr.Store(policy)
	return d
}

// SetPolicy swaps the policy in place (e.g. after reloading permission.json).
// In-flight checks finish against the old policy; new checks use the new one.
func (d *Decider) SetPolicy(policy *Policy) {
	if policy == nil {
		policy = &Policy{}
	}
	d.policyPtr.Store(policy)
}

// policy returns the current policy.
func (d *Decider) policy() *Policy {
	return d.policyPtr.Load()
}

// SetAskHandler installs or replaces the ask handler (thread-safe).
func (d *Decider) SetAskHandler(ask AskHandler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ask = ask
}

// SetCwd rebinds the working directory used by the path guard (e.g. after a
// session switch to a different working directory).
func (d *Decider) SetCwd(cwd string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cwd = cwd
}

// SetAskHandler installs or replaces the interactive ask handler; a nil
// handler keeps decisions unattended (risk-classified autoDecide).

// AskHandlerInstalled reports whether an interactive ask handler is set —
// unattended sessions (e.g. remote-task forks) must have none, or their asks
// would block on a human who is only observing.
func (d *Decider) AskHandlerInstalled() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ask != nil
}

// SetApprover installs or replaces the unattended LLM approver.
func (d *Decider) SetApprover(a Approver) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.approver = a
}

// Decide returns the decision for a tool call plus a human-readable reason.
func (d *Decider) Decide(ctx CheckContext) (Decision, string) {
	// Bash commands are matched against the bash rule set (and the
	// built-in high-risk library).
	if ctx.ToolName == "bash" {
		return d.decideBash(commandArg(ctx.Args))
	}

	// Path guard: read/write/edit-family tools targeting outside cwd.
	if d := d.decidePathGuard(ctx); d != Allow {
		return d, "target outside working directory"
	}

	// Tool rules by name (path-scoped rules match the target path). Only a
	// matched rule decides here; an unmatched pass-through falls to the
	// default-policy fallback below.
	if d, reason, matched := d.decideTool(ctx.ToolName, pathArg(ctx.Args)); matched {
		return d, reason
	}

	// No rule hit: the tools fallback from the default policy decides
	// (empty means allow, so policy files without the setting are
	// unaffected).
	return defaultDecision(d.policy().DefaultPolicy.Tools), ""
}

// decideBash evaluates a normalized shell command.
func (d *Decider) decideBash(cmd string) (Decision, string) {
	cmd = normalizeCommand(cmd)
	if cmd == "" {
		return Deny, "empty command"
	}

	// Whole-command builtins first: `curl * | sh` is dangerous as a unit
	// even though its two sides are individually safe.
	for _, r := range builtinRules {
		if MatchPattern(r.pattern, cmd) {
			if r.hard {
				return Deny, "high-risk command: " + r.pattern
			}
			// Soft builtins may be overridden by an explicit user rule on
			// the whole command (checked next).
			return d.applySoftBuiltin(cmd, r.pattern)
		}
	}

	// Semantic rm detection on the whole command: a hard deny that no
	// user rule can override.
	if destructiveRm(cmd) {
		return Deny, "high-risk command: destructive recursive delete"
	}

	// Whole-command user rules keep their pattern semantics.
	if dd, rule := matchBashRules(d.policy().Bash, cmd); dd != "" {
		return dd, "bash rule: " + rule
	}

	// Split into logical segments so `ls ; rm -rf /` and `a && b | c`
	// cannot smuggle a high-risk command past the rules: every segment is
	// evaluated independently and the strictest outcome wins.
	segments := splitSegments(cmd)
	if len(segments) > 1 {
		strictest := Allow
		reason := ""
		for _, seg := range segments {
			d, why := d.evaluateSegment(seg, 0)
			if moreRestrictive(d, strictest) {
				strictest = d
				reason = why
			}
			if strictest == Deny {
				return Deny, reason
			}
		}
		return strictest, reason
	}
	return d.evaluateSegment(cmd, 0)
}

// applySoftBuiltin denies a whole-command soft builtin match unless an
// explicit user rule allows the command.
func (d *Decider) applySoftBuiltin(cmd, pattern string) (Decision, string) {
	if dd, rule := matchBashRules(d.policy().Bash, cmd); dd != "" {
		return dd, "bash rule: " + rule
	}
	return Deny, "high-risk command: " + pattern
}

// evaluateSegment applies the rule chain to one command segment, unwrapping
// shell wrappers (sh -c, eval, …) recursively up to a depth limit.
func (d *Decider) evaluateSegment(cmd string, depth int) (Decision, string) {
	if depth >= 4 {
		return Deny, "shell wrapper nesting too deep"
	}
	// Semantic rm detection: denies destructive recursive deletes
	// regardless of flag/path spelling, for every segment.
	if destructiveRm(cmd) {
		return Deny, "high-risk command: destructive recursive delete"
	}
	// Unwrap shell wrappers so sh -c "rm -rf /" is checked as rm -rf /.
	if inner, ok := unwrapShell(cmd); ok {
		return d.evaluateSegment(normalizeCommand(inner), depth+1)
	}

	// Hard-deny builtins can never be overridden.
	for _, r := range builtinRules {
		if r.hard && MatchPattern(r.pattern, cmd) {
			return Deny, "high-risk command: " + r.pattern
		}
	}

	// User rules, most specific first. A match wins regardless of the
	// built-in soft rules, so the user can explicitly allow e.g. a
	// specific curl|sh command.
	if dd, rule := matchBashRules(d.policy().Bash, cmd); dd != "" {
		return dd, "bash rule: " + rule
	}

	// Soft builtins deny by default (segment level).
	for _, r := range builtinRules {
		if !r.hard && MatchPattern(r.pattern, cmd) {
			return Deny, "high-risk command: " + r.pattern
		}
	}

	// Home-subdirectory recursive deletes (rm -rf ~/x) are destructive but
	// user-scoped: ask for confirmation rather than silently denying. When
	// no one is around, autoDecide falls back to denying them.
	if destructiveRmAsk(cmd) {
		return Ask, "recursive delete in home directory"
	}

	return defaultDecision(d.policy().DefaultPolicy.Bash), ""
}

// moreRestrictive orders decisions: deny is strictest, then ask, then
// allow.
func moreRestrictive(a, b Decision) bool {
	rank := func(d Decision) int {
		switch d {
		case Deny:
			return 2
		case Ask:
			return 1
		}
		return 0
	}
	return rank(a) > rank(b)
}

// matchBashRules finds the most specific matching rule. Rules are sorted
// by pattern length (longer = more specific); ties keep configuration
// order (stable sort).
func matchBashRules(rules []BashRule, cmd string) (Decision, string) {
	ordered := make([]BashRule, len(rules))
	copy(ordered, rules)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len(ordered[i].Pattern) > len(ordered[j].Pattern)
	})
	for _, r := range ordered {
		if MatchPattern(r.Pattern, cmd) {
			return Decision(r.Decision), r.Pattern
		}
	}
	return "", ""
}

// decideTool checks tool-name rules. Path-scoped tool rules
// ("write:/home/alice/project/*") match the tool's target path, not the
// session cwd. The bool result tells a matched rule from a pass-through,
// so the caller can fall back to the default policy only on the latter.
func (d *Decider) decideTool(toolName string, targetPath string) (Decision, string, bool) {
	if d.policy().Tools == nil {
		return Allow, "", false
	}
	if v, ok := d.policy().Tools[toolName]; ok {
		return Decision(v), "tool rule: " + toolName, true
	}
	// Deterministic ordering: keys sorted so identical matches are stable.
	keys := make([]string, 0, len(d.policy().Tools))
	for k := range d.policy().Tools {
		if idx := strings.IndexByte(k, ':'); idx > 0 && k[:idx] == toolName {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		idx := strings.IndexByte(k, ':')
		if targetPath != "" && MatchPattern(k[idx+1:], targetPath) {
			return Decision(d.policy().Tools[k]), "tool rule: " + k, true
		}
	}
	return Allow, "", false
}

// decidePathGuard applies the external-directory guard for path tools.
func (d *Decider) decidePathGuard(ctx CheckContext) Decision {
	if d.policy().Special.ExternalDirectory == "" {
		return Allow
	}
	if !pathTool(ctx.ToolName) {
		return Allow
	}
	path := pathArg(ctx.Args)
	if path == "" {
		return Allow
	}
	abs, err := resolvePath(d.cwd, path)
	if err != nil {
		return Deny
	}
	if withinDir(d.cwd, abs) {
		return Allow
	}
	return Decision(d.policy().Special.ExternalDirectory)
}

// pathTool reports whether the tool takes a file path argument.
func pathTool(toolName string) bool {
	switch toolName {
	case "read", "write", "edit", "grep", "find", "ls":
		return true
	}
	return false
}

// pathArg extracts the primary path argument. "pattern" is excluded: it
// is a regex for grep, not a filesystem path.
func pathArg(args map[string]any) string {
	for _, key := range []string{"path", "dir"} {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// resolvePath makes target absolute relative to cwd and cleans it, so
// "../.." traversal and "src/x.go" are judged against the real location.
func resolvePath(cwd, target string) (string, error) {
	if !filepath.IsAbs(target) {
		target = filepath.Join(cwd, target)
	}
	return filepath.Abs(target)
}

// withinDir reports whether target is inside dir. Both are absolute and
// cleaned by callers, so "../" traversal cannot pass the prefix check.
func withinDir(dir, target string) bool {
	dir = strings.TrimSuffix(dir, "/")
	target = strings.TrimSuffix(target, "/")
	if target == dir {
		return true
	}
	return strings.HasPrefix(target, dir+"/")
}

// commandArg extracts the bash command from tool arguments.
func commandArg(args map[string]any) string {
	if v, ok := args["command"].(string); ok {
		return v
	}
	return ""
}

// defaultDecision resolves an empty default to allow.
func defaultDecision(v string) Decision {
	if v == "" || v == string(Allow) {
		return Allow
	}
	return Decision(v)
}

// resolveAsk handles an "ask" decision via the AskHandler. Without a
// handler (headless) the call is denied.
func (d *Decider) resolveAsk(req AskRequest) (Decision, string) {
	d.mu.RLock()
	ask := d.ask
	d.mu.RUnlock()
	if ask == nil {
		// Unattended mode: risk-classified automatic decision (with LLM
		// approval for high-risk commands when configured).
		allowed, reason := d.autoDecide(req)
		if allowed {
			logx.Infof("[perm] ask %s auto-decided ALLOW (unattended)", askSubject(req))
		} else {
			logx.Infof("[perm] ask %s auto-decided DENY (unattended): %s", askSubject(req), reason)
		}
		if allowed {
			return Allow, ""
		}
		return Deny, reason
	}
	logx.Infof("[perm] ask %s -> waiting for human answer", askSubject(req))
	resp, err := ask.Ask(req)
	if err != nil {
		logx.Warnf("[perm] ask %s prompt FAILED: %v", askSubject(req), err)
		return Deny, "permission prompt failed: " + err.Error()
	}
	if resp.Deny {
		logx.Infof("[perm] ask %s denied by user", askSubject(req))
		return Deny, "denied by user"
	}
	if resp.AllowAlways {
		d.mu.Lock()
		d.allowedAlways[req.Reason] = true
		d.mu.Unlock()
	}
	logx.Infof("[perm] ask %s allowed by user (always=%v)", askSubject(req), resp.AllowAlways)
	return Allow, ""
}

// askSubject renders the checked command/tool for one log line, bounded so a
// huge command cannot flood the log.
func askSubject(req AskRequest) string {
	subject := req.ToolName
	switch {
	case req.Command != "":
		subject += " " + req.Command
	case req.Path != "":
		subject += " " + req.Path
	}
	if len(subject) > 120 {
		subject = subject[:120] + "…"
	}
	return subject
}

// Check evaluates a tool call and resolves ask decisions to a final
// allow/deny, returning the block reason for denials.
func (d *Decider) Check(ctx CheckContext) (allowed bool, reason string) {
	decision, why := d.Decide(ctx)
	switch decision {
	case Allow:
		logx.Debugf("[perm] allow %s %s args=%s", ctx.ToolName, askSubject(AskRequest{ToolName: ctx.ToolName, Command: commandArg(ctx.Args), Path: pathArg(ctx.Args)}), shortArgs(ctx.Args))
		return true, ""
	case Deny:
		logx.Warnf("[perm] deny %s %s: %s", ctx.ToolName, askSubject(AskRequest{ToolName: ctx.ToolName, Command: commandArg(ctx.Args), Path: pathArg(ctx.Args)}), why)
		return false, why
	case Ask:
		return d.resolveAskResult(ctx, why)
	}
	return false, "unknown decision"
}

func (d *Decider) resolveAskResult(ctx CheckContext, why string) (bool, string) {
	// allow-always cache.
	d.mu.RLock()
	always := d.allowedAlways[why]
	d.mu.RUnlock()
	if always {
		return true, ""
	}
	req := AskRequest{Reason: why, ToolName: ctx.ToolName, Cwd: ctx.Cwd}
	if ctx.ToolName == "bash" {
		req.Kind = "bash"
		req.Command = normalizeCommand(commandArg(ctx.Args))
	} else {
		req.Kind = "tool"
		req.Path = pathArg(ctx.Args)
	}
	decision, reason := d.resolveAsk(req)
	if decision == Allow {
		return true, ""
	}
	return false, reason
}

// autoDecide handles an ask decision when no human is available
// (headless mode): risk-classified automatic decisions, with optional LLM
// approval for high-risk commands. Extremely high-risk commands
// (built-in hard denies and humanOnly patterns) never reach this path.
func (d *Decider) autoDecide(req AskRequest) (bool, string) {
	if req.Kind != "bash" {
		// Tool-level ask: only the external-directory guard produces asks
		// here; default to deny in unattended mode unless the policy says
		// otherwise.
		if d.policy().Special.ExternalDirectory == "allow" {
			return true, ""
		}
		return false, "permission denied (unattended mode)"
	}
	cmd := req.Command
	level := classifyRisk(cmd)

	// humanOnly patterns always require a human; denied unattended.
	for _, pattern := range d.policy().HumanOnly {
		if MatchPattern(pattern, cmd) {
			return false, "requires human approval: " + pattern
		}
	}

	switch level {
	case RiskReadOnly:
		return autoDecideLevel(d.policy().AutoDecide.ReadOnly, true), ""
	case RiskProjectLocal:
		return autoDecideLevel(d.policy().AutoDecide.ProjectLocal, false), ""
	case RiskSystem, RiskNetwork, RiskUnknown:
		// The system/network/unknown autoDecide values are honored here
		// too: "allow"/"deny" decide directly, anything else ("", "llm")
		// falls through to LLM approval when enabled (denied otherwise).
		switch cfg := autoDecideForLevel(d.policy(), level); cfg {
		case "allow":
			return true, ""
		case "deny":
			return false, "high-risk command denied by autoDecide policy (" + riskLevelName(level) + ")"
		}
		return d.approveHighRisk(req, level)
	}
	return false, "permission denied (unattended mode)"
}

// autoDecideForLevel returns the configured autoDecide value for a risk
// level.
func autoDecideForLevel(p *Policy, level RiskLevel) string {
	switch level {
	case RiskSystem:
		return p.AutoDecide.System
	case RiskNetwork:
		return p.AutoDecide.Network
	default: // RiskUnknown and anything else
		return p.AutoDecide.Unknown
	}
}

// riskLevelName returns the config key name for a risk level.
func riskLevelName(level RiskLevel) string {
	switch level {
	case RiskSystem:
		return "system"
	case RiskNetwork:
		return "network"
	case RiskUnknown:
		return "unknown"
	}
	return "unknown"
}

// autoDecideLevel resolves a configured level decision; readOnly defaults
// to allow, everything else to deny.
func autoDecideLevel(cfg string, readOnlyDefault bool) bool {
	switch cfg {
	case "allow":
		return true
	case "deny":
		return false
	case "llm":
		return true // handled before this helper for high-risk levels
	}
	return readOnlyDefault
}

// approveHighRisk sends system/network/unknown commands to the LLM
// approver when enabled; otherwise denies.
func (d *Decider) approveHighRisk(req AskRequest, level RiskLevel) (bool, string) {
	if !d.policy().LLMApproval.Enabled {
		return false, "permission denied (unattended; enable llmApproval for high-risk commands)"
	}
	d.mu.RLock()
	approver := d.approver
	d.mu.RUnlock()
	if approver == nil {
		return false, "LLM approval enabled but no approver configured"
	}
	req.Reason = "high-risk command (" + level.String() + ")"
	allowed, reason, err := approver.Approve(req)
	if err != nil {
		return false, "LLM approval failed: " + err.Error()
	}
	if !allowed {
		return false, "LLM approval denied: " + reason
	}
	return true, ""
}

// shortArgs bounds a permission log's argument dump.
func shortArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	data, _ := json.Marshal(args)
	if len(data) > 160 {
		return string(data[:160]) + "…"
	}
	return string(data)
}
