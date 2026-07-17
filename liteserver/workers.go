package liteserver

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/nettact/server-core/agentws"
	"github.com/nettact/server-core/alert"
	"github.com/nettact/server-core/eventbus"
	"github.com/nettact/server-core/identity"
	"github.com/nettact/server-core/incidentops"
	"github.com/nettact/server-core/ingest"
	"github.com/nettact/server-core/metrics"
	"github.com/nettact/server-core/registry"
)

// workers owns every background goroutine on one context so shutdown is a single
// cancel + wait. Nothing here outlives stop(), so a months-long tray run grows no
// goroutines and never writes through a closed DB.
type workers struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	stopping bool
	wg       sync.WaitGroup
}

func newWorkers() *workers {
	ctx, cancel := context.WithCancel(context.Background())
	return &workers{ctx: ctx, cancel: cancel}
}

// add reserves a wg slot unless shutdown has begun, serializing with stop via mu
// so a positive Add can never race the wg.Wait in stop (WaitGroup misuse) nor
// spawn a worker that outlives shutdown and writes through a closed DB. Callers
// that get true own exactly one wg.Done.
func (w *workers) add() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopping {
		return false
	}
	w.wg.Add(1)
	return true
}

// every runs fn on a ticker until the workers context is cancelled. fn does its
// own error logging; every only owns the schedule and the wg bookkeeping.
func (w *workers) every(d time.Duration, fn func(context.Context)) {
	if !w.add() {
		return
	}
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-t.C:
				fn(w.ctx)
			}
		}
	}()
}

// stop cancels all workers and waits for them to return, bounded by ctx. It
// reports whether every worker actually stopped: a false return means the
// deadline fired first and workers may still be running, so the caller must NOT
// close the DB (that would be a use-after-close).
func (w *workers) stop(ctx context.Context) bool {
	w.mu.Lock()
	w.stopping = true
	w.mu.Unlock()
	w.cancel()
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// deps bundles the services the periodic and event-driven workers need.
type deps struct {
	metrics     *metrics.Store
	ingest      *ingest.Service
	identity    *identity.Service
	registry    *registry.Service
	incidentops *incidentops.Service
	bus         *eventbus.Bus
	hub         *agentws.Hub
	ret         metrics.RetentionConfig
}

func startWorkers(w *workers, d deps) {
	// Downsampling: raw → 1m/1h/1d rollups.
	w.every(time.Minute, func(ctx context.Context) {
		if err := d.metrics.Rollup(ctx); err != nil {
			log.Printf("rollup: %v", err)
		}
	})

	// Tiered retention + dedup-row prune + expired-session prune, hourly. Session
	// pruning matters for the desktop: one session row is created per launch and
	// per tray activation, and ValidateSession only reaps rows it happens to see.
	w.every(time.Hour, func(ctx context.Context) {
		if err := d.metrics.Retention(ctx, d.ret); err != nil {
			log.Printf("retention: %v", err)
		}
		// The agent WAL keeps at most 72h of unacked data, so week-old dedup rows
		// can never legitimately replay.
		if err := d.ingest.PrunePackets(ctx, 7*24*time.Hour); err != nil {
			log.Printf("prune packets: %v", err)
		}
		if _, err := d.identity.PruneSessions(ctx); err != nil {
			log.Printf("prune sessions: %v", err)
		}
		// Evidence retention: drop agent-collected snapshot detail and shared trace
		// hop detail for incidents resolved past the retention window, marking them
		// evidence_expired while preserving the incident/alert/evidence summaries.
		if err := d.incidentops.Retention(ctx); err != nil {
			log.Printf("incidentops retention: %v", err)
		}
	})

	// Incident snapshot/trace maintenance on a short managed interval: finalize
	// snapshots past their deadline, time out expired traces, close orphaned
	// cohorts and rehydrate the eligible queued trace work. Idempotent and cheap
	// when idle. Runs on the workers context/waitgroup, so shutdown waits for an
	// in-flight tick and it never writes through a closed DB.
	w.every(5*time.Second, func(ctx context.Context) {
		if err := d.incidentops.Tick(ctx); err != nil {
			log.Printf("incidentops tick: %v", err)
		}
	})

	// Offline sweeper: a live WebSocket keeps an agent out of the sweep, so this
	// only covers agents whose socket is gone. A short grace absorbs reconnect
	// blips; the tight tick surfaces a real disconnect within seconds.
	const offlineGrace = 10 * time.Second
	w.every(5*time.Second, func(ctx context.Context) {
		if n, err := d.registry.SweepStale(ctx, offlineGrace, d.hub.ConnectedIDs()); err != nil {
			log.Printf("offline sweep: %v", err)
		} else if n > 0 {
			log.Printf("offline sweep: %d agent(s) marked offline", n)
		}
	})
}

// wireIncidentOps registers the incident snapshot + traceroute orchestration's
// post-commit event subscriptions. The fault engine publishes these off its write
// transaction, so the handlers run synchronously on the publisher's goroutine
// (the coalesced rule-eval worker for telemetry-driven faults, or the HTTP
// request goroutine for a configuration-driven termination) — never inside the
// rule transaction and never on an unmanaged goroutine of our own. Each handler
// opens its own DB transaction under the workers context, which is cancelled
// before the DB is closed, so no handler writes through a closed DB.
func wireIncidentOps(w *workers, bus *eventbus.Bus, io *incidentops.Service) {
	if bus == nil || io == nil {
		return
	}
	// Incident opened -> one collecting snapshot entry + IncidentSnapshotRequest
	// dispatched to each distinct involved Agent.
	bus.Subscribe(eventbus.TopicIncidentOpened, func(m eventbus.Message) {
		ev, ok := m.Payload.(eventbus.IncidentEvent)
		if !ok {
			return
		}
		if err := io.OnIncidentOpened(w.ctx, ev); err != nil {
			log.Printf("incidentops: incident-opened snapshot dispatch (%s): %v", ev.IncidentID, err)
		}
	})
	// Evidence added -> single-flight traceroute trigger for the detecting Agent of
	// an eligible network-availability fault.
	bus.Subscribe(eventbus.TopicEvidenceAdded, func(m eventbus.Message) {
		ev, ok := m.Payload.(eventbus.EvidenceAdded)
		if !ok {
			return
		}
		if err := io.OnEvidence(w.ctx, ev); err != nil {
			log.Printf("incidentops: evidence-added trace trigger (%s): %v", ev.EvidenceID, err)
		}
	})
	// Alert resolved -> deactivate the trace references it held and close any cohort
	// whose active reference count fell to zero (the execution itself is untouched).
	bus.Subscribe(eventbus.TopicAlertResolved, func(m eventbus.Message) {
		ev, ok := m.Payload.(alert.Raised)
		if !ok {
			return
		}
		if err := io.OnAlertResolved(w.ctx, ev.ID); err != nil {
			log.Printf("incidentops: alert-resolved refs (%s): %v", ev.ID, err)
		}
	})
}
