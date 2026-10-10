// Package guide 内置 doge 使用指南：全文随二进制分发（go:embed），并以
// 内嵌 agent 工具（guide）暴露——先给模块列表（渐进式披露），模型按模块名
// 取对应用法，避免一次性灌入全文。独立部署（只有二进制、无仓库文档）时，
// agent 依然能回答任何 doge 用法/配置问题。
package guide

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"doge/internal/agent"
	"doge/internal/ai"
)

//go:embed guide.md
var guideMD string

type (
	// Module 指南的一个可查询单元：### 小节（或无小节的 ## 大节），
	// 由标题下方的 <!-- module: key | 说明 --> 标记声明稳定 key 与一行说明
	Module struct {
		Key         string
		Group       string // 所属 ## 大节标题（如「4. 配置文件详解」）
		Title       string // 小节标题原文（含编号，如「4.2 models.json —— 自定义模型目录」）
		Description string // 一行说明（marker 内声明，供模块列表展示）
		Content     string // 从标题行到下一同级/上级标题前的正文
	}
)

// parseModules 把 guide.md 按 ## / ### 标题切成模块。模块 key 与说明来自标题行
// 下一行的 <!-- module: key | 说明 --> 注释（markdown 渲染不可见）；没带标记的
// 小节归入上一个带标记的模块。
func parseModules(doc string) []Module {
	var mods []Module
	group := ""
	var cur *Module
	flush := func() {
		if cur != nil {
			mods = append(mods, *cur)
			cur = nil
		}
	}
	lines := strings.Split(doc, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "## "):
			flush()
			group = strings.TrimSpace(line[3:])
			// 无小节的大节（如「1. 构建与版本」）自身是一个模块
			if key, desc := markerAfter(lines, i); key != "" {
				cur = &Module{Key: key, Group: group, Title: group, Description: desc}
			}
		case strings.HasPrefix(line, "### "):
			key, desc := markerAfter(lines, i)
			if key == "" {
				// 无标记小节：不重开模块，正文（含该标题行）归入上一个模块
				break
			}
			flush()
			cur = &Module{Key: key, Group: group, Title: strings.TrimSpace(line[4:]), Description: desc}
		}
		if cur != nil && !strings.HasPrefix(strings.TrimSpace(line), "<!-- module:") {
			cur.Content += line + "\n"
		}
	}
	flush()
	return mods
}

// markerAfter 返回 heading 行之后紧邻的 <!-- module: key | 说明 --> 标记内容；
// 说明可省略（旧式 `<!-- module: key -->` 仍被接受）
func markerAfter(lines []string, heading int) (key, desc string) {
	for j := heading + 1; j < len(lines) && j <= heading+2; j++ {
		l := strings.TrimSpace(lines[j])
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "<!-- module:") && strings.HasSuffix(l, "-->") {
			payload := strings.TrimSpace(l[len("<!-- module:") : len(l)-3])
			if idx := strings.Index(payload, "|"); idx >= 0 {
				return strings.TrimSpace(payload[:idx]), strings.TrimSpace(payload[idx+1:])
			}
			return payload, ""
		}
		return "", ""
	}
	return "", ""
}

var modules = parseModules(guideMD)

// Modules 返回全部模块（按文档出现顺序）

// Lookup 按 key 取模块
func Lookup(key string) (Module, bool) {
	for _, m := range modules {
		if m.Key == key {
			return m, true
		}
	}
	return Module{}, false
}

// Content 返回指南全文（markdown，源文件 internal/guide/guide.md）

// Tool 构造内嵌 guide 工具：不带参数返回模块列表（分组 + 一行说明），
// module=<key> 返回该模块正文。指南恒可用，模型回答 doge 自身用法问题时
// 应先查这里而不是凭记忆猜测。
func Tool() *agent.AgentTool {
	list := renderModuleList(modules)
	keys := make([]string, 0, len(modules))
	for _, m := range modules {
		keys = append(keys, m.Key)
	}
	return &agent.AgentTool{
		Name:        "guide",
		Label:       "doge usage guide",
		Description: "Query doge's BUILT-IN usage guide (embedded in this binary — always available, matches this exact version). Call it WITHOUT module to get the module list, then module=<name> to load one module's content. Covers: run modes and CLI flags, config files field by field (models.json, auth.json, permission.json, settings.json, mcp.json, mesh.json), config directories and merge rules, env vars, TUI slash commands, skills/prompts, MCP, mesh networking and remote agent tools. Use it whenever the user asks anything about doge itself — how to use a feature, where a config lives, what a flag does — instead of guessing.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"module": map[string]any{
					"type":        "string",
					"description": "module key from the list (empty = list modules)",
				},
			},
			"additionalProperties": false,
		},
		Execute: func(_ string, params map[string]any, _ context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			key, _ := params["module"].(string)
			key = strings.TrimSpace(key)
			if key == "" {
				return &agent.AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(list)}}, nil
			}
			m, ok := Lookup(key)
			if !ok {
				return &agent.AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(
					"guide: unknown module \"" + key + "\". Available: " + strings.Join(keys, ", ") + " (call guide without module for the annotated list)")}}, nil
			}
			return &agent.AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(m.Content)}}, nil
		},
	}
}

// renderModuleList 按大节分组渲染模块表：key + 一行说明 + 行数提示
// （模型据说明选模块、按行数估读取代价）
func renderModuleList(mods []Module) string {
	var b strings.Builder
	b.WriteString("doge 内置使用指南模块（guide(module=<key>) 取正文；本表即目录）:\n")
	group := ""
	for _, m := range mods {
		if m.Group != group {
			group = m.Group
			b.WriteString("\n[" + group + "]\n")
		}
		desc := m.Description
		if desc == "" {
			desc = m.Title
		}
		fmt.Fprintf(&b, "  %-18s %s（约 %d 行）\n", m.Key, desc, strings.Count(m.Content, "\n"))
	}
	b.WriteString("\n用法、配置、功能问题优先查对应模块；模块 key 稳定，可跨会话引用。\n")
	return b.String()
}
