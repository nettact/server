// Command nettact-lite is the self-hosted Lite server: a single binary that
// wires the server-core modules over one SQLite database (architecture §7). It
// is a thin wrapper over the liteserver runtime package — it parses flags,
// installs signal handling, and calls liteserver.Start/Shutdown. All
// orchestration (DB, admin bootstrap, services, workers, listener) lives in
// liteserver, shared with the desktop all-in-one build.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nettact/server-core/metrics"
	"github.com/nettact/server-lite/liteserver"
)

func main() {
	addr := flag.String("addr", ":12450", "listen address (a listen address saved in the web console overrides this flag)")
	dbPath := flag.String("db", "./nettact.db", "SQLite database path")
	webuiDir := flag.String("webui-dir", "", "web console download/install directory (default: <db dir>/webui)")
	dev := flag.Bool("dev", false, "dev mode: open CORS for the Vite origin, non-Secure cookie")
	adminUser := flag.String("admin-user", "", "bootstrap admin username (first run only)")
	adminPass := flag.String("admin-pass", "", "bootstrap admin password (first run only)")
	maxAgents := flag.Int("max-agents", 50, "max enrolled agents (0 = unlimited)")
	// Raw only serves chart reads of ranges ≤2h (longer ranges read the rollups),
	// so its default is days, not weeks — at 1s probe intervals every extra raw
	// day is GBs of SQLite.
	retainRawDays := flag.Int("retain-raw-days", 2, "raw sample retention (days)")
	retain1mDays := flag.Int("retain-1m-days", 30, "1-minute rollup retention (days)")
	retain1hDays := flag.Int("retain-1h-days", 730, "1-hour rollup retention (days); 1-day rollups kept forever")
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate; with -tls-key serves HTTPS/WSS natively")
	tlsKey := flag.String("tls-key", "", "path to TLS private key; with -tls-cert serves HTTPS/WSS natively")
	// Browsers only send Secure cookies over HTTPS, so a Secure session cookie on a
	// plain-HTTP deployment makes every login silently fail (except on localhost).
	// auto ties the flag to how THIS process serves; true is for TLS-terminating
	// reverse proxies (browser sees https, we serve http).
	secureCookie := flag.String("secure-cookie", "auto",
		"session cookie Secure attribute: auto (set iff TLS is enabled), true (always; use behind a TLS-terminating reverse proxy), false (never)")
	flag.Parse()

	addrFromFlag := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			addrFromFlag = true
		}
	})

	// TLS is all-or-nothing: with only one flag set (a missing or mistyped mounted
	// secret), silently falling back to plaintext would expose bearer tokens and
	// telemetry. The library validates this again; catching it here gives a
	// friendly message instead of a wrapped error.
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("-tls-cert and -tls-key must be provided together")
	}

	useTLS := *tlsCert != "" && *tlsKey != ""
	var secure bool
	switch *secureCookie {
	case "auto":
		secure = useTLS
	case "true":
		secure = true
	case "false":
		secure = false
	default:
		log.Fatalf("-secure-cookie must be auto, true or false (got %q)", *secureCookie)
	}
	if *dev {
		secure = false // dev mode always serves the Vite origin over plain http
	}

	srv, err := liteserver.Start(context.Background(), liteserver.Config{
		Addr:         *addr,
		AddrFromFlag: addrFromFlag,
		TLSCert:      *tlsCert,
		TLSKey:       *tlsKey,
		DBPath:       *dbPath,
		WebUIDir:     *webuiDir,
		AdminUser:    *adminUser,
		AdminPass:    *adminPass,
		Dev:          *dev,
		SecureCookie: secure,
		MaxAgents:    *maxAgents,
		Retention: metrics.RetentionConfig{
			RawSeconds: int64(*retainRawDays) * 86400,
			M1Seconds:  int64(*retain1mDays) * 86400,
			H1Seconds:  int64(*retain1hDays) * 86400,
			D1Seconds:  0, // 1-day rollups kept forever
		},
		// Desktop stays nil: standalone Lite exposes no one-time-login endpoint and
		// keeps the default -addr :12450 bind.
	})
	if err != nil {
		log.Fatalf("start: %v", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case <-stop:
		log.Println("shutting down…")
	case err := <-srv.Err():
		log.Printf("serve: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
