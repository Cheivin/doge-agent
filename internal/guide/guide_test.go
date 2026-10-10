package guide

import (
	"context"
	"strings"
	"testing"

	"doge/internal/agent"
)

func TestModulesParsed(t *testing.T) {
	mods := modules
	if len(mods) < 15 {
		t.Fatalf("modules = %d, want the guide split into a queryable module list", len(mods))
	}
	seen := map[string]bool{}
	for _, m := range mods {
		if m.Key == "" || m.Title == "" || m.Group == "" {
			t.Fatalf("module missing fields: %+v", m)
		}
		// 每个模块必须带一行说明：模块列表靠它让模型不取正文就能选对模块
		if m.Description == "" {
			t.Fatalf("module %q has no description (add \"<!-- module: %s | 说明 -->\" under its heading)", m.Key, m.Key)
		}
		if seen[m.Key] {
			t.Fatalf("duplicate module key %q", m.Key)
		}
		seen[m.Key] = true
		if strings.Count(m.Content, "\n") < 3 {
			t.Fatalf("module %q suspiciously short:\n%s", m.Key, m.Content)
		}
		// 模块正文从标题行开始，且不应吞并下一个模块的内容
		if !strings.Contains(m.Content, m.Title) {
			t.Fatalf("module %q content misses its heading", m.Key)
		}
		// 说明写在 marker 里，不应泄漏进正文
		if strings.Contains(m.Content, "<!-- module:") {
			t.Fatalf("module %q content leaks the marker line", m.Key)
		}
	}
	// 关键模块必须在：模型、权限、mesh、TUI
	for _, key := range []string{"models-json", "permissions-json", "mesh-json", "tui"} {
		if _, ok := Lookup(key); !ok {
			t.Fatalf("module %q missing", key)
		}
	}
}

func TestGuideToolListAndGet(t *testing.T) {
	tool := Tool()
	if tool.Name != "guide" {
		t.Fatalf("name = %q", tool.Name)
	}

	// 不带参数：返回模块列表（目录 + 每模块一行说明）
	res, err := tool.Execute("call-1", map[string]any{}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	list := text(t, res)
	for _, want := range []string{"models-json", "mesh-json", "模块", "自定义模型定义"} {
		if !strings.Contains(list, want) {
			t.Fatalf("list output missing %q:\n%s", want, list)
		}
	}

	// module=<key>：返回该模块正文
	res, err = tool.Execute("call-2", map[string]any{"module": "models-json"}, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := text(t, res)
	if !strings.Contains(body, "models.json") || strings.Count(body, "\n") < 20 {
		t.Fatalf("module content too short:\n%s", body[:min(200, len(body))])
	}

	// 未知模块：报错并列出可用 key
	res, _ = tool.Execute("call-3", map[string]any{"module": "nope"}, context.Background(), nil)
	if out := text(t, res); !strings.Contains(out, "unknown module") || !strings.Contains(out, "models-json") {
		t.Fatalf("unknown module output = %q", out)
	}
}

func text(t *testing.T, res *agent.AgentToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty tool result")
	}
	var b strings.Builder
	for _, c := range res.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}
