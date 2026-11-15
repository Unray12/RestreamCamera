// Package webui serves the embedded single page application.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:assets
var files embed.FS

// Handler returns the SPA file handler.
func Handler() http.Handler {
	sub, err := fs.Sub(files, "assets")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic(err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(index)
			return
		}
		if _, err := fs.Stat(sub, p); err != nil {
			// SPA fallback for client-side routes.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(index)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		r2 := new(http.Request)
		*r2 = *r
		r2.URL.Path = "/" + p
		fileServer.ServeHTTP(w, r2)
	})
}
