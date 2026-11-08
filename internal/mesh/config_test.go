package mesh

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfigAllowRemoteToolsDefault: allowRemoteTools/allowRemoteForwards
// 缺省键视为 false（fail-closed：直接调用能触碰节点配置，须显式开启）
func TestLoadConfigAllowRemoteToolsDefault(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		path := filepath.Join(dir, "mesh.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// 文件不存在 → 默认关
	cfg, err := LoadConfig(filepath.Join(dir, "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowRemoteTools {
		t.Fatal("missing file should default allowRemoteTools to false")
	}
	if cfg.AllowRemoteForwards {
		t.Fatal("missing file should default allowRemoteForwards to false")
	}

	// 老配置无该键 → 默认关
	cfg, err = LoadConfig(write(`{"name":"old"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowRemoteTools || cfg.AllowRemoteForwards {
		t.Fatal("absent keys should default to false")
	}

	// 显式 false → 关
	cfg, err = LoadConfig(write(`{"name":"off","allowRemoteTools":false,"allowRemoteForwards":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowRemoteTools || cfg.AllowRemoteForwards {
		t.Fatal("explicit false must stay false")
	}

	// 显式 true → 开
	cfg, err = LoadConfig(write(`{"name":"on","allowRemoteTools":true,"allowRemoteForwards":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowRemoteTools || !cfg.AllowRemoteForwards {
		t.Fatal("explicit true must stay true")
	}
}

// TestSaveConfigAtomic: SaveConfig 原子落盘，不留 .tmp 残留
func TestSaveConfigAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mesh.json")
	cfg := &Config{Name: "n", Token: "t"}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "n" || got.Token != "t" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file should not survive SaveConfig")
	}
}
