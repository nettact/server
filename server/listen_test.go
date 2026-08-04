package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nettact/server-core/settings"
	"github.com/nettact/server-core/store"
	"github.com/nettact/server-core/store/storetest"
)

// seedListenAddr writes a listen_addr setting into a (possibly new) DB before
// the server starts, simulating a value saved from the console on a prior run.
func seedListenAddr(t *testing.T, dbPath, addr string) {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := settings.New(db).Set(context.Background(), settings.KeyListenAddr, addr); err != nil {
		t.Fatalf("seed listen_addr: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
}

// freePort reserves an OS-assigned port and releases it, returning the address.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestListenAddrResolvedFromDB(t *testing.T) {
	dbPath := filepath.Join(storetest.Dir(t), "listen-db.db")
	want := freePort(t)
	seedListenAddr(t, dbPath, want)

	srv, err := Start(context.Background(), Config{
		Addr:      "127.0.0.1:0", // fallback that must NOT be used
		DBPath:    dbPath,
		AdminUser: "admin",
		AdminPass: "test-password",
		MaxAgents: 5,
		Desktop:   &DesktopConfig{LoginTokenTTL: time.Minute},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	if got := srv.ln.Addr().String(); got != want {
		t.Fatalf("bound addr = %q; want db-configured %q", got, want)
	}
	if srv.listen.source != "db" || srv.listen.fallbackFrom != "" {
		t.Fatalf("resolution = %+v; want source=db", srv.listen)
	}
}

func TestListenAddrFallbackWhenDBAddrUnbindable(t *testing.T) {
	dbPath := filepath.Join(storetest.Dir(t), "listen-fallback.db")
	// Occupy a port and seed it as the configured address.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer busy.Close()
	seedListenAddr(t, dbPath, busy.Addr().String())

	srv, err := Start(context.Background(), Config{
		Addr:      "127.0.0.1:0",
		DBPath:    dbPath,
		AdminUser: "admin",
		AdminPass: "test-password",
		MaxAgents: 5,
		Desktop:   &DesktopConfig{LoginTokenTTL: time.Minute},
	})
	if err != nil {
		t.Fatalf("Start must fall back, got error: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	if srv.ln.Addr().String() == busy.Addr().String() {
		t.Fatal("server claims the busy port")
	}
	if srv.listen.source != "default" || srv.listen.fallbackFrom != busy.Addr().String() {
		t.Fatalf("resolution = %+v; want fallback from %s", srv.listen, busy.Addr().String())
	}
}

func TestStartReturnsErrListenWhenFallbackUnbindable(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer busy.Close()

	_, err = Start(context.Background(), Config{
		Addr:      busy.Addr().String(),
		DBPath:    filepath.Join(storetest.Dir(t), "listen-busy.db"),
		AdminUser: "admin",
		AdminPass: "test-password",
		MaxAgents: 5,
		Desktop:   &DesktopConfig{LoginTokenTTL: time.Minute},
	})
	if err == nil {
		t.Fatal("Start succeeded on a busy port")
	}
	if !errors.Is(err, ErrListen) {
		t.Fatalf("err = %v; want ErrListen", err)
	}
}

func TestDesktopListenChangeFiresCallback(t *testing.T) {
	dbPath := filepath.Join(storetest.Dir(t), "listen-cb.db")
	changed := make(chan string, 1)
	srv, err := Start(context.Background(), Config{
		Addr:      "127.0.0.1:0",
		DBPath:    dbPath,
		AdminUser: "admin",
		AdminPass: "test-password",
		MaxAgents: 5,
		Desktop: &DesktopConfig{
			LoginTokenTTL:       time.Minute,
			OnListenAddrChanged: func(addr string) { changed <- addr },
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	// Log in via the one-time desktop login URL to get a session cookie.
	loginURL, err := srv.MintLoginURL(LoginTarget{})
	if err != nil {
		t.Fatalf("MintLoginURL: %v", err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(loginURL) //nolint:gosec // loopback test server
	if err != nil {
		t.Fatalf("redeem login URL: %v", err)
	}
	_ = resp.Body.Close()
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %+v", cookies)
	}

	newAddr := freePort(t)
	req, _ := http.NewRequest(http.MethodPut, srv.BaseURL()+"/api/v1/settings",
		strings.NewReader(`{"listen_addr":"`+newAddr+`"}`))
	req.AddCookie(cookies[0])
	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT settings: %v", err)
	}
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", putResp.StatusCode)
	}

	select {
	case got := <-changed:
		if got != newAddr {
			t.Fatalf("callback addr = %q; want %q", got, newAddr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnListenAddrChanged did not fire")
	}
}
