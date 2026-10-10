package uploads

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// zip 解包限制（防炸弹：总解压量、条目数）
const (
	zipMaxTotalBytes = 256 << 20
	zipMaxEntries    = 4096
)

// ExtractZip 把 zip 安全解包到 destDir：逐项拒绝路径穿越（zip-slip）、绝对
// 路径与 symlink 条目，限制单文件（maxFileBytes ≤0 取默认）与总解压量。
// 返回解出的文件相对路径列表（不含目录）。
func ExtractZip(zipPath, destDir string, maxFileBytes int64) ([]string, error) {
	if maxFileBytes <= 0 {
		maxFileBytes = DefaultConfig().MaxFileBytes
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	if len(zr.File) > zipMaxEntries {
		return nil, errors.New("zip has too many entries")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	root := filepath.Clean(destDir) + string(os.PathSeparator)
	var files []string
	var total int64
	for _, f := range zr.File {
		name, err := zipEntryName(f)
		if err != nil {
			return nil, err
		}
		if name == "" {
			continue // 根目录/无效条目
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(filepath.Join(destDir, name), 0o755); err != nil {
				return nil, err
			}
			continue
		}
		target := filepath.Join(destDir, name)
		if !strings.HasPrefix(target, root) {
			return nil, fmt.Errorf("zip entry escapes destination: %s", name)
		}
		written, err := extractZipEntry(f, target, maxFileBytes, zipMaxTotalBytes-total)
		if err != nil {
			return nil, err
		}
		// 按实际解压字节累计与限量——条目头声明的大小可伪造，不能作为依据
		total += written
		files = append(files, name)
	}
	return files, nil
}

// zipEntryName 校验并清洗条目名：拒绝绝对路径、..、symlink 与控制字符。
// 根目录条目返回空串。
func zipEntryName(f *zip.File) (string, error) {
	if f.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("zip entry %s is a symlink", f.Name)
	}
	name := strings.TrimPrefix(filepath.ToSlash(f.Name), "./")
	if name == "" || name == "." {
		return "", nil
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(f.Name) {
		return "", fmt.Errorf("absolute zip entry: %s", f.Name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", fmt.Errorf("unsafe zip entry: %s", f.Name)
		}
		if strings.ContainsAny(seg, "\x00") {
			return "", fmt.Errorf("unsafe zip entry: %s", f.Name)
		}
	}
	return filepath.Clean(name), nil
}

// extractZipEntry 落一个文件条目（逐段建父目录），限流按实际解压字节：
// 单文件 maxFileBytes、剩余总量 budget，返回写入字节数
func extractZipEntry(f *zip.File, target string, maxFileBytes, budget int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	src, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer src.Close()
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode().Perm()&0o755)
	if err != nil {
		return 0, err
	}
	limit := maxFileBytes
	if budget < limit {
		limit = budget
	}
	// 多读 1 字节探测超限：LimitReader 放行 max+1 即说明实际内容超限
	written, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil && written > limit {
		err = fmt.Errorf("zip entry %s exceeds size limit", f.Name)
	}
	if err != nil {
		_ = os.Remove(target) // 失败不留半成品
		return 0, err
	}
	return written, nil
}
