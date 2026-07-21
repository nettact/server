package webui

import (
	"io/fs"
	"net/http"
	"strings"
)

// spaHandler serves a built web-console dist from fsys. Non-/api paths that
// don't match a file fall back to index.html so vue-router history mode works.
// /api paths 404 here — they are only reachable through this handler when the
// API router didn't match, which means the endpoint doesn't exist.
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	index, _ := fs.ReadFile(fsys, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(fsys, p); err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
