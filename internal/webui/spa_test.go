package webui

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func testDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte("<html>SHELL</html>")},
		"assets/index-abc123.js": {Data: []byte("console.log(1)")},
		"favicon.ico":            {Data: []byte("ico")},
	}
}

func TestSPAFallbackAndCaching(t *testing.T) {
	h := spaHandler(testDist())

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantShell  bool
	}{
		{"root", "/", http.StatusOK, true},
		{"explicit index", "/index.html", http.StatusOK, true},
		{"first-level route", "/agents", http.StatusOK, true},
		{"nested route", "/monitoring/groups/g1/edit", http.StatusOK, true},
		{"deep route", "/target-status/t1/agents/a1/history", http.StatusOK, true},
		// Route params carry IPs and hostnames, so a dotted segment must still
		// resolve to the shell — it is a route, not a file.
		{"route with dotted param", "/agents/8.8.8.8", http.StatusOK, true},
		{"existing asset", "/assets/index-abc123.js", http.StatusOK, false},
		// The regression this guards: a stale shell asks for an asset hash that no
		// longer exists. Answering with HTML makes the browser reject the module and
		// paint nothing; a 404 keeps the cause visible.
		{"missing hashed asset", "/assets/index-oldhash.js", http.StatusNotFound, false},
		{"missing root script", "/sw.js", http.StatusNotFound, false},
		{"api never falls back", "/api/v1/nope", http.StatusNotFound, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := get(t, h, tc.path)
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d; want %d", resp.StatusCode, tc.wantStatus)
			}
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(b) == "<html>SHELL</html>"; got != tc.wantShell {
				t.Fatalf("served shell = %v; want %v (body %q)", got, tc.wantShell, b)
			}
			if !tc.wantShell {
				return
			}
			// The shell names hashed assets that change every build, so a cached
			// copy would point at files that are already gone.
			if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("Cache-Control = %q; want no-store", cc)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
				t.Fatalf("Content-Type = %q", ct)
			}
		})
	}
}

// A dev dist (NETTACT_WEBUI_LOCAL / ../web-console/dist) is rebuilt by Vite
// while the server keeps running, and every rebuild renames the hashed assets.
// The handler must therefore read index.html per request: a shell snapshotted at
// startup would keep naming assets the rebuild deleted, blanking the console for
// as long as the process lives — the very failure no-store is meant to prevent.
func TestSPAIndexFollowsRebuiltDist(t *testing.T) {
	dir := t.TempDir()
	writeDist := func(shell string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(shell), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeDist("<html>BUILD-1</html>")
	h := spaHandler(os.DirFS(dir)) // constructed once, long-lived — like the real server
	readVia := func(path string) string {
		t.Helper()
		resp := get(t, h, path)
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, p := range []string{"/", "/index.html", "/agents"} {
		if got := readVia(p); got != "<html>BUILD-1</html>" {
			t.Fatalf("%s before rebuild = %q", p, got)
		}
	}

	writeDist("<html>BUILD-2</html>")
	for _, p := range []string{"/", "/index.html", "/agents"} {
		if got := readVia(p); got != "<html>BUILD-2</html>" {
			t.Fatalf("%s after rebuild = %q; want the rebuilt shell", p, got)
		}
	}
}

// Vite empties outDir before writing, so a request can land while index.html is
// briefly absent. That must be a retryable 503, not a stale shell.
func TestSPAIndexMissingIsRetryable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>SHELL</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := spaHandler(os.DirFS(dir))
	if resp := get(t, h, "/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("warm-up status = %d", resp.StatusCode)
	}
	if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/agents"} {
		resp := get(t, h, p)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d; want 503", p, resp.StatusCode)
		}
		if ra := resp.Header.Get("Retry-After"); ra == "" {
			t.Fatalf("%s: missing Retry-After", p)
		}
		resp.Body.Close()
	}
}

func TestSPAHashedAssetsAreImmutable(t *testing.T) {
	h := spaHandler(testDist())
	resp := get(t, h, "/assets/index-abc123.js")
	defer resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control = %q; want the immutable directive", cc)
	}
}
