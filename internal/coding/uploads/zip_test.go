package uploads

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeZip 打一个测试 zip，entries 为 名→内容；isDir 标记目录条目
func makeZip(t *testing.T, path string, entries map[string]string, dirs ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, d := range dirs {
		if _, err := zw.Create(d + "/"); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "skill.zip")
	makeZip(t, zipPath, map[string]string{
		"html-ppt/SKILL.md":           "---\nname: html-ppt\ndescription: demo\n---\nbody",
		"html-ppt/assets/logo.png":    "png-bytes",
		"html-ppt/deep/nested/ref.md": "ref",
	}, "html-ppt")

	dest := filepath.Join(dir, "skills")
	files, err := ExtractZip(zipPath, dest, 0)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("files = %v", files)
	}
	for _, p := range []string{
		"html-ppt/SKILL.md", "html-ppt/assets/logo.png", "html-ppt/deep/nested/ref.md",
	} {
		if _, err := os.Stat(filepath.Join(dest, p)); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}
}

func TestExtractZipRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../evil.txt", "/abs/evil.txt", "a/../../evil.txt"} {
		zipPath := filepath.Join(dir, "bad.zip")
		makeZip(t, zipPath, map[string]string{name: "x"})
		_, err := ExtractZip(zipPath, filepath.Join(dir, "out"), 0)
		if err == nil || !strings.Contains(err.Error(), "zip entry") {
			t.Fatalf("ExtractZip(%q) err = %v, want unsafe-entry error", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
			t.Fatalf("escaped file written for %q", name)
		}
	}
}

func TestExtractZipSizeLimit(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "big.zip")
	makeZip(t, zipPath, map[string]string{"big.bin": strings.Repeat("x", 100)})
	if _, err := ExtractZip(zipPath, filepath.Join(dir, "out"), 10); err == nil {
		t.Fatal("expected size-limit error")
	}
}

// TestExtractZipLyingHeader 谎报头的 zip 炸弹：Deflate 压缩的全零大文件实际
// 解压 8MB，中央目录声明的解压大小被改成 1——限流必须按实际字节生效，不能
// 信条目头声明值；拒绝后半成品不残留
func TestExtractZipLyingHeader(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, err := zw.CreateHeader(&zip.FileHeader{Name: "bomb.txt", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(make([]byte, 8<<20)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	// 中央目录条目偏移 24 处为声明的解压大小（4 字节 LE），改成 1
	data := buf.Bytes()
	i := bytes.Index(data, []byte("PK\x01\x02"))
	if i < 0 {
		t.Fatal("no central directory entry")
	}
	binary.LittleEndian.PutUint32(data[i+24:i+28], 1)

	zipPath := filepath.Join(dir, "bomb.zip")
	if err := os.WriteFile(zipPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")
	if _, err := ExtractZip(zipPath, dest, 1<<20); err == nil {
		t.Fatal("lying zip header accepted")
	}
	if _, err := os.Stat(filepath.Join(dest, "bomb.txt")); !os.IsNotExist(err) {
		t.Fatal("partial file left behind")
	}
}
