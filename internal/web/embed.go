package web

import (
	"net/http"

	"doge/internal/webui"
)

// staticHandler serves the embedded shared UI core (webui.FS: index.html +
// app.js + style.css). /platform.js 是传输适配层固定路径：web 端由共享内核的
// platform-web.js 实现（desktop 端同名路径由其 overlay 提供 platform-desktop.js）。
func staticHandler() http.Handler {
	files := http.FileServer(http.FS(webui.FS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/platform.js" {
			r.URL.Path = "/platform-web.js"
		}
		files.ServeHTTP(w, r)
	})
}
