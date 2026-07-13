package liteserver

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nettact/server-core/identity"
	"github.com/nettact/server-core/settings"
)

func startDesktopTestServer(t *testing.T, dbPath string, ttl time.Duration) *Server {
	t.Helper()
	srv, err := Start(context.Background(), Config{
		Addr:      "127.0.0.1:0",
		DBPath:    dbPath,
		AdminUser: "admin",
		AdminPass: "test-password",
		MaxAgents: 5,
		Desktop:   &DesktopConfig{LoginTokenTTL: ttl},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

func TestDesktopStartLoginReplayShutdownAndRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "desktop.db")
	srv := startDesktopTestServer(t, dbPath, time.Minute)

	u, err := url.Parse(srv.BaseURL())
	if err != nil {
		t.Fatalf("parse BaseURL: %v", err)
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split BaseURL host: %v", err)
	}
	port, _ := strconv.Atoi(portText)
	if host != "127.0.0.1" || port == 0 || port == 8080 {
		t.Fatalf("BaseURL = %q; want dynamic 127.0.0.1 port other than 8080", srv.BaseURL())
	}

	resp, err := http.Get(srv.BaseURL() + "/api/v1/healthz") //nolint:gosec // loopback test server
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}

	loginURL, err := srv.MintLoginURL()
	if err != nil {
		t.Fatalf("MintLoginURL: %v", err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err = client.Get(loginURL) //nolint:gosec // loopback one-time URL under test
	if err != nil {
		t.Fatalf("redeem login URL: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("redeem response = %d Location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("security headers = Cache-Control %q, Referrer-Policy %q",
			resp.Header.Get("Cache-Control"), resp.Header.Get("Referrer-Policy"))
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].Value == "" {
		t.Fatalf("session cookie = %+v", cookies)
	}

	meReq, _ := http.NewRequest(http.MethodGet, srv.BaseURL()+"/api/v1/auth/me", nil)
	meReq.AddCookie(cookies[0])
	meResp, err := http.DefaultClient.Do(meReq)
	if err != nil {
		t.Fatalf("auth/me: %v", err)
	}
	defer meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("auth/me status = %d", meResp.StatusCode)
	}
	var user identity.User
	if err := json.NewDecoder(meResp.Body).Decode(&user); err != nil || user.Username != "admin" {
		t.Fatalf("auth/me user = %+v, err=%v", user, err)
	}

	replay, err := client.Get(loginURL) //nolint:gosec // replay is the subject of the test
	if err != nil {
		t.Fatalf("replay login URL: %v", err)
	}
	_ = replay.Body.Close()
	if replay.StatusCode != http.StatusFound || len(replay.Cookies()) != 0 {
		t.Fatalf("replay response = %d cookies=%d", replay.StatusCode, len(replay.Cookies()))
	}

	stored, err := srv.setSvc.Get(context.Background(), settings.KeyConsoleBaseURL)
	if err != nil || stored != srv.BaseURL() {
		t.Fatalf("console_base_url = %q, %v; want %q", stored, err, srv.BaseURL())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		cancel()
		t.Fatalf("Shutdown: %v", err)
	}
	cancel()
	select {
	case _, ok := <-srv.Err():
		if ok {
			t.Fatal("Err channel delivered a value on clean shutdown")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Err channel did not close on clean shutdown")
	}

	second := startDesktopTestServer(t, dbPath, time.Minute)
	if !strings.HasPrefix(second.BaseURL(), "http://127.0.0.1:") {
		t.Fatalf("restart BaseURL = %q", second.BaseURL())
	}
}

func TestValidateDesktopAndStandaloneAddresses(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"desktop-dynamic-loopback", Config{Addr: "127.0.0.1:0", Desktop: &DesktopConfig{}}, true},
		{"desktop-fixed-port", Config{Addr: "127.0.0.1:8081", Desktop: &DesktopConfig{}}, false},
		{"desktop-lan", Config{Addr: "0.0.0.0:0", Desktop: &DesktopConfig{}}, false},
		{"desktop-tls", Config{Addr: "127.0.0.1:0", TLSCert: "cert", TLSKey: "key", Desktop: &DesktopConfig{}}, false},
		{"standalone-any", Config{Addr: ":8080"}, true},
		{"partial-tls", Config{Addr: ":8080", TLSCert: "cert"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.cfg)
			if (err == nil) != tc.ok {
				t.Fatalf("validate(%+v) = %v; ok=%v", tc.cfg, err, tc.ok)
			}
		})
	}
}

func TestLoginTokensAreBoundedExpiringAndSingleUse(t *testing.T) {
	tokens := newLoginTokens(-time.Second)
	expired, err := tokens.mint()
	if err != nil {
		t.Fatalf("mint expired token: %v", err)
	}
	if tokens.redeem(expired) {
		t.Fatal("expired token redeemed")
	}

	tokens = newLoginTokens(time.Minute)
	issued := make([]string, maxOutstandingLoginTokens+1)
	for i := range issued {
		issued[i], err = tokens.mint()
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if len(tokens.m) != maxOutstandingLoginTokens {
		t.Fatalf("token store size = %d; want %d", len(tokens.m), maxOutstandingLoginTokens)
	}
	last := issued[len(issued)-1]
	if !tokens.redeem(last) || tokens.redeem(last) {
		t.Fatal("token was not exactly single-use")
	}
	survivingOld := 0
	for _, token := range issued[:len(issued)-1] {
		if tokens.redeem(token) {
			survivingOld++
		}
	}
	if survivingOld != maxOutstandingLoginTokens-1 {
		t.Fatalf("surviving pre-capacity tokens = %d; want %d", survivingOld, maxOutstandingLoginTokens-1)
	}
}

func TestWorkersStopGatesNewWork(t *testing.T) {
	w := newWorkers()
	if !w.add() {
		t.Fatal("initial worker reservation failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if w.stop(ctx) {
		t.Fatal("stop reported success while a reserved worker was still running")
	}
	if w.add() {
		t.Fatal("worker reservation succeeded after stop began")
	}
	w.wg.Done()
}
