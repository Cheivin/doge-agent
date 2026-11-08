package console

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"doge/internal/coding/automation"
)

// autoEnv 建临时目录 + 真实 automation 管理面（store+runner 走文件全链路）
func autoEnv(t *testing.T) AutomationSource {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	store := automation.NewStore(filepath.Join(dir, "automations.json"), automation.DefaultConfig())
	runner := automation.NewRunner(store, nil, filepath.Join(dir, "automations"), automation.DefaultConfig())
	return automation.NewAPI(store, runner)
}

// autoDraft 合法的任务草案
func autoDraft(t *testing.T) *automation.Automation {
	t.Helper()
	return &automation.Automation{
		Title:    "喝水提醒",
		Prompt:   "提醒喝水",
		Cwd:      t.TempDir(),
		Schedule: automation.Schedule{Kind: "interval", IntervalUnit: "minute", Interval: 20},
	}
}

func TestParseAutomationPatch(t *testing.T) {
	body := map[string]json.RawMessage{
		"title":   json.RawMessage(`"新标题"`),
		"maxRuns": json.RawMessage(`3`),
		"noop":    json.RawMessage(`"ignored"`),
	}
	p, e := ParseAutomationPatch(body)
	if e != nil {
		t.Fatalf("ParseAutomationPatch: %v", e)
	}
	if p.Title == nil || *p.Title != "新标题" || p.MaxRuns == nil || *p.MaxRuns != 3 {
		t.Fatalf("patch = %+v", p)
	}
	if p.Prompt != nil || p.Schedule != nil {
		t.Fatalf("absent keys must stay nil: %+v", p)
	}
	// 非法 schedule（结构体解不出即 invalid，kind 语义校验在 store 层）
	bad := map[string]json.RawMessage{"schedule": json.RawMessage(`"not-an-object"`)}
	if _, e := ParseAutomationPatch(bad); e == nil || e.Kind != KindInvalid {
		t.Fatalf("bad schedule err = %v", e)
	}
}

func TestAutomationsListGetRuns(t *testing.T) {
	src := autoEnv(t)
	created, e := AutomationCreate(src, autoDraft(t))
	if e != nil {
		t.Fatalf("create: %v", e)
	}
	id := created["automation"].(*automation.Automation).ID

	// 列表
	list, e := AutomationList(src)
	if e != nil || len(list["automations"].([]*automation.AutomationCard)) != 1 {
		t.Fatalf("list = %v, %v", list, e)
	}
	// 详情：无历史补空数组（runs:null 会破坏前端）
	get, e := AutomationGet(src, id, 0)
	if e != nil {
		t.Fatalf("get: %v", e)
	}
	if runs, ok := get["runs"].([]*automation.Run); !ok || len(runs) != 0 {
		t.Fatalf("runs = %v", get["runs"])
	}
	// 未知的 id → notfound
	if _, e := AutomationGet(src, "nope", 0); e == nil || e.Kind != KindNotFound {
		t.Fatalf("missing get err = %v", e)
	}
	// 台账默认条数与空数组口径
	runs, e := AutomationRuns(src, id, 0)
	if e != nil {
		t.Fatalf("runs: %v", e)
	}
	if rs, ok := runs["runs"].([]*automation.Run); !ok || len(rs) != 0 {
		t.Fatalf("runs = %v", runs["runs"])
	}
	// 调度器不可用视为空台账（两端统一 web 口径，不报 notfound）
	empty := automation.NewAPI(nil, nil)
	if rs, e := AutomationRuns(empty, id, 5); e != nil || rs["runs"] == nil {
		t.Fatalf("unavailable runs = %v, %v", rs, e)
	}
	// 部分更新与启停/删除
	title := "改过的标题"
	patch := map[string]json.RawMessage{"title": json.RawMessage(`"改过的标题"`)}
	updated, e := AutomationPatch(src, id, patch)
	if e != nil || updated["automation"].(*automation.Automation).Title != title {
		t.Fatalf("patch = %v, %v", updated, e)
	}
	if _, e := AutomationSetEnabled(src, id, false); e != nil {
		t.Fatalf("setEnabled: %v", e)
	}
	if e := AutomationDelete(src, id); e != nil {
		t.Fatalf("delete: %v", e)
	}
	if e := AutomationDelete(src, id); e == nil || e.Kind != KindNotFound {
		t.Fatalf("double delete err = %v", e)
	}
}
