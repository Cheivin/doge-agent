// Package uploads 上传暂存区：Web/mesh 等各来源的文件统一先落此处，
// 拿到 uploadID 后再按用途（进对话/放置/应用配置）取用，与来源解耦。
//
// 存储布局：<dir>/<id>/<name> 为内容，<dir>/<id>/meta.json 为元数据。
// 会话 JSONL 只存引用，不内嵌文件内容（多模态图片除外）。
package uploads

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"doge/pkg/logx"
)

const (
	metaFile = "meta.json"
	// maxNameBytes 清洗后文件名长度上限
	maxNameBytes = 255
	// minFreeBytes uploads 卷剩余空间低于该值时拒绝新上传
	minFreeBytes = 1 << 30
	// sniffLen MIME 嗅探首块长度
	sniffLen = 512
)

// ErrTooLarge 单文件超出上限
var ErrTooLarge = errors.New("file exceeds size limit")

// ErrQuota 暂存区总配额已满
var ErrQuota = errors.New("upload quota exceeded")

// sweepInterval 后台清理周期
const sweepInterval = time.Hour

// ErrNotFound 上传不存在
var ErrNotFound = errors.New("upload not found")

// Config 上传限制与清理策略（settings.json uploads 节，零值取默认）
type Config struct {
	MaxRequestBytes     int64 // 单次 HTTP 请求体上限
	MaxFileBytes        int64 // 单文件上限
	QuotaBytes          int64 // 暂存区总配额
	RetentionDays       int   // TTL，超期清理
	MaxImageInlineBytes int64 // 图片直接注入对话的上限（超过转引用行）
	MaxTextInlineBytes  int64 // 文本全文注入对话的上限
}

// DefaultConfig 默认限制：单请求/单文件 100MiB、配额 512MiB、保留 7 天
func DefaultConfig() Config {
	return Config{
		MaxRequestBytes:     100 << 20,
		MaxFileBytes:        100 << 20,
		QuotaBytes:          512 << 20,
		RetentionDays:       7,
		MaxImageInlineBytes: 5 << 20,
		MaxTextInlineBytes:  64 << 10,
	}
}

// WithDefaults 补齐零值字段
func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = d.MaxRequestBytes
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = d.MaxFileBytes
	}
	if c.QuotaBytes <= 0 {
		c.QuotaBytes = d.QuotaBytes
	}
	if c.RetentionDays <= 0 {
		c.RetentionDays = d.RetentionDays
	}
	if c.MaxImageInlineBytes <= 0 {
		c.MaxImageInlineBytes = d.MaxImageInlineBytes
	}
	if c.MaxTextInlineBytes <= 0 {
		c.MaxTextInlineBytes = d.MaxTextInlineBytes
	}
	return c
}

// Meta 单个上传的元数据
type Meta struct {
	ID        string `json:"id"`
	Name      string `json:"name"` // 清洗后的原始文件名
	Size      int64  `json:"size"`
	MIME      string `json:"mimeType"` // http.DetectContentType 嗅探值，客户端声明仅作参考
	SHA256    string `json:"sha256"`
	Source    string `json:"source"` // "web" | "mesh:<peerID>"
	Window    string `json:"window,omitempty"`
	CreatedAt int64  `json:"createdAt"` // Unix 毫秒

	// Path 内容绝对路径，读取时由 Store 补上（不落盘）
	Path string `json:"-"`
}

// Store 上传暂存区（配置在构造时固定，运行期只读）
type Store struct {
	dir string
	cfg Config
}

// NewStore 构建暂存区（目录惰性创建）
func NewStore(dir string, cfg Config) *Store {
	return &Store{dir: dir, cfg: cfg.WithDefaults()}
}

// Config 返回生效配置
func (s *Store) Config() Config { return s.cfg }

// Dir 返回暂存区根目录
func (s *Store) Dir() string { return s.dir }

// StartSweep 起后台周期清理（TTL + 配额），随 ctx 结束退出。挂在 store 而
// 非 web server 上：TUI-only / mesh-only 模式同样需要清理生效。启动先清
// 一轮不等首个周期；目录不存在时无磁盘动作，保持惰性创建语义。
func (s *Store) StartSweep(ctx context.Context) {
	go func() {
		s.sweepOnce()
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepOnce()
			}
		}
	}()
}

// sweepOnce 清理一轮，失败降级为日志
func (s *Store) sweepOnce() {
	removed, err := s.Sweep()
	if err != nil {
		logx.Errorf("[uploads] 暂存区清理失败: %v", err)
		return
	}
	if removed > 0 {
		logx.Infof("[uploads] 暂存区清理 %d 条", removed)
	}
}

// Save 流式落盘一个上传：边写边算 sha256，超上限立即中止并清掉半成品。
func (s *Store) Save(name, source, window string, src io.Reader) (*Meta, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	if err := checkDisk(s.dir); err != nil {
		return nil, err
	}
	// 配额写前校验：已满即拒，不等周期清理兜底（并发写的窗口内可能超出
	// 一个文件，可接受——Sweep 会按最旧先删压回配额内）
	if err := s.checkQuota(); err != nil {
		return nil, err
	}
	name = CleanName(name)
	if name == "" {
		name = "upload"
	}
	id := newID()
	entry := filepath.Join(s.dir, id)
	if err := os.Mkdir(entry, 0o755); err != nil {
		return nil, err
	}
	fail := func(err error) (*Meta, error) {
		_ = os.RemoveAll(entry)
		return nil, err
	}

	// 首块先行，MIME 嗅探用
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(src, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return fail(err)
	}
	head = head[:n]

	f, err := os.OpenFile(filepath.Join(entry, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fail(err)
	}
	hash := sha256.New()
	// 多读 1 字节判超限：LimitReader 放行 maxFileBytes+1 即说明超了
	lim := io.LimitReader(src, s.cfg.MaxFileBytes-int64(len(head))+1)
	size, err := writeHeadAndRest(f, hash, head, lim)
	if err != nil {
		f.Close()
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	if size > s.cfg.MaxFileBytes {
		return fail(ErrTooLarge)
	}
	meta := &Meta{
		ID:        id,
		Name:      name,
		Size:      size,
		MIME:      http.DetectContentType(head),
		SHA256:    hex.EncodeToString(hash.Sum(nil)),
		Source:    source,
		Window:    window,
		CreatedAt: time.Now().UnixMilli(),
		Path:      filepath.Join(entry, name),
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(entry, metaFile), data, 0o644); err != nil {
		return fail(err)
	}
	return meta, nil
}

// writeHeadAndRest 写首块与剩余流，返回总大小
func writeHeadAndRest(f *os.File, hash io.Writer, head []byte, rest io.Reader) (int64, error) {
	if len(head) > 0 {
		if _, err := f.Write(head); err != nil {
			return 0, err
		}
		if _, err := hash.Write(head); err != nil {
			return 0, err
		}
	}
	copied, err := io.Copy(io.MultiWriter(f, hash), rest)
	return int64(len(head)) + copied, err
}

// Get 返回元数据与内容文件句柄（调用方负责 Close）
func (s *Store) Get(id string) (*Meta, *os.File, error) {
	meta, err := s.meta(id)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(meta.Path)
	if err != nil {
		return nil, nil, err
	}
	return meta, f, nil
}

// Meta 返回元数据（含内容路径）
func (s *Store) Meta(id string) (*Meta, error) { return s.meta(id) }

// List 列出全部上传（按时间新→旧）
func (s *Store) List() ([]Meta, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := s.meta(e.Name())
		if err != nil {
			continue // 半成品/损坏条目跳过，Sweep 会收尾
		}
		out = append(out, *meta)
	}
	// 同毫秒创建时按 ID 定序，保证排序稳定（Sweep 的淘汰顺序也随之确定）
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out, nil
}

// Delete 删除一个上传
func (s *Store) Delete(id string) error {
	if !validID(id) {
		return ErrNotFound
	}
	entry := filepath.Join(s.dir, id)
	if _, err := os.Stat(entry); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	return os.RemoveAll(entry)
}

// Sweep 清理：先删超 TTL 的条目，再按最旧先删降到配额内。返回删除条数。
func (s *Store) Sweep() (int, error) {
	metas, err := s.List()
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().AddDate(0, 0, -s.cfg.RetentionDays).UnixMilli()
	var total int64
	for _, m := range metas {
		total += m.Size
	}
	removed := 0
	// List 已按新→旧排序，从最旧的尾部开始删
	for i := len(metas) - 1; i >= 0; i-- {
		m := metas[i]
		if m.CreatedAt >= cutoff && total <= s.cfg.QuotaBytes {
			break // 未过期且未超配额，剩下的都不用动
		}
		if err := s.Delete(m.ID); err != nil {
			continue
		}
		total -= m.Size
		removed++
	}
	return removed, nil
}

// checkQuota 配额写前校验：现存总量已达配额即拒绝新上传
func (s *Store) checkQuota() error {
	metas, err := s.List()
	if err != nil {
		return err
	}
	var used int64
	for i := range metas {
		used += metas[i].Size
	}
	if used >= s.cfg.QuotaBytes {
		return ErrQuota
	}
	return nil
}

// meta 读取并补全内容路径
func (s *Store) meta(id string) (*Meta, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.dir, id, metaFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m.Path = filepath.Join(s.dir, m.ID, m.Name)
	return &m, nil
}

// checkDisk uploads 卷剩余空间不足时拒绝写入
func checkDisk(dir string) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return nil // 拿不到信息时不拦截
	}
	if int64(stat.Bavail)*int64(stat.Bsize) < minFreeBytes {
		return errors.New("uploads: disk space low")
	}
	return nil
}

// CleanName 清洗文件名：取 basename、去控制字符、限长
func CleanName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimLeft(b.String(), ".") // 防 "." / ".." 残留
	if len(out) > maxNameBytes {
		// 从尾部按 rune 回退到字节上限内，避免切碎多字节字符产生非法文件名
		for len(out) > maxNameBytes {
			_, size := utf8.DecodeLastRuneInString(out)
			out = out[:len(out)-size]
		}
	}
	return out
}

// newID 生成 32 字节 hex 的上传 ID
func newID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 失败没有可恢复路径
	}
	return hex.EncodeToString(b)
}

// validID 只允许十六进制目录名，防路径穿越
func validID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// HumanSize 大小可读化（API/前端展示用）
func HumanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
