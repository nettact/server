package webui

import (
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// staticExts are the file extensions a built dist actually contains. A request
// for one of these that isn't on disk is a missing build artefact, never a
// vue-router path — see the 404 rule in spaHandler. Route params may contain
// dots (IPs, hostnames), so a bare "has an extension" test would swallow real
// routes; matching a closed set does not.
var staticExts = map[string]bool{
	".js": true, ".mjs": true, ".css": true, ".map": true, ".json": true, ".wasm": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".ico": true, ".webp": true, ".avif": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".txt": true, ".webmanifest": true,
}

// spaHandler serves a built web-console dist from fsys. Non-/api paths that
// don't match a file fall back to index.html so vue-router history mode works.
// /api paths 404 here — they are only reachable through this handler when the
// API router didn't match, which means the endpoint doesn't exist.
//
// Two rules keep the fallback from turning a stale cache into a blank console:
//
//   - index.html is served with no-store. It names hashed asset files that change
//     on every build, so a browser-cached copy points at assets that no longer
//     exist. Only "/" carried validators before (it went through http.FileServer);
//     every deep route got a bare 200 with no Last-Modified and no Cache-Control,
//     which browsers are free to cache heuristically — so after a rebuild the home
//     page revalidated and worked while every other URL replayed a stale shell.
//   - a missing *static file* 404s instead of falling back. Serving the HTML shell
//     for /assets/index-<oldhash>.js makes the browser reject the module on its
//     MIME type and render nothing at all, which hides the real cause; a 404 makes
//     the missing artefact visible in the network panel.
//
// The shell is read from fsys on every request rather than snapshotted at
// construction: the dev dist (NETTACT_WEBUI_LOCAL / ../web-console/dist) is
// rebuilt by Vite under a long-lived server, and a snapshot would keep naming
// asset hashes the rebuild deleted — the same blank console, moved server-side
// where no-store can't help.
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" || p == "index.html" {
			serveIndex(w, fsys)
			return
		}
		if _, err := fs.Stat(fsys, p); err != nil {
			if isStaticPath(p) {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, fsys)
			return
		}
		// Vite emits content-hashed filenames under assets/, so those bytes are
		// immutable: cache them hard. Everything else keeps the default (no
		// Cache-Control, revalidated via the FileServer's Last-Modified).
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// isStaticPath reports whether p addresses a build artefact rather than a
// vue-router route: anything under the dist asset dir, or any path ending in a
// known static extension.
func isStaticPath(p string) bool {
	return strings.HasPrefix(p, "assets/") || staticExts[strings.ToLower(path.Ext(p))]
}

// serveIndex writes the current index.html. A read failure means the dist is
// mid-rebuild (Vite empties outDir before writing) or has gone away: answer 503
// + Retry-After so the browser reloads into the finished build, rather than
// replaying a shell whose assets no longer exist.
func serveIndex(w http.ResponseWriter, fsys fs.FS) {
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "1")
		http.Error(w, "web console is being rebuilt", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(index)))
	_, _ = w.Write(index)
}
