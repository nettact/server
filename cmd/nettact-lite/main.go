// Command nettact-lite is the self-hosted Lite server: a single binary that
// wires the server-core modules over one SQLite database (architecture §7).
// M1 serves only the telemetry ingest + read API; login, enrollment, alerting
// and the embedded web UI arrive in later milestones.
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
	"github.com/nettact/server-core/eventbus"
	"github.com/nettact/server-core/ingest"
	"github.com/nettact/server-core/registry"
	"github.com/nettact/server-core/site"
	"github.com/nettact/server-core/store"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "./nettact.db", "SQLite database path")
	dev := flag.Bool("dev", false, "dev mode: open auth + CORS for the Vite origin")
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

	bus := eventbus.New()
	reg := registry.New(db)
	ing := ingest.New(db, bus)

	handler := api.Router(api.Deps{Ingest: ing, Registry: reg, Site: siteSvc, Dev: *dev})
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("nettact-lite listening on %s (dev=%v, db=%s)", *addr, *dev, *dbPath)
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
