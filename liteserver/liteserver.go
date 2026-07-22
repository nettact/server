// Package liteserver is the Lite server as an importable library: one Start that
// opens the SQLite database, bootstraps the single admin, wires every
// server-core service, binds its own listener, and serves the HTTP/WS surface
// plus the runtime-downloaded Vue console. The standalone nettact-lite command
// and the desktop all-in-one both drive the same code through this package — the
// command is a thin flags→Config wrapper, and the desktop passes a non-nil
// Desktop config to enable loopback-only one-time browser login.
//
// Start does the full bring-up synchronously (DB, migrations, admin, listener)
// and returns a ready *Server: a nil error from Start is readiness — the listener
// is bound and the console is reachable. Teardown happens exclusively through
// Shutdown, never by cancelling the ctx passed to Start.
package liteserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/nettact/protocol/enroll"
	"github.com/nettact/protocol/wire"
	"github.com/nettact/server-core/agentws"
	"github.com/nettact/server-core/alert"
	"github.com/nettact/server-core/api"
	"github.com/nettact/server-core/audit"
	"github.com/nettact/server-core/cleanup"
	"github.com/nettact/server-core/config"
	"github.com/nettact/server-core/eventbus"
	"github.com/nettact/server-core/hostlive"
	"github.com/nettact/server-core/identity"
	"github.com/nettact/server-core/incident"
	"github.com/nettact/server-core/incidentops"
	"github.com/nettact/server-core/ingest"
	"github.com/nettact/server-core/inventory"
	"github.com/nettact/server-core/metrics"
	"github.com/nettact/server-core/notification"
	"github.com/nettact/server-core/opissue"
	"github.com/nettact/server-core/registry"
	"github.com/nettact/server-core/rules"
	"github.com/nettact/server-core/settings"
	"github.com/nettact/server-core/site"
	"github.com/nettact/server-core/sse"
	"github.com/nettact/server-core/store"
	"github.com/nettact/server-core/targetstatus"
	"github.com/nettact/server-lite/internal/webui"
)

// Config drives one Start. Zero values select the documented defaults.
type Config struct {
	// Addr is the fallback listen address — an explicit -addr flag or the built-in
	// default — passed to net.Listen("tcp", ...) unless a listen_addr setting
	// saved in the web console overrides it (DB > flag > default).
	Addr string

	// AddrFromFlag marks Addr as coming from an explicit -addr flag (vs the
	// built-in default). Used only for source reporting in server-info.
	AddrFromFlag bool

	// TLSCert/TLSKey enable HTTPS/WSS natively. Both or neither (validated).
	TLSCert string
	TLSKey  string

	DBPath string // SQLite database path

	// WebUIDir is the root directory for the runtime-downloaded web console
	// (versions install to WebUIDir/<version>/). Empty selects
	// filepath.Dir(DBPath) + "/webui".
	WebUIDir string

	// AdminUser/AdminPass seed the single admin on first run (EnsureAdmin). On a
	// later run they are ignored and the existing admin is used.
	AdminUser string
	AdminPass string

	Dev bool // relax CORS for the Vite origin

	// SecureCookie sets Secure on the session cookie. It is explicit (not derived
	// from Dev) because the desktop runs Dev=false over loopback HTTP and must
	// therefore keep SecureCookie=false or the browser drops the cookie.
	SecureCookie bool

	MaxAgents int                     // max enrolled agents (0 = unlimited)
	Retention metrics.RetentionConfig // downsampling/retention windows

	// Desktop, when non-nil, enables the desktop-only surface: the
	// GET /desktop/login one-time-token endpoint and per-launch console_base_url
	// rewrite. It also tightens Addr validation to a loopback literal with no
	// TLS. nil means standalone — none of that surface exists.
	Desktop *DesktopConfig
}

// DesktopConfig holds desktop-only knobs.
type DesktopConfig struct {
	// LoginTokenTTL bounds how long a minted one-time login URL stays redeemable.
	// 0 selects 2 minutes.
	LoginTokenTTL time.Duration

	// OnListenAddrChanged, when non-nil, fires from a background goroutine
	// (shortly after the settings PUT response has been written) when the console
	// saves a new listen address. The desktop host restarts the embedded server
	// in response; without it a saved change waits for the next launch.
	OnListenAddrChanged func(newAddr string)

	// OnAlertsChanged, when non-nil, fires from a background goroutine after an
	// alert is raised or resolved. The desktop refreshes its tray summary.
	OnAlertsChanged func()
}

// Server is a running Lite server. It owns the listener, HTTP server, agent hub,
// background workers, and SQLite handle; Shutdown releases them in order.
type Server struct {
	cfg     Config
	db      *store.DB
	httpSrv *http.Server
	ln      net.Listener
	baseURL string
	listen  listenResolution

	agentHub *agentws.Hub
	broker   *sse.Broker
	workers  *workers
	webui    *webui.Manager

	idSvc    *identity.Service
	regSvc   *registry.Service
	setSvc   *settings.Service
	auditSvc *audit.Service
	alertSvc *alert.Service
	adminID  string

	login *loginTokens // nil unless Desktop != nil

	errCh        chan error
	shutdownOnce sync.Once
}

// ErrListen marks a bind failure from Start (port in use, permission denied).
// The desktop host matches it with errors.Is to show a port-specific dialog.
var ErrListen = errors.New("liteserver: listen failed")

// listenResolution is the outcome of the DB > flag > default listen-address
// resolution, reported through server-info.
type listenResolution struct {
	addr         string
	source       string // "default" | "flag" | "db"
	fallbackFrom string // configured addr that failed to bind (source reverted to flag/default)
}

// resolveListenAddr applies the listen-address priority: a valid listen_addr
// setting saved in the console wins over cfg.Addr (explicit flag or built-in
// default). A malformed stored value is logged and ignored rather than
// preventing startup.
func resolveListenAddr(ctx context.Context, set *settings.Service, cfg Config) listenResolution {
	flagOrDefault := "default"
	if cfg.AddrFromFlag {
		flagOrDefault = "flag"
	}
	v, err := set.Get(ctx, settings.KeyListenAddr)
	if err != nil || v == "" {
		return listenResolution{addr: cfg.Addr, source: flagOrDefault}
	}
	if _, _, splitErr := net.SplitHostPort(v); splitErr != nil {
		log.Printf("ignoring malformed listen_addr setting %q: %v", v, splitErr)
		return listenResolution{addr: cfg.Addr, source: flagOrDefault}
	}
	return listenResolution{addr: v, source: "db"}
}

// Start brings the server fully up and returns it ready. ctx bounds only the
// startup work (DB open, migrations, admin bootstrap, listen); the running
// server derives its own worker context and is stopped only via Shutdown. A
// non-nil error means nothing was left running.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}
	if cfg.Desktop != nil && cfg.Desktop.LoginTokenTTL == 0 {
		cfg.Desktop.LoginTokenTTL = 2 * time.Minute
	}
	// A zero Retention selects the standard windows, so a caller (the desktop)
	// need not import server-core just to fill them in. The standalone CLI always
	// passes explicit values.
	if cfg.Retention == (metrics.RetentionConfig{}) {
		cfg.Retention = metrics.RetentionConfig{
			RawSeconds: 2 * 86400,
			M1Seconds:  30 * 86400,
			H1Seconds:  730 * 86400,
			D1Seconds:  0, // 1-day rollups kept forever
		}
	}
	if cfg.WebUIDir == "" {
		cfg.WebUIDir = filepath.Join(filepath.Dir(cfg.DBPath), "webui")
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Any failure past this point must close the DB so a caller that got an error
	// is not left holding an open SQLite handle.
	ok := false
	defer func() {
		if !ok {
			_ = db.Close()
		}
	}()

	siteSvc := site.New(db)
	if err := siteSvc.EnsureDefault(ctx); err != nil {
		return nil, fmt.Errorf("ensure default site: %w", err)
	}

	idSvc := identity.New(db)
	admin, err := idSvc.EnsureAdmin(ctx, cfg.AdminUser, cfg.AdminPass)
	if err != nil {
		return nil, fmt.Errorf("bootstrap admin: %w", err)
	}

	bus := eventbus.New()
	reg := registry.New(db, cfg.MaxAgents, bus)

	// Leaf services the fault engine and orchestration build on.
	metricsStore := metrics.New(db)
	settingsSvc := settings.New(db)
	notifSvc := notification.New(db)
	alertSvc := alert.New(db)

	// Incident snapshot + traceroute orchestration. Constructed before the fault
	// engine (which uses it as the synchronous incident-base snapshot writer) and
	// before the hub (whose Pusher it becomes); its Pusher is injected once the hub
	// exists, below, closing the construction cycle without an import cycle.
	incidentOps := incidentops.New(db, metricsStore, settingsSvc, bus)

	// Fault engine: evaluates group rules, maintains alerts/incidents, writes the
	// immutable incident base snapshot inside its open transaction (via incidentOps),
	// dispatches notifications, and publishes lifecycle events post-commit.
	rulesSvc := rules.New(db, metricsStore, notifSvc, settingsSvc, bus, incidentOps)

	// Config force-resolves alerts of removed targets/rules through the fault engine
	// (AlertTerminator), so it takes rulesSvc rather than the alert read model.
	cfgSvc := config.New(db, reg, bus, rulesSvc)
	if err := cfgSvc.SeedDefaults(ctx, site.DefaultSiteID); err != nil {
		log.Printf("seed default targets: %v", err)
	}

	auditSvc := audit.New(db)
	invSvc := inventory.New(db)
	// Ingest evaluates rules inside its own sample transaction (atomic telemetry +
	// rule-state visibility), so it takes the fault engine as its Evaluator.
	ing := ingest.New(db, bus, metricsStore, rulesSvc)
	hostLive := hostlive.New()
	opSvc := opissue.New(db, bus)
	incidentSvc := incident.New(db)
	// Authoritative current target-status aggregation (read-time; owns no state).
	tgtStatusSvc := targetstatus.New(db)

	// History-data cleanup: durable async delete jobs over the metrics store,
	// driven by a worker tick and recovered after a restart.
	cleanupSvc := cleanup.New(db, metricsStore)

	// SSE broker fans live changes out to connected consoles. It multiplexes two
	// streams per site on one connection: authoritative "issues" snapshots and
	// precise "target.status.changed" events.
	broker := sse.NewBroker()
	bus.Subscribe(eventbus.TopicIssueChanged, func(m eventbus.Message) {
		if ev, ok := m.Payload.(eventbus.IssueChanged); ok {
			broker.Notify(ev.SiteID, sse.Event{Name: sse.EventIssues})
		}
	})
	// Precise target-status change: carry the affected site + target ids so the
	// client coalesces a batch refresh. An empty target-id set means the whole site
	// changed and the client fully refreshes.
	bus.Subscribe(eventbus.TopicTargetStatusChanged, func(m eventbus.Message) {
		ev, ok := m.Payload.(eventbus.TargetStatusChanged)
		if !ok {
			return
		}
		broker.Notify(ev.SiteID, sse.Event{
			Name: sse.EventTargetStatusChanged,
			Data: targetStatusEventData(ev.SiteID, ev.TargetIDs),
		})
	})
	// Agent liveness flips affect every target in the agent's scope, so a bridge
	// fans it out to a site-wide status refresh (empty target-id set). This is the
	// only place an agent online↔offline transition reaches target status.
	bus.Subscribe(eventbus.TopicAgentLivenessChanged, func(m eventbus.Message) {
		ev, ok := m.Payload.(eventbus.AgentLivenessChanged)
		if !ok {
			return
		}
		broker.Notify(ev.SiteID, sse.Event{
			Name: sse.EventTargetStatusChanged,
			Data: targetStatusEventData(ev.SiteID, nil),
		})
	})

	agentHub := agentws.New(agentws.Deps{
		Registry:    reg,
		Ingest:      ing,
		Config:      cfgSvc,
		HostLive:    hostLive,
		OpIssue:     opSvc,
		Bus:         bus,
		IncidentOps: incidentOps,
	})
	// The hub is the agent-WebSocket Pusher for incident-snapshot / trace requests;
	// inject it now that it exists (before serving, so no lock is needed) so the
	// orchestration's dispatch and reconnect re-push reach live sessions.
	incidentOps.SetPusher(agentHub)

	w := newWorkers()
	// Post-commit orchestration subscriptions (incident opened / evidence added /
	// alert resolved). Registered before recovery and before serving so no early
	// lifecycle event is missed; the fault engine publishes these off its write
	// transaction, so the handlers never run inside it.
	wireIncidentOps(w, bus, incidentOps)

	// Recovery before listening: finalize snapshots/traces whose deadline elapsed
	// while the server was down, close cohorts orphaned by refs/alerts that are no
	// longer active, and rehydrate the still-eligible queued trace work.
	if err := incidentOps.Recover(ctx); err != nil {
		log.Printf("incidentops recover: %v", err)
	}
	// Requeue any cleanup job left mid-run by a previous stop; its pending items
	// re-execute idempotently on the next worker tick. A failure here is non-fatal:
	// the cleanup Tick self-heals (it requeues orphaned 'running' jobs on every run),
	// so a transient startup lock cannot wedge the subsystem.
	if err := cleanupSvc.Recover(ctx); err != nil {
		log.Printf("cleanup recover (tick will self-heal): %v", err)
	}

	startWorkers(w, deps{
		metrics:     metricsStore,
		ingest:      ing,
		identity:    idSvc,
		registry:    reg,
		incidentops: incidentOps,
		cleanup:     cleanupSvc,
		bus:         bus,
		hub:         agentHub,
		ret:         cfg.Retention,
	})
	// Desktop tray summary: alert lifecycle changes kick an immediate refresh.
	if cfg.Desktop != nil && cfg.Desktop.OnAlertsChanged != nil {
		onAlerts := func(eventbus.Message) { go cfg.Desktop.OnAlertsChanged() }
		bus.Subscribe(eventbus.TopicAlertRaised, onAlerts)
		bus.Subscribe(eventbus.TopicAlertResolved, onAlerts)
	}

	webuiMgr := webui.New(cfg.WebUIDir, webui.Version)

	// Resolve the listen address (DB > flag > default) before building the router
	// so its status closure can report the outcome; the actual bind happens below.
	listenRes := resolveListenAddr(ctx, settingsSvc, cfg)

	// s is allocated before the router because the api Deps closures read it
	// (lazily, at request time — well after Start has filled ln/baseURL/listen).
	s := &Server{
		cfg:      cfg,
		db:       db,
		agentHub: agentHub,
		broker:   broker,
		workers:  w,
		webui:    webuiMgr,
		idSvc:    idSvc,
		regSvc:   reg,
		setSvc:   settingsSvc,
		auditSvc: auditSvc,
		alertSvc: alertSvc,
		adminID:  admin.ID,
		errCh:    make(chan error, 1),
	}

	apiDeps := api.Deps{
		Identity:     idSvc,
		Registry:     reg,
		Metrics:      metricsStore,
		Cleanup:      cleanupSvc,
		Config:       cfgSvc,
		Site:         siteSvc,
		Inventory:    invSvc,
		Rules:        rulesSvc,
		Alert:        alertSvc,
		Incident:     incidentSvc,
		IncidentOps:  incidentOps,
		Notification: notifSvc,
		Settings:     settingsSvc,
		Audit:        auditSvc,
		HostLive:     hostLive,
		OpIssue:      opSvc,
		TargetStatus: tgtStatusSvc,
		SSE:          broker,
		AgentWS:      agentHub,
		Bus:          bus,
		SPA:          webuiMgr.Handler(),
		Dev:          cfg.Dev,
		SecureCookie: cfg.SecureCookie,
		ListenStatus: func(context.Context) *api.ListenStatus {
			return &api.ListenStatus{
				EffectiveAddr: s.ln.Addr().String(),
				Source:        s.listen.source,
				Desktop:       cfg.Desktop != nil,
				FallbackFrom:  s.listen.fallbackFrom,
				OverridesFlag: s.listen.source == "db" && cfg.AddrFromFlag,
			}
		},
	}
	if cfg.Desktop != nil && cfg.Desktop.OnListenAddrChanged != nil {
		apiDeps.ApplyListenAddr = func(_ context.Context, addr string) error {
			// Defer the callback so the settings PUT response flushes before the
			// desktop tears the listener down for the restart.
			time.AfterFunc(500*time.Millisecond, func() { cfg.Desktop.OnListenAddrChanged(addr) })
			return nil
		}
	}
	handler := api.Router(apiDeps)

	// Desktop mode adds the one-time-token login endpoint in front of the router.
	if cfg.Desktop != nil {
		s.login = newLoginTokens(cfg.Desktop.LoginTokenTTL)
		mux := http.NewServeMux()
		mux.HandleFunc("/desktop/login", s.handleDesktopLogin)
		mux.Handle("/", handler)
		handler = mux
	}

	ln, err := net.Listen("tcp", listenRes.addr)
	if err != nil && listenRes.source == "db" {
		// A saved-but-unbindable address must never brick the server (on desktop
		// there would be no UI left to fix it): fall back to the flag/default addr
		// and report the failure through server-info (FallbackFrom).
		log.Printf("configured listen_addr %s failed (%v); falling back to %s", listenRes.addr, err, cfg.Addr)
		listenRes.fallbackFrom = listenRes.addr
		listenRes.addr = cfg.Addr
		if cfg.AddrFromFlag {
			listenRes.source = "flag"
		} else {
			listenRes.source = "default"
		}
		ln, err = net.Listen("tcp", cfg.Addr)
	}
	if err != nil {
		w.stop(context.Background())
		return nil, fmt.Errorf("%w: listen %s: %v", ErrListen, listenRes.addr, err)
	}
	s.ln = ln
	s.listen = listenRes

	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	s.baseURL = scheme + "://" + ln.Addr().String()

	s.httpSrv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// In desktop mode the bound port can change (a saved listen setting, or a
	// fallback after a failed bind), so rewrite the deep-link origin whenever it
	// differs. A loopback-only bind makes any previously stored LAN URL
	// unreachable anyway, so force-overwrite is always correct here.
	if cfg.Desktop != nil {
		if cur, _ := settingsSvc.Get(ctx, settings.KeyConsoleBaseURL); cur != s.baseURL {
			if err := settingsSvc.Set(ctx, settings.KeyConsoleBaseURL, s.baseURL); err != nil {
				// The deep-link origin must match this launch's ephemeral port or tray
				// notifications would open a dead port. This is part of readiness, so
				// fail Start and clean up the listener/workers (the ok=false defer
				// closes the DB) rather than claim readiness with a stale deep link.
				w.stop(context.Background())
				_ = ln.Close()
				return nil, fmt.Errorf("persist console_base_url: %w", err)
			}
		}
	}

	useTLS := cfg.TLSCert != "" && cfg.TLSKey != ""
	go func() {
		// This is the sole sender on errCh and the sole place it is closed, so a
		// real serve error is delivered (buffered, never blocks) before the close.
		defer close(s.errCh)
		var serveErr error
		if useTLS {
			serveErr = s.httpSrv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
		} else {
			serveErr = s.httpSrv.Serve(ln)
		}
		// A clean Shutdown surfaces http.ErrServerClosed — that is not a terminal
		// error, so only a genuine serve failure is published on Err(). Either way
		// the deferred close lets lifetime watchers unblock when serving ends.
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			s.errCh <- serveErr
		}
	}()

	log.Printf("nettact-lite listening on %s (dev=%v, db=%s, max_agents=%d, desktop=%v, webui=%s@%s)",
		s.baseURL, cfg.Dev, cfg.DBPath, cfg.MaxAgents, cfg.Desktop != nil, webui.Version, cfg.WebUIDir)

	// Nothing can fail Start past this point, so the background download loop
	// will always be paired with a Shutdown that closes it.
	s.webui.Start()

	ok = true
	return s, nil
}

// BaseURL is the server's own origin with the actual bound port, e.g.
// http://127.0.0.1:52344. Deep links and the agent's ServerURL derive from it.
func (s *Server) BaseURL() string { return s.baseURL }

// Err delivers a terminal serve error if the HTTP server stops on its own (not
// via Shutdown). It never sends a value for a clean Shutdown. At most one value
// is sent, and the channel is closed when serving ends (including a clean
// Shutdown), so a lifetime watcher blocked on receive always unblocks.
func (s *Server) Err() <-chan error { return s.errCh }

// ActiveAlertCount reports the default site's firing alert count, consumed
// in-process by the desktop tray summary.
func (s *Server) ActiveAlertCount(ctx context.Context) (int, error) {
	return s.alertSvc.CountActive(ctx, site.DefaultSiteID)
}

// MintEnrollmentToken issues a one-time enrollment token for the default site,
// used by the desktop host to enroll its bundled agent in-process (no token ever
// reaches a command line).
func (s *Server) MintEnrollmentToken(ctx context.Context, note string) (string, error) {
	return s.regSvc.CreateEnrollmentToken(ctx, site.DefaultSiteID, note, 5*time.Minute)
}

// EnrollAgent redeems an enrollment request directly against the registry — the
// in-process equivalent of POST /api/v1/enroll, including the audit entry. The
// desktop passes this as agentrt.Config.Enroller so its bundled agent enrolls
// without an HTTP round-trip. Token redemption, quota, and signature checks all
// run exactly as they do over HTTP.
func (s *Server) EnrollAgent(ctx context.Context, req enroll.EnrollRequest) (enroll.EnrollResponse, error) {
	resp, err := s.regSvc.Enroll(ctx, req)
	if err != nil {
		return enroll.EnrollResponse{}, err
	}
	s.auditSvc.Log(ctx, resp.AgentID, "agent.enroll", resp.SiteID, req.Hostname)
	return resp, nil
}

// DialAgent attaches an in-process agent link to the embedded hub, bypassing the
// loopback WebSocket entirely. It matches wire.Dialer; the desktop passes it as
// agentrt.Config.Dialer so telemetry and config never leave the process.
func (s *Server) DialAgent(ctx context.Context, token string) (wire.Conn, error) {
	return s.agentHub.DialLocal(ctx, token)
}

// Shutdown stops the server in the one safe order: close hijacked agent
// WebSockets first (http.Shutdown neither closes nor waits for them), then the
// HTTP server, then the background workers, then the SQLite handle strictly last
// (a live worker writing through a closed DB would be a use-after-close). It is
// idempotent; ctx bounds each stage.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.shutdownOnce.Do(func() {
		s.webui.Close(ctx)
		s.agentHub.CloseAll("server shutting down")
		// Drop SSE subscribers so their handlers return and stop querying the DB;
		// this must precede db.Close (below) and lets http.Shutdown finish promptly
		// instead of waiting on long-lived event streams.
		s.broker.Close()
		if e := s.httpSrv.Shutdown(ctx); e != nil {
			err = e
		}
		if s.workers.stop(ctx) {
			if e := s.db.Close(); e != nil && err == nil {
				err = e
			}
		} else if err == nil {
			// The shutdown deadline fired before every worker returned; a worker may
			// still be mid-write, so closing the DB now would be a use-after-close.
			// Leave the handle open (the process exits shortly after) and report it.
			err = errors.New("liteserver: workers did not stop within the shutdown deadline; DB left open to avoid use-after-close")
		}
	})
	return err
}

// targetStatusEventData marshals the "target.status.changed" SSE payload: the
// affected site and target ids. A nil/empty id slice is emitted as an empty JSON
// array, which the client reads as "the whole site changed → full refresh".
func targetStatusEventData(siteID string, targetIDs []string) []byte {
	if targetIDs == nil {
		targetIDs = []string{}
	}
	data, err := json.Marshal(map[string]any{"site_id": siteID, "target_ids": targetIDs})
	if err != nil {
		return []byte(`{"site_id":"","target_ids":[]}`)
	}
	return data
}

// validate enforces the config invariants. In desktop mode it structurally
// guarantees "loopback fallback, never LAN by default": the fallback host must
// be a loopback literal with a valid numeric port and TLS unset. (A saved
// listen_addr setting may still select 0.0.0.0 — that is validated separately
// at save time.)
func validate(cfg Config) error {
	if cfg.Addr == "" {
		return errors.New("liteserver: Addr is required")
	}
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return fmt.Errorf("liteserver: invalid Addr %q: %w", cfg.Addr, err)
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return errors.New("liteserver: TLSCert and TLSKey must be set together")
	}
	if cfg.Desktop != nil {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("liteserver: desktop mode requires a loopback host, got %q", host)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			// Port 0 (OS-assigned) stays allowed for tests; the desktop app passes
			// the fixed default 12450.
			return fmt.Errorf("liteserver: desktop mode requires a numeric port, got %q", port)
		}
		if cfg.TLSCert != "" {
			return errors.New("liteserver: desktop mode does not use TLS")
		}
	}
	return nil
}
