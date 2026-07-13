package liteserver

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/nettact/server-core/agentws"
	"github.com/nettact/server-core/eventbus"
	"github.com/nettact/server-core/identity"
	"github.com/nettact/server-core/ingest"
	"github.com/nettact/server-core/metrics"
	"github.com/nettact/server-core/registry"
	"github.com/nettact/server-core/rules"
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
	metrics  *metrics.Store
	ingest   *ingest.Service
	identity *identity.Service
	registry *registry.Service
	rules    *rules.Service
	bus      *eventbus.Bus
	hub      *agentws.Hub
	ret      metrics.RetentionConfig
}

func startWorkers(w *workers, d deps) {
	// Rule evaluation, coalesced per agent (see ruleEval), driven off the ingest
	// event. Wired before the periodic jobs so no early telemetry is missed.
	re := newRuleEval(w, d.rules)
	d.bus.Subscribe(eventbus.TopicTelemetryIngested, func(m eventbus.Message) {
		ev, ok := m.Payload.(eventbus.TelemetryIngested)
		if !ok {
			return
		}
		re.kick(ev.AgentID, ev.SiteID)
	})

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

// ruleEval coalesces rule evaluation per agent: one evaluation in flight per
// agent, and a burst of packets while it runs marks the agent dirty for exactly
// one re-run instead of spawning a goroutine per packet. Its goroutines are
// tracked in the shared workers.wg and run on the workers context, so shutdown
// waits for an in-flight evaluation and no eval writes through a closed DB.
type ruleEval struct {
	w     *workers
	rules *rules.Service

	mu      sync.Mutex
	running map[string]bool
	dirty   map[string]bool
}

func newRuleEval(w *workers, r *rules.Service) *ruleEval {
	return &ruleEval{w: w, rules: r, running: map[string]bool{}, dirty: map[string]bool{}}
}

func (re *ruleEval) kick(agentID, siteID string) {
	re.mu.Lock()
	if re.running[agentID] {
		re.dirty[agentID] = true
		re.mu.Unlock()
		return
	}
	// Reserve the wg slot under re.mu together with the running flag. add() gates
	// on workers.stopping, so a late ingest that fans out during shutdown can
	// never Add after stop() began its wg.Wait — it is dropped here instead.
	if !re.w.add() {
		re.mu.Unlock()
		return
	}
	re.running[agentID] = true
	re.mu.Unlock()

	go func() {
		defer re.w.wg.Done()
		for {
			if err := re.rules.EvaluateAgent(re.w.ctx, agentID, siteID); err != nil {
				log.Printf("rule eval (%s): %v", agentID, err)
			}
			re.mu.Lock()
			if re.dirty[agentID] && re.w.ctx.Err() == nil {
				delete(re.dirty, agentID)
				re.mu.Unlock()
				continue
			}
			re.running[agentID] = false
			re.mu.Unlock()
			return
		}
	}()
}
