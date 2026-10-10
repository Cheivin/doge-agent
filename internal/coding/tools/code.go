package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
)

// CodeCaller executes a tool call made from inside a code script. It is
// registered by the coding layer, which applies the permission policy and
// runs the real tool implementation. It lives on the session's Operations so
// concurrent sessions never route script calls across each other's policies.
type CodeCaller func(name string, args map[string]any) (output string, isError bool, err error)

// CodeTool returns the PTC-style code execution tool: the model writes a
// script (JS or Python) that orchestrates tool calls through the generated
// doge_tools module. Each script-side call is routed back to the host over
// fd3 (JSON-RPC), where it goes through the permission policy and the real
// tool implementation. The caller is taken from ops.CodeCaller (per-session).
func CodeTool(ops *Operations) *agent.AgentTool {
	return &agent.AgentTool{
		Name: "code",
		Description: `Execute a script (JavaScript or Python) that orchestrates tool calls programmatically. ` +
			`PREFER this tool when a task needs multiple tool calls, loops, or data aggregation ` +
			`(e.g. counting occurrences across files, summing values, processing many paths) — one script ` +
			`replaces many individual read/bash calls and keeps the context small. ` +
			`Use for batch operations, data aggregation, and multi-step sequences with dependencies. ` +
			`In the script, import the generated doge_tools module: JavaScript: const tools = require('./doge_tools'); ` +
			`Python: import doge_tools. Available async functions: read({path}), write({path, content}), ` +
			`edit({path, edits}), bash({command, timeout}), grep({pattern, path}), find({path, pattern}), ls({path}). ` +
			`Each returns {output, isError}. Process/filter/aggregate data in the script and print only the ` +
			`conclusion with console.log/print — the script's stdout is what the model sees. ` +
			`Prefer this tool over many individual read/bash calls when results need aggregation or looping.`,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"language": map[string]any{
					"type":        "string",
					"enum":        []string{"js", "python"},
					"description": "Script language: js (node) or python (python3)",
				},
				"script": map[string]any{
					"type":        "string",
					"description": "The script source. Use tools.read/bash/etc. from doge_tools and print the result.",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Execution timeout in seconds (default 120)",
				},
			},
			"required": []string{"language", "script"},
		},
		ExecutionMode: agent.ToolExecutionSequential,
		Execute: func(toolCallID string, params map[string]any, ctx context.Context, _ agent.AgentToolUpdateCallback) (*agent.AgentToolResult, error) {
			language, _ := params["language"].(string)
			script, _ := params["script"].(string)
			if language == "" || script == "" {
				return ToolError("code 工具需要 language 和 script 参数"), nil
			}
			timeout := intParam(params, "timeout", 120)
			if timeout <= 0 {
				timeout = 120
			}
			out, err := runCodeScript(ctx, ops.CodeCaller, language, script, timeout)
			if err != nil {
				return ToolError(err.Error()), nil
			}
			return &agent.AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(out)}}, nil
		},
	}
}

// probeRuntime resolves the interpreter for a language, honoring the
// DOGE_CODE_RUNTIME override ("node" or "python3").
func probeRuntime(language string) (string, error) {
	want := "node"
	if language == "python" {
		want = "python3"
	}
	if forced := os.Getenv("DOGE_CODE_RUNTIME"); forced != "" {
		switch forced {
		case "node", "python3", "python":
			if forced == "python" {
				forced = "python3"
			}
			if _, err := exec.LookPath(forced); err != nil {
				return "", fmt.Errorf("DOGE_CODE_RUNTIME=%s 不可用: %v", forced, err)
			}
			return forced, nil
		default:
			return "", fmt.Errorf("DOGE_CODE_RUNTIME 只支持 node 或 python3，收到 %q", forced)
		}
	}
	path, err := exec.LookPath(want)
	if err != nil {
		return "", fmt.Errorf("未找到 %s：code 工具需要安装 node 或 python3（可设 DOGE_CODE_RUNTIME 指定）", want)
	}
	return path, nil
}

// runCodeScript executes a script with the doge_tools module on fd3 and
// returns the captured stdout (truncated).
func runCodeScript(ctx context.Context, caller CodeCaller, language, script string, timeout int) (string, error) {
	runtime, err := probeRuntime(language)
	if err != nil {
		return "", err
	}

	dir, err := os.MkdirTemp("", "doge-code-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	var scriptPath, modulePath string
	if language == "python" {
		modulePath = filepath.Join(dir, "doge_tools.py")
		scriptPath = filepath.Join(dir, "main.py")
		if err := os.WriteFile(modulePath, []byte(pythonToolsModule), 0o644); err != nil {
			return "", err
		}
		if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
			return "", err
		}
	} else {
		modulePath = filepath.Join(dir, "doge_tools.js")
		scriptPath = filepath.Join(dir, "main.js")
		if err := os.WriteFile(modulePath, []byte(jsToolsModule), 0o644); err != nil {
			return "", err
		}
		if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
			return "", err
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	// fd 3 in the child is a bidirectional socketpair carrying the RPC
	// channel (a plain pipe is one-directional).
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return "", fmt.Errorf("socketpair: %v", err)
	}
	// The host end must be non-blocking: os.File.Close cannot interrupt a
	// blocking read from another goroutine, which would leak the RPC
	// goroutine forever. Non-blocking + netpoller lets Close wake it up.
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		return "", fmt.Errorf("set nonblock: %v", err)
	}
	hostEnd := os.NewFile(uintptr(fds[0]), "rpc-host")
	childEnd := os.NewFile(uintptr(fds[1]), "rpc-child")
	defer hostEnd.Close()
	defer childEnd.Close()

	cmd := exec.CommandContext(runCtx, runtime, scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = dir
	// fd 3 in the child is the RPC channel.
	cmd.ExtraFiles = []*os.File{childEnd}

	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output

	if caller == nil {
		return "", fmt.Errorf("code 工具内部错误：工具调用桥接未初始化")
	}

	// RPC server: read JSON-RPC lines from the child and answer tool
	// calls through the registered caller.
	rpcDone := make(chan struct{})
	go func() {
		defer close(rpcDone)
		scanner := bufio.NewScanner(hostEnd)
		for scanner.Scan() {
			if os.Getenv("DOGE_CODE_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "[code-rpc] recv: %s\n", scanner.Text())
			}
			var req struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
				Params struct {
					Name string         `json:"name"`
					Args map[string]any `json:"args"`
				} `json:"params"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				continue
			}
			resp := map[string]any{"id": req.ID}
			if req.Method != "call_tool" {
				resp["error"] = map[string]any{"message": "unknown method: " + req.Method}
			} else {
				out, isErr, err := caller(req.Params.Name, req.Params.Args)
				if err != nil {
					resp["error"] = map[string]any{"message": err.Error()}
				} else {
					resp["result"] = map[string]any{"output": out, "isError": isErr}
				}
			}
			data, _ := json.Marshal(resp)
			if os.Getenv("DOGE_CODE_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "[code-rpc] send: %s\n", string(data))
			}
			if _, err := hostEnd.Write(append(data, '\n')); err != nil {
				return
			}
		}
	}()

	err = cmd.Run()
	// The child is gone: close the host end so the RPC reader sees EOF
	// and the server goroutine exits.
	hostEnd.Close()
	<-rpcDone
	if runCtx.Err() == context.DeadlineExceeded {
		killProcessGroup(cmd)
		return "", fmt.Errorf("code 脚本超时（%ds）", timeout)
	}
	if err != nil {
		// Include the interpreter's stderr for syntax/runtime errors.
		return "", fmt.Errorf("code 脚本执行失败: %v\n%s", err, truncateOutput(output.String()))
	}

	result := truncateOutput(output.String())
	return result, nil
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// jsToolsModule is injected into the script's directory; the model's script
// requires it and calls the exported functions.
const jsToolsModule = `// Generated by doge. Tool calls are routed to the host and checked by
// doge's permission layer.
'use strict';
const fs = require('fs');

// fd 3 is a pipe to the doge host. net.Socket cannot wrap pipe fds, so we
// use synchronous fd reads/writes with a line-buffered protocol.
let rpcBuf = Buffer.alloc(0);
function rpcRead() {
  const chunk = Buffer.alloc(65536);
  let n;
  try {
    n = fs.readSync(3, chunk, 0, chunk.length);
  } catch (e) {
    if (e.code === 'EOF' || e.code === 'ENOTCONN') return null;
    throw e;
  }
  if (n === 0) return null;
  return chunk.slice(0, n);
}
function rpcWriteLine(data) {
  const buf = Buffer.from(data + '\n');
  let off = 0;
  while (off < buf.length) {
    off += fs.writeSync(3, buf, off, buf.length - off);
  }
}
let nextId = 1;
const pending = new Map();
function pump() {
  const chunk = rpcRead();
  if (chunk === null) return false;
  rpcBuf = Buffer.concat([rpcBuf, chunk]);
  let idx;
  while ((idx = rpcBuf.indexOf(10)) >= 0) {
    const line = rpcBuf.slice(0, idx).toString('utf8');
    rpcBuf = rpcBuf.slice(idx + 1);
    if (!line) continue;
    let msg;
    try { msg = JSON.parse(line); } catch (e) { continue; }
    const p = pending.get(msg.id);
    if (!p) continue;
    pending.delete(msg.id);
    if (msg.error) p.reject(new Error(msg.error.message));
    else p.resolve(msg.result);
  }
  return true;
}
function call(name, args) {
  return new Promise((resolve, reject) => {
    const id = nextId++;
    pending.set(id, { resolve, reject });
    rpcWriteLine(JSON.stringify({ id, method: 'call_tool', params: { name, args: args || {} } }));
    while (pending.has(id)) {
      if (!pump()) {
        pending.delete(id);
        reject(new Error('doge host closed the RPC channel'));
        return;
      }
    }
  });
}
const tools = {};
for (const n of ['read', 'write', 'edit', 'bash', 'grep', 'find', 'ls']) {
  tools[n] = (args) => call(n, args);
}
module.exports = tools;
`

// pythonToolsModule is the Python counterpart of jsToolsModule.
const pythonToolsModule = `# Generated by doge. Tool calls are routed to the host and checked by
# doge's permission layer.
import json
import os
import threading

_rpc = os.fdopen(3, 'rb+', buffering=0)
_next_id = [0]
_pending = {}
_lock = threading.Lock()


def _reader():
    while True:
        line = _rpc.readline()
        if not line:
            break
        try:
            msg = json.loads(line.decode('utf-8'))
        except Exception:
            continue
        mid = msg.get('id')
        with _lock:
            entry = _pending.pop(mid, None)
        if entry:
            if msg.get('error'):
                entry['error'] = msg['error']['message']
            else:
                entry['result'] = msg['result']
            entry['event'].set()


threading.Thread(target=_reader, daemon=True).start()


def _call(name, args=None):
    with _lock:
        _next_id[0] += 1
        mid = _next_id[0]
        entry = {'event': threading.Event(), 'result': None, 'error': None}
        _pending[mid] = entry
    payload = json.dumps({'id': mid, 'method': 'call_tool',
                          'params': {'name': name, 'args': args or {}}}) + '\n'
    _rpc.write(payload.encode('utf-8'))
    entry['event'].wait()
    if entry['error']:
        raise RuntimeError(entry['error'])
    return entry['result']


def read(args=None):
    return _call('read', args)


def write(args=None):
    return _call('write', args)


def edit(args=None):
    return _call('edit', args)


def bash(args=None):
    return _call('bash', args)


def grep(args=None):
    return _call('grep', args)


def find(args=None):
    return _call('find', args)


def ls(args=None):
    return _call('ls', args)
`
