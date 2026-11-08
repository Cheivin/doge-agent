package web

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"doge/internal/ai"
	"doge/internal/coding"
)

// newReloadTestSession 构建真实 AgentSession（mock provider 免密本地端点），
// 供模型配置热更新链路回归用
func newReloadTestSession(t *testing.T, dir string) *coding.AgentSession {
	t.Helper()
	manager := coding.NewSessionManager(filepath.Join(dir, "sessions"))
	auth := coding.LoadAuthStore(filepath.Join(dir, "auth.json"))
	runtime := coding.NewModelRuntime(auth)
	runtime.Models.AddProvider(&ai.Provider{ID: "mock", Name: "Mock", BaseURL: "http://127.0.0.1:9", RequiresKey: false})
	runtime.Models.AddModel(&ai.Model{
		ID: "m1", API: ai.APIOpenAICompletions, Provider: "mock", BaseURL: "http://127.0.0.1:9",
		ContextWindow: 123456, MaxTokens: 4096,
	})
	model, _, err := runtime.ResolveModel("mock/m1")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := coding.LoadSettings(filepath.Join(dir, "settings.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := coding.NewAgentSession(coding.AgentSessionOptions{
		Cwd:            dir,
		Model:          model,
		SessionManager: manager,
		Settings:       settings,
		ModelRuntime:   runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// TestConfigModelsEditReloadsAgentModel：编辑当前活跃模型（不改 provider/id），
// 保存后 agent 状态里的模型必须是新目录条目（baseUrl/上下文窗口即时生效），
// 不需要新建会话或重启。这是「编辑模型配置后不生效」的回归测试。
func TestConfigModelsEditReloadsAgentModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	// 种 models.json：mock/m1 走自定义 baseUrl
	if err := os.WriteFile(coding.ModelsFile(), []byte(
		`[{"id":"m1","name":"m1","api":"openai-completions","provider":"mock","baseUrl":"http://127.0.0.1:9","contextWindow":123456}]`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	sess := newReloadTestSession(t, dir)
	_, ts := newTestServer(t, NewBackend(sess, "test", false))
	c := login(t, ts, "tok-123")

	// 编辑同一模型：换 baseUrl 与上下文窗口
	code, m := reqJSON(t, c, "POST", ts.URL+"/api/config/models", map[string]any{
		"id": "m1", "name": "m1", "api": "openai-completions", "provider": "mock",
		"baseUrl": "http://127.0.0.1:8080/v1", "contextWindow": 200000,
	})
	if code != http.StatusOK {
		t.Fatalf("upsert status=%d body=%v", code, m)
	}
	if m["reloaded"] != true {
		t.Fatalf("reload flag = %v, want true", m["reloaded"])
	}
	cur := sess.Agent.State().Model
	if cur == nil {
		t.Fatal("agent model is nil")
	}
	if cur.BaseURL != "http://127.0.0.1:8080/v1" {
		t.Fatalf("agent model baseUrl = %q, want edited value (edit must hot-reload)", cur.BaseURL)
	}
	if cur.ContextWindow != 200000 {
		t.Fatalf("agent model contextWindow = %d, want 200000", cur.ContextWindow)
	}
}

// TestConfigModelsEditKeepsExtendedFields：编辑保留 models.json 里的扩展字段
// （headers/sessionAffinity/impersonate/thinkingLevelMap），不能静默丢配置。
func TestConfigModelsEditKeepsExtendedFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	if err := os.WriteFile(coding.ModelsFile(), []byte(
		`[{"id":"m1","name":"m1","api":"openai-completions","provider":"mock","baseUrl":"http://127.0.0.1:9",
		   "headers":{"X-Gateway":"gw1"},"sessionAffinity":"openai","impersonate":"curl/8","contextWindow":123456}]`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	sess := newReloadTestSession(t, dir)
	_, ts := newTestServer(t, NewBackend(sess, "test", false))
	c := login(t, ts, "tok-123")

	// 常规编辑：只改 contextWindow（模拟前端只回传基础字段）
	code, m := reqJSON(t, c, "POST", ts.URL+"/api/config/models", map[string]any{
		"id": "m1", "name": "m1", "api": "openai-completions", "provider": "mock",
		"baseUrl": "http://127.0.0.1:9", "contextWindow": 200000,
	})
	if code != http.StatusOK {
		t.Fatalf("upsert status=%d body=%v", code, m)
	}
	custom, err := coding.LoadCustomModels(coding.ModelsFile())
	if err != nil {
		t.Fatal(err)
	}
	if len(custom) != 1 {
		t.Fatalf("custom models = %d, want 1", len(custom))
	}
	e := custom[0]
	if e.Headers["X-Gateway"] != "gw1" {
		t.Fatalf("headers lost on edit: %v", e.Headers)
	}
	if e.SessionAffinity != "openai" {
		t.Fatalf("sessionAffinity lost on edit: %q", e.SessionAffinity)
	}
	if e.Impersonate != "curl/8" {
		t.Fatalf("impersonate lost on edit: %q", e.Impersonate)
	}
}

// TestConfigModelsEditReloadsAllWindows：编辑模型配置后，不只是请求窗口——
// 其他窗口分叉的会话、TUI 共享会话都必须重新指向新目录条目。只重载请求
// 窗口时其他会话仍持旧定义，「改了配置没生效」即来源于此。
func TestConfigModelsEditReloadsAllWindows(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	llm := startMockLLM(t)
	if err := os.WriteFile(coding.ModelsFile(), []byte(
		`[{"id":"mock-model","name":"mock-model","api":"openai-completions","provider":"mock","baseUrl":"`+llm.URL+`","contextWindow":100000}]`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := newRealShared(t, llm)
	s, ts := newWindowServer(t, shared, false)
	c := login(t, ts, "tok-123")

	// 窗口 w2 开自己的会话（分叉共享同一 ModelRuntime）
	if code := doWindow(t, c, "POST", ts.URL+"/api/session/new", `{}`, "w2"); code != http.StatusOK {
		t.Fatalf("window new session status=%d", code)
	}
	var forked *coding.AgentSession
	for _, sess := range s.ctrl.actives() {
		if sess != shared {
			forked = sess
		}
	}
	if forked == nil {
		t.Fatal("window fork not registered")
	}

	// 默认窗口编辑模型：换 baseUrl
	code, m := reqJSON(t, c, "POST", ts.URL+"/api/config/models", map[string]any{
		"id": "mock-model", "name": "mock-model", "api": "openai-completions", "provider": "mock",
		"baseUrl": "http://sentinel.example/v1", "contextWindow": 100000,
	})
	if code != http.StatusOK {
		t.Fatalf("upsert status=%d body=%v", code, m)
	}
	if m["reloaded"] != true {
		t.Fatalf("reload flag = %v, want true", m["reloaded"])
	}
	// 两个会话的 agent 模型都必须指向新 baseUrl
	for _, tc := range []struct {
		name string
		sess *coding.AgentSession
	}{{"shared", shared}, {"forked", forked}} {
		cur := tc.sess.Agent.State().Model
		if cur == nil {
			t.Fatalf("%s: agent model is nil", tc.name)
		}
		if cur.BaseURL != "http://sentinel.example/v1" {
			t.Fatalf("%s session model baseUrl = %q, want edited value", tc.name, cur.BaseURL)
		}
	}
}

// TestConfigModelsRenameFollowsDefaultInProjectSettings：默认模型写在项目级
// settings 时（project wins），改名编辑后项目文件里的指向必须跟着更新。
// 旧实现拿合并视图判断、只写全局文件——项目旧值仍在，同步无效。
func TestConfigModelsRenameFollowsDefaultInProjectSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	projSettings := filepath.Join(dir, ".doge", "settings.json")
	if err := os.MkdirAll(filepath.Dir(projSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := coding.SaveSettings(projSettings, coding.Settings{
		DefaultProvider: "gw", DefaultModel: "old",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(coding.ModelsFile(), []byte(
		`[{"id":"old","name":"old","api":"openai-completions","provider":"gw"}]`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeBackend{statusModel: "gw/old"}
	cb := &cwdBackend{fakeBackend: f, cwd: dir}
	_, ts := newTestServer(t, cb)
	c := login(t, ts, "tok-123")

	code, m := reqJSON(t, c, "POST", ts.URL+"/api/config/models?previous=gw%2Fold", map[string]any{
		"id": "new", "name": "new", "api": "openai-completions", "provider": "gw",
	})
	if code != http.StatusOK {
		t.Fatalf("rename upsert status=%d body=%v", code, m)
	}
	proj, err := coding.LoadSettings(filepath.Join(dir, "settings.json"), projSettings)
	if err != nil {
		t.Fatal(err)
	}
	if proj.Settings.DefaultModel != "new" {
		t.Fatalf("project settings default not re-pointed: %+v", proj.Settings)
	}
}

// TestConfigModelsDeleteFollowsDefaultInProjectSettings：删除项目级默认指向
// 的模型后，项目文件里的失效指向必须清除。
func TestConfigModelsDeleteFollowsDefaultInProjectSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	projSettings := filepath.Join(dir, ".doge", "settings.json")
	if err := os.MkdirAll(filepath.Dir(projSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := coding.SaveSettings(projSettings, coding.Settings{
		DefaultProvider: "gw", DefaultModel: "old",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(coding.ModelsFile(), []byte(
		`[{"id":"old","name":"old","api":"openai-completions","provider":"gw"}]`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeBackend{statusModel: "gw/old"}
	cb := &cwdBackend{fakeBackend: f, cwd: dir}
	_, ts := newTestServer(t, cb)
	c := login(t, ts, "tok-123")

	code, _ := reqJSON(t, c, "DELETE", ts.URL+"/api/config/models/gw/old", nil)
	if code != http.StatusOK {
		t.Fatalf("delete status=%d", code)
	}
	proj, err := coding.LoadSettings(filepath.Join(dir, "settings.json"), projSettings)
	if err != nil {
		t.Fatal(err)
	}
	if proj.Settings.DefaultProvider != "" || proj.Settings.DefaultModel != "" {
		t.Fatalf("project settings stale default not cleared: %+v", proj.Settings)
	}
}

// TestConfigModelsEditHealsDuplicateEntries：models.json 里同 key 重复条目
// （0.11 及更早的编辑是纯追加，历史遗留）编辑后必须治愈——落盘去重，
// 否则编辑替换第一条、旧条目仍在，界面看仍是"加了新的没改原的"。
func TestConfigModelsEditHealsDuplicateEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	// 遗留文件：同 key 两条（旧 baseUrl 各异）
	if err := os.WriteFile(coding.ModelsFile(), []byte(
		`[{"id":"m1","name":"m1","api":"openai-completions","provider":"gw","baseUrl":"http://a.example/v1"},
		  {"id":"m1","name":"m1","api":"openai-completions","provider":"gw","baseUrl":"http://b.example/v1"}]`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeBackend{statusModel: "gw/m1"}
	_, ts := newTestServer(t, f)
	c := login(t, ts, "tok-123")

	code, m := reqJSON(t, c, "POST", ts.URL+"/api/config/models", map[string]any{
		"id": "m1", "name": "m1", "api": "openai-completions", "provider": "gw",
		"baseUrl": "http://c.example/v1",
	})
	if code != http.StatusOK {
		t.Fatalf("upsert status=%d body=%v", code, m)
	}
	custom, err := coding.LoadCustomModels(coding.ModelsFile())
	if err != nil {
		t.Fatal(err)
	}
	if len(custom) != 1 {
		t.Fatalf("duplicate entries not healed: %d entries remain", len(custom))
	}
	if custom[0].BaseURL != "http://c.example/v1" {
		t.Fatalf("edited entry lost, kept %q", custom[0].BaseURL)
	}
}

// TestSettingsPutClearsStaleProjectDefault：改默认模型时项目层旧默认被清
// （project wins 会遮蔽全局新值，不清场则 PUT 成功但生效值仍是旧默认）。
func TestSettingsPutClearsStaleProjectDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	projSettings := filepath.Join(dir, ".doge", "settings.json")
	if err := os.MkdirAll(filepath.Dir(projSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := coding.SaveSettings(projSettings, coding.Settings{
		DefaultProvider: "gw", DefaultModel: "old",
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeBackend{}
	cb := &cwdBackend{fakeBackend: f, cwd: dir}
	_, ts := newTestServer(t, cb)
	c := login(t, ts, "tok-123")

	code, m := reqJSON(t, c, "PUT", ts.URL+"/api/config/settings", map[string]any{
		"defaultProvider": "gw", "defaultModel": "new",
	})
	if code != http.StatusOK {
		t.Fatalf("settings put status=%d body=%v", code, m)
	}
	proj, err := coding.ReadSettingsFile(projSettings)
	if err != nil {
		t.Fatal(err)
	}
	if proj.DefaultProvider != "" || proj.DefaultModel != "" {
		t.Fatalf("project stale default not cleared: %+v", proj)
	}
}
