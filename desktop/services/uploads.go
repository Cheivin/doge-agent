package services

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/permissions"
	"doge/internal/coding/uploads"
	"doge/internal/mcp"
	"doge/pkg/logx"
)

type (
	// UploadsService 上传暂存区与确定性放置/配置应用（对应 /api/uploads*）
	UploadsService struct {
		d       Deps
		applyMu sync.Mutex // 串行化写盘段：并发 confirm 会互相覆盖 .bak 与目标
	}

	// UploadInput 前端传入的单个附件（data 为 base64）
	UploadInput struct {
		Name     string `json:"name"`     // 文件名
		MimeType string `json:"mimeType"` // MIME 类型
		Data     string `json:"data"`     // 内容 base64
	}

	// UploadEntry 上传结果条目（单文件失败带 error，不中断其余）
	UploadEntry struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Size      int64  `json:"size"`
		MimeType  string `json:"mimeType"`
		SHA256    string `json:"sha256,omitempty"`
		Source    string `json:"source,omitempty"`
		Window    string `json:"window,omitempty"`
		CreatedAt int64  `json:"createdAt,omitempty"`
		Error     string `json:"error,omitempty"`
	}

	// UploadContent 附件内容（data 为 base64，前端转 data URL 展示/下载）
	UploadContent struct {
		Name     string `json:"name"`     // 文件名
		MimeType string `json:"mimeType"` // MIME 类型
		Data     string `json:"data"`     // 内容 base64
	}
)

// store 取暂存区，不可用返回 unavailable
func (s *UploadsService) store() (*uploads.Store, error) {
	if s.d.Uploads == nil {
		return nil, errUnavailable("uploads not available")
	}
	return s.d.Uploads, nil
}

// entry 由 Meta 组装上传条目
func entryFrom(m *uploads.Meta) UploadEntry {
	return UploadEntry{
		ID:        m.ID,
		Name:      m.Name,
		Size:      m.Size,
		MimeType:  m.MIME,
		SHA256:    m.SHA256,
		Source:    m.Source,
		Window:    m.Window,
		CreatedAt: m.CreatedAt,
	}
}

// Upload 接收附件进暂存区（base64 解码后落盘，单文件限额由 store 保证）
func (s *UploadsService) Upload(files []UploadInput) (map[string]any, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errInvalid("no file field")
	}
	out := make([]UploadEntry, 0, len(files))
	saved := 0
	for _, f := range files {
		item := UploadEntry{Name: uploads.CleanName(f.Name)}
		data, derr := base64.StdEncoding.DecodeString(f.Data)
		if derr != nil {
			item.Error = "invalid base64 content"
			out = append(out, item)
			continue
		}
		meta, serr := store.Save(f.Name, "desktop", "desktop", bytes.NewReader(data))
		if serr != nil {
			if errors.Is(serr, uploads.ErrTooLarge) {
				item.Error = "file exceeds size limit"
			} else {
				item.Error = serr.Error()
			}
		} else {
			item = entryFrom(meta)
			saved++
		}
		out = append(out, item)
	}
	return map[string]any{"uploads": out, "saved": saved}, nil
}

// List 暂存区清单（新→旧）
func (s *UploadsService) List() (map[string]any, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	metas, err := store.List()
	if err != nil {
		return nil, errInternal(err)
	}
	infos := make([]UploadEntry, 0, len(metas))
	for i := range metas {
		infos = append(infos, entryFrom(&metas[i]))
	}
	return map[string]any{"uploads": infos}, nil
}

// Delete 删除单个附件
func (s *UploadsService) Delete(id string) error {
	store, err := s.store()
	if err != nil {
		return err
	}
	if err := store.Delete(id); err != nil {
		return mapUploadError(err)
	}
	return nil
}

// Content 附件内容（base64，前端转 data URL；仅非 svg 图片内联展示）
func (s *UploadsService) Content(id string) (*UploadContent, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	meta, f, err := store.Get(id)
	if err != nil {
		return nil, mapUploadError(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, errInternal(err)
	}
	return &UploadContent{
		Name:     meta.Name,
		MimeType: meta.MIME,
		Data:     base64.StdEncoding.EncodeToString(data),
	}, nil
}

// Place 把暂存区文件复制到白名单目的地（会话 cwd 子树/skills 目录/models.json/
// mcp.json），symlink 一律拒绝
func (s *UploadsService) Place(id, dest string, overwrite bool) (map[string]any, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	meta, err := store.Meta(id)
	if err != nil {
		return nil, mapUploadError(err)
	}
	cwd := s.d.Backend.Status().Cwd
	dest, err = resolvePlacePath(dest, cwd)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	// 目标已存在：symlink 一律拒绝（防写入时跟随逃逸）；非覆盖冲突 busy
	if st, err := os.Lstat(dest); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, errInvalid("destination is a symlink")
		}
		if !overwrite {
			return nil, errBusy("destination already exists")
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, errInternal(err)
	}
	// 同目录临时文件 + rename 原子替换：中途失败不留半截目标文件
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".doge-place-*")
	if err != nil {
		return nil, errInternal(err)
	}
	src, err := os.Open(meta.Path)
	if err != nil {
		_ = os.Remove(tmp.Name())
		tmp.Close()
		return nil, errInternal(err)
	}
	_, err = io.Copy(tmp, src)
	src.Close()
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return nil, errInternal(err)
	}
	s.applyMu.Lock()
	err = os.Rename(tmp.Name(), dest)
	s.applyMu.Unlock()
	if err != nil {
		_ = os.Remove(tmp.Name())
		return nil, errInternal(err)
	}
	logx.Infof("[desktop] upload %s placed to %s", meta.ID, dest)
	return map[string]any{"ok": true, "path": dest}, nil
}

// Apply 应用暂存区文件为配置（kind: mcp/models/skills-zip/permissions）。
// 双段确认：默认 dry-run 只校验 + 预览；confirm=true 才写盘并重载。
func (s *UploadsService) Apply(id, kind string, confirm bool) (map[string]any, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	meta, err := store.Meta(id)
	if err != nil {
		return nil, mapUploadError(err)
	}
	switch kind {
	case "mcp":
		return s.applyMcp(meta, confirm)
	case "models":
		return s.applyModels(meta, confirm)
	case "skills-zip":
		return s.applySkillsZip(store, meta, confirm)
	case "permissions":
		return s.applyPermissions(meta, confirm)
	default:
		return nil, errInvalid("unknown kind (want mcp|models|skills-zip|permissions)")
	}
}

// applyMcp 按社区 mcpServers 格式合并进项目 mcp.json，confirm 后重建注册表
// 并把新工具组发给活动会话
func (s *UploadsService) applyMcp(meta *uploads.Meta, confirm bool) (map[string]any, error) {
	body, err := os.ReadFile(meta.Path)
	if err != nil {
		return nil, errInternal(err)
	}
	servers, err := parseMcpServersBody(body)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	if !confirm {
		return map[string]any{"ok": true, "dryRun": true, "diff": mcpServersSummary(servers)}, nil
	}
	if s.d.Mcp == nil {
		return nil, errUnavailable("mcp not enabled")
	}
	s.applyMu.Lock()
	err = s.d.Mcp.UpsertServers(servers)
	s.applyMu.Unlock()
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	if err := s.d.Mcp.Reload(); err != nil {
		return nil, errInternal(err)
	}
	s.d.Session.RefreshMcpTools(s.d.Mcp.BuildTools())
	logx.Infof("[desktop] mcp config applied from upload %s (%d servers)", meta.ID, len(servers))
	return map[string]any{"ok": true, "reloaded": true, "applied": len(servers)}, nil
}

// mcpServersSummary 生成 dry-run 预览文本
func mcpServersSummary(servers map[string]*mcp.Server) string {
	names := make([]string, 0, len(servers))
	for k := range servers {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "将合并 %d 个 MCP server：", len(names))
	for _, n := range names {
		fmt.Fprintf(&b, "\n- %s (%s)", n, transportOf(servers[n]))
	}
	return b.String()
}

func transportOf(srv *mcp.Server) string {
	if srv == nil {
		return "unknown"
	}
	if srv.URL != "" {
		return "http"
	}
	return "stdio"
}

// applyModels 整体替换全局 models.json（先备份 .bak），confirm 后热重载
func (s *UploadsService) applyModels(meta *uploads.Meta, confirm bool) (map[string]any, error) {
	data, err := os.ReadFile(meta.Path)
	if err != nil {
		return nil, errInternal(err)
	}
	var models []*ai.Model
	if err := json.Unmarshal(data, &models); err != nil {
		return nil, errInvalid("models.json must be a JSON array of model definitions: " + err.Error())
	}
	if len(models) == 0 {
		return nil, errInvalid("models.json is empty")
	}
	for _, m := range models {
		if m.ID == "" || m.Provider == "" {
			return nil, errInvalid("every model needs id and provider")
		}
	}
	specs := make([]string, 0, len(models))
	for _, m := range models {
		specs = append(specs, m.Provider+"/"+m.ID)
	}
	if !confirm {
		return map[string]any{"ok": true, "dryRun": true,
			"diff": fmt.Sprintf("将导入 %d 个模型定义：\n- %s", len(models), strings.Join(specs, "\n- "))}, nil
	}
	target := coding.ModelsFile()
	s.applyMu.Lock()
	backupFile(target)
	werr := writeFileAtomic(target, data, 0o644)
	s.applyMu.Unlock()
	if werr != nil {
		return nil, errInternal(werr)
	}
	reloaded, reloadErr := true, ""
	if err := s.d.Backend.ReloadConfig(); err != nil {
		reloaded, reloadErr = false, err.Error()
	}
	logx.Infof("[desktop] models.json applied from upload %s (%d models)", meta.ID, len(models))
	return map[string]any{"ok": true, "reloaded": reloaded, "applied": len(models), "error": reloadErr}, nil
}

// applySkillsZip 把 zip 里的技能包安装到全局 skills 目录（覆盖同名），confirm
// 后热刷新（技能工具与系统提示即时重建）
func (s *UploadsService) applySkillsZip(store *uploads.Store, meta *uploads.Meta, confirm bool) (map[string]any, error) {
	tmp, err := os.MkdirTemp("", "doge-skill-zip-")
	if err != nil {
		return nil, errInternal(err)
	}
	defer os.RemoveAll(tmp)
	files, err := uploads.ExtractZip(meta.Path, tmp, store.Config().MaxFileBytes)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	skills := coding.LoadSkills(tmp)
	if len(skills) == 0 {
		return nil, errInvalid(fmt.Sprintf("no SKILL.md found in zip (%d files)", len(files)))
	}
	if !confirm {
		var b strings.Builder
		fmt.Fprintf(&b, "将安装 %d 个技能：", len(skills))
		for _, sk := range skills {
			fmt.Fprintf(&b, "\n- %s: %s", sk.Name, sk.Description)
		}
		return map[string]any{"ok": true, "dryRun": true, "diff": b.String(), "files": len(files)}, nil
	}
	dest := filepath.Join(coding.AgentDir(), "skills")
	installed := make([]string, 0, len(skills))
	s.applyMu.Lock()
	for _, sk := range skills {
		srcDir := filepath.Dir(sk.FilePath)
		target := filepath.Join(dest, sk.Name)
		// 覆盖安装：先清掉同名旧技能，否则发现时旧版先到先得
		if err := os.RemoveAll(target); err != nil {
			s.applyMu.Unlock()
			return nil, errInternal(err)
		}
		if err := os.CopyFS(target, os.DirFS(srcDir)); err != nil {
			s.applyMu.Unlock()
			return nil, errInternal(err)
		}
		installed = append(installed, sk.Name)
	}
	s.applyMu.Unlock()
	reloaded, reloadErr := true, ""
	if err := s.d.Backend.ReloadConfig(); err != nil {
		reloaded, reloadErr = false, err.Error()
	}
	logx.Infof("[desktop] skills installed from upload %s: %s", meta.ID, strings.Join(installed, ","))
	return map[string]any{"ok": true, "reloaded": reloaded, "installed": installed, "error": reloadErr}, nil
}

// applyPermissions 整体替换全局 permissions.json（先备份 .bak），confirm 后热重载
func (s *UploadsService) applyPermissions(meta *uploads.Meta, confirm bool) (map[string]any, error) {
	data, err := os.ReadFile(meta.Path)
	if err != nil {
		return nil, errInternal(err)
	}
	policy, err := validatePermissionsData(data)
	if err != nil {
		return nil, errInvalid(err.Error())
	}
	if !confirm {
		return map[string]any{"ok": true, "dryRun": true, "diff": permissionsSummary(policy)}, nil
	}
	target := filepath.Join(coding.AgentDir(), "permissions.json")
	s.applyMu.Lock()
	backupFile(target)
	werr := writeFileAtomic(target, data, 0o644)
	s.applyMu.Unlock()
	if werr != nil {
		return nil, errInternal(werr)
	}
	reloaded, reloadErr := true, ""
	if err := s.d.Backend.ReloadConfig(); err != nil {
		reloaded, reloadErr = false, err.Error()
	}
	logx.Infof("[desktop] permissions.json applied from upload %s", meta.ID)
	return map[string]any{"ok": true, "reloaded": reloaded, "error": reloadErr}, nil
}

// placeRoot place 目的地的白名单根（exact=只允许精确写到该文件）
type placeRoot struct {
	path  string
	exact bool
}

// resolvePlacePath 解析并校验 place 目的地：相对路径相对 cwd；拒绝 ".." 段；
// 必须落在白名单根内（已存在部分 EvalSymlinks，防 symlink 逃逸）
func resolvePlacePath(dest, cwd string) (string, error) {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return "", errors.New("dest required")
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(cwd, dest)
	}
	dest = filepath.Clean(dest)
	for _, seg := range strings.Split(filepath.ToSlash(dest), string(os.PathSeparator)) {
		if seg == ".." {
			return "", errors.New("dest must not contain ..")
		}
	}
	real, err := evalRealPath(dest)
	if err != nil {
		return "", err
	}
	roots := []placeRoot{
		{cwd, false},
		{filepath.Join(coding.AgentDir(), "skills"), false},
		{coding.ModelsFile(), true},
		{filepath.Join(coding.AgentDir(), "mcp.json"), true},
	}
	for _, root := range roots {
		rootReal, err := evalRealPath(root.path)
		if err != nil {
			continue
		}
		if root.exact {
			if real == rootReal {
				return dest, nil
			}
			continue
		}
		if real == rootReal || strings.HasPrefix(real, rootReal+string(os.PathSeparator)) {
			return dest, nil
		}
	}
	return "", errors.New("destination outside allowed roots")
}

// evalRealPath 解析路径中已存在的最长前缀为真实路径（逐层穿透 symlink），
// 不存在的尾部按字面拼接
func evalRealPath(p string) (string, error) {
	p = filepath.Clean(p)
	cur := p
	var tail []string
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{real}, tail...)...), nil
		}
		dir, base := filepath.Split(cur)
		if dir == cur {
			return p, nil
		}
		tail = append([]string{base}, tail...)
		cur = filepath.Clean(dir)
	}
}

// validatePermissionsData 解析上传的权限策略（临时文件走标准加载路径）
func validatePermissionsData(data []byte) (*permissions.Policy, error) {
	tmp, err := os.CreateTemp("", "doge-perm-*.json")
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return permissions.LoadPolicy(path)
}

// permissionsSummary 生成 dry-run 预览文本
func permissionsSummary(p *permissions.Policy) string {
	var b strings.Builder
	fmt.Fprintf(&b, "将替换全局权限策略：\n- 默认：tools=%s bash=%s",
		orUnset(p.DefaultPolicy.Tools), orUnset(p.DefaultPolicy.Bash))
	if p.Special.ExternalDirectory != "" {
		fmt.Fprintf(&b, "\n- 外部目录：%s", p.Special.ExternalDirectory)
	}
	fmt.Fprintf(&b, "\n- 工具规则：%d 条，bash 规则：%d 条", len(p.Tools), len(p.Bash))
	return b.String()
}

func orUnset(s string) string {
	if s == "" {
		return "allow(缺省)"
	}
	return s
}

// backupFile 现文件存在时备份为 <path>.bak
func backupFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // 目标还不存在，无需备份
	}
	_ = os.WriteFile(path+".bak", data, 0o600)
}

// writeFileAtomic 原子落盘：写同目录临时文件后 rename
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".doge-apply-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// mapUploadError 暂存区错误 → ApiError
func mapUploadError(err error) *ApiError {
	if errors.Is(err, uploads.ErrNotFound) {
		return errNotFound("upload not found")
	}
	return errInternal(err)
}
