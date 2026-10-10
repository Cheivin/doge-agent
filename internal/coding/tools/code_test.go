package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeCaller implements the tool bridge without permissions.
func fakeCaller(name string, args map[string]any) (string, bool, error) {
	switch name {
	case "bash":
		cmd, _ := args["command"].(string)
		if strings.Contains(cmd, "rm -rf /") {
			return "Permission denied: high-risk command", true, nil
		}
		return "bash-ok: " + cmd, false, nil
	case "read":
		p, _ := args["path"].(string)
		return "read-ok: " + p, false, nil
	}
	return "unknown tool: " + name, true, nil
}

func TestCodeToolJS(t *testing.T) {
	ops := &Operations{CodeCaller: fakeCaller}
	tool := CodeTool(ops)
	result, err := tool.Execute("t1", map[string]any{
		"language": "js",
		"script": `
const tools = require('./doge_tools');
(async () => {
  const r1 = await tools.bash({command: 'ls'});
  const r2 = await tools.read({path: '/tmp/x'});
  console.log('RESULT:' + r1.output + '|' + r2.output);
})().catch(e => { console.error('ERR:' + e.message); process.exit(1); });
`,
		"timeout": float64(30),
	}, context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "RESULT:bash-ok: ls|read-ok: /tmp/x") {
		t.Errorf("js output = %q", text)
	}
}

func TestCodeToolPython(t *testing.T) {
	ops := &Operations{CodeCaller: fakeCaller}
	tool := CodeTool(ops)
	result, err := tool.Execute("t2", map[string]any{
		"language": "python",
		"script": `
import doge_tools
r1 = doge_tools.bash({'command': 'ls'})
r2 = doge_tools.read({'path': '/tmp/x'})
print('RESULT:' + r1['output'] + '|' + r2['output'])
`,
		"timeout": float64(30),
	}, context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "RESULT:bash-ok: ls|read-ok: /tmp/x") {
		t.Errorf("python output = %q", text)
	}
}

func TestCodeToolPermissionPassthrough(t *testing.T) {
	ops := &Operations{CodeCaller: fakeCaller}
	tool := CodeTool(ops)
	result, err := tool.Execute("t3", map[string]any{
		"language": "js",
		"script": `
const tools = require('./doge_tools');
(async () => {
  const r = await tools.bash({command: 'rm -rf /'});
  console.log('OUT:' + r.output + ' isError=' + r.isError);
})();
`,
		"timeout": float64(30),
	}, context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "Permission denied") || !strings.Contains(text, "isError=true") {
		t.Errorf("permission passthrough output = %q", text)
	}
}

func TestCodeToolTimeout(t *testing.T) {
	ops := &Operations{CodeCaller: fakeCaller}

	start := time.Now()
	tool := CodeTool(ops)
	result, err := tool.Execute("t4", map[string]any{
		"language": "js",
		"script":   `setTimeout(() => {}, 60000);`,
		"timeout":  float64(2),
	}, context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
	if !strings.Contains(result.Content[0].Text, "超时") {
		t.Errorf("timeout message = %q", result.Content[0].Text)
	}
}

func TestCodeToolMissingRuntime(t *testing.T) {
	// Force a bogus runtime to verify the probe error path.
	t.Setenv("DOGE_CODE_RUNTIME", "definitely-not-a-runtime")
	ops := &Operations{CodeCaller: fakeCaller}
	tool := CodeTool(ops)
	result, err := tool.Execute("t5", map[string]any{
		"language": "js",
		"script":   `console.log('x');`,
	}, context.Background(), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(result.Content[0].Text, "DOGE_CODE_RUNTIME") {
		t.Errorf("missing runtime message = %q", result.Content[0].Text)
	}
}
