package webui

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const (
	defaultBaseURL = "https://github.com/nettact/web-console/releases/download"

	// installedVersionFile records the exact web-console release installed in
	// a version directory. The directory name alone is not trusted: an
	// interrupted/manual copy can leave different UI bytes under the expected
	// path. Installs without this marker are treated as stale and downloaded
	// again.
	installedVersionFile = ".installed-version"

	// envBaseURL overrides the release download base URL (mirror / air-gapped
	// deployments). Layout must match GitHub Releases: <base>/<tag>/<asset>.
	envBaseURL = "NETTACT_WEBUI_BASE_URL"

	// envLocalDir points at a locally built web-console dist (dev builds). When
	// set and valid it is served directly and downloading is disabled.
	envLocalDir = "NETTACT_WEBUI_LOCAL"

	// defaultLocalDir is tried by dev builds when envLocalDir is unset: the
	// sibling web-console checkout's build output, relative to the working
	// directory (module roots in the workspace are siblings of web-console).
	defaultLocalDir = "../web-console/dist"
)

// Manager owns the console UI lifecycle: it serves whatever is available now
// (installed SPA, local dev dist, or the placeholder) and, when the stamped
// version is missing or does not match its marker, downloads it in the background.
type Manager struct {
	dir     string // versions install to dir/<version>/
	version string
	baseURL string

	handler atomic.Pointer[http.Handler]

	started bool
	cancel  context.CancelFunc
	done    chan struct{}

	// Dependency seams, defaulted in New; tests override for hermetic runs.
	client  *http.Client
	backoff func(attempt int) time.Duration
	logf    func(format string, args ...any)
}

// New resolves the initial serving state synchronously (disk stat only, no
// network): a valid NETTACT_WEBUI_LOCAL dist wins, then an already-installed
// dir/<version>/, else the placeholder page.
func New(dir, version string) *Manager {
	m := &Manager{
		dir:     dir,
		version: version,
		baseURL: defaultBaseURL,
		client:  &http.Client{Timeout: 5 * time.Minute},
		backoff: defaultBackoff,
		logf:    log.Printf,
	}
	if v := os.Getenv(envBaseURL); v != "" {
		m.baseURL = v
	}

	if local := os.Getenv(envLocalDir); local != "" {
		if hasIndex(os.DirFS(local)) {
			m.store(spaHandler(os.DirFS(local)))
			m.started = true // nothing to download
			m.logf("webui: serving local dist from %s (%s)", local, envLocalDir)
			return m
		}
		m.logf("webui: %s=%q has no index.html; ignoring", envLocalDir, local)
	} else if version == "dev" && hasIndex(os.DirFS(defaultLocalDir)) {
		m.store(spaHandler(os.DirFS(defaultLocalDir)))
		m.started = true
		m.logf("webui: dev build, serving local dist from %s", defaultLocalDir)
		return m
	}

	installed := filepath.Join(dir, version)
	if version != "dev" && isInstalledVersion(installed, version) {
		m.store(spaHandler(os.DirFS(installed)))
		m.started = true
		return m
	}

	m.store(placeholderHandler(version == "dev"))
	return m
}

// Handler returns a stable http.Handler wired once into the router; the
// underlying implementation is swapped atomically when the SPA installs.
func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*m.handler.Load()).ServeHTTP(w, r)
	})
}

// Start launches the background download loop. It is a no-op when the SPA is
// already being served, the local override is active, or Version is "dev".
// Call at most once.
func (m *Manager) Start() {
	if m.started || m.version == "dev" {
		m.started = true
		return
	}
	m.started = true
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	go m.run(ctx)
}

// Close stops the download loop and waits for it to exit, bounded by ctx: an
// in-flight extract/rename does not observe cancellation promptly, and the
// caller's shutdown deadline must not be spent waiting on the frontend.
// Idempotent; safe if Start was never called.
func (m *Manager) Close(ctx context.Context) {
	if m.cancel == nil {
		return
	}
	m.cancel()
	select {
	case <-m.done:
	case <-ctx.Done():
		m.logf("webui: download loop still busy at shutdown deadline; abandoning wait")
	}
	m.cancel = nil
}

func (m *Manager) store(h http.Handler) {
	m.handler.Store(&h)
}

func hasIndex(fsys fs.FS) bool {
	_, err := fs.Stat(fsys, "index.html")
	return err == nil
}

// isInstalledVersion requires both a usable SPA and an exact version marker.
// Older installs without a marker are deliberately invalidated once so the
// binary's stamped WEB_CONSOLE_VERSION is guaranteed to match the served UI.
func isInstalledVersion(dir, want string) bool {
	if !hasIndex(os.DirFS(dir)) {
		return false
	}
	got, err := os.ReadFile(filepath.Join(dir, installedVersionFile))
	return err == nil && string(got) == want
}

// defaultBackoff doubles from 30s and caps at 10 minutes; the loop retries
// forever (a missing frontend is recoverable whenever connectivity returns).
func defaultBackoff(attempt int) time.Duration {
	d := 30 * time.Second << attempt
	if max := 10 * time.Minute; d > max || d <= 0 {
		return max
	}
	return d
}
