package uploads

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveAndGet(t *testing.T) {
	s := NewStore(t.TempDir(), Config{})
	data := []byte("hello upload")
	meta, err := s.Save("../../evil/name.txt", "web", "win-1", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if meta.Name != "name.txt" {
		t.Fatalf("name = %q, want name.txt", meta.Name)
	}
	if meta.MIME != "text/plain; charset=utf-8" {
		t.Fatalf("mime = %q", meta.MIME)
	}
	sum := sha256.Sum256(data)
	if meta.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("sha256 mismatch")
	}
	if meta.Size != int64(len(data)) {
		t.Fatalf("size = %d", meta.Size)
	}
	got, f, err := s.Get(meta.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer f.Close()
	body, _ := io.ReadAll(f)
	if !bytes.Equal(body, data) {
		t.Fatal("content mismatch")
	}
	if got.Path != filepath.Join(s.Dir(), meta.ID, meta.Name) {
		t.Fatalf("path = %q", got.Path)
	}
}

func TestSaveEmpty(t *testing.T) {
	s := NewStore(t.TempDir(), Config{})
	meta, err := s.Save("", "web", "", strings.NewReader(""))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if meta.Name != "upload" || meta.Size != 0 {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestSaveTooLarge(t *testing.T) {
	s := NewStore(t.TempDir(), Config{MaxFileBytes: 10})
	_, err := s.Save("big.bin", "web", "", bytes.NewReader(make([]byte, 100)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	entries, _ := os.ReadDir(s.Dir())
	if len(entries) != 0 {
		t.Fatalf("half-written entry left behind: %d", len(entries))
	}
}

func TestGetNotFound(t *testing.T) {
	s := NewStore(t.TempDir(), Config{})
	if _, _, err := s.Get(".."); err == nil {
		t.Fatal("expected error for traversal id")
	}
	if _, _, err := s.Get("deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCleanName(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd": "passwd",
		"/abs/path/x.png":  "x.png",
		"..":               "",
		".":                "",
		"a\x00b.txt":       "ab.txt",
		"":                 "",
	}
	for in, want := range cases {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListAndDelete(t *testing.T) {
	s := NewStore(t.TempDir(), Config{})
	first, err := s.Save("a.txt", "web", "", strings.NewReader("a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Save("b.txt", "web", "", strings.NewReader("b"))
	if err != nil {
		t.Fatal(err)
	}
	// 同毫秒创建无法区分先后，把 first 的 CreatedAt 压到更早再验证排序
	patchAge(t, s, first.ID, time.Now().Add(-time.Minute).UnixMilli())
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("list order wrong, want newest first")
	}
	if err := s.Delete(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	list, _ = s.List()
	if len(list) != 1 {
		t.Fatalf("remaining = %d", len(list))
	}
}

func TestSweepTTL(t *testing.T) {
	s := NewStore(t.TempDir(), Config{QuotaBytes: 1 << 20, RetentionDays: 7})
	old, err := s.Save("old.txt", "web", "", strings.NewReader("12345"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("new.txt", "web", "", strings.NewReader("12345")); err != nil {
		t.Fatal(err)
	}
	patchAge(t, s, old.ID, time.Now().AddDate(0, 0, -30).UnixMilli())
	removed, err := s.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (expired)", removed)
	}
	if _, err := s.Meta(old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old still present: %v", err)
	}
}

func TestSweepQuota(t *testing.T) {
	s := NewStore(t.TempDir(), Config{QuotaBytes: 6, RetentionDays: 7})
	if _, err := s.Save("a.txt", "web", "", strings.NewReader("aaaaa")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("b.txt", "web", "", strings.NewReader("bbbbb")); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (over quota)", removed)
	}
	// 总量 10B > 6B：淘汰一个后剩 5B ≤ 配额，存活一个
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("survivors = %d, want 1", len(list))
	}
}

// patchAge 直改 meta.json 的 CreatedAt（TTL 判定依据）
func patchAge(t *testing.T, s *Store, id string, createdAt int64) {
	t.Helper()
	m, err := s.Meta(id)
	if err != nil {
		t.Fatal(err)
	}
	m.CreatedAt = createdAt
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), id, "meta.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSaveQuotaPrecheck 配额写前校验：现存总量已达配额即拒绝，不等周期清理
func TestSaveQuotaPrecheck(t *testing.T) {
	store := NewStore(t.TempDir(), Config{QuotaBytes: 10, MaxFileBytes: 100})
	if _, err := store.Save("a.txt", "web", "", strings.NewReader("12345")); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := store.Save("b.txt", "web", "", strings.NewReader("67890")); err != nil {
		t.Fatalf("second save: %v", err)
	}
	_, err := store.Save("c.txt", "web", "", strings.NewReader("x"))
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("third save err = %v, want ErrQuota", err)
	}
	// 淘汰一个再写：配额内恢复可用
	metas, _ := store.List()
	if len(metas) != 2 {
		t.Fatalf("metas = %d", len(metas))
	}
	if err := store.Delete(metas[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save("c.txt", "web", "", strings.NewReader("x")); err != nil {
		t.Fatalf("save after eviction: %v", err)
	}
}
