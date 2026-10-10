package web

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"doge/internal/coding/uploads"
)

// uploadInfo is the API representation of one uploaded file.
type uploadInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MimeType  string `json:"mimeType"`
	SHA256    string `json:"sha256"`
	Source    string `json:"source,omitempty"`
	Window    string `json:"window,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

func uploadInfoFrom(m *uploads.Meta) uploadInfo {
	return uploadInfo{
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

// handleUploadsCreate accepts multipart/form-data uploads (one or more "file"
// fields) into the staging store. Each file is validated and stored
// independently; a failing file does not abort the rest and is reported
// per-entry with an "error" field. The request body is capped by
// settings.uploads.maxRequestBytes (413 beyond it).
func (s *Server) handleUploadsCreate(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not available")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.uploads.Config().MaxRequestBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "multipart request required")
		return
	}
	type entry struct {
		uploadInfo
		Error string `json:"error,omitempty"`
	}
	var out []entry
	saved := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "request too large")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid multipart body")
			return
		}
		if part.FormName() != "file" {
			drain(part)
			continue
		}
		info := entry{uploadInfo: uploadInfo{Name: uploads.CleanName(part.FileName())}}
		meta, err := s.uploads.Save(part.FileName(), "web", clientID(r), part)
		if err != nil {
			if errors.Is(err, uploads.ErrTooLarge) {
				info.Error = "file exceeds size limit"
			} else {
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					writeError(w, http.StatusRequestEntityTooLarge, "request too large")
					return
				}
				info.Error = err.Error()
			}
		} else {
			info.uploadInfo = uploadInfoFrom(meta)
			saved++
		}
		out = append(out, info)
	}
	if len(out) == 0 {
		writeError(w, http.StatusBadRequest, "no file field")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uploads": out, "saved": saved})
}

// drain consumes and discards a part so the multipart reader can advance.
func drain(part *multipart.Part) {
	_, _ = io.Copy(io.Discard, part)
}

// handleUploadsList lists staged uploads (newest first).
func (s *Server) handleUploadsList(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not available")
		return
	}
	metas, err := s.uploads.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	infos := make([]uploadInfo, 0, len(metas))
	for i := range metas {
		infos = append(infos, uploadInfoFrom(&metas[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"uploads": infos})
}

// handleUploadsGet returns one upload's metadata.
func (s *Server) handleUploadsGet(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not available")
		return
	}
	meta, err := s.uploads.Meta(r.PathValue("id"))
	if err != nil {
		writeUploadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, uploadInfoFrom(meta))
}

// handleUploadsContent streams an upload's bytes (for previews/downloads; no
// Range support — clients fetch whole files).
func (s *Server) handleUploadsContent(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not available")
		return
	}
	meta, f, err := s.uploads.Get(r.PathValue("id"))
	if err != nil {
		writeUploadError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", meta.MIME)
	// 存储型 XSS 防护：嗅探出 text/html 的文件（如 <script> 开头）内联展示
	// 即同源执行（cookie 自动附带，可调 prompt/place）。仅图片（svg 除外，
	// svg 可携带脚本）允许 inline，其余一律 attachment；nosniff 防嗅探绕过
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", contentDisposition(meta.MIME)+"; filename*=UTF-8''"+url.PathEscape(meta.Name))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// contentDisposition 按嗅探 MIME 决定内联还是附件下载
func contentDisposition(mime string) string {
	if strings.HasPrefix(mime, "image/") && mime != "image/svg+xml" {
		return "inline"
	}
	return "attachment"
}

// handleUploadsDelete removes one upload from the staging store.
func (s *Server) handleUploadsDelete(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not available")
		return
	}
	if err := s.uploads.Delete(r.PathValue("id")); err != nil {
		writeUploadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeUploadError(w http.ResponseWriter, err error) {
	if errors.Is(err, uploads.ErrNotFound) {
		writeError(w, http.StatusNotFound, "upload not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
