package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doge/internal/ai"
	"doge/internal/coding"
)

// cwdBackend 覆盖 Status().Cwd 的 fakeBackend 包装（项目级技能目录测试用）
type cwdBackend struct {
	*fakeBackend
	cwd string
}

func (b *cwdBackend) Status() Status {
	st := b.fakeBackend.Status()
	st.Cwd = b.cwd
	return st
}

// getJSONAny GET 并把响应解成任意 JSON 值（数组端点用）
func getJSONAny(t *testing.T, client *http.Client, url string, want int) any {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("GET %s status = %d (want %d)", url, resp.StatusCode, want)
	}
	var v any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// deleteJSON 发 DELETE 请求并校验状态码
func deleteJSON(t *testing.T, client *http.Client, url string, want int) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("DELETE %s status = %d (want %d)", url, resp.StatusCode, want)
	}
}

// TestThinkingEndpoint: POST /api/thinking 转发合法级别到 backend，非法级别 400。
func TestThinkingEndpoint(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	postJSON(t, c, ts.URL+"/api/thinking", `{"level":"low"}`, http.StatusOK)
	if f.thinkingSet != "low" {
		t.Fatalf("thinking level = %q, want low", f.thinkingSet)
	}
	postJSON(t, c, ts.URL+"/api/thinking", `{"level":"bogus"}`, http.StatusBadRequest)
	postJSON(t, c, ts.URL+"/api/thinking", `{"level":""}`, http.StatusBadRequest)
	if f.thinkingSet != "low" {
		t.Fatalf("invalid level reached backend, level = %q", f.thinkingSet)
	}
}

// TestStatsContextFields: GET /api/stats 带上下文字段——最后一条带 Usage 的
// assistant 消息 Input+CacheRead，以及当前模型的上下文窗口。
func TestStatsContextFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
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
	// 两条带 Usage 的 assistant 消息，应取最后一条
	if err := sess.Session.AppendMessage(&ai.Message{
		Role: ai.RoleAssistant, Provider: "mock", Model: "m1",
		Usage: &ai.Usage{Input: 100, CacheRead: 40},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Session.AppendMessage(&ai.Message{
		Role: ai.RoleAssistant, Provider: "mock", Model: "m1",
		Usage: &ai.Usage{Input: 200, CacheRead: 50},
	}); err != nil {
		t.Fatal(err)
	}

	_, ts := newTestServer(t, NewBackend(sess, "test", false))
	c := bearerClient("tok-123")
	m := getJSON(t, c, ts.URL+"/api/stats", http.StatusOK)
	if m["contextTokens"].(float64) != 250 {
		t.Fatalf("contextTokens = %v, want 250", m["contextTokens"])
	}
	if m["contextWindow"].(float64) != 123456 {
		t.Fatalf("contextWindow = %v, want 123456", m["contextWindow"])
	}
}

// TestConfigModelsEndpoints: models.json CRUD——GET 返回原样数组，POST 按
// provider+id upsert，DELETE 只允许删自定义模型，写盘后触发重载。
func TestConfigModelsEndpoints(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	modelsFile := filepath.Join(dir, "models.json")
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	// 空文件 → 空数组
	if arr, ok := getJSONAny(t, c, ts.URL+"/api/config/models", http.StatusOK).([]any); !ok || len(arr) != 0 {
		t.Fatalf("empty catalog = %#v", arr)
	}

	// 新增
	postJSON(t, c, ts.URL+"/api/config/models", `{
		"provider":"zhipu","id":"glm-custom","name":"GLM Custom","api":"openai-completions",
		"baseUrl":"https://example.com/v1","contextWindow":131072,"maxTokens":8192,
		"input":["text","image"],"reasoning":true,"impersonate":"chrome"
	}`, http.StatusOK)
	arr, ok := getJSONAny(t, c, ts.URL+"/api/config/models", http.StatusOK).([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("catalog after upsert = %#v", arr)
	}
	first, _ := arr[0].(map[string]any)
	if first["baseUrl"] != "https://example.com/v1" || first["maxTokens"].(float64) != 8192 {
		t.Fatalf("custom model = %#v", first)
	}

	// 同 provider+id 再提交 → 替换不追加
	postJSON(t, c, ts.URL+"/api/config/models", `{
		"provider":"zhipu","id":"glm-custom","name":"GLM Custom v2","api":"openai-completions",
		"baseUrl":"https://example.com/v2","contextWindow":200000,"maxTokens":16384
	}`, http.StatusOK)
	arr, _ = getJSONAny(t, c, ts.URL+"/api/config/models", http.StatusOK).([]any)
	if len(arr) != 1 {
		t.Fatalf("catalog after replace = %#v", arr)
	}
	first, _ = arr[0].(map[string]any)
	if first["name"] != "GLM Custom v2" || first["contextWindow"].(float64) != 200000 {
		t.Fatalf("replaced model = %#v", first)
	}

	// 缺 provider/id → 400
	postJSON(t, c, ts.URL+"/api/config/models", `{"id":"no-provider"}`, http.StatusBadRequest)

	// 删除内置模型 → 400
	deleteJSON(t, c, ts.URL+"/api/config/models/openai/gpt-5.2", http.StatusBadRequest)

	// 删除自定义模型 → 200，文件落盘为 []
	deleteJSON(t, c, ts.URL+"/api/config/models/zhipu/glm-custom", http.StatusOK)
	data, err := os.ReadFile(modelsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("models.json after delete = %s", string(data))
	}
	if arr, ok := getJSONAny(t, c, ts.URL+"/api/config/models", http.StatusOK).([]any); !ok || len(arr) != 0 {
		t.Fatalf("catalog after delete = %#v", arr)
	}
}

// seedSkill 写一个最小技能目录
func seedSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\nbody"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// findSkill 从列表响应里按名字取出技能对象
func findSkill(t *testing.T, m map[string]any, name string) map[string]any {
	t.Helper()
	list, _ := m["skills"].([]any)
	for _, it := range list {
		if sk, _ := it.(map[string]any); sk["name"] == name {
			return sk
		}
	}
	t.Fatalf("skill %q not in %#v", name, m["skills"])
	return nil
}

// TestSkillToggleAndDelete: 全局启停写 settings.json 的 disabledSkills 且列表
// 带 disabled 状态；DELETE 只删用户级技能目录，项目级拒绝。
func TestSkillToggleAndDelete(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	proj := filepath.Join(dir, "proj")
	seedSkill(t, filepath.Join(dir, "skills", "demo"), "demo", "A demo skill")
	seedSkill(t, filepath.Join(proj, ".doge", "skills", "mine"), "mine", "A project skill")

	_, ts := newTestServer(t, &cwdBackend{fakeBackend: &fakeBackend{}, cwd: proj})
	c := bearerClient("tok-123")

	// 初始列表：disabled=false
	m := getJSON(t, c, ts.URL+"/api/skills", http.StatusOK)
	if findSkill(t, m, "demo")["disabled"] != false {
		t.Fatalf("demo = %#v", findSkill(t, m, "demo"))
	}

	// 全局停用 demo → settings.json 落盘，列表置 disabled
	postJSON(t, c, ts.URL+"/api/skills/demo/enabled", `{"enabled":false}`, http.StatusOK)
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "disabledSkills") {
		t.Fatalf("settings.json = %s", string(data))
	}
	m = getJSON(t, c, ts.URL+"/api/skills", http.StatusOK)
	if findSkill(t, m, "demo")["disabled"] != true {
		t.Fatalf("demo after disable = %#v", findSkill(t, m, "demo"))
	}
	if findSkill(t, m, "mine")["disabled"] != false {
		t.Fatalf("mine after disable demo = %#v", findSkill(t, m, "mine"))
	}

	// 重复停用去重，再启用恢复
	postJSON(t, c, ts.URL+"/api/skills/demo/enabled", `{"enabled":false}`, http.StatusOK)
	postJSON(t, c, ts.URL+"/api/skills/demo/enabled", `{"enabled":true}`, http.StatusOK)
	data, err = os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "demo") {
		t.Fatalf("settings.json after enable = %s", string(data))
	}

	// 项目级技能拒绝删除
	deleteJSON(t, c, ts.URL+"/api/skills/mine", http.StatusBadRequest)
	if _, err := os.Stat(filepath.Join(proj, ".doge", "skills", "mine")); err != nil {
		t.Fatalf("project skill should survive: %v", err)
	}

	// 未知技能 404
	deleteJSON(t, c, ts.URL+"/api/skills/nope", http.StatusNotFound)

	// 用户级技能删除成功，目录消失
	deleteJSON(t, c, ts.URL+"/api/skills/demo", http.StatusOK)
	if _, err := os.Stat(filepath.Join(dir, "skills", "demo")); !os.IsNotExist(err) {
		t.Fatalf("user skill dir should be gone, err = %v", err)
	}
}
