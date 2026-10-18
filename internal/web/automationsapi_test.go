package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doge/internal/coding"
	"doge/internal/coding/automation"
)

// newAutomationServer 构建带真实 automation 管理面的测试服务（store+runner
// 落临时目录，走文件全链路）
func newAutomationServer(t *testing.T) (*httptest.Server, *automation.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store := automation.NewStore(filepath.Join(dir, "automations.json"), automation.DefaultConfig())
	runner := automation.NewRunner(store, nil, filepath.Join(dir, "automations"), automation.DefaultConfig())
	f := &fakeBackend{}
	s, ts := newTestServer(t, f)
	s.automations = automation.NewAPI(store, runner)
	return ts, store, dir
}

// postJSON 便捷发送
func reqJSON(t *testing.T, c *http.Client, method, url string, body any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, url, bytes.NewReader(data))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, m
}

func TestAutomationsAPIListCreate(t *testing.T) {
	ts, _, _ := newAutomationServer(t)
	client := login(t, ts, "tok-123")

	// 空列表
	m := getJSON(t, client, ts.URL+"/api/automations", 200)
	if list, ok := m["automations"].([]any); !ok || len(list) != 0 {
		t.Fatalf("empty list = %v", m)
	}

	// 创建（interval 路径，cwd 由后端补默认）
	code, m := reqJSON(t, client, "POST", ts.URL+"/api/automations", map[string]any{
		"title": "每20分钟喝水提醒", "prompt": "提醒喝水",
		"schedule": map[string]any{"kind": "interval", "intervalUnit": "minute", "interval": 20},
	})
	if code != 200 {
		t.Fatalf("create status=%d body=%v", code, m)
	}
	a := m["automation"].(map[string]any)
	id := a["id"].(string)
	if a["cwd"] == "" || a["nextRunAt"] == nil {
		t.Fatalf("created = %v", a)
	}
	if a["sourceSessionId"] != "sess-1" { // fakeBackend 的会话 id，回注目标
		t.Fatalf("sourceSession = %v", a["sourceSessionId"])
	}

	// 列表带卡片（lastRun 省略）
	m = getJSON(t, client, ts.URL+"/api/automations", 200)
	list := m["automations"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	card := list[0].(map[string]any)
	if card["id"] != id || card["lastRun"] != nil {
		t.Fatalf("card = %v", card)
	}

	// 校验失败 → 400（两个调度字段同时给）
	code, m = reqJSON(t, client, "POST", ts.URL+"/api/automations", map[string]any{
		"title": "t", "prompt": "p",
		"schedule": map[string]any{"kind": "delay", "delayMinutes": 5, "cron": "0 9 * * *"},
	})
	if code != 400 {
		t.Fatalf("mutex-violating create status=%d", code)
	}
}

func TestAutomationsAPIGetPatchDelete(t *testing.T) {
	ts, store, _ := newAutomationServer(t)
	client := login(t, ts, "tok-123")

	a := &automation.Automation{
		Title: "任务", Prompt: "p", Cwd: t.TempDir(),
		Schedule: automation.Schedule{Kind: automation.ScheduleDelay, DelayMinutes: 30, AnchorAt: time.Now().Unix()},
	}
	if err := store.Create("", a); err != nil {
		t.Fatal(err)
	}

	// 详情 + 空 runs
	m := getJSON(t, client, ts.URL+"/api/automations/"+a.ID, 200)
	if m["automation"].(map[string]any)["title"] != "任务" {
		t.Fatalf("get = %v", m)
	}
	if runs, ok := m["runs"].([]any); !ok || len(runs) != 0 {
		t.Fatalf("runs = %v", m)
	}

	// PATCH：出现的字段才改（title 改、prompt 不动）
	code, m := reqJSON(t, client, "PATCH", ts.URL+"/api/automations/"+a.ID, map[string]any{
		"title": "改名任务",
	})
	if code != 200 {
		t.Fatalf("patch status=%d body=%v", code, m)
	}
	got, _ := store.Get(a.ID)
	if got.Title != "改名任务" || got.Prompt != "p" {
		t.Fatalf("patched = %+v", got)
	}

	// PATCH schedule 整体替换 + 重锚
	code, _ = reqJSON(t, client, "PATCH", ts.URL+"/api/automations/"+a.ID, map[string]any{
		"schedule": map[string]any{"kind": "cron", "cron": "0 9 * * *"},
	})
	if code != 200 {
		t.Fatalf("patch schedule status=%d", code)
	}
	got, _ = store.Get(a.ID)
	if got.Schedule.Kind != automation.ScheduleCron || got.Schedule.DelayMinutes != 0 {
		t.Fatalf("schedule replaced = %+v", got.Schedule)
	}

	// 删除 + 404
	code, _ = reqJSON(t, client, "DELETE", ts.URL+"/api/automations/"+a.ID, nil)
	if code != 200 {
		t.Fatalf("delete status=%d", code)
	}
	resp, _ := client.Get(ts.URL + "/api/automations/" + a.ID)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete = %d", resp.StatusCode)
	}
}

func TestAutomationsAPIEnableDisableRun(t *testing.T) {
	ts, store, dir := newAutomationServer(t)
	client := login(t, ts, "tok-123")

	a := &automation.Automation{
		Title: "任务", Prompt: "p", Cwd: dir,
		Schedule: automation.Schedule{Kind: automation.ScheduleInterval, IntervalUnit: "minute", Interval: 10, AnchorAt: time.Now().Unix()},
	}
	if err := store.Create("", a); err != nil {
		t.Fatal(err)
	}

	// disable → paused；再 disable → 400
	code, _ := reqJSON(t, client, "POST", fmt.Sprintf("%s/api/automations/%s/disable", ts.URL, a.ID), nil)
	if code != 200 {
		t.Fatalf("disable status=%d", code)
	}
	code, _ = reqJSON(t, client, "POST", fmt.Sprintf("%s/api/automations/%s/disable", ts.URL, a.ID), nil)
	if code != 400 {
		t.Fatalf("double disable status=%d", code)
	}
	// enable 恢复
	code, _ = reqJSON(t, client, "POST", fmt.Sprintf("%s/api/automations/%s/enable", ts.URL, a.ID), nil)
	if code != 200 {
		t.Fatalf("enable status=%d", code)
	}

	// run：runner 未绑定会话（nil sess）→ 执行会失败但认领/结算闭环仍走完。
	// 等结算落定再返回，否则异步 goroutine 与 TempDir 清理竞争
	code, m := reqJSON(t, client, "POST", fmt.Sprintf("%s/api/automations/%s/run", ts.URL, a.ID), nil)
	if code != 200 {
		t.Fatalf("run status=%d body=%v", code, m)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := store.Get(a.ID)
		if err == nil && !got.Running && got.RunCountTotal == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Running || got.RunCountTotal != 1 {
		t.Fatalf("manual run not settled: %+v", got)
	}
}

func TestAutomationsAPIRunsWithoutRunner(t *testing.T) {
	// runner 缺席（Runs ErrUnavailable）：列表仍可用，runs 返回空数组而非 500
	dir := t.TempDir()
	store := automation.NewStore(filepath.Join(dir, "automations.json"), automation.DefaultConfig())
	f := &fakeBackend{}
	s, ts := newTestServer(t, f)
	s.automations = automation.NewAPI(store, nil)
	client := login(t, ts, "tok-123")

	m := getJSON(t, client, ts.URL+"/api/automations", 200)
	if _, ok := m["automations"].([]any); !ok {
		t.Fatalf("list without runner = %v", m)
	}
	m = getJSON(t, client, ts.URL+"/api/automations/auto-x/runs", 200)
	if runs, ok := m["runs"].([]any); !ok || len(runs) != 0 {
		t.Fatalf("runs without runner = %v", m)
	}
}

func TestAutomationsAPINilView(t *testing.T) {
	// 管理面未装配：全部端点 503
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	client := login(t, ts, "tok-123")
	for _, spec := range []struct {
		method, path string
	}{
		{"GET", "/api/automations"},
		{"POST", "/api/automations"},
		{"GET", "/api/automations/auto-x"},
		{"PATCH", "/api/automations/auto-x"},
		{"DELETE", "/api/automations/auto-x"},
		{"POST", "/api/automations/auto-x/run"},
		{"POST", "/api/automations/auto-x/enable"},
		{"GET", "/api/automations/auto-x/runs"},
	} {
		req, _ := http.NewRequest(spec.method, ts.URL+spec.path, strings.NewReader("{}"))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s %s = %d, want 503", spec.method, spec.path, resp.StatusCode)
		}
	}
}

// 保证 coding 包引用（fakeBackend 的类型链）
var _ = coding.SessionKindAutomation
