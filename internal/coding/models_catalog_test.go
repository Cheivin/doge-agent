package coding

import (
	"os"
	"path/filepath"
	"testing"

	"doge/internal/ai"
)

// TestSaveCustomModelsRoundTrip 保存后原样读回；空列表落盘为 []
func TestSaveCustomModelsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	in := []*ai.Model{
		{ID: "glm-custom", Name: "GLM Custom", Provider: "zhipu", API: ai.APIOpenAICompletions,
			BaseURL: "https://example.com/v1", ContextWindow: 131072, MaxTokens: 8192,
			Input: []string{"text", "image"}, Reasoning: true},
	}
	if err := SaveCustomModels(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadCustomModels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != "glm-custom" || out[0].Provider != "zhipu" ||
		out[0].BaseURL != "https://example.com/v1" || out[0].ContextWindow != 131072 ||
		out[0].MaxTokens != 8192 || len(out[0].Input) != 2 || !out[0].Reasoning {
		t.Fatalf("round trip = %+v", out)
	}

	// 删光后写 []
	if err := SaveCustomModels(path, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[]\n" {
		t.Fatalf("empty catalog = %q", string(data))
	}
	if custom, err := LoadCustomModels(path); err != nil || len(custom) != 0 {
		t.Fatalf("load empty = %v, %v", custom, err)
	}
}

// TestMergeModelEdit：编辑保存合并——请求未携带的扩展字段回填旧值，
// 显式传空（headers 置空对象）允许清除；新建（旧条目 nil）原样返回。
func TestMergeModelEdit(t *testing.T) {
	old := &ai.Model{
		ID: "m1", Name: "旧名", API: "openai-completions", Provider: "gw",
		Headers:         map[string]string{"X-Key": "k1"},
		SessionAffinity: "openai",
		Impersonate:     "codex",
		ThinkingLevelMap: map[string]any{"high": "high"},
		Cost:            ai.ModelCost{Input: 1, Output: 2, Currency: "¥"},
	}
	// 常规编辑：基础字段变化，扩展字段保留
	next := &ai.Model{ID: "m1", Name: "新名", API: "openai-completions", Provider: "gw", ContextWindow: 200000}
	got := MergeModelEdit(old, next)
	if got.Headers["X-Key"] != "k1" || got.SessionAffinity != "openai" ||
		got.Impersonate != "codex" || got.ThinkingLevelMap == nil ||
		got.Cost.Input != 1 || got.Cost.Currency != "¥" {
		t.Fatalf("extended fields lost on edit: %+v", got)
	}
	if got.Name != "新名" || got.ContextWindow != 200000 {
		t.Fatalf("basic fields not applied: %+v", got)
	}
	// 显式清空：headers 传空对象表示清除
	clear := &ai.Model{ID: "m1", Name: "n", API: "openai-completions", Provider: "gw", Headers: map[string]string{}}
	if got := MergeModelEdit(old, clear); len(got.Headers) != 0 {
		t.Fatalf("explicit empty headers should clear, got %v", got.Headers)
	}
	// 新建：旧条目 nil 原样返回
	fresh := &ai.Model{ID: "m2", Name: "n", API: "openai-completions", Provider: "gw"}
	if got := MergeModelEdit(nil, fresh); got != fresh {
		t.Fatal("nil old entry must return next as-is")
	}
}
