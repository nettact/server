package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nettact/server-core/store/storetest"
)

const (
	testIncidentID  = "inc_2f1c9a4e-6b7d-4a11-9c33-0d5e8f7a2b64"
	testIncidentID2 = "inc_9a0b1c2d-3e4f-4a5b-8c9d-0e1f2a3b4c5d"
	testStormID     = "stm_7d4e1b02-33af-4c58-8e91-6a2b0c4d5e73"
)

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// redeemLogin follows a minted URL exactly one hop and returns the redirect
// target plus how many cookies were set. Everything the desktop login promises
// is observable in those two values.
func redeemLogin(t *testing.T, client *http.Client, loginURL string) (location string, cookies int) {
	t.Helper()
	resp, err := client.Get(loginURL) //nolint:gosec // loopback one-time URL under test
	if err != nil {
		t.Fatalf("redeem login URL: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("redeem status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
	return resp.Header.Get("Location"), len(resp.Cookies())
}

// TestLoginTargetRedirects covers the reason typed targets exist: a notification
// click must land on the thing the notification was about, signed in, without
// the caller ever naming a URL.
func TestLoginTargetRedirects(t *testing.T) {
	tests := []struct {
		name   string
		target LoginTarget
		want   string
	}{
		{"console root", LoginTarget{}, "/"},
		{"incident", LoginTarget{Kind: TargetIncident, ID: testIncidentID}, "/incidents?incident=" + testIncidentID},
		{"storm", LoginTarget{Kind: TargetStorm, ID: testStormID}, "/incidents?storm=" + testStormID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := startDesktopTestServer(t, filepath.Join(storetest.Dir(t), "desktop.db"), time.Minute)
			loginURL, err := srv.MintLoginURL(tc.target)
			if err != nil {
				t.Fatalf("MintLoginURL: %v", err)
			}
			client := noRedirectClient()

			location, cookies := redeemLogin(t, client, loginURL)
			if location != tc.want {
				t.Fatalf("Location = %q, want %q", location, tc.want)
			}
			if cookies != 1 {
				t.Fatalf("set %d cookies, want exactly 1 session cookie", cookies)
			}

			// Replay: the binding is consumed with the token, so a second click on a
			// stale notification cannot re-enter the target with no session.
			location, cookies = redeemLogin(t, client, loginURL)
			if location != "/" || cookies != 0 {
				t.Fatalf("replay gave Location=%q with %d cookies; want \"/\" and none", location, cookies)
			}
		})
	}
}

// TestLoginTargetsAreBoundPerToken pins that the redirect travels with the
// token, not with the server: clicking two different incident notifications must
// open two different incidents whatever order they are redeemed in.
func TestLoginTargetsAreBoundPerToken(t *testing.T) {
	srv := startDesktopTestServer(t, filepath.Join(storetest.Dir(t), "desktop.db"), time.Minute)

	first, err := srv.MintLoginURL(LoginTarget{Kind: TargetIncident, ID: testIncidentID})
	if err != nil {
		t.Fatalf("mint first: %v", err)
	}
	second, err := srv.MintLoginURL(LoginTarget{Kind: TargetIncident, ID: testIncidentID2})
	if err != nil {
		t.Fatalf("mint second: %v", err)
	}
	if first == second {
		t.Fatal("two mints produced the same URL; tokens must be unique")
	}

	client := noRedirectClient()
	// Redeemed in reverse order on purpose.
	if location, _ := redeemLogin(t, client, second); location != "/incidents?incident="+testIncidentID2 {
		t.Fatalf("second token Location = %q", location)
	}
	if location, _ := redeemLogin(t, client, first); location != "/incidents?incident="+testIncidentID {
		t.Fatalf("first token Location = %q", location)
	}
}

// TestLoginTargetIllegalIDDegradesToRoot pins the open-redirect posture. A bad
// id must not fail the login (the user clicked a notification and wants a
// console) and must not steer the browser anywhere but the root.
func TestLoginTargetIllegalIDDegradesToRoot(t *testing.T) {
	illegal := []struct {
		name   string
		target LoginTarget
	}{
		{"javascript url", LoginTarget{Kind: TargetIncident, ID: "javascript:alert(1)"}},
		{"absolute url", LoginTarget{Kind: TargetIncident, ID: "https://evil.example.com/"}},
		{"protocol relative", LoginTarget{Kind: TargetIncident, ID: "//evil.example.com/"}},
		{"path traversal", LoginTarget{Kind: TargetIncident, ID: "../../etc/passwd"}},
		{"malformed id", LoginTarget{Kind: TargetIncident, ID: "inc_zzz"}},
		{"uppercase uuid", LoginTarget{Kind: TargetIncident, ID: strings.ToUpper(testIncidentID)}},
		{"empty id", LoginTarget{Kind: TargetIncident}},
		{"storm id under incident kind", LoginTarget{Kind: TargetIncident, ID: testStormID}},
		{"unknown kind", LoginTarget{Kind: LoginTargetKind(99), ID: testIncidentID}},
	}
	for _, tc := range illegal {
		t.Run(tc.name, func(t *testing.T) {
			srv := startDesktopTestServer(t, filepath.Join(storetest.Dir(t), "desktop.db"), time.Minute)
			loginURL, err := srv.MintLoginURL(tc.target)
			if err != nil {
				t.Fatalf("MintLoginURL: %v", err)
			}
			location, cookies := redeemLogin(t, noRedirectClient(), loginURL)
			if location != "/" {
				t.Fatalf("Location = %q, want \"/\"", location)
			}
			if cookies != 1 {
				t.Fatalf("set %d cookies; the login itself must still succeed", cookies)
			}
		})
	}
}

// TestDesktopLoginRejectsNonLoopbackWithoutBurningToken checks both halves of
// the guard: a remote caller gets nothing, and the rejection happens before
// redemption so a probe cannot invalidate the token the real browser is about
// to use.
func TestDesktopLoginRejectsNonLoopbackWithoutBurningToken(t *testing.T) {
	srv := startDesktopTestServer(t, filepath.Join(storetest.Dir(t), "desktop.db"), time.Minute)
	loginURL, err := srv.MintLoginURL(LoginTarget{Kind: TargetIncident, ID: testIncidentID})
	if err != nil {
		t.Fatalf("MintLoginURL: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, loginURL, nil)
	req.RemoteAddr = "10.1.2.3:9999"
	rec := httptest.NewRecorder()
	srv.handleDesktopLogin(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("non-loopback status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Fatalf("non-loopback Location = %q, want \"/\"", got)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("non-loopback caller received %d cookies", len(cookies))
	}

	// The same token must still work from loopback.
	location, cookies := redeemLogin(t, noRedirectClient(), loginURL)
	if location != "/incidents?incident="+testIncidentID || cookies != 1 {
		t.Fatalf("after rejected remote attempt: Location=%q cookies=%d", location, cookies)
	}
}

// TestRedirectPathRejectsEverythingUnrecognized is the unit-level table behind
// the HTTP tests above: redirectPath is the only thing that turns caller input
// into a redirect, so it must never emit anything but a same-origin path.
func TestRedirectPathRejectsEverythingUnrecognized(t *testing.T) {
	tests := []struct {
		target LoginTarget
		want   string
	}{
		{LoginTarget{}, "/"},
		{LoginTarget{Kind: TargetConsole, ID: testIncidentID}, "/"},
		{LoginTarget{Kind: TargetIncident, ID: testIncidentID}, "/incidents?incident=" + testIncidentID},
		{LoginTarget{Kind: TargetStorm, ID: testStormID}, "/incidents?storm=" + testStormID},
		{LoginTarget{Kind: TargetIncident, ID: testStormID}, "/"},
		{LoginTarget{Kind: TargetStorm, ID: testIncidentID}, "/"},
		{LoginTarget{Kind: TargetIncident, ID: strings.ToUpper(testIncidentID)}, "/"},
		{LoginTarget{Kind: TargetIncident, ID: "inc_"}, "/"},
		{LoginTarget{Kind: TargetIncident, ID: testIncidentID + "x"}, "/"},
		{LoginTarget{Kind: TargetIncident, ID: " " + testIncidentID}, "/"},
		{LoginTarget{Kind: TargetIncident, ID: testIncidentID + "&next=https://evil.example.com"}, "/"},
		{LoginTarget{Kind: LoginTargetKind(42), ID: testIncidentID}, "/"},
	}
	for _, tc := range tests {
		got := tc.target.redirectPath()
		if got != tc.want {
			t.Fatalf("redirectPath(%+v) = %q, want %q", tc.target, got, tc.want)
		}
		if !strings.HasPrefix(got, "/") || strings.HasPrefix(got, "//") {
			t.Fatalf("redirectPath(%+v) = %q is not a same-origin path", tc.target, got)
		}
	}
}

// TestMintLoginURLUsesLoopbackBaseURL is the acceptance criterion that started
// this work: the login URL must be built on the address the server is really
// listening on, never on console_base_url, or the session lands in a different
// browser origin than the one that gets the cookie.
func TestMintLoginURLUsesLoopbackBaseURL(t *testing.T) {
	srv := startDesktopTestServer(t, filepath.Join(storetest.Dir(t), "desktop.db"), time.Minute)
	loginURL, err := srv.MintLoginURL(LoginTarget{Kind: TargetIncident, ID: testIncidentID})
	if err != nil {
		t.Fatalf("MintLoginURL: %v", err)
	}
	if !strings.HasPrefix(loginURL, srv.BaseURL()+"/desktop/login?token=") {
		t.Fatalf("login URL %q is not built on BaseURL %q", loginURL, srv.BaseURL())
	}
	if !strings.HasPrefix(loginURL, "http://127.0.0.1:") {
		t.Fatalf("login URL %q is not loopback", loginURL)
	}
	// The target belongs to the token record, not the URL — a URL carrying the
	// incident id would leak the target to anything that sees the browser history.
	if strings.Contains(loginURL, testIncidentID) {
		t.Fatalf("login URL leaks the target id: %q", loginURL)
	}
}

// TestMintLoginURLRequiresDesktopMode pins that a standalone server has no
// one-time-login surface at all.
func TestMintLoginURLRequiresDesktopMode(t *testing.T) {
	srv := &Server{}
	if _, err := srv.MintLoginURL(LoginTarget{}); err != ErrNotDesktop {
		t.Fatalf("MintLoginURL error = %v, want %v", err, ErrNotDesktop)
	}
}
