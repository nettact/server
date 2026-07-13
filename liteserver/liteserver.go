// Package liteserver is the Lite server as an importable library: one Start that
// opens the SQLite database, bootstraps the single admin, wires every
// server-core service, binds its own listener, and serves the HTTP/WS surface
// plus the embedded Vue console. The standalone nettact-lite command and the
// desktop all-in-one both drive the same code through this package — the command
// is a thin flags→Config wrapper, and the desktop passes a non-nil Desktop config
// to enable loopback-only one-time browser login.
//
// Start does the full bring-up synchronously (DB, migrations, admin, listener)
// and returns a ready *Server: a nil error from Start is readiness — the listener
// is bound and the console is reachable. Teardown happens exclusively through
// Shutdown, never by cancelling the ctx passed to Start.
package liteserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/nettact/protocol/enroll"
	"github.com/nettact/protocol/wire"
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

// Config drives one Start. Zero values select the documented defaults.
type Config struct {
	// Addr is passed verbatim to net.Listen("tcp", Addr), preserving the full
	// standalone semantics (":8080", "0.0.0.0:8443", "host:port", "127.0.0.1:0").
	Addr string

	// TLSCert/TLSKey enable HTTPS/WSS natively. Both or neither (validated).
	TLSCert string
	TLSKey  string

	DBPath string // SQLite database path

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
	// rewrite. It also tightens Addr validation to a loopback literal on port 0
	// with no TLS. nil means standalone — none of that surface exists.
	Desktop *DesktopConfig
}

// DesktopConfig holds desktop-only knobs.
type DesktopConfig struct {
	// LoginTokenTTL bounds how long a minted one-time login URL stays redeemable.
	// 0 selects 2 minutes.
	LoginTokenTTL time.Duration
}

// Server is a running Lite server. It owns the listener, HTTP server, agent hub,
// background workers, and SQLite handle; Shutdown releases them in order.
type Server struct {
	cfg     Config
	db      *store.DB
	httpSrv *http.Server
	ln      net.Listener
	baseURL string

	agentHub *agentws.Hub
	workers  *workers

	idSvc    *identity.Service
	regSvc   *registry.Service
	setSvc   *settings.Service
	auditSvc *audit.Service
	adminID  string

	login *loginTokens // nil unless Desktop != nil

	errCh        chan error
	shutdownOnce sync.Once
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

	reg := registry.New(db, cfg.MaxAgents)
	bus := eventbus.New()
	cfgSvc := config.New(db, reg, bus)
	if err := cfgSvc.SeedDefaults(ctx, site.DefaultSiteID); err != nil {
		log.Printf("seed default targets: %v", err)
	}
	auditSvc := audit.New(db)
	invSvc := inventory.New(db)
	metricsStore := metrics.New(db)
	ing := ingest.New(db, bus, metricsStore)
	hostLive := hostlive.New()

	agentHub := agentws.New(agentws.Deps{
		Registry: reg,
		Ingest:   ing,
		Config:   cfgSvc,
		HostLive: hostLive,
		Bus:      bus,
	})

	alertSvc := alert.New(db, bus)
	rulesSvc := rules.New(db, alertSvc, metricsStore)
	notifSvc := notification.New(db)
	settingsSvc := settings.New(db)
	incidentSvc := incident.New(db, bus, notifSvc, settingsSvc)
	incidentSvc.Wire()

	w := newWorkers()
	startWorkers(w, deps{
		metrics:  metricsStore,
		ingest:   ing,
		identity: idSvc,
		registry: reg,
		rules:    rulesSvc,
		bus:      bus,
		hub:      agentHub,
		ret:      cfg.Retention,
	})

	handler := api.Router(api.Deps{
		Identity:     idSvc,
		Registry:     reg,
		Metrics:      metricsStore,
		Config:       cfgSvc,
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
		Dev:          cfg.Dev,
		SecureCookie: cfg.SecureCookie,
	})

	s := &Server{
		cfg:      cfg,
		db:       db,
		agentHub: agentHub,
		workers:  w,
		idSvc:    idSvc,
		regSvc:   reg,
		setSvc:   settingsSvc,
		auditSvc: auditSvc,
		adminID:  admin.ID,
		errCh:    make(chan error, 1),
	}

	// Desktop mode adds the one-time-token login endpoint in front of the router.
	if cfg.Desktop != nil {
		s.login = newLoginTokens(cfg.Desktop.LoginTokenTTL)
		mux := http.NewServeMux()
		mux.HandleFunc("/desktop/login", s.handleDesktopLogin)
		mux.Handle("/", handler)
		handler = mux
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		w.stop(context.Background())
		return nil, fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}
	s.ln = ln

	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	s.baseURL = scheme + "://" + ln.Addr().String()

	s.httpSrv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// In desktop mode the OS-assigned port changes every launch, so rewrite the
	// deep-link origin unconditionally (skip the write only when unchanged). A
	// loopback-only bind makes any previously stored LAN URL unreachable anyway,
	// so force-overwrite is always correct here.
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

	log.Printf("nettact-lite listening on %s (dev=%v, db=%s, max_agents=%d, desktop=%v)",
		s.baseURL, cfg.Dev, cfg.DBPath, cfg.MaxAgents, cfg.Desktop != nil)

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
		s.agentHub.CloseAll("server shutting down")
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

// validate enforces the config invariants. In desktop mode it structurally
// guarantees "OS-assigned loopback port, never 8080, never LAN": the host must be
// a loopback literal, the port must be exactly 0, and TLS must be unset.
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
		if port != "0" {
			return fmt.Errorf("liteserver: desktop mode requires port 0 (OS-assigned), got %q", port)
		}
		if cfg.TLSCert != "" {
			return errors.New("liteserver: desktop mode does not use TLS")
		}
	}
	return nil
}
