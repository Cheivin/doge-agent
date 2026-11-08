package services

import (
	"doge/internal/console"
)

// FsList 列出指定目录下的子目录（新建会话的目录选择器数据源），path 为空时
// 从用户主目录开始；隐藏目录（.开头）不展示
func (s *ConfigService) FsList(path string) (map[string]any, error) {
	result, e := console.ListDirs(path)
	if e != nil {
		return nil, consoleErr(e)
	}
	return result, nil
}
