// Command nettact-lite is the self-hosted Lite server: a single binary that
// wires the server-core modules over one SQLite database (architecture §7).
// M2 adds single-user login, ed25519 agent enrollment with a max_agents quota,
// and central monitoring-target config. Agents hold a persistent WebSocket to
// the server (bearer-authenticated) that carries telemetry up and config down.
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

	"github.com/nettact/server-core/agentws"
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
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate; with -tls-key serves HTTPS/WSS natively")
	tlsKey := flag.String("tls-key", "", "path to TLS private key; with -tls-cert serves HTTPS/WSS natively")
	flag.Parse()

	// TLS is all-or-nothing: with only one flag set (a missing or mistyped
	// mounted secret), silently falling back to plaintext would expose bearer
	// tokens and telemetry — refuse to start instead.
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("-tls-cert and -tls-key must be provided together")
	}

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
	bus := eventbus.New()
	cfg := config.New(db, reg, bus)
	if err := cfg.SeedDefaults(ctx, site.DefaultSiteID); err != nil {
		log.Printf("seed default targets: %v", err)
	}
	auditSvc := audit.New(db)
	invSvc := inventory.New(db)
	metricsStore := metrics.New(db)
	ing := ingest.New(db, bus, metricsStore)
	// In-memory store for ephemeral, pull-on-demand host snapshots (never persisted).
	hostLive := hostlive.New()

	// Hub for the persistent agent WebSocket channel: telemetry in, config and
	// snapshot requests out. Subscribes to config changes on the bus, so it must
	// be wired before the HTTP server starts publishing them.
	agentHub := agentws.New(agentws.Deps{
		Registry: reg,
		Ingest:   ing,
		Config:   cfg,
		HostLive: hostLive,
		Bus:      bus,
	})

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

	// Offline sweeper: liveness is connection-driven now (a live WebSocket keeps
	// the agent excluded from the sweep entirely), so the sweep only has to cover
	// agents whose socket is gone. A short grace after the final last_seen_at
	// touch absorbs reconnect blips; the tight tick makes a real disconnect show
	// as offline within seconds instead of minutes.
	const offlineGrace = 10 * time.Second
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			if n, err := reg.SweepStale(context.Background(), offlineGrace, agentHub.ConnectedIDs()); err != nil {
				log.Printf("offline sweep: %v", err)
			} else if n > 0 {
				log.Printf("offline sweep: %d agent(s) marked offline", n)
			}
		}
	}()

	handler := api.Router(api.Deps{
		Identity:     idSvc,
		Registry:     reg,
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
		AgentWS:      agentHub,
		Bus:          bus,
		SPA:          webui.Handler(),
		Dev:          *dev,
		SecureCookie: !*dev,
	})
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// With both TLS flags the binary serves HTTPS/WSS natively (no reverse proxy
	// needed for a trusted agent channel); otherwise plain HTTP/WS.
	useTLS := *tlsCert != "" && *tlsKey != ""
	go func() {
		scheme := "http"
		if useTLS {
			scheme = "https"
		}
		log.Printf("nettact-lite listening on %s://%s (dev=%v, db=%s, max_agents=%d)", scheme, *addr, *dev, *dbPath, *maxAgents)
		var err error
		if useTLS {
			err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down…")
	// Close the agent WebSockets first: they are hijacked connections, which
	// srv.Shutdown neither closes nor waits for, so leaving them open would
	// burn the whole shutdown deadline for nothing.
	agentHub.CloseAll("server shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
