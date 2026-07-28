package liteserver

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/nettact/server-core/settings"
	"github.com/nettact/server-core/store"
	"github.com/nettact/server-core/store/storetest"
)

// seedConsoleBaseURL writes a console_base_url setting into a (possibly new) DB
// before the server starts, simulating a value the user saved on a prior run.
func seedConsoleBaseURL(t *testing.T, dbPath, v string) {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := settings.New(db).Set(context.Background(), settings.KeyConsoleBaseURL, v); err != nil {
		t.Fatalf("seed console_base_url: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
}

func storedConsoleBaseURL(t *testing.T, srv *Server) string {
	t.Helper()
	v, err := srv.setSvc.Get(context.Background(), settings.KeyConsoleBaseURL)
	if err != nil {
		t.Fatalf("read console_base_url: %v", err)
	}
	return v
}

// TestConsoleBaseURLSeedAndPreserve pins who owns the deep-link origin. The
// desktop seeds it (notifications need somewhere to point on a fresh install)
// and refreshes its own loopback seed when the bound port moves — but a value
// the user configured is the only one a notification recipient on another
// machine can open, so it must survive every restart untouched.
func TestConsoleBaseURLSeedAndPreserve(t *testing.T) {
	ctx := context.Background()

	t.Run("user value preserved", func(t *testing.T) {
		dbPath := filepath.Join(storetest.Dir(t), "console-user.db")
		const lan = "http://192.168.1.9:12450"
		seedConsoleBaseURL(t, dbPath, lan)

		srv := startDesktopTestServer(t, dbPath, time.Minute)
		if got := storedConsoleBaseURL(t, srv); got != lan {
			t.Fatalf("console_base_url = %q; want the user's %q untouched", got, lan)
		}
		if got := srv.ConsoleBaseURL(ctx); got != lan {
			t.Fatalf("ConsoleBaseURL = %q; want %q", got, lan)
		}
		if srv.BaseURL() == lan {
			t.Fatal("BaseURL must stay loopback regardless of the console setting")
		}
	})

	t.Run("stale loopback seed refreshed", func(t *testing.T) {
		dbPath := filepath.Join(storetest.Dir(t), "console-stale.db")
		// A seed from an earlier launch whose port is long gone.
		seedConsoleBaseURL(t, dbPath, "http://127.0.0.1:59999")

		srv := startDesktopTestServer(t, dbPath, time.Minute)
		if got := storedConsoleBaseURL(t, srv); got != srv.BaseURL() {
			t.Fatalf("console_base_url = %q; want it refreshed to %q", got, srv.BaseURL())
		}
	})

	t.Run("seeded when unset and falls back when cleared", func(t *testing.T) {
		dbPath := filepath.Join(storetest.Dir(t), "console-seed.db")
		srv := startDesktopTestServer(t, dbPath, time.Minute)
		if got := storedConsoleBaseURL(t, srv); got != srv.BaseURL() {
			t.Fatalf("console_base_url = %q; want seeded %q", got, srv.BaseURL())
		}

		// Clearing the setting deletes the row; the accessor then falls back to the
		// loopback origin rather than handing out an empty deep-link base.
		if err := srv.setSvc.Set(ctx, settings.KeyConsoleBaseURL, ""); err != nil {
			t.Fatalf("clear console_base_url: %v", err)
		}
		if got := srv.ConsoleBaseURL(ctx); got != srv.BaseURL() {
			t.Fatalf("ConsoleBaseURL after clear = %q; want fallback %q", got, srv.BaseURL())
		}
	})

	t.Run("standalone writes nothing", func(t *testing.T) {
		dbPath := filepath.Join(storetest.Dir(t), "console-standalone.db")
		srv, err := Start(ctx, Config{
			Addr:      "127.0.0.1:0",
			DBPath:    dbPath,
			AdminUser: "admin",
			AdminPass: "test-password",
			MaxAgents: 5,
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() {
			c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(c)
		})
		if got := storedConsoleBaseURL(t, srv); got != "" {
			t.Fatalf("standalone wrote console_base_url = %q; want unset", got)
		}
	})
}

// TestBaseOriginAndEffectiveAddr covers the two address derivations without
// binding anything: a real 0.0.0.0 bind would trip the Windows firewall prompt,
// and the interesting input — the dual-stack "[::]" a wildcard bind reports — is
// exactly what the pure functions have to normalize away.
func TestBaseOriginAndEffectiveAddr(t *testing.T) {
	wildcard6 := &net.TCPAddr{IP: net.IPv6unspecified, Port: 12450}
	wildcard4 := &net.TCPAddr{IP: net.IPv4zero, Port: 12450}
	loopback := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 54321}

	t.Run("baseOrigin", func(t *testing.T) {
		cases := []struct {
			name   string
			scheme string
			bound  net.Addr
			want   string
		}{
			// Only a wildcard host is substituted — it is undialable as printed.
			{"dual-stack wildcard", "http", wildcard6, "http://127.0.0.1:12450"},
			{"ipv4 wildcard", "http", wildcard4, "http://127.0.0.1:12450"},
			{"tls wildcard", "https", wildcard6, "https://127.0.0.1:12450"},
			{"already loopback", "http", loopback, "http://127.0.0.1:54321"},
			// A specific bound host is the only address known to reach the listener:
			// an IPv6-only or single-interface bind would not answer on 127.0.0.1.
			{"ipv6 loopback preserved", "http",
				&net.TCPAddr{IP: net.IPv6loopback, Port: 12450}, "http://[::1]:12450"},
			{"lan interface preserved", "http",
				&net.TCPAddr{IP: net.IPv4(192, 168, 1, 5), Port: 8080}, "http://192.168.1.5:8080"},
		}
		for _, c := range cases {
			if got := baseOrigin(c.scheme, c.bound); got != c.want {
				t.Errorf("%s: baseOrigin = %q; want %q", c.name, got, c.want)
			}
		}
	})

	t.Run("effectiveAddr", func(t *testing.T) {
		cases := []struct {
			name       string
			configured string
			bound      net.Addr
			want       string
		}{
			// The report must echo the saved setting, or server-info's string compare
			// against it shows a permanent "restart pending".
			{"wildcard stays as configured", "0.0.0.0:12450", wildcard6, "0.0.0.0:12450"},
			{"os-assigned port resolved", "127.0.0.1:0", loopback, "127.0.0.1:54321"},
			{"hostless config falls back", ":12450", wildcard6, "[::]:12450"},
			{"malformed config falls back", "nonsense", wildcard4, "0.0.0.0:12450"},
		}
		for _, c := range cases {
			if got := effectiveAddr(c.configured, c.bound); got != c.want {
				t.Errorf("%s: effectiveAddr = %q; want %q", c.name, got, c.want)
			}
		}
	})
}

// TestIsLoopbackOrigin pins which stored console_base_url values this package may
// overwrite: only ones that could not possibly help someone on another machine.
func TestIsLoopbackOrigin(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"http://127.0.0.1:12450", true},
		{"http://127.0.0.1", true},
		{"http://localhost:12450", true},
		{"http://LocalHost:12450", true},
		{"http://[::1]:12450", true},
		{"https://127.0.0.1:12450", true},
		{"http://192.168.1.9:12450", false},
		{"https://nas.example.com", false},
		{"http://0.0.0.0:12450", false},
		{"", false},
		// No scheme: url.Parse reads "127.0.0.1" as the scheme and leaves the host
		// empty, so this is treated as user-authored and preserved.
		{"127.0.0.1:12450", false},
	}
	for _, c := range cases {
		if got := isLoopbackOrigin(c.v); got != c.want {
			t.Errorf("isLoopbackOrigin(%q) = %v; want %v", c.v, got, c.want)
		}
	}
}
