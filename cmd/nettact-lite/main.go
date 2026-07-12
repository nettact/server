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
	"sync"
	"syscall"
	"time"

	"github.com/nettact/server-core/alert"
	"github.com/nettact/server-core/api"
	"github.com/nettact/server-core/audit"
	"github.com/nettact/server-core/config"
	"github.com/nettact/server-core/eventbus"
	"github.com/nettact/server-core/hostlive"
	"github.com/nettact/server-core/identity"
	"github.com/nettact/server-core/incident"
	"github.com/nettact/server-core/ingest"
	"github.com/nettact/server-core/inventory"
	"github.com/nettact/server-core/metrics"
	"github.com/nettact/server-core/notification"
	"github.com/nettact/server-core/registry"
	"github.com/nettact/server-core/rules"
	"github.com/nettact/server-core/settings"
	"github.com/nettact/server-core/site"
	"github.com/nettact/server-core/store"
	"github.com/nettact/server-lite/internal/webui"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "./nettact.db", "SQLite database path")
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
	invSvc := inventory.New(db)
	metricsStore := metrics.New(db)
	bus := eventbus.New()
	ing := ingest.New(db, bus, metricsStore)
	// In-memory store for ephemeral, pull-on-demand host snapshots (never persisted).
	hostLive := hostlive.New()

	// Detection pipeline: ingest → rules → alerts → incidents → notifications.
	alertSvc := alert.New(db, bus)
	rulesSvc := rules.New(db, alertSvc, metricsStore)
	notifSvc := notification.New(db)
	settingsSvc := settings.New(db)
	incidentSvc := incident.New(db, bus, notifSvc, settingsSvc)
	incidentSvc.Wire()

	// Rule worker: evaluate on each telemetry ingest, coalesced per agent — one
	// evaluation in flight per agent, and a burst of packets while it runs marks
	// the agent dirty for exactly one re-run instead of spawning a goroutine per
	// packet (at 50 agents × 1s probes those unbounded goroutines all pile onto
	// the single write connection when they update alert state).
	var evalMu sync.Mutex
	evalRunning := map[string]bool{}
	evalDirty := map[string]bool{}
	kickEval := func(agentID, siteID string) {
		evalMu.Lock()
		if evalRunning[agentID] {
			evalDirty[agentID] = true
			evalMu.Unlock()
			return
		}
		evalRunning[agentID] = true
		evalMu.Unlock()
		go func() {
			for {
				if err := rulesSvc.EvaluateAgent(context.Background(), agentID, siteID); err != nil {
					log.Printf("rule eval (%s): %v", agentID, err)
				}
				evalMu.Lock()
				if evalDirty[agentID] {
					delete(evalDirty, agentID)
					evalMu.Unlock()
					continue
				}
				evalRunning[agentID] = false
				evalMu.Unlock()
				return
			}
		}()
	}
	bus.Subscribe(eventbus.TopicTelemetryIngested, func(m eventbus.Message) {
		ev, ok := m.Payload.(eventbus.TelemetryIngested)
		if !ok {
			return
		}
		kickEval(ev.AgentID, ev.SiteID)
	})

	// Downsampling + tiered retention jobs (long-term history stays bounded).
	retCfg := metrics.RetentionConfig{
		RawSeconds: int64(*retainRawDays) * 86400,
		M1Seconds:  int64(*retain1mDays) * 86400,
		H1Seconds:  int64(*retain1hDays) * 86400,
		D1Seconds:  0, // 1-day rollups kept forever
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			if err := metricsStore.Rollup(context.Background()); err != nil {
				log.Printf("rollup: %v", err)
			}
		}
	}()
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			if err := metricsStore.Retention(context.Background(), retCfg); err != nil {
				log.Printf("retention: %v", err)
			}
			// The agent WAL keeps at most 72h of unacked data, so week-old dedup
			// rows can never legitimately replay.
			if err := ing.PrunePackets(context.Background(), 7*24*time.Hour); err != nil {
				log.Printf("prune packets: %v", err)
			}
		}
	}()

	// Offline sweeper: flip agents to offline (and record history) when they stop
	// checking in, so the online count and status history stay truthful.
	const offlineAfter = 90 * time.Second
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			if n, err := reg.SweepStale(context.Background(), offlineAfter); err != nil {
				log.Printf("offline sweep: %v", err)
			} else if n > 0 {
				log.Printf("offline sweep: %d agent(s) marked offline", n)
			}
		}
	}()

	handler := api.Router(api.Deps{
		Identity:     idSvc,
		Registry:     reg,
		Ingest:       ing,
		Metrics:      metricsStore,
		Config:       cfg,
		Site:         siteSvc,
		Inventory:    invSvc,
		Rules:        rulesSvc,
		Alert:        alertSvc,
		Incident:     incidentSvc,
		Notification: notifSvc,
		Settings:     settingsSvc,
		Audit:        auditSvc,
		HostLive:     hostLive,
		SPA:          webui.Handler(),
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
