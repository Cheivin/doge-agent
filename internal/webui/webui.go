// Package webui 共享控制台前端内核：Web 控制台与桌面客户端复用同一份 UI
// （app.js / style.css / index.html），传输差异收敛在 /platform.js 适配层——
// web 端由本包 static/platform-web.js 提供，desktop 端由其 overlay 的
// platform-desktop.js 提供（同一 interface），经 Union 合并资产服务。
package webui

import (
	"embed"
	"errors"
	"io/fs"
	"path"
	"strings"
)

//go:embed static
var staticFS embed.FS

// FS 静态前端（根 = static/）：index.html、app.js、style.css、platform-web.js。
// desktop 端经 Union 叠加其 overlay（platform-desktop.js / desktop.css）。
var FS = mustSub()

func mustSub() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

// unionFS 桌面资产合并：桌面专属 overlay 优先，未命中回退共享内核 FS。
// /platform.js 固定取 overlay 的 platform-desktop.js（web 端同名路径由
// internal/web 的静态服务改写为 platform-web.js）。
type unionFS struct {
	overlay fs.FS
}

func (u unionFS) Open(name string) (fs.File, error) {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" {
		name = "." // fs.FS 根目录约定（http.FileServer 的 "/" 请求走这里）
	}
	if name == "platform.js" {
		return u.overlay.Open("platform-desktop.js")
	}
	f, err := u.overlay.Open(name)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return FS.Open(name)
}

// Union 返回 overlay 优先的合并 FS（desktop/main.go 与 e2e 基座共用）。
// wails 的 BundledAssetFileServer 与 http.FileServer 都可直接消费。
func Union(overlay fs.FS) fs.FS { return unionFS{overlay: overlay} }
