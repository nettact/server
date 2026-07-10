// Command nettact-lite is the self-hosted Lite server: a single binary that
// wires the server-core modules over one SQLite database (architecture §7).
// M2 adds single-user login, ed25519 agent enrollment with a max_agents quota,
// bearer-authenticated telemetry, and central monitoring-target config that is
// pushed to agents via the telemetry ack.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nettact/server-core/api"
	"github.com/nettact/server-core/audit"
	"github.com/nettact/server-core/config"
	"github.com/nettact/server-core/eventbus"
	"github.com/nettact/server-core/identity"
	"github.com/nettact/server-core/ingest"
	"github.com/nettact/server-core/registry"
	"github.com/nettact/server-core/site"
	"github.com/nettact/server-core/store"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "./nettact.db", "SQLite database path")
	dev := flag.Bool("dev", false, "dev mode: open CORS for the Vite origin, non-Secure cookie")
	adminUser := flag.String("admin-user", "", "bootstrap admin username (first run only)")
	adminPass := flag.String("admin-pass", "", "bootstrap admin password (first run only)")
	maxAgents := flag.Int("max-agents", 3, "max enrolled agents (0 = unlimited)")
	flag.Parse()

	db, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	siteSvc := site.New(db)
	if err := siteSvc.EnsureDefault(ctx); err != nil {
		log.Fatalf("ensure default site: %v", err)
	}

	idSvc := identity.New(db)
	if err := idSvc.EnsureAdmin(ctx, *adminUser, *adminPass); err != nil {
		log.Fatalf("bootstrap admin: %v", err)
	}

	reg := registry.New(db, *maxAgents)
	cfg := config.New(db, reg)
	if err := cfg.SeedDefaults(ctx, site.DefaultSiteID); err != nil {
		log.Printf("seed default targets: %v", err)
	}
	auditSvc := audit.New(db)
	bus := eventbus.New()
	ing := ingest.New(db, bus)

	handler := api.Router(api.Deps{
		Identity:     idSvc,
		Registry:     reg,
		Ingest:       ing,
		Config:       cfg,
		Site:         siteSvc,
		Audit:        auditSvc,
		Dev:          *dev,
		SecureCookie: !*dev,
	})
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("nettact-lite listening on %s (dev=%v, db=%s, max_agents=%d)", *addr, *dev, *dbPath, *maxAgents)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
