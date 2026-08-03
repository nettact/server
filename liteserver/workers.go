package liteserver

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/nettact/server-core/agentconnectivity"
	"github.com/nettact/server-core/agentws"
	"github.com/nettact/server-core/cleanup"
	"github.com/nettact/server-core/eventbus"
	"github.com/nettact/server-core/fault"
	"github.com/nettact/server-core/gamedata"
	"github.com/nettact/server-core/identity"
	"github.com/nettact/server-core/incidentops"
	"github.com/nettact/server-core/ingest"
	"github.com/nettact/server-core/inventory"
	"github.com/nettact/server-core/metrics"
	"github.com/nettact/server-core/notifypolicy"
	"github.com/nettact/server-core/registry"
	"github.com/nettact/server-core/settings"
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

// nowThenEvery runs fn once immediately and then on a ticker. Use it for
// maintenance whose interval is longer than a plausible session: the desktop
// tray is routinely launched, used and closed inside an hour, and a plain hourly
// worker would then never fire once across a machine's whole lifetime.
func (w *workers) nowThenEvery(d time.Duration, fn func(context.Context)) {
	if !w.add() {
		return
	}
	go func() {
		defer w.wg.Done()
		fn(w.ctx)
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
	metrics           *metrics.Store
	ingest            *ingest.Service
	identity          *identity.Service
	registry          *registry.Service
	incidentops       *incidentops.Service
	inventory         *inventory.Service
	gamedata          *gamedata.Service
	cleanup           *cleanup.Service
	agentconnectivity *agentconnectivity.Engine
	notifypolicy      *notifypolicy.Service
	fault             *fault.Service
	settings          *settings.Service
	bus               *eventbus.Bus
	hub               *agentws.Hub
	ret               metrics.RetentionConfig
}

func startWorkers(w *workers, d deps) {
	// Downsampling: raw → 1m/1h/1d rollups. Five minutes, not one: every run
	// rewrites each active series' tail page in every tier (SQLite page-level
	// write amplification), so the cadence is a direct multiplier on steady-state
	// disk writes. Freshness does not ride on it — charts up to 2h read the raw
	// tier (pickTier), so the rollups only serve windows long enough that a
	// ≤5-minute right edge is invisible.
	//
	// Startup-then-every for the same reason the retention workers use it: at the
	// old one-minute cadence a missed first tick cost a minute, but five minutes
	// is long enough to matter on a desktop tray that is routinely opened and
	// closed inside it — a short session would then never roll up at all, and
	// every session would start with up to five minutes of raw samples missing
	// from the tiers that serve >2h charts. The catch-up run is bounded by the
	// per-series watermarks, so doing it at startup is cheap when nothing is due.
	w.nowThenEvery(5*time.Minute, func(ctx context.Context) {
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

	// Fluctuation retention: diagnostics for availability dips, aged out wholesale
	// except the ones a fault claimed as precursors, which PruneFluctuations leaves
	// for the incident's own evidence retention to release.
	//
	// Startup-then-hourly for the same reason device retention is: a fluctuation is
	// written for every recovered failing round, so this is one of the faster-growing
	// tables in the store, and the desktop tray is routinely opened and closed inside
	// an hour. On a plain hourly ticker the first tick would rarely arrive, and the
	// configured retention would quietly never apply on exactly the deployment that
	// generates the rows.
	w.nowThenEvery(time.Hour, func(ctx context.Context) {
		days, _ := d.settings.Int(ctx, settings.KeyFluctuationRetentionDays)
		if n, err := d.fault.PruneFluctuations(ctx, time.Now().UTC().AddDate(0, 0, -days)); err != nil {
			log.Printf("prune fluctuations: %v", err)
		} else if n > 0 {
			log.Printf("prune fluctuations: removed %d", n)
		}
	})

	// LAN device retention. Discovery is upsert-only — an agent never reports that
	// a device left, and ingest ignores OpRemove — so age is the only departure
	// signal there is, and without this the devices table only grows. MAC
	// randomization makes that unbounded rather than merely untidy: a phone mints a
	// fresh address on every Wi-Fi join, so the console's device list would fill
	// with addresses that existed for one association. Runs immediately at startup
	// as well as hourly, because the desktop tray often does not stay up for an hour.
	w.nowThenEvery(time.Hour, func(ctx context.Context) {
		if n, err := d.inventory.Retention(ctx); err != nil {
			log.Printf("device retention: %v", err)
		} else if n > 0 {
			log.Printf("device retention: %d stale device(s) removed", n)
		}
	})

	// Game data retention: per-second rows on a short window, runs on a long one.
	// The per-second rows are the fastest-growing in the store — one bucket per
	// second a game is presenting frames plus one machine reading per second the
	// sensor is watching anything, so an evening of play is thousands — which is
	// also why this runs at startup rather than only hourly: the desktop tray is
	// routinely opened and closed inside an hour, and on a plain ticker the
	// configured window would quietly never apply on exactly the machines that
	// generate the rows.
	w.nowThenEvery(time.Hour, func(ctx context.Context) {
		seconds, runs, err := d.gamedata.Retention(ctx)
		if err != nil {
			log.Printf("game data retention: %v", err)
		} else if seconds > 0 || runs > 0 {
			log.Printf("game data retention: removed %d per-second row(s) and %d run(s)", seconds, runs)
		}
	})

	// Abandoned-run sweep. A run's ending is written by the agent, so a force-kill,
	// crash or power cut leaves it open forever: the agent is dead at the moment the
	// orphan is created, and by the time it is back it has forgotten the run, so the
	// server is the only party left that can close it. Every minute rather than
	// hourly because the wrong state is on screen — the console reads a missing
	// ending as "in progress", and an abandoned run then sits above newer finished
	// ones claiming to still be playing.
	w.every(time.Minute, func(ctx context.Context) {
		if n, err := d.gamedata.CloseAbandonedRuns(ctx); err != nil {
			log.Printf("game run reap: %v", err)
		} else if n > 0 {
			log.Printf("game run reap: closed %d abandoned run(s)", n)
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

	// History-data cleanup: run the one queued delete job (if any) to completion on
	// its own short interval, so a long deletion never starves rollup/retention or
	// the incident ticks. The tick runs synchronously, which enforces the
	// single-job-at-a-time policy; on shutdown the workers context is cancelled and
	// Tick returns between items, leaving any unfinished job for Recover to requeue.
	w.every(2*time.Second, func(ctx context.Context) {
		if err := d.cleanup.Tick(ctx); err != nil {
			log.Printf("cleanup tick: %v", err)
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

	// Agent liveness detector: the same live connected set the sweeper uses drives
	// the offline/recovery state machine. It measures grace from the first tick an
	// agent is seen absent, so a server restart never mass-confirms offline faults.
	w.every(5*time.Second, func(ctx context.Context) {
		if err := d.agentconnectivity.Tick(ctx, d.hub.ConnectedIDs()); err != nil {
			log.Printf("agent liveness tick: %v", err)
		}
	})

	// Notification delivery: send everything whose policy delay has expired. The
	// short interval keeps the delay honest without polling hard, and restart
	// recovery is implicit — a delivery's due_at is an absolute time, so one that
	// came due while the server was down is simply overdue on the first tick.
	w.every(3*time.Second, func(ctx context.Context) {
		if d.notifypolicy == nil {
			return
		}
		if err := d.notifypolicy.Tick(ctx); err != nil {
			log.Printf("notification delivery tick: %v", err)
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
	// Fault confirmed -> single-flight traceroute trigger for the detecting Agent of
	// an eligible network-availability fault.
	bus.Subscribe(eventbus.TopicFaultConfirmed, func(m eventbus.Message) {
		ev, ok := m.Payload.(fault.SignalEvent)
		if !ok {
			return
		}
		if err := io.OnSignalConfirmed(w.ctx, ev); err != nil {
			log.Printf("incidentops: fault-confirmed trace trigger (%s): %v", ev.SignalID, err)
		}
	})
	// Fault resolved -> deactivate the trace references it held and close any cohort
	// whose active reference count fell to zero (the execution itself is untouched).
	bus.Subscribe(eventbus.TopicFaultResolved, func(m eventbus.Message) {
		ev, ok := m.Payload.(fault.SignalEvent)
		if !ok {
			return
		}
		if err := io.OnSignalResolved(w.ctx, ev.SignalID); err != nil {
			log.Printf("incidentops: fault-resolved refs (%s): %v", ev.SignalID, err)
		}
	})
}
