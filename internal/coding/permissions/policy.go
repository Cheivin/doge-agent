// Package permissions implements a policy decision layer for tool calls,
// a layered policy model: allow/deny/ask decisions by
// tool name, bash command patterns, and path guards. It is a decision
// layer, not a sandbox.
package permissions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Decision is the outcome of a permission check.
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
	Ask   Decision = "ask"
)

// Policy is the user-facing configuration: default fallbacks plus rules.
type Policy struct {
	// DefaultPolicy sets the fallback for tools and bash when no rule
	// matches. Empty means "allow".
	DefaultPolicy struct {
		Tools string `json:"tools,omitempty"`
		Bash  string `json:"bash,omitempty"`
	} `json:"defaultPolicy,omitempty"`

	// Tools maps tool names (optionally "tool:path/glob") to decisions.
	Tools map[string]string `json:"tools,omitempty"`

	// Bash maps command glob patterns to decisions. Rules are matched in
	// specificity order (longer patterns first), so broad rules act as
	// defaults and specific ones override them.
	Bash []BashRule `json:"bash,omitempty"`

	// Special guards.
	Special struct {
		// ExternalDirectory decides read/write/edit calls whose target is
		// outside the session working directory. Empty means "allow".
		ExternalDirectory string `json:"externalDirectory,omitempty"`
	} `json:"special,omitempty"`

	// AutoDecide configures unattended (headless) permission decisions per
	// risk level: "allow", "deny", or "llm" (LLM approval). Empty means
	// "deny" for everything except read-only, which defaults to "allow".
	AutoDecide struct {
		ReadOnly     string `json:"readOnly,omitempty"`
		ProjectLocal string `json:"projectLocal,omitempty"`
		System       string `json:"system,omitempty"`
		Network      string `json:"network,omitempty"`
		Unknown      string `json:"unknown,omitempty"`
	} `json:"autoDecide,omitempty"`

	// HumanOnly lists command patterns that always require a human
	// decision: in unattended mode they are denied without LLM approval.
	HumanOnly []string `json:"humanOnly,omitempty"`

	// LLMApproval enables LLM-based approval for ask decisions in
	// unattended mode.
	LLMApproval struct {
		Enabled bool `json:"enabled,omitempty"`
		// Model overrides the approval model (provider/model spec);
		// empty uses the session model.
		Model string `json:"model,omitempty"`
	} `json:"llmApproval,omitempty"`

	// UnattendedAllowDirs 运行时字段：无人值守会话（remote/automation fork）
	// 在 externalDirectory=deny 下仍允许访问的绝对目录前缀（如上传暂存区，
	// 远端任务下发的文件落在这里）。不落盘，仅由 UnattendedPolicy 构造
	UnattendedAllowDirs []string `json:"-"`
}

// BashRule is one command pattern rule. Pattern supports * and ? globs
// against the normalized command string.
type BashRule struct {
	Pattern  string `json:"pattern"`
	Decision string `json:"decision"`
	// Tier 规则来源层级：0 全局文件，1 项目文件；项目层不得放宽全局层
	Tier int `json:"-"`
}

// bash 规则来源层级：LoadPolicy 首个文件记为全局，其余记为项目
const (
	tierGlobal  = 0
	tierProject = 1
)

// UnmarshalJSON accepts bash rules as either an array of {pattern,
// decision} objects or a plain object map (object key order is preserved
// for last-match-wins semantics; specificity sorting applies either way).
func (p *Policy) UnmarshalJSON(data []byte) error {
	type alias Policy
	var raw struct {
		DefaultPolicy map[string]string `json:"defaultPolicy"`
		Tools         map[string]string `json:"tools"`
		Bash          json.RawMessage   `json:"bash"`
		Special       map[string]string `json:"special"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.DefaultPolicy != nil {
		p.DefaultPolicy.Tools = raw.DefaultPolicy["tools"]
		p.DefaultPolicy.Bash = raw.DefaultPolicy["bash"]
	}
	p.Tools = raw.Tools
	if raw.Special != nil {
		p.Special.ExternalDirectory = raw.Special["externalDirectory"]
	}
	var extra struct {
		AutoDecide struct {
			ReadOnly     string `json:"readOnly"`
			ProjectLocal string `json:"projectLocal"`
			System       string `json:"system"`
			Network      string `json:"network"`
			Unknown      string `json:"unknown"`
		} `json:"autoDecide"`
		HumanOnly   []string `json:"humanOnly"`
		LLMApproval struct {
			Enabled bool   `json:"enabled"`
			Model   string `json:"model"`
		} `json:"llmApproval"`
	}
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	p.AutoDecide.ReadOnly = extra.AutoDecide.ReadOnly
	p.AutoDecide.ProjectLocal = extra.AutoDecide.ProjectLocal
	p.AutoDecide.System = extra.AutoDecide.System
	p.AutoDecide.Network = extra.AutoDecide.Network
	p.AutoDecide.Unknown = extra.AutoDecide.Unknown
	p.HumanOnly = extra.HumanOnly
	p.LLMApproval.Enabled = extra.LLMApproval.Enabled
	p.LLMApproval.Model = extra.LLMApproval.Model
	if len(raw.Bash) == 0 {
		return nil
	}
	// Array form.
	var arr []BashRule
	if err := json.Unmarshal(raw.Bash, &arr); err == nil && arr != nil {
		p.Bash = arr
		return nil
	}
	// Object form: preserve key order so more specific (later) rules win
	// on ties; the decider sorts by specificity regardless.
	dec := json.NewDecoder(strings.NewReader(string(raw.Bash)))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return errors.New("bash rules must be an array or an object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key := keyTok.(string)
		var val string
		if err := dec.Decode(&val); err != nil {
			return err
		}
		p.Bash = append(p.Bash, BashRule{Pattern: key, Decision: val})
	}
	return nil
}

// LoadPolicy loads the permission policy from the given files, in order.
// The first file is the global tier, every later file a project tier.
// Bash matching picks the longest pattern within each tier and the
// strictest decision across tiers, so a project file can never widen a
// global deny/ask into allow, and only global-tier rules may override the
// built-in soft denies (see applySoftBuiltin).
func LoadPolicy(files ...string) (*Policy, error) {
	p := &Policy{}
	// Built-in hard-deny baseline is enforced by the decider regardless of
	// the loaded policy; nothing to seed here.
	global := true // 首个非空路径占全局层位置，文件缺失也不让位
	for _, f := range files {
		if f == "" {
			continue
		}
		tier := tierProject
		if global {
			tier = tierGlobal
			global = false
		}
		data, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		next := &Policy{}
		if err := json.Unmarshal(data, next); err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		// 按文件位置标注 bash 规则来源层级
		for j := range next.Bash {
			next.Bash[j].Tier = tier
		}
		mergePolicy(p, next)
	}
	return p, nil
}

// mergePolicy overlays a stricter policy onto the base. Project/agent
// files cannot widen a global deny: allow in a later file that conflicts
// with an existing deny is ignored.
func mergePolicy(base, next *Policy) {
	if next.DefaultPolicy.Tools != "" {
		if base.DefaultPolicy.Tools != DenyString() {
			base.DefaultPolicy.Tools = next.DefaultPolicy.Tools
		}
	}
	if next.DefaultPolicy.Bash != "" {
		if base.DefaultPolicy.Bash != DenyString() {
			base.DefaultPolicy.Bash = next.DefaultPolicy.Bash
		}
	}
	for k, v := range next.Tools {
		// A later file may only tighten a tool rule.
		if prev, ok := base.Tools[k]; ok && prev == string(Deny) && v != string(Deny) {
			continue
		}
		if base.Tools == nil {
			base.Tools = map[string]string{}
		}
		base.Tools[k] = v
	}
	// Bash rules: later rules append, keeping their source tier; matching
	// picks the longest pattern within each tier and the strictest decision
	// across tiers (see matchBashRules), so project rules cannot loosen
	// global ones and the hard-deny builtin layer stays unloosenable.
	base.Bash = append(base.Bash, next.Bash...)
	if next.Special.ExternalDirectory != "" {
		if base.Special.ExternalDirectory != string(Deny) {
			base.Special.ExternalDirectory = next.Special.ExternalDirectory
		}
	}
	// Auto-decision levels: later files may only tighten (deny > llm >
	// allow). A project file cannot widen a global "system": "deny" back
	// to "allow" or "llm".
	mergeLevel := func(dst *string, src string) {
		if src == "" {
			return
		}
		if levelRank(*dst) > levelRank(src) {
			return // existing is stricter; keep it
		}
		*dst = src
	}
	mergeLevel(&base.AutoDecide.ReadOnly, next.AutoDecide.ReadOnly)
	mergeLevel(&base.AutoDecide.ProjectLocal, next.AutoDecide.ProjectLocal)
	mergeLevel(&base.AutoDecide.System, next.AutoDecide.System)
	mergeLevel(&base.AutoDecide.Network, next.AutoDecide.Network)
	mergeLevel(&base.AutoDecide.Unknown, next.AutoDecide.Unknown)
	base.HumanOnly = append(base.HumanOnly, next.HumanOnly...)
	if next.LLMApproval.Enabled {
		base.LLMApproval.Enabled = true
	}
	if next.LLMApproval.Model != "" {
		base.LLMApproval.Model = next.LLMApproval.Model
	}
}

// DenyString is a helper for merge comparisons.
func DenyString() string { return string(Deny) }

// levelRank orders autoDecide level strictness: deny > llm > allow.
func levelRank(v string) int {
	switch v {
	case string(Deny):
		return 2
	case "llm":
		return 1
	}
	return 0 // allow or empty
}

// DefaultPaths returns the global and project policy file paths.
func DefaultPaths(agentDir, cwd string) []string {
	return []string{
		filepath.Join(agentDir, "permissions.json"),
		filepath.Join(cwd, ".doge", "permissions.json"),
	}
}

// UnattendedPolicy derives the policy for unattended sessions (remote mesh
// task forks, automation runs): the external-directory guard is forced to
// deny so a task cannot read ~/.doge/agent (auth.json, mesh-key) or rewrite
// permissions.json/automations.json to escalate, with explicit exceptions for
// the given absolute directory prefixes (the uploads staging dir — remote
// tasks receive files there). The base policy is deep-copied; the caller's
// policy is never mutated.
func UnattendedPolicy(base *Policy, allowExternalDirs ...string) *Policy {
	if base == nil {
		base = &Policy{}
	}
	cp := *base
	cp.Special.ExternalDirectory = string(Deny)
	dirs := make([]string, 0, len(allowExternalDirs))
	for _, d := range allowExternalDirs {
		if d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		dirs = append(dirs, abs)
	}
	cp.UnattendedAllowDirs = dirs
	return &cp
}
