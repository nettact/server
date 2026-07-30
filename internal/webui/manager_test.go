package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// buildDistTarGz returns a tar.gz containing the given files and its sha256 hex.
func buildDistTarGz(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// fakeRelease serves /<tag>/SHA256SUMS and /<tag>/web-console-dist-<tag>.tar.gz
// and counts requests.
type fakeRelease struct {
	tag      string
	tarball  []byte
	sumsLine string
	requests atomic.Int64
	fail     atomic.Bool // when true, everything 500s
}

func (f *fakeRelease) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if f.fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/" + f.tag + "/SHA256SUMS":
			fmt.Fprintln(w, f.sumsLine)
		case "/" + f.tag + "/web-console-dist-" + f.tag + ".tar.gz":
			w.Write(f.tarball)
		default:
			http.NotFound(w, r)
		}
	})
}

func newTestManager(t *testing.T, dir, version, baseURL string) *Manager {
	t.Helper()
	m := New(dir, version)
	m.baseURL = baseURL
	m.client = &http.Client{Timeout: 5 * time.Second}
	m.backoff = func(int) time.Duration { return time.Millisecond }
	m.logf = t.Logf
	return m
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result()
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := httputil.DumpResponse(resp, true)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewUsesDownloadCenterByDefault(t *testing.T) {
	t.Setenv(envBaseURL, "")
	m := New(t.TempDir(), "v0.1.0")
	if m.baseURL != "https://d.nettact.org/web-console" {
		t.Fatalf("baseURL = %q, want NetTact download center", m.baseURL)
	}
}

func TestFreshInstallAndSwap(t *testing.T) {
	const tag = "v0.1.0"
	tarball, sum := buildDistTarGz(t, map[string]string{"index.html": "<html>REAL-SPA</html>", "assets/app.js": "js"})
	rel := &fakeRelease{tag: tag, tarball: tarball, sumsLine: sum + "  web-console-dist-" + tag + ".tar.gz"}
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()

	dir := t.TempDir()
	m := newTestManager(t, dir, tag, srv.URL)
	h := m.Handler()

	if resp := get(t, h, "/"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("pre-install / status = %d; want 503", resp.StatusCode)
	}

	m.Start()
	defer m.Close(context.Background())
	waitFor(t, 5*time.Second, func() bool { return get(t, h, "/").StatusCode == http.StatusOK })

	if got := body(t, get(t, h, "/")); !bytes.Contains([]byte(got), []byte("REAL-SPA")) {
		t.Fatalf("/ does not serve the SPA: %q", got)
	}
	if got := body(t, get(t, h, "/deep/route")); !bytes.Contains([]byte(got), []byte("REAL-SPA")) {
		t.Fatalf("/deep/route did not fall back to index.html: %q", got)
	}
	if resp := get(t, h, "/api/v1/nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/api status = %d; want 404", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, tag, "index.html")); err != nil {
		t.Fatalf("installed index.html missing: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, tag, installedVersionFile)); err != nil || string(got) != tag {
		t.Fatalf("installed version = %q, %v; want %q", got, err, tag)
	}
}

func TestAlreadyInstalledServesWithoutNetwork(t *testing.T) {
	const tag = "v0.1.0"
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, tag), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tag, "index.html"), []byte("PREINSTALLED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tag, installedVersionFile), []byte(tag), 0o644); err != nil {
		t.Fatal(err)
	}

	rel := &fakeRelease{tag: tag}
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()

	m := newTestManager(t, dir, tag, srv.URL)
	m.Start()
	defer m.Close(context.Background())

	resp := get(t, m.Handler(), "/")
	if resp.StatusCode != http.StatusOK || !bytes.Contains([]byte(body(t, resp)), []byte("PREINSTALLED")) {
		t.Fatalf("preinstalled dist not served (status %d)", resp.StatusCode)
	}
	if n := rel.requests.Load(); n != 0 {
		t.Fatalf("made %d network requests; want 0", n)
	}
}

func TestMismatchedOrMissingInstalledVersionDownloadsAgain(t *testing.T) {
	const tag = "v0.2.0"
	oldTag := "v0.1.0"
	for _, tc := range []struct {
		name   string
		marker *string
	}{
		{name: "mismatched", marker: &oldTag},
		{name: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tarball, sum := buildDistTarGz(t, map[string]string{"index.html": "CURRENT"})
			rel := &fakeRelease{tag: tag, tarball: tarball, sumsLine: sum + "  web-console-dist-" + tag + ".tar.gz"}
			srv := httptest.NewServer(rel.handler())
			defer srv.Close()

			dir := t.TempDir()
			installed := filepath.Join(dir, tag)
			if err := os.MkdirAll(installed, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(installed, "index.html"), []byte("STALE"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.marker != nil {
				if err := os.WriteFile(filepath.Join(installed, installedVersionFile), []byte(*tc.marker), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			m := newTestManager(t, dir, tag, srv.URL)
			h := m.Handler()
			if resp := get(t, h, "/"); resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("stale install status = %d; want 503 before replacement", resp.StatusCode)
			}

			m.Start()
			defer m.Close(context.Background())
			waitFor(t, 5*time.Second, func() bool {
				resp := get(t, h, "/")
				return resp.StatusCode == http.StatusOK &&
					bytes.Contains([]byte(body(t, resp)), []byte("CURRENT"))
			})

			if n := rel.requests.Load(); n != 2 {
				t.Fatalf("made %d network requests; want checksum + tarball", n)
			}
			if got, err := os.ReadFile(filepath.Join(installed, installedVersionFile)); err != nil || string(got) != tag {
				t.Fatalf("installed version = %q, %v; want %q", got, err, tag)
			}
		})
	}
}

func TestCorruptChecksumThenRecovers(t *testing.T) {
	const tag = "v0.1.0"
	tarball, sum := buildDistTarGz(t, map[string]string{"index.html": "OK"})
	rel := &fakeRelease{tag: tag, tarball: tarball, sumsLine: "deadbeef  web-console-dist-" + tag + ".tar.gz"}
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()

	dir := t.TempDir()
	m := newTestManager(t, dir, tag, srv.URL)
	h := m.Handler()
	m.Start()
	defer m.Close(context.Background())

	// First attempt fails on checksum; placeholder must persist and no partial
	// install may appear.
	waitFor(t, 5*time.Second, func() bool { return rel.requests.Load() >= 2 })
	if _, err := os.Stat(filepath.Join(dir, tag)); !os.IsNotExist(err) {
		t.Fatalf("partial install appeared after bad checksum: %v", err)
	}
	if resp := get(t, h, "/"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status after bad checksum = %d; want 503", resp.StatusCode)
	}

	rel.sumsLine = sum + "  web-console-dist-" + tag + ".tar.gz"
	waitFor(t, 5*time.Second, func() bool { return get(t, h, "/").StatusCode == http.StatusOK })
}

func TestOfflineKeepsPlaceholderAndCloseReturns(t *testing.T) {
	const tag = "v0.1.0"
	rel := &fakeRelease{tag: tag}
	rel.fail.Store(true)
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()

	m := newTestManager(t, t.TempDir(), tag, srv.URL)
	m.backoff = func(int) time.Duration { return time.Hour } // park after first failure
	h := m.Handler()
	m.Start()

	waitFor(t, 5*time.Second, func() bool { return rel.requests.Load() >= 1 })
	if resp := get(t, h, "/"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("offline status = %d; want 503", resp.StatusCode)
	}

	done := make(chan struct{})
	go func() { m.Close(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return promptly while loop was backing off")
	}
}

func TestInstallCleansUpOldVersions(t *testing.T) {
	const tag = "v0.1.0"
	tarball, sum := buildDistTarGz(t, map[string]string{"index.html": "OK"})
	rel := &fakeRelease{tag: tag, tarball: tarball, sumsLine: sum + "  web-console-dist-" + tag + ".tar.gz"}
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()

	dir := t.TempDir()
	for _, stale := range []string{"v0.0.9", ".tmp-stale"} {
		if err := os.MkdirAll(filepath.Join(dir, stale), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Unrelated operator data in a shared WebUIDir must survive cleanup.
	for _, keep := range []string{"backups", "notes"} {
		if err := os.MkdirAll(filepath.Join(dir, keep), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "operator.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestManager(t, dir, tag, srv.URL)
	h := m.Handler()
	m.Start()
	defer m.Close(context.Background())
	waitFor(t, 5*time.Second, func() bool { return get(t, h, "/").StatusCode == http.StatusOK })

	for _, stale := range []string{"v0.0.9", ".tmp-stale"} {
		if _, err := os.Stat(filepath.Join(dir, stale)); !os.IsNotExist(err) {
			t.Fatalf("stale entry %s survived install", stale)
		}
	}
	for _, keep := range []string{"backups", "notes", "operator.txt"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Fatalf("unrelated entry %s was deleted by cleanup: %v", keep, err)
		}
	}
}

func TestTarEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	tarball, _ := buildDistTarGz(t, map[string]string{"../escape.txt": "evil", "index.html": "x"})
	tarPath := filepath.Join(dir, "evil.tar.gz")
	if err := os.WriteFile(tarPath, tarball, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractTo(t.TempDir(), tarPath); err == nil {
		t.Fatal("extractTo accepted a .. escape entry")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("escape file was written outside the extract dir")
	}
}

func TestDevVersionNeverDownloads(t *testing.T) {
	rel := &fakeRelease{tag: "dev"}
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()

	m := newTestManager(t, t.TempDir(), "dev", srv.URL)
	m.Start()
	defer m.Close(context.Background())

	if resp := get(t, m.Handler(), "/"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dev / status = %d; want 503 placeholder", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)
	if n := rel.requests.Load(); n != 0 {
		t.Fatalf("dev build made %d network requests; want 0", n)
	}
}

func TestLocalOverrideServesDirectly(t *testing.T) {
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "index.html"), []byte("LOCAL-DIST"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envLocalDir, local)

	rel := &fakeRelease{tag: "v0.1.0"}
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()

	m := newTestManager(t, t.TempDir(), "v0.1.0", srv.URL)
	m.Start()
	defer m.Close(context.Background())

	resp := get(t, m.Handler(), "/")
	if resp.StatusCode != http.StatusOK || !bytes.Contains([]byte(body(t, resp)), []byte("LOCAL-DIST")) {
		t.Fatalf("local override not served (status %d)", resp.StatusCode)
	}
	if n := rel.requests.Load(); n != 0 {
		t.Fatalf("local override made %d network requests; want 0", n)
	}
}

// The embedded path is what ships in the Store builds: it must serve the
// bundled SPA and must never touch the network, even with Start called and a
// non-dev version stamped.
func TestEmbeddedServesBundledDistWithoutNetwork(t *testing.T) {
	bundled := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>BUNDLED-SPA</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("js")},
	}

	m := NewEmbedded(bundled)
	m.logf = t.Logf
	m.client = nil // any download attempt would nil-panic rather than pass silently
	m.Start()
	defer m.Close(context.Background())

	resp := get(t, m.Handler(), "/")
	if resp.StatusCode != http.StatusOK || !bytes.Contains([]byte(body(t, resp)), []byte("BUNDLED-SPA")) {
		t.Fatalf("bundled dist not served (status %d)", resp.StatusCode)
	}
	// Deep routes still reach the SPA shell (vue-router history mode).
	if resp := get(t, m.Handler(), "/targets/42"); resp.StatusCode != http.StatusOK {
		t.Fatalf("deep route status = %d; want 200 index fallback", resp.StatusCode)
	}
	if resp := get(t, m.Handler(), "/assets/app.js"); resp.StatusCode != http.StatusOK {
		t.Fatalf("bundled asset status = %d; want 200", resp.StatusCode)
	}
}

// Once ci/fetchwebui has run, the embed directory holds a *released* console.
// On a dev build that must not shadow the developer's own build: `go run` would
// silently serve the release and local frontend edits would appear to do
// nothing. A stamped (packaged) build keeps serving its embed, which is the
// whole point of bundling and must not depend on the working directory.
func TestEmbeddedDevPrefersSiblingDistButStampedDoesNot(t *testing.T) {
	// Lay out <root>/web-console/dist next to a working directory, so the
	// const defaultLocalDir ("../web-console/dist") resolves to it.
	root := t.TempDir()
	local := filepath.Join(root, "web-console", "dist")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "index.html"), []byte("<html>LOCAL-SPA</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "desktop")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	embedded := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>EMBEDDED-SPA</html>")}}

	for _, tc := range []struct {
		version string
		want    string
	}{
		{version: "dev", want: "LOCAL-SPA"},
		{version: "v1.2.3", want: "EMBEDDED-SPA"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			prev := Version
			Version = tc.version
			defer func() { Version = prev }()

			m := NewEmbedded(embedded)
			m.logf = t.Logf
			m.client = nil // a download attempt would nil-panic rather than pass silently

			got := body(t, get(t, m.Handler(), "/"))
			if !bytes.Contains([]byte(got), []byte(tc.want)) {
				t.Fatalf("version %q served %q; want %s", tc.version, got, tc.want)
			}
		})
	}
}

// An explicit NETTACT_WEBUI_LOCAL outranks the sibling-dist convenience path.
func TestEmbeddedLocalEnvBeatsSiblingDist(t *testing.T) {
	root := t.TempDir()
	sibling := filepath.Join(root, "web-console", "dist")
	explicit := filepath.Join(root, "explicit")
	for dir, marker := range map[string]string{sibling: "SIBLING-SPA", explicit: "EXPLICIT-SPA"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>"+marker+"</html>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	work := filepath.Join(root, "desktop")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	t.Setenv(envLocalDir, explicit)

	m := NewEmbedded(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>EMBEDDED-SPA</html>")}})
	m.logf = t.Logf

	if got := body(t, get(t, m.Handler(), "/")); !bytes.Contains([]byte(got), []byte("EXPLICIT-SPA")) {
		t.Fatalf("served %q; want the NETTACT_WEBUI_LOCAL dist", got)
	}
}

// A build assembled without the fetch step must still start and serve the API;
// the console degrades to a page that names the omission.
func TestEmbeddedWithoutIndexServesMissingBundlePage(t *testing.T) {
	m := NewEmbedded(fstest.MapFS{".gitkeep": &fstest.MapFile{}})
	m.logf = t.Logf
	m.Start()
	defer m.Close(context.Background())

	resp := get(t, m.Handler(), "/")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("missing bundle status = %d; want 503", resp.StatusCode)
	}
	if !bytes.Contains([]byte(body(t, resp)), []byte("NETTACT_WEBUI_LOCAL")) {
		t.Fatal("missing-bundle page should name the local-dist escape hatch")
	}
}

// NETTACT_WEBUI_LOCAL outranks the bundled dist so a developer can iterate
// against a live Vite build without repacking the binary.
func TestEmbeddedLocalOverrideWins(t *testing.T) {
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "index.html"), []byte("LOCAL-DIST"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envLocalDir, local)

	m := NewEmbedded(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("BUNDLED-SPA")}})
	m.logf = t.Logf

	resp := get(t, m.Handler(), "/")
	if !bytes.Contains([]byte(body(t, resp)), []byte("LOCAL-DIST")) {
		t.Fatal("local override should outrank the bundled dist")
	}
}
