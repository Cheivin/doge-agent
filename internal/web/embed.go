package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed frontend
var frontendFS embed.FS

// staticHandler serves the embedded SPA (index.html + app.js + style.css).
func staticHandler() http.Handler {
	sub, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
