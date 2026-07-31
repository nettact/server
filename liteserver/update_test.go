package liteserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nettact/server-core/store/storetest"
	"github.com/nettact/server-core/updatecheck"
)

// releaseCatalog stands in for d.nettact.org's public release catalog.
func releaseCatalog(t *testing.T, serverTag, agentTag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/releases" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"products": []map[string]string{
			{"id": "server-lite", "latestTag": serverTag},
			{"id": "desktop", "latestTag": "v9.9.9"},
			{"id": "agent", "latestTag": agentTag},
		}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A standalone server reports its own update status through server-info, which
// is what the console's banner and the agent list's update icons read.
func TestServerInfoCarriesUpdateStatus(t *testing.T) {
	catalog := releaseCatalog(t, "v99.0.0", "v98.0.0")
	t.Setenv(updatecheck.EnvBaseURL, catalog.URL)

	srv := startDesktopTestServer(t, filepath.Join(storetest.Dir(t), "update.db"), time.Minute)

	// The daily worker runs its first check at startup, but off the goroutine that
	// Start returns on — drive one synchronously so the assertion is not timing
	// dependent.
	st, err := srv.CheckUpdatesNow(context.Background())
	if err != nil {
		t.Fatalf("CheckUpdatesNow: %v", err)
	}
	// This test server passes a Desktop config with no AppVersion, so the current
	// version is empty — unparsable, therefore older than any release.
	if !st.UpdateAvailable || st.LatestVersion != "v9.9.9" {
		t.Fatalf("CheckUpdatesNow = %+v; want the desktop product's v9.9.9", st)
	}
	if st.InstallType != updatecheck.InstallDesktop {
		t.Errorf("InstallType = %q, want %q", st.InstallType, updatecheck.InstallDesktop)
	}
	if st.DownloadURL != updatecheck.DownloadPageURL {
		t.Errorf("DownloadURL = %q, want the download center", st.DownloadURL)
	}
	if st.LatestAgentVersion != "v98.0.0" {
		t.Errorf("LatestAgentVersion = %q, want v98.0.0", st.LatestAgentVersion)
	}

	var info struct {
		Update *updatecheck.Status `json:"update"`
	}
	getJSON(t, srv, "/api/v1/server-info", &info)
	if info.Update == nil {
		t.Fatal("server-info carried no update block after a successful check")
	}
	if info.Update.LatestVersion != "v9.9.9" || !info.Update.UpdateAvailable {
		t.Errorf("server-info update = %+v", info.Update)
	}
}

// The notice switch lives in server settings precisely so the desktop tray and
// the web console share it: turning notices off in one silences the other.
func TestUpdateNoticeSwitchIsShared(t *testing.T) {
	srv := startDesktopTestServer(t, filepath.Join(storetest.Dir(t), "notice.db"), time.Minute)
	ctx := context.Background()

	if srv.UpdateNoticesDisabled(ctx) {
		t.Fatal("update notices start disabled; want enabled")
	}
	if err := srv.SetUpdateNoticesDisabled(ctx, true); err != nil {
		t.Fatalf("SetUpdateNoticesDisabled: %v", err)
	}
	if !srv.UpdateNoticesDisabled(ctx) {
		t.Error("notice switch did not persist")
	}

	// The console reads the same value through the generic settings API.
	var all map[string]string
	getJSON(t, srv, "/api/v1/settings", &all)
	if all["update_notice_disabled"] != "1" {
		t.Errorf("settings = %v; want update_notice_disabled=1", all)
	}

	if err := srv.SetUpdateNoticesDisabled(ctx, false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if srv.UpdateNoticesDisabled(ctx) {
		t.Error("notice switch did not clear")
	}
}

// Switching update checking off entirely must leave every path harmless.
func TestUpdateCheckingOffSwitch(t *testing.T) {
	t.Setenv(updatecheck.EnvBaseURL, "off")
	srv := startDesktopTestServer(t, filepath.Join(storetest.Dir(t), "off.db"), time.Minute)

	if _, err := srv.CheckUpdatesNow(context.Background()); err == nil {
		t.Error("CheckUpdatesNow succeeded with update checking off")
	}
	var info map[string]any
	getJSON(t, srv, "/api/v1/server-info", &info)
	if _, ok := info["update"]; ok {
		t.Error("server-info carried an update block with update checking off")
	}
}

// getJSON performs an authenticated GET, redeeming a one-time login URL for the
// session cookie the way the desktop tray does.
func getJSON(t *testing.T, srv *Server, path string, out any) {
	t.Helper()
	loginURL, err := srv.MintLoginURL(LoginTarget{})
	if err != nil {
		t.Fatalf("MintLoginURL: %v", err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(loginURL) //nolint:gosec // loopback one-time URL under test
	if err != nil {
		t.Fatalf("redeem login: %v", err)
	}
	_ = resp.Body.Close()
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("redeem login returned %d cookies", len(cookies))
	}

	req, _ := http.NewRequest(http.MethodGet, srv.BaseURL()+path, nil)
	req.AddCookie(cookies[0])
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}
