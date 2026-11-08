package services

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"

	"doge/internal/coding/uploads"
	"doge/internal/console"
	"doge/internal/mcp"
)

type (
	// UploadsService 上传暂存区与确定性放置/配置应用（对应 /api/uploads*）
	UploadsService struct {
		d Deps
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
	resp, e := console.PlaceUpload(s.d.Uploads, id, dest, s.d.Backend.Status().Cwd, overwrite)
	if e != nil {
		return nil, consoleErr(e)
	}
	return resp, nil
}

// Apply 应用暂存区文件为配置（kind: mcp/models/skills-zip/permissions）。
// 双段确认：默认 dry-run 只校验 + 预览；confirm=true 才写盘并重载。
func (s *UploadsService) Apply(id, kind string, confirm bool) (map[string]any, error) {
	resp, e := console.ApplyUpload(s.d.Uploads, id, kind, confirm, console.ApplyDeps{
		Reload: func() error { return reloadAllSessions(s.d) },
		Mcp:    s.applyMcpServers,
	})
	if e != nil {
		return nil, consoleErr(e)
	}
	return resp, nil
}

// applyMcpServers kind=mcp 的 confirm 段：合并进项目 mcp.json，重建注册表并
// 把新工具组发给活动会话。mcp 未启用报 unavailable（web 侧为 404，类别留传输侧）。
func (s *UploadsService) applyMcpServers(servers map[string]*mcp.Server) *console.Error {
	if s.d.Mcp == nil {
		return console.Unavailable("mcp not enabled")
	}
	var err error
	console.LockApply(func() { err = s.d.Mcp.UpsertServers(servers) })
	if err != nil {
		return console.Invalid(err.Error())
	}
	if err := s.d.Mcp.Reload(); err != nil {
		return console.Wrap(err)
	}
	s.d.Session.RefreshMcpTools(s.d.Mcp.BuildTools())
	return nil
}

// mapUploadError 暂存区错误 → ApiError
func mapUploadError(err error) *ApiError {
	if errors.Is(err, uploads.ErrNotFound) {
		return errNotFound("upload not found")
	}
	return errInternal(err)
}
