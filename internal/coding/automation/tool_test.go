package automation

import (
	"strings"
	"testing"

	"doge/internal/agent"
	"doge/internal/coding"
)

// runTool 执行工具并返回结果文本
func runTool(t *testing.T, deps *ToolDeps, params map[string]any) string {
	t.Helper()
	res, err := executeTool(deps, params)
	if err != nil {
		t.Fatalf("executeTool(%v): %v", params["action"], err)
	}
	return resultText(res)
}

// runToolErr 执行工具并断言结果包含 want 片段（工具错误以文本结果返回）
func runToolErr(t *testing.T, deps *ToolDeps, params map[string]any, want string) {
	t.Helper()
	res, err := executeTool(deps, params)
	if err != nil {
		t.Fatalf("executeTool(%v): %v", params["action"], err)
	}
	if out := resultText(res); !strings.Contains(out, want) {
		t.Fatalf("action %s: out = %q, want %q", params["action"], out, want)
	}
}

// resultText 拼接结果文本块
func resultText(res *agent.AgentToolResult) string {
	var sb strings.Builder
	for _, b := range res.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func TestToolLifecycle(t *testing.T) {
	env := newTestEnv(t)
	deps := &ToolDeps{Store: env.store, Runner: env.runner, Session: env.sess}

	// create（interval 路径）
	out := runTool(t, deps, map[string]any{
		"action": "create", "title": "每20分钟喝水提醒", "prompt": "提醒用户喝水",
		"intervalUnit": "minute", "interval": 20,
	})
	if !strings.Contains(out, "Created auto-") || !strings.Contains(out, "每20分钟喝水提醒") {
		t.Fatalf("create out = %q", out)
	}
	list, _ := env.store.List()
	if len(list) != 1 {
		t.Fatalf("store list = %d", len(list))
	}
	id := list[0].ID
	if list[0].Cwd != env.sess.GetCwd() {
		t.Fatalf("default cwd = %q", list[0].Cwd)
	}

	// list
	if out := runTool(t, deps, map[string]any{"action": "list"}); !strings.Contains(out, id) {
		t.Fatalf("list out = %q", out)
	}

	// get
	if out := runTool(t, deps, map[string]any{"action": "get", "id": id}); !strings.Contains(out, "提醒用户喝水") {
		t.Fatalf("get out = %q", out)
	}

	// update（改间隔 → 重锚）
	if out := runTool(t, deps, map[string]any{
		"action": "update", "id": id, "intervalUnit": "minute", "interval": 5,
	}); !strings.Contains(out, "every 5 minute(s)") {
		t.Fatalf("update out = %q", out)
	}
	if got := mustGet(t, env, id); got.Schedule.Interval != 5 {
		t.Fatalf("interval = %d", got.Schedule.Interval)
	}

	// disable / enable
	runTool(t, deps, map[string]any{"action": "disable", "id": id})
	if got := mustGet(t, env, id); got.Status != StatusPaused {
		t.Fatalf("status = %s", got.Status)
	}
	runTool(t, deps, map[string]any{"action": "enable", "id": id})
	if got := mustGet(t, env, id); got.Status != StatusActive {
		t.Fatalf("status = %s", got.Status)
	}

	// run（手动触发，异步）+ history
	runTool(t, deps, map[string]any{"action": "run", "id": id})
	if !poll(t, func() bool {
		got, _ := env.store.Get(id)
		return got.RunCountTotal == 1 && !got.Running
	}) {
		t.Fatalf("manual run not settled: %+v", mustGet(t, env, id))
	}
	if out := runTool(t, deps, map[string]any{"action": "history", "id": id}); !strings.Contains(out, "manual") {
		t.Fatalf("history out = %q", out)
	}

	// delete
	runTool(t, deps, map[string]any{"action": "delete", "id": id})
	if list, _ := env.store.List(); len(list) != 0 {
		t.Fatalf("delete left %d", len(list))
	}
}

func TestToolCreateScheduleMutex(t *testing.T) {
	env := newTestEnv(t)
	deps := &ToolDeps{Store: env.store, Runner: env.runner, Session: env.sess}
	// 两个调度字段同时给 → 拒绝
	runToolErr(t, deps, map[string]any{
		"action": "create", "title": "t", "prompt": "p",
		"delayMinutes": 5, "cron": "0 9 * * *",
	}, "exactly one")
	// 一个都不给 → 拒绝
	runToolErr(t, deps, map[string]any{"action": "create", "title": "t", "prompt": "p"}, "exactly one")
	// cron 路径合法
	runTool(t, deps, map[string]any{
		"action": "create", "title": "每天9点", "prompt": "p", "cron": "0 9 * * *",
	})
	// delay 路径合法
	runTool(t, deps, map[string]any{
		"action": "create", "title": "8分钟后", "prompt": "p", "delayMinutes": 8,
	})
	// 非法 cron → 拒绝
	runToolErr(t, deps, map[string]any{
		"action": "create", "title": "t", "prompt": "p", "cron": "99 * * * *",
	}, "cron")
}

func TestToolRecursionGuard(t *testing.T) {
	env := newTestEnv(t)
	// 用 automation 会话做工具绑定的来源会话，模拟"裁剪被绕过"的调用
	fork, err := env.sess.ForkAutomation(env.dir, nil, coding.RemoteFull)
	if err != nil {
		t.Fatal(err)
	}
	deps := &ToolDeps{Store: env.store, Runner: env.runner, Session: fork}
	for _, action := range []string{"create", "update", "delete", "enable", "disable", "run"} {
		params := map[string]any{"action": action, "id": "auto-x"}
		if action == "create" {
			params = map[string]any{
				"action": "create", "title": "t", "prompt": "p", "delayMinutes": 5,
			}
		}
		runToolErr(t, deps, params, "self-replication")
	}
	// 只读 action（list/get/history）不受限
	runTool(t, deps, map[string]any{"action": "list"})
}
