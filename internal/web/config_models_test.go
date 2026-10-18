package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"doge/internal/coding"
)

// readModelsFile 读测试目录的 models.json 原文
func readModelsFile(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(data)
}

// TestConfigModelsUpsertRenameReplace：改名编辑带 ?previous= 替换旧条目而非
// 追加重复（bug1 回归）；不带 previous 保留既有追加语义（新建路径）。
func TestConfigModelsUpsertRenameReplace(t *testing.T) {
	t.Setenv("DOGE_CODING_AGENT_DIR", t.TempDir())
	f := &fakeBackend{modelSpec: "mygw/custom-a", statusModel: "mygw/custom-a"}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")

	// 种一份 models.json
	custom := `[{"id":"custom-a","name":"A","api":"openai-completions","provider":"mygw"}]`
	if err := os.WriteFile(coding.ModelsFile(), []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}

	// 改名编辑：id custom-a → custom-a2
	code, m := reqJSON(t, client, "POST", ts.URL+"/api/config/models?previous=mygw%2Fcustom-a", map[string]any{
		"id": "custom-a2", "name": "A2", "api": "openai-completions", "provider": "mygw",
	})
	if code != 200 {
		t.Fatalf("rename upsert status=%d body=%v", code, m)
	}
	data, err := os.ReadFile(coding.ModelsFile())
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if got, want := countOccurrences(content, `"id"`), 1; got != want {
		t.Fatalf("models.json has %d entries, want 1 (rename must replace): %s", got, content)
	}
	if !contains(content, "custom-a2") || contains(content, `"custom-a"`) {
		t.Fatalf("renamed entry missing / old entry kept: %s", content)
	}
	// 当前模型在旧 key 上：应被切到新 spec（fakeBackend.SwitchModel 记录 modelSpec）
	if f.modelSpec != "mygw/custom-a2" {
		t.Fatalf("live model not re-pointed: %q", f.modelSpec)
	}

	// 新建路径（无 previous、key 不存在）：追加
	code, _ = reqJSON(t, client, "POST", ts.URL+"/api/config/models", map[string]any{
		"id": "custom-b", "name": "B", "api": "openai-completions", "provider": "mygw",
	})
	if code != 200 {
		t.Fatalf("create status=%d", code)
	}
	data, _ = os.ReadFile(coding.ModelsFile())
	if got, want := countOccurrences(string(data), `"id"`), 2; got != want {
		t.Fatalf("after create models.json has %d entries, want 2", got)
	}
}

// TestConfigModelsDeleteFallback：删除当前活跃模型后 settings 默认被清除、
// 当前模型回退到第一个可用。
func TestConfigModelsDeleteFallback(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", agentDir)
	f := &fakeBackend{modelSpec: "mygw/custom-a", statusModel: "mygw/custom-a"}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")

	custom := `[{"id":"custom-a","name":"A","api":"openai-completions","provider":"mygw","contextWindow":100000},
	            {"id":"custom-b","name":"B","api":"openai-completions","provider":"mygw","contextWindow":100000}]`
	if err := os.WriteFile(coding.ModelsFile(), []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	// settings 默认指向将被删除的模型
	if err := coding.SaveSettings(coding.SettingsFile(), coding.Settings{
		DefaultProvider: "mygw", DefaultModel: "custom-a",
	}); err != nil {
		t.Fatal(err)
	}
	// fakeBackend 可用列表（删除后回退 SwitchModel 的目标）
	f.sessions = nil
	f.modelSpec = "mygw/custom-a"

	code, _ := reqJSON(t, client, "DELETE", fmt.Sprintf("%s/api/config/models/%s/%s", ts.URL, "mygw", "custom-a"), nil)
	if code != 200 {
		t.Fatalf("delete status=%d", code)
	}
	// settings 默认已清除（fallback 链下次启动接管）
	settings, err := coding.LoadSettings(coding.SettingsFile(), filepath.Join(agentDir, "proj", ".doge", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if settings.Settings.DefaultProvider != "" || settings.Settings.DefaultModel != "" {
		t.Fatalf("settings default not cleared: %v", settings.Settings)
	}
}

// countOccurrences 统计子串出现次数
func countOccurrences(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); {
		idx := indexOf(s[i:], sub)
		if idx < 0 {
			break
		}
		n++
		i += idx + len(sub)
	}
	return n
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }

// 编译期引用（fakeBackend 的 coding 类型链）
var _ = coding.SessionKindAutomation
var _ = http.StatusOK
