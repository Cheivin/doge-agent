// Package tools implements the built-in coding agent tools: read, write,
// edit, bash, grep, find, ls. IO is abstracted behind Operations so tools can
// run against custom backends later; the default is the local filesystem.
package tools

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"doge/internal/agent"
	"doge/internal/ai"
)

// Output truncation limits.
const (
	MaxOutputLines = 10000
	MaxOutputBytes = 256 << 10
)

// ExecOptions control process execution for the bash tool and operations.
type ExecOptions struct {
	Timeout    time.Duration
	Env        []string
	WorkingDir string
	// CommandPrefix initializes the shell before the command runs.
	CommandPrefix string
	// OnOutput streams stdout+stderr chunks as they are produced.
	OnOutput func(chunk []byte)
	// SessionEnv are extra environment variables (DOGE_SESSION_ID etc).
	SessionEnv []string
}

// ExecResult is the outcome of a command execution.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
}

// Operations abstracts the IO tools need. All functions must be safe for
// concurrent use.
type Operations struct {
	ReadFile  func(path string) ([]byte, error)
	WriteFile func(path string, data []byte, perm uint32) error
	MkdirAll  func(path string, perm uint32) error
	Stat      func(path string) (fs.FileInfo, error)
	ReadDir   func(path string) ([]fs.DirEntry, error)
	WalkDir   func(path string, fn fs.WalkDirFunc) error
	Exec      func(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error)
	// Abs resolves a path to an absolute one (defaults to filepath.Abs when
	// nil; cwd-bound operations override it).
	Abs func(path string) (string, error)

	// SessionID and SessionFile identify the owning session; they are injected
	// as DOGE_SESSION_* into child processes. These are per-session fields:
	// concurrent sessions each carry their own Operations (and are idle when
	// swapped), so no cross-session sharing happens here.
	SessionID   string
	SessionFile string

	// CodeCaller routes script-side tool calls (code tool RPC) back to the
	// owning session's permission policy and tool implementations. Per-session
	// like SessionID: each AgentSession installs its own.
	CodeCaller CodeCaller
}

// LocalOperations returns operations backed by the local filesystem and
// shell.
func LocalOperations() *Operations {
	return &Operations{
		ReadFile:  os.ReadFile,
		WriteFile: func(path string, data []byte, perm uint32) error { return os.WriteFile(path, data, 0o644) },
		MkdirAll:  func(path string, perm uint32) error { return os.MkdirAll(path, 0o755) },
		Stat:      os.Stat,
		ReadDir:   os.ReadDir,
		WalkDir:   filepath.WalkDir,
		Exec:      localExec,
		Abs:       filepath.Abs,
	}
}

// CwdCell is a thread-safe working directory for cwd-bound operations.
// Updating the cell rebinds every tool built from the wrapped operations
// without touching the process working directory, so several agents can work
// in different directories side by side.
type CwdCell struct {
	mu  sync.RWMutex
	cwd string
}

// NewCwdCell builds a cell bound to dir ("" means the process cwd).
func NewCwdCell(dir string) *CwdCell { return &CwdCell{cwd: dir} }

func (c *CwdCell) Set(dir string) {
	c.mu.Lock()
	c.cwd = dir
	c.mu.Unlock()
}

func (c *CwdCell) Get() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cwd
}

// WithCwd wraps operations so relative paths resolve against the cell's
// working directory and commands run there (ExecOptions.WorkingDir).
func WithCwd(ops *Operations, cell *CwdCell) *Operations {
	resolve := func(p string) string {
		cwd := cell.Get()
		if cwd == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(cwd, p)
	}
	return &Operations{
		ReadFile:  func(path string) ([]byte, error) { return ops.ReadFile(resolve(path)) },
		WriteFile: func(path string, data []byte, perm uint32) error { return ops.WriteFile(resolve(path), data, perm) },
		MkdirAll:  func(path string, perm uint32) error { return ops.MkdirAll(resolve(path), perm) },
		Stat:      func(path string) (fs.FileInfo, error) { return ops.Stat(resolve(path)) },
		ReadDir:   func(path string) ([]fs.DirEntry, error) { return ops.ReadDir(resolve(path)) },
		WalkDir:   func(path string, fn fs.WalkDirFunc) error { return ops.WalkDir(resolve(path), fn) },
		Exec: func(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error) {
			if opts.WorkingDir == "" {
				if cwd := cell.Get(); cwd != "" {
					opts.WorkingDir = cwd
				}
			}
			return ops.Exec(ctx, command, opts)
		},
		Abs: func(path string) (string, error) {
			if filepath.IsAbs(path) {
				return filepath.Clean(path), nil
			}
			cwd := cell.Get()
			if cwd == "" {
				return filepath.Abs(path)
			}
			return filepath.Join(cwd, path), nil
		},
		// Carry per-session state through the wrapper so bash/code tools see it.
		SessionID:   ops.SessionID,
		SessionFile: ops.SessionFile,
		CodeCaller:  ops.CodeCaller,
	}
}

// localExec runs a command in the user's shell. The process runs in its own
// process group so the whole tree can be killed on timeout or cancel.
func localExec(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error) {
	shell := shellPath()
	args := []string{"-c", command}
	if opts.CommandPrefix != "" {
		args = []string{"-c", opts.CommandPrefix + "\n" + command}
	}
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	env := os.Environ()
	env = append(env, opts.Env...)
	env = append(env, opts.SessionEnv...)
	cmd.Env = env
	if opts.WorkingDir != "" {
		cmd.Dir = opts.WorkingDir
	}

	var stdout strings.Builder
	var mu sync.Mutex
	onOutput := func(data []byte) {
		if len(data) == 0 {
			return
		}
		mu.Lock()
		stdout.Write(data)
		mu.Unlock()
		if opts.OnOutput != nil {
			opts.OnOutput(data)
		}
	}

	killTree := func() {
		if cmd.Process != nil {
			// Negative pid signals the whole process group.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				onOutput(buf[:n])
			}
			if err != nil {
				break
			}
		}
		buf2 := make([]byte, 32<<10)
		for {
			n, err := stderrPipe.Read(buf2)
			if n > 0 {
				onOutput(buf2[:n])
			}
			if err != nil {
				break
			}
		}
	}()

	timedOut := false
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
	}()
	select {
	case <-ctx.Done():
		// Command still running; kill the whole tree.
		timedOut = ctx.Err() == context.DeadlineExceeded
		killTree()
		<-waitErr
	case err := <-waitErr:
		_ = err
	}
	<-done

	mu.Lock()
	out := &ExecResult{Stdout: truncateOutput(stdout.String()), ExitCode: cmd.ProcessState.ExitCode(), TimedOut: timedOut}
	mu.Unlock()
	return out, nil
}

func shellPath() string {
	if v := os.Getenv("SHELL"); v != "" {
		return v
	}
	return "/bin/sh"
}

// truncateOutput applies the line/byte limits to tool output.
func truncateOutput(s string) string {
	if len(s) <= MaxOutputBytes {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) > MaxOutputLines {
		lines = lines[:MaxOutputLines]
	}
	out := strings.Join(lines, "\n")
	if len(out) > MaxOutputBytes {
		out = out[:MaxOutputBytes]
	}
	return out
}

// fileMutationQueue serializes writes to the same path across concurrent
// tool executions.
type fileMutationQueue struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

var defaultMutationQueue = &fileMutationQueue{locks: map[string]*sync.Mutex{}}

// WithPathLock runs fn with the per-path lock held.
func WithPathLock(path string, fn func() error) error {
	mu := defaultMutationQueue.lockFor(path)
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

func (q *fileMutationQueue) lockFor(path string) *sync.Mutex {
	q.mu.Lock()
	defer q.mu.Unlock()
	if m, ok := q.locks[path]; ok {
		return m
	}
	m := &sync.Mutex{}
	q.locks[path] = m
	return m
}

// ToolResult builds a successful tool result.
func ToolResult(text string) *agent.AgentToolResult {
	return &agent.AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(text)}}
}

// ToolError builds an error tool result.
func ToolError(message string) *agent.AgentToolResult {
	return &agent.AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(message)}}
}
