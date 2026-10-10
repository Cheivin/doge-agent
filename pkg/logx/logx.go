// Package logx 进程级诊断日志，agent/ai/permission/mesh/web 事件写入同一时序文件
package logx

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Level 日志级别，Off 禁用所有输出
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelOff
)

var levelNames = map[Level]string{
	LevelDebug: "debug", LevelInfo: "info", LevelWarn: "warn", LevelError: "error", LevelOff: "off",
}

// String 返回级别名称
func (l Level) String() string {
	if s, ok := levelNames[l]; ok {
		return s
	}
	return "info"
}

// ParseLevel 解析日志级别字符串（"" = info）
func ParseLevel(s string) (Level, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return LevelInfo, nil
	}
	for l, name := range levelNames {
		if name == s {
			return l, nil
		}
	}
	return LevelInfo, fmt.Errorf("invalid log level %q (want debug|info|warn|error|off)", s)
}

// Options 配置文件日志接收器
type Options struct {
	Path     string // 日志文件路径，父目录必须存在
	Level    Level  // 初始日志级别
	Stderr   bool   // 是否回显到 stderr（DOGE_DEBUG）
	Banner   string // 运行顶部的横幅（版本、pid 等）
	MaxBytes int64  // 文件轮转阈值（0 = 10MB）
}

// dateFmt 归档日期格式：doge.log.<YYYY-MM-DD>
const dateFmt = "2006-01-02"

// nowFn 每日轮转时钟，测试可覆盖
var nowFn = time.Now

type logger struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	written  int64
	maxBytes int64
	level    Level
	stderr   bool
	ready    bool
	day      string // 当前文件的本地日期，跨日则轮转
}

var std = &logger{}

// Setup 打开日志接收器，启动时调用一次
func Setup(opts Options) error {
	if opts.Path == "" {
		return nil
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 10 << 20
	}
	f, err := os.OpenFile(opts.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	// Anchor the rotation day to the existing file's mtime: a process that
	// restarts on a new day archives yesterday's file on the first write.
	day := nowFn().Format(dateFmt)
	if st != nil && !st.ModTime().IsZero() {
		day = st.ModTime().Format(dateFmt)
	}
	std.mu.Lock()
	old := std.f
	std.f = f
	std.path = opts.Path
	std.written = st.Size()
	std.maxBytes = opts.MaxBytes
	std.level = opts.Level
	std.stderr = opts.Stderr
	std.day = day
	std.ready = true
	std.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	if opts.Banner != "" {
		Infof("===== %s =====", opts.Banner)
	}
	return nil
}

// Close 刷新并关闭接收器
func Close() {
	std.mu.Lock()
	defer std.mu.Unlock()
	std.ready = false
	if std.f != nil {
		_ = std.f.Close()
		std.f = nil
	}
}

// SetLevel 运行时更改日志级别
func SetLevel(l Level) {
	std.mu.Lock()
	std.level = l
	std.mu.Unlock()
}

// CurrentLevel 返回当前日志级别
func CurrentLevel() (l Level) {
	std.mu.Lock()
	defer std.mu.Unlock()
	return std.level
}

// Enabled 报告指定级别的日志是否会被输出
func Enabled(lv Level) bool {
	std.mu.Lock()
	defer std.mu.Unlock()
	return std.ready && lv >= std.level
}

// Debugf/Infof/Warnf/Errorf 写入一行日志，未初始化或级别过滤则空操作
func Debugf(format string, args ...any) { write(LevelDebug, format, args...) }
func Infof(format string, args ...any)  { write(LevelInfo, format, args...) }
func Warnf(format string, args ...any)  { write(LevelWarn, format, args...) }
func Errorf(format string, args ...any) { write(LevelError, format, args...) }

func write(lv Level, format string, args ...any) {
	std.mu.Lock()
	defer std.mu.Unlock()
	if !std.ready || lv < std.level {
		return
	}
	line := fmt.Sprintf("%s %s %s\n",
		time.Now().Format("2006-01-02T15:04:05.000"),
		strings.ToUpper(lv.String()[:4]),
		fmt.Sprintf(format, args...))
	if today := nowFn().Format(dateFmt); today != std.day || std.written+int64(len(line)) > std.maxBytes {
		// Archive under the date of the file being closed, so a size
		// rollover and a day rollover both produce dated archives.
		std.rotateLocked(std.day)
		std.day = today
	}
	if std.f != nil {
		if n, err := std.f.WriteString(line); err == nil {
			std.written += int64(n)
		}
	}
	if std.stderr {
		fmt.Fprint(os.Stderr, line)
	}
}

// rotateLocked 归档当前文件为 doge.log.<date> 并重新打开新文件
func (l *logger) rotateLocked(oldDay string) {
	if l.f == nil {
		return
	}
	_ = l.f.Close()
	l.f = nil
	name := l.path + "." + oldDay
	if _, err := os.Stat(name); err == nil {
		for i := 1; ; i++ {
			name = fmt.Sprintf("%s.%s.%d", l.path, oldDay, i)
			if _, err := os.Stat(name); os.IsNotExist(err) {
				break
			}
		}
	}
	_ = os.Rename(l.path, name)
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	l.f = f
	l.written = 0
}
