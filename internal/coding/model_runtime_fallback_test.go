package coding

import (
	"os"
	"path/filepath"
	"testing"

	"doge/internal/ai"
)

// writeFile 落一份文本文件
func writeFile(path, data string) error {
	return os.WriteFile(path, []byte(data), 0o600)
}

// writeModelsFile 落一份 models.json（两个自定义模型，第一个是 fallback 链头）
func writeModelsFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "models.json")
	data := `[
  {"id": "custom-a", "name": "Custom A", "api": "openai-completions", "provider": "mygw", "baseUrl": "https://gw.example.com/v1"},
  {"id": "custom-b", "name": "Custom B", "api": "openai-completions", "provider": "mygw", "baseUrl": "https://gw.example.com/v1"}
]`
	if err := writeFile(path, data); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDefaultModelFallbackChain：默认模型 fallback 链——models.json 第一个
// 自定义模型优先于内置 openai/gpt-5.2；无自定义时回落内置；customFirst 指向
// 的条目被删后继续沿链回落。
func TestDefaultModelFallbackChain(t *testing.T) {
	dir := t.TempDir()
	auth := LoadAuthStore(filepath.Join(dir, "auth.json"))

	// 无自定义：内置 openai/gpt-5.2
	r := NewModelRuntime(auth)
	m, _, err := r.DefaultModel()
	if err != nil || m.Provider != "openai" || m.ID != "gpt-5.2" {
		t.Fatalf("builtin fallback = %v err=%v", m, err)
	}

	// 有自定义：models.json 第一个优先
	r = NewModelRuntime(auth)
	if err := r.AddCustomModels(writeModelsFile(t, dir)); err != nil {
		t.Fatal(err)
	}
	m, _, err = r.DefaultModel()
	if err != nil || m.Provider != "mygw" || m.ID != "custom-a" {
		t.Fatalf("custom-first fallback = %v err=%v", m, err)
	}

	// Reload 后链保持（customFirst 更新）
	if err := r.Reload(writeModelsFile(t, dir)); err != nil {
		t.Fatal(err)
	}
	if m, _, _ := r.DefaultModel(); m.ID != "custom-a" {
		t.Fatalf("after reload = %v", m)
	}
}

// TestResolveDefaultWithFallback：settings 默认指向存在/不存在的模型。
func TestResolveDefaultWithFallback(t *testing.T) {
	dir := t.TempDir()
	auth := LoadAuthStore(filepath.Join(dir, "auth.json"))
	r := NewModelRuntime(auth)
	if err := r.AddCustomModels(writeModelsFile(t, dir)); err != nil {
		t.Fatal(err)
	}

	// settings 默认有效：精确命中
	if m, _ := r.ResolveDefaultWithFallback("mygw", "custom-b"); m == nil || m.ID != "custom-b" {
		t.Fatalf("valid default = %v", m)
	}
	// settings 默认失效（改名/删除后残留）：回退到 customFirst
	if m, _ := r.ResolveDefaultWithFallback("mygw", "gone-model"); m == nil || m.ID != "custom-a" {
		t.Fatalf("stale default fallback = %v, want custom-a", m)
	}
	// 未设置默认：同样走 fallback 链
	if m, _ := r.ResolveDefaultWithFallback("", ""); m == nil || m.ID != "custom-a" {
		t.Fatalf("unset default = %v", m)
	}
}

// TestResolveModelRenameFollow：改名编辑后按新 spec 可解析、旧 spec 不存在。
func TestResolveModelRenameFollow(t *testing.T) {
	dir := t.TempDir()
	auth := LoadAuthStore(filepath.Join(dir, "auth.json"))
	r := NewModelRuntime(auth)
	if err := r.AddCustomModels(writeModelsFile(t, dir)); err != nil {
		t.Fatal(err)
	}
	// 模拟 previous 替换后的目录状态
	r.Models.Reset()
	RegisterBuiltinProviders(r.Models)
	for _, m := range builtinModels {
		r.Models.AddModel(m)
	}
	r.Models.AddModel(&ai.Model{ID: "custom-a-renamed", API: "openai-completions", Provider: "mygw", BaseURL: "https://gw.example.com/v1"})
	if _, _, err := r.ResolveModel("mygw/custom-a-renamed"); err != nil {
		t.Fatalf("new spec: %v", err)
	}
	if _, _, err := r.ResolveModel("mygw/custom-a"); err == nil {
		t.Fatal("old spec should be gone after rename")
	}
}
