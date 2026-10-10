package services

import (
	"os"
	"path/filepath"
	"strings"
)

type (
	// FsDir 目录选择器的目录项
	FsDir struct {
		Name string `json:"name"` // 目录名
		Path string `json:"path"` // 绝对路径
	}
)

// FsList 列出指定目录下的子目录（新建会话的目录选择器数据源），path 为空时
// 从用户主目录开始；隐藏目录（.开头）不展示
func (s *ConfigService) FsList(path string) (map[string]any, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errInternal(err)
		}
		path = home
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	dirs := []FsDir{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dirs = append(dirs, FsDir{Name: e.Name(), Path: filepath.Join(abs, e.Name())})
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		parent = "" // 已到根
	}
	return map[string]any{"path": abs, "parent": parent, "dirs": dirs}, nil
}
