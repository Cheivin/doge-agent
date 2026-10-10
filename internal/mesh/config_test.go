package mesh

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfigAllowRemoteToolsDefault: allowRemoteTools 缺省键视为 true
// （同 token 即可 remote_tool 直接调用节点级工具），显式 false 才关闭
func TestLoadConfigAllowRemoteToolsDefault(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		path := filepath.Join(dir, "mesh.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// 文件不存在 → 默认开
	cfg, err := LoadConfig(filepath.Join(dir, "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowRemoteTools {
		t.Fatal("missing file should default allowRemoteTools to true")
	}

	// 老配置无该键 → 默认开
	cfg, err = LoadConfig(write(`{"name":"old"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowRemoteTools {
		t.Fatal("absent key should default allowRemoteTools to true")
	}

	// 显式 false → 关
	cfg, err = LoadConfig(write(`{"name":"off","allowRemoteTools":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowRemoteTools {
		t.Fatal("explicit false must stay false")
	}

	// 显式 true → 开
	cfg, err = LoadConfig(write(`{"name":"on","allowRemoteTools":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowRemoteTools {
		t.Fatal("explicit true must stay true")
	}
}
