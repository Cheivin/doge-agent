package web

import (
	"testing"
)

// TestConfigSetupNode：/api/config 的 setup 节从真实状态推导（无哨兵文件）——
// 可用模型与默认模型两路信号组合出 ready。
func TestConfigSetupNode(t *testing.T) {
	t.Setenv("DOGE_CODING_AGENT_DIR", t.TempDir())
	f := &fakeBackend{usable: false}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")

	m := getJSON(t, client, ts.URL+"/api/config", 200)
	setup, ok := m["setup"].(map[string]any)
	if !ok {
		t.Fatalf("no setup node in /api/config: %v", m)
	}
	if setup["ready"] != false || setup["hasUsableModel"] != false {
		t.Fatalf("setup = %v, want all-false without usable models", setup)
	}
	if _, ok := setup["hasDefaultModel"]; !ok {
		t.Fatalf("setup missing hasDefaultModel: %v", setup)
	}

	f.usable = true
	m = getJSON(t, client, ts.URL+"/api/config", 200)
	setup = m["setup"].(map[string]any)
	if setup["hasUsableModel"] != true {
		t.Fatalf("hasUsableModel = %v, want true", setup["hasUsableModel"])
	}
}

// TestConfigProviders：内置 provider 目录 + 环境变量名导出（引导页数据源）。
func TestConfigProviders(t *testing.T) {
	t.Setenv("DOGE_CODING_AGENT_DIR", t.TempDir())
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")

	m := getJSON(t, client, ts.URL+"/api/config/providers", 200)
	providers, ok := m["providers"].([]any)
	if !ok || len(providers) < 9 {
		t.Fatalf("providers = %v", m)
	}
	byID := map[string]map[string]any{}
	for _, p := range providers {
		pm := p.(map[string]any)
		byID[pm["id"].(string)] = pm
	}
	// EnvKeys 导出：openai 带 OPENAI_API_KEY
	openai := byID["openai"]
	if envs, ok := openai["envKeys"].([]any); !ok || len(envs) == 0 {
		t.Fatalf("openai envKeys = %v", openai)
	}
	// local 免密钥
	if byID["local"]["keyConfigured"] != true {
		t.Fatalf("local keyConfigured = %v", byID["local"])
	}
	// 未配密的远端 provider 不算就绪
	if byID["deepseek"]["keyConfigured"] != false {
		t.Fatalf("deepseek keyConfigured = %v", byID["deepseek"])
	}
}
