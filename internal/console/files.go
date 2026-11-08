package console

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"doge/internal/coding"
)

const (
	defaultLogLines = 200
	maxLogLines     = 5000
)

// logDateRe 约束日志归档选择（doge.log.<YYYY-MM-DD>[.N]），防止参数变任意路径访问
var logDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(\.\d+)?$`)

// Logs 诊断日志尾部；date 读归档（YYYY-MM-DD[.N]）
func Logs(lines int, date string) (map[string]any, *Error) {
	if lines <= 0 {
		lines = defaultLogLines
	}
	if lines > maxLogLines {
		lines = maxLogLines
	}
	path := coding.LogFile()
	if date != "" {
		if !logDateRe.MatchString(date) {
			return nil, Invalid("invalid date (want YYYY-MM-DD)")
		}
		path += "." + date
	}
	tail, err := TailFile(path, lines)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NotFound("log file not found")
		}
		return nil, Wrap(err)
	}
	return map[string]any{"file": path, "lines": tail}, nil
}

// TailFile 返回文件末 n 行。日志文件按天滚动有界（10MB），整读可接受
func TailFile(path string, n int) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// DirEntry 目录选择器条目（JSON 形状 name/path 与既有 API 一致）
type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ListDirs 列出指定目录的子目录（新建会话的目录选择器数据源），path 为空时
// 从用户主目录开始；隐藏目录（.开头）不展示
func ListDirs(path string) (map[string]any, *Error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, Wrap(err)
		}
		path = home
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, Invalid(err.Error())
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, Invalid(err.Error())
	}
	dirs := []DirEntry{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dirs = append(dirs, DirEntry{Name: e.Name(), Path: filepath.Join(abs, e.Name())})
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		parent = "" // 已到根
	}
	return map[string]any{"path": abs, "parent": parent, "dirs": dirs}, nil
}
