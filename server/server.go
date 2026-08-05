// Package server is the server as an importable library: one Start that
// opens the SQLite database, bootstraps the single admin, wires every
// server-core service, binds its own listener, and serves the HTTP/WS surface
// plus the runtime-downloaded Vue console. The standalone nettact-server command
// and the desktop all-in-one both drive the same code through this package — the
// command is a thin flags→Config wrapper, and the desktop passes a non-nil
// Desktop config to enable loopback-only one-time browser login.
//
// Start does the full bring-up synchronously (DB, migrations, admin, listener)
// and returns a ready *Server: a nil error from Start is readiness — the listener
// is bound and the console is reachable. Teardown happens exclusively through
// Shutdown, never by cancelling the ctx passed to Start.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nettact/protocol/enroll"
	"github.com/nettact/protocol/wire"
	"github.com/nettact/server-core/agentconnectivity"
	"github.com/nettact/server-core/agentstatus"
	"github.com/nettact/server-core/agentws"
	"github.com/nettact/server-core/api"
	"github.com/nettact/server-core/audit"
	"github.com/nettact/server-core/cleanup"
	"github.com/nettact/server-core/config"
	"github.com/nettact/server-core/eventbus"
	"github.com/nettact/server-core/fault"
	"github.com/nettact/server-core/gamedata"
	"github.com/nettact/server-core/hostlive"
	"github.com/nettact/server-core/identity"
	"github.com/nettact/server-core/incident"
	"github.com/nettact/server-core/incidentops"
	"github.com/nettact/server-core/ingest"
	"github.com/nettact/server-core/inventory"
	"github.com/nettact/server-core/metrics"
	"github.com/nettact/server-core/notification"
	"github.com/nettact/server-core/notifypolicy"
	"github.com/nettact/server-core/opissue"
	"github.com/nettact/server-core/registry"
	"github.com/nettact/server-core/settings"
	"github.com/nettact/server-core/site"
	"github.com/nettact/server-core/sse"
	"github.com/nettact/server-core/store"
	"github.com/nettact/server-core/targetstatus"
	"github.com/nettact/server-core/updatecheck"
	"github.com/nettact/server/internal/version"
	"github.com/nettact/server/internal/webui"
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
	// filepath.Dir(DBPath) + "/webui". Ignored when WebUIFS is set.
	WebUIDir string

	// WebUIFS, when non-nil, is a built web-console dist supplied by the host
	// (index.html at its root). It is served directly and the
	// runtime downloader is never started, so WebUIDir is unused.
	//
	// The desktop app sets this: Microsoft Store and App Store review read a
	// runtime fetch of application content as downloading a separate
	// executable, so packaged builds ship the console as application resources.
	// Server deployments leave it nil and keep downloading, which is what lets
	// the console update without reshipping the server image.
	WebUIFS fs.FS

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
	// GET /desktop/login one-time-token endpoint and the console_base_url seed
	// (written only when unset or still loopback-valued). It also tightens Addr
	// validation to a loopback literal with no TLS. nil means standalone — none
	// of that surface exists.
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

	// OnIncidentsChanged, when non-nil, fires from a background goroutine after an
	// incident opens or resolves. The desktop refreshes its tray summary.
	OnIncidentsChanged func()

	// NativeDeepLinks says the host has registered a per-user nettact:// protocol
	// handler and can therefore receive a notification click. Native OS
	// notifications then carry a credential-free nettact:// URI, which routes the
	// click back through the host so it can mint a login against the loopback
	// address it is really serving. Leave it false where no handler exists: an
	// unhandled URI makes the notification do nothing at all.
	NativeDeepLinks bool

	// AppVersion is the desktop build's own stamped version. The desktop is what
	// the user updates — the embedded server ships inside it — so it, not
	// version.Version, is what the update check compares.
	AppVersion string

	// StoreInstall marks a Microsoft Store (MSIX) install. Those builds are
	// updated by the Store, so the check asks the Store (CheckStoreUpdate) and
	// the console points at the Store page instead of the download center.
	StoreInstall bool

	// CheckStoreUpdate queries the Store for a pending package update. Non-nil
	// only on packaged Windows builds; a failure degrades to "no update found"
	// without disturbing the rest of the check.
	CheckStoreUpdate func(ctx context.Context) (updatecheck.CheckResult, error)

	// OnUpdate fires from a background goroutine when the daily check finds a
	// newer version and update notices are switched on. The desktop turns it into
	// one tray balloon per version.
	OnUpdate func(updatecheck.Status)
}

// Server is a running server instance. It owns the listener, HTTP server, agent hub,
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

	idSvc       *identity.Service
	regSvc      *registry.Service
	setSvc      *settings.Service
	auditSvc    *audit.Service
	incidentSvc *incident.Service
	updateSvc   *updatecheck.Service // nil when update checking is switched off
	adminID     string

	login *loginTokens // nil unless Desktop != nil

	errCh        chan error
	shutdownOnce sync.Once
}

// ErrListen marks a bind failure from Start (port in use, permission denied).
// The desktop host matches it with errors.Is to show a port-specific dialog.
var ErrListen = errors.New("server: listen failed")

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

// baseOrigin derives the server's own dialable origin from the bound listener
// address. A wildcard bind reports an unspecified host — 0.0.0.0 or, dual-stack,
// [::] — which is valid to bind but useless to dial, so only that case is
// rewritten to 127.0.0.1 (a wildcard listener always accepts loopback). Any
// specific bound host — 127.0.0.1, [::1], a LAN interface — is preserved: it is
// the one address known to reach this listener, and an IPv6-only or single-
// interface bind would not answer on IPv4 loopback at all.
func baseOrigin(scheme string, bound net.Addr) string {
	if tcp, ok := bound.(*net.TCPAddr); ok && tcp.IP.IsUnspecified() {
		return scheme + "://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port))
	}
	return scheme + "://" + bound.String()
}

// effectiveAddr reports the binding the way it was configured: the configured
// host paired with the port actually bound. Reporting the socket's own address
// instead would surface a dual-stack wildcard as "[::]:12450" — which no longer
// matches the saved "0.0.0.0:12450", so the console would show a permanent
// "restart pending" badge and parse the host back as loopback. Taking the port
// from the socket keeps an OS-assigned (":0") or fallback bind honest.
func effectiveAddr(configured string, bound net.Addr) string {
	host, _, err := net.SplitHostPort(configured)
	if err != nil || host == "" {
		return bound.String()
	}
	_, port, err := net.SplitHostPort(bound.String())
	if err != nil {
		return bound.String()
	}
	return net.JoinHostPort(host, port)
}

// isLoopbackOrigin reports whether v is an origin that only resolves on this
// machine. Such a value can only be one this package seeded (or an equivalent
// the user typed), so it is safe to refresh; anything else is a deliberate
// choice — a LAN address, a hostname, a reverse proxy — and must be preserved.
// A value that is not a parsable absolute URL is treated as user-authored.
func isLoopbackOrigin(v string) bool {
	u, err := url.Parse(v)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	// A zero Retention selects the standard windows (raw 2d / 1m 30d / 1h 2y /
	// 1d forever), which is what every caller uses — the windows are not
	// configurable per deployment.
	if cfg.Retention == (metrics.RetentionConfig{}) {
		cfg.Retention = metrics.DefaultRetention()
	}
	if cfg.WebUIDir == "" && cfg.WebUIFS == nil {
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
	admin, generatedPass, err := idSvc.EnsureAdmin(ctx, cfg.AdminUser, cfg.AdminPass)
	if err != nil {
		return nil, fmt.Errorf("bootstrap admin: %w", err)
	}
	// First run with no supplied credentials: print the generated password once,
	// prominently, so a standalone operator can log in. Never in desktop mode —
	// there the admin logs in through a one-time token and the random password is
	// intentionally never surfaced.
	if generatedPass != "" && cfg.Desktop == nil {
		log.Printf("\n"+
			"========================================================================\n"+
			"  NetTact first run: an initial admin account has been created.\n"+
			"    username: %s\n"+
			"    password: %s\n"+
			"  This password is printed ONCE. Log in and change it in Settings\n"+
			"  (or run `nettact-server passwd`).\n"+
			"========================================================================",
			admin.Username, generatedPass)
	}

	bus := eventbus.New()
	reg := registry.New(db, cfg.MaxAgents, bus)

	// Leaf services the fault engine and orchestration build on.
	metricsStore := metrics.New(db)
	// The query side reasons about which rollup tier still holds data for a
	// window's age; it must use the same windows the pruner deletes by.
	metricsStore.SetRetention(cfg.Retention)
	settingsSvc := settings.New(db)
	notifSvc := notification.New(db, cfg.Desktop != nil && cfg.Desktop.NativeDeepLinks)

	// Incident snapshot + traceroute orchestration. Constructed before the fault
	// engine (which uses it as the synchronous incident-base snapshot writer) and
	// before the hub (whose Pusher it becomes); its Pusher is injected once the hub
	// exists, below, closing the construction cycle without an import cycle.
	incidentOps := incidentops.New(db, metricsStore, settingsSvc, bus)

	// Fault engine: runs the built-in detectors round by round, maintains fault
	// signals and incidents, writes the immutable incident base snapshot inside its
	// open transaction (via incidentOps), and publishes lifecycle events
	// post-commit. It records faults; it never sends anything.
	faultSvc := fault.New(db, bus, incidentOps)

	// Notification policy decides whether/when/where a recorded fault is
	// announced. It reads incidents the fault engine writes, so it is constructed
	// after and injected back as the engine's planner — the two never import each
	// other.
	policySvc := notifypolicy.New(db, notifSvc, settingsSvc, bus)
	faultSvc.SetPlanner(policySvc)
	if err := policySvc.EnsureBuiltins(ctx, site.DefaultSiteID); err != nil {
		log.Printf("ensure built-in notification policies: %v", err)
	}

	// Config force-resolves the faults of removed/changed targets through the fault
	// engine (FaultTerminator).
	cfgSvc := config.New(db, reg, bus, faultSvc)
	// The site's undeletable default monitor group must exist before the console
	// (or the first-run onboarding wizard) writes targets. Starter-target creation
	// is owned by the wizard now, so first boot leaves an empty target list.
	if _, err := cfgSvc.EnsureDefaultGroup(ctx, site.DefaultSiteID); err != nil {
		log.Printf("ensure default monitor group: %v", err)
	}

	auditSvc := audit.New(db)
	// Inventory takes settings for the device retention windows (see its worker).
	invSvc := inventory.New(db, settingsSvc)
	// Game presentation history (runs + per-second buckets), likewise taking
	// settings for its two retention windows.
	gameSvc := gamedata.New(db, settingsSvc)
	// Ingest evaluates the batch's probe rounds inside its own sample transaction
	// (atomic telemetry + detector state), so it takes the fault engine as its
	// Evaluator.
	ing := ingest.New(db, bus, metricsStore, faultSvc)
	hostLive := hostlive.New()
	opSvc := opissue.New(db, bus)
	incidentSvc := incident.New(db)
	// Authoritative current target-status aggregation (read-time; owns no state).
	tgtStatusSvc := targetstatus.New(db, metricsStore)

	// Agent status list (AGENT-001): per-agent health + resource rollup (read-time).
	agentStatusSvc := agentstatus.New(db, metricsStore, settingsSvc)
	// Agent liveness detector (AGENT-002): offline/recovery state machine, driven
	// by a worker tick fed the live connected-session set. It records through the
	// fault engine like every other detector.
	agentConnEng := agentconnectivity.New(db, settingsSvc, faultSvc, bus)

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
	// fans it out to a site-wide status refresh (empty target-id set). It also
	// refreshes the agent-status list (a liveness change flips an agent's status).
	bus.Subscribe(eventbus.TopicAgentLivenessChanged, func(m eventbus.Message) {
		ev, ok := m.Payload.(eventbus.AgentLivenessChanged)
		if !ok {
			return
		}
		broker.Notify(ev.SiteID, sse.Event{
			Name: sse.EventTargetStatusChanged,
			Data: targetStatusEventData(ev.SiteID, nil),
		})
		broker.Notify(ev.SiteID, sse.Event{
			Name: sse.EventAgentStatusChanged,
			Data: agentStatusEventData(ev.SiteID),
		})
	})
	// Incident lifecycle drives the fault centre, so bridge all three topics to one
	// event the console listens on to refetch the list it is showing.
	incidentBridge := func(m eventbus.Message) {
		if ev, ok := m.Payload.(eventbus.IncidentEvent); ok {
			broker.Notify(ev.SiteID, sse.Event{
				Name: sse.EventIncidentChanged,
				Data: incidentEventData(ev.SiteID, ev.IncidentID),
			})
		}
	}
	bus.Subscribe(eventbus.TopicIncidentOpened, incidentBridge)
	bus.Subscribe(eventbus.TopicIncidentUpdated, incidentBridge)
	bus.Subscribe(eventbus.TopicIncidentResolved, incidentBridge)

	// Agent-status list also refreshes on a connectivity-fault open/resolve, and on
	// target-fault / operational-issue changes (they drive an agent's abnormal state
	// and reason counts). The client coalesces these into one wholesale refetch.
	agentStatusBridge := func(siteID string) {
		broker.Notify(siteID, sse.Event{Name: sse.EventAgentStatusChanged, Data: agentStatusEventData(siteID)})
	}
	bus.Subscribe(eventbus.TopicAgentConnectivityChanged, func(m eventbus.Message) {
		if ev, ok := m.Payload.(eventbus.AgentConnectivityChanged); ok {
			agentStatusBridge(ev.SiteID)
		}
	})
	faultBridge := func(m eventbus.Message) {
		if ev, ok := m.Payload.(fault.SignalEvent); ok {
			agentStatusBridge(ev.SiteID)
		}
	}
	bus.Subscribe(eventbus.TopicFaultConfirmed, faultBridge)
	bus.Subscribe(eventbus.TopicFaultResolved, faultBridge)
	bus.Subscribe(eventbus.TopicIssueChanged, func(m eventbus.Message) {
		if ev, ok := m.Payload.(eventbus.IssueChanged); ok {
			agentStatusBridge(ev.SiteID)
		}
	})
	// Agent-group rename/delete/membership publishes config.changed; refresh the
	// agent-status list so its group chips and the group filter stay current.
	bus.Subscribe(eventbus.TopicConfigChanged, func(m eventbus.Message) {
		if ev, ok := m.Payload.(eventbus.ConfigChanged); ok {
			agentStatusBridge(ev.SiteID)
		}
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
	// A storm normally closes in the same transaction as its last member, so this
	// can only find something a crash left behind. It closes such a storm silently
	// rather than announcing a recovery it never actually observed.
	if err := policySvc.RecoverStorms(ctx); err != nil {
		log.Printf("notifypolicy recover storms: %v", err)
	}

	startWorkers(w, deps{
		metrics:           metricsStore,
		ingest:            ing,
		identity:          idSvc,
		registry:          reg,
		incidentops:       incidentOps,
		inventory:         invSvc,
		gamedata:          gameSvc,
		cleanup:           cleanupSvc,
		agentconnectivity: agentConnEng,
		notifypolicy:      policySvc,
		fault:             faultSvc,
		settings:          settingsSvc,
		bus:               bus,
		hub:               agentHub,
		ret:               cfg.Retention,
	})
	// Desktop tray summary: incident lifecycle changes kick an immediate refresh.
	// The tray counts incidents, not signals, so it matches the fault centre.
	if cfg.Desktop != nil && cfg.Desktop.OnIncidentsChanged != nil {
		onIncidents := func(eventbus.Message) { go cfg.Desktop.OnIncidentsChanged() }
		bus.Subscribe(eventbus.TopicIncidentOpened, onIncidents)
		bus.Subscribe(eventbus.TopicIncidentResolved, onIncidents)
	}

	// A packaged dist (desktop) is served straight from the supplied fs.FS;
	// everything else resolves an installed version on disk and downloads in the
	// background.
	var webuiMgr *webui.Manager
	if cfg.WebUIFS != nil {
		webuiMgr = webui.NewPackaged(cfg.WebUIFS)
	} else {
		webuiMgr = webui.New(cfg.WebUIDir, webui.Version)
	}

	// Update checking. A standalone server checks its own release; the desktop
	// passes the app version it actually ships as, plus a Store query when it was
	// installed from the Store. New returns nil when the off switch is set, and
	// every call below is nil-safe.
	updateCfg := updatecheck.Config{
		InstallType:    updatecheck.InstallServer,
		CurrentVersion: version.Version,
		Settings:       settingsSvc,
	}
	if cfg.Desktop != nil {
		updateCfg.InstallType = updatecheck.InstallDesktop
		if cfg.Desktop.StoreInstall {
			updateCfg.InstallType = updatecheck.InstallStore
			updateCfg.Checker = cfg.Desktop.CheckStoreUpdate
		}
		updateCfg.CurrentVersion = cfg.Desktop.AppVersion
		updateCfg.OnUpdate = cfg.Desktop.OnUpdate
	}
	updateSvc := updatecheck.New(updateCfg)

	// Resolve the listen address (DB > flag > default) before building the router
	// so its status closure can report the outcome; the actual bind happens below.
	listenRes := resolveListenAddr(ctx, settingsSvc, cfg)

	// s is allocated before the router because the api Deps closures read it
	// (lazily, at request time — well after Start has filled ln/baseURL/listen).
	s := &Server{
		cfg:         cfg,
		db:          db,
		agentHub:    agentHub,
		broker:      broker,
		workers:     w,
		webui:       webuiMgr,
		idSvc:       idSvc,
		regSvc:      reg,
		setSvc:      settingsSvc,
		auditSvc:    auditSvc,
		incidentSvc: incidentSvc,
		updateSvc:   updateSvc,
		adminID:     admin.ID,
		errCh:       make(chan error, 1),
	}

	// Where this process runs decides whether the listen-address control is the
	// operator's to use at all (see container.go). Probed once, here.
	container := detectContainer()

	apiDeps := api.Deps{
		Identity:          idSvc,
		Registry:          reg,
		Metrics:           metricsStore,
		Cleanup:           cleanupSvc,
		Config:            cfgSvc,
		Site:              siteSvc,
		Inventory:         invSvc,
		GameData:          gameSvc,
		Fault:             faultSvc,
		NotifyPolicy:      policySvc,
		Incident:          incidentSvc,
		IncidentOps:       incidentOps,
		Notification:      notifSvc,
		Settings:          settingsSvc,
		Audit:             auditSvc,
		HostLive:          hostLive,
		OpIssue:           opSvc,
		TargetStatus:      tgtStatusSvc,
		AgentStatus:       agentStatusSvc,
		AgentConnectivity: agentConnEng,
		SSE:               broker,
		AgentWS:           agentHub,
		Bus:               bus,
		SPA:               webuiMgr.Handler(),
		Dev:               cfg.Dev,
		SecureCookie:      cfg.SecureCookie,
		Update:            updateSvc,
		Version:           updateCfg.CurrentVersion,
		ListenStatus: func(context.Context) *api.ListenStatus {
			return &api.ListenStatus{
				EffectiveAddr: effectiveAddr(s.listen.addr, s.ln.Addr()),
				Source:        s.listen.source,
				Desktop:       cfg.Desktop != nil,
				FallbackFrom:  s.listen.fallbackFrom,
				OverridesFlag: s.listen.source == "db" && cfg.AddrFromFlag,
				Container:     container.inContainer,
				NetworkMode:   container.networkMode,
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
	// A wildcard bind must not leak into URLs: a browser sent to
	// http://[::]:12450 goes nowhere. baseOrigin rewrites only that case to
	// 127.0.0.1 (which the desktop's tray login relies on — its binds are always
	// 127.0.0.1 or 0.0.0.0, so its base URL is always loopback) and preserves any
	// specific bound host, which is the only reachable origin for an IPv6- or
	// interface-scoped standalone listener.
	s.baseURL = baseOrigin(scheme, ln.Addr())

	s.httpSrv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// In desktop mode seed the deep-link origin so notifications have somewhere to
	// point on a fresh install, and keep an auto-seeded one in step with the bound
	// port (which moves when a saved listen setting or a bind fallback changes it).
	// A value the user configured — a LAN address, a hostname, a reverse proxy —
	// is the one thing a notification recipient can actually open from another
	// machine, so it is never touched; only an empty or loopback value, which can
	// only have come from this seed (or an equivalent the user typed), is written.
	if cfg.Desktop != nil {
		cur, _ := settingsSvc.Get(ctx, settings.KeyConsoleBaseURL)
		if (cur == "" || isLoopbackOrigin(cur)) && cur != s.baseURL {
			if err := settingsSvc.Set(ctx, settings.KeyConsoleBaseURL, s.baseURL); err != nil {
				// A deep-link origin that never got written (or still names a dead port)
				// sends every notification recipient nowhere. This is part of readiness,
				// so fail Start and clean up the listener/workers (the ok=false defer
				// closes the DB) rather than claim readiness with a broken deep link.
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

	webuiSrc := cfg.WebUIDir
	if cfg.WebUIFS != nil {
		webuiSrc = "packaged"
	}
	log.Printf("nettact-server %s listening on %s (dev=%v, db=%s, max_agents=%d, desktop=%v, webui=%s@%s)",
		version.Version, ln.Addr(), cfg.Dev, cfg.DBPath, cfg.MaxAgents, cfg.Desktop != nil, webui.Version, webuiSrc)

	// Nothing can fail Start past this point, so the background download loop
	// will always be paired with a Shutdown that closes it.
	s.webui.Start()

	// Check for a newer release now and daily after that. nowThenEvery is used
	// rather than a plain ticker because a desktop tray is routinely launched,
	// used and closed inside an hour, and a 24h ticker would then never fire.
	if updateSvc != nil {
		w.nowThenEvery(24*time.Hour, updateSvc.RunOnce)
	}

	ok = true
	return s, nil
}

// BaseURL is the server's own dialable origin: the bound host and actual port,
// with a wildcard bind reported as 127.0.0.1 (e.g. http://127.0.0.1:52344 for a
// 0.0.0.0 bind). On the desktop — whose binds are always 127.0.0.1 or 0.0.0.0 —
// it is therefore always loopback: what the tray opens locally (the one-time
// login URL) and what seeds console_base_url. Use ConsoleBaseURL for anything a
// person on another machine has to open.
func (s *Server) BaseURL() string { return s.baseURL }

// ListenFallbackFrom reports the saved listen address that failed to bind on
// this launch ("" when the server is on its configured address). The desktop
// host checks it after a listen-change restart: a fallback means the address
// the user just saved is NOT in effect, so the "console is now at X"
// notification must not claim it is.
func (s *Server) ListenFallbackFrom() string { return s.listen.fallbackFrom }

// ConsoleBaseURL is the origin to send someone to reach this console — the
// console_base_url setting, which the user can point at a LAN address, hostname,
// or reverse proxy. It falls back to the loopback BaseURL when unset. Notification
// deep links read the same setting through server-core.
func (s *Server) ConsoleBaseURL(ctx context.Context) string {
	if v := s.setSvc.ConsoleBaseURL(ctx); v != "" {
		return v
	}
	return s.baseURL
}

// Err delivers a terminal serve error if the HTTP server stops on its own (not
// via Shutdown). It never sends a value for a clean Shutdown. At most one value
// is sent, and the channel is closed when serving ends (including a clean
// Shutdown), so a lifetime watcher blocked on receive always unblocks.
func (s *Server) Err() <-chan error { return s.errCh }

// OpenIncidentCount reports the default site's open incident count, consumed
// in-process by the desktop tray summary. It counts incidents rather than fault
// signals so the tray badge and the fault centre always show the same number of
// the same thing.
func (s *Server) OpenIncidentCount(ctx context.Context) (int, error) {
	return s.incidentSvc.CountOpen(ctx, site.DefaultSiteID)
}

// CheckUpdatesNow runs one immediate update check on behalf of a person who
// asked for one (the desktop tray menu) and reports the outcome. It deliberately
// does not fire the OnUpdate notification path: the caller is already showing
// the answer.
func (s *Server) CheckUpdatesNow(ctx context.Context) (updatecheck.Status, error) {
	return s.updateSvc.CheckNow(ctx)
}

// UpdateNoticesDisabled reports whether update notices are switched off. The
// desktop tray reads it to render its "Update notifications" checkbox — the
// switch is one server setting shared with the web console, so turning notices
// off in either place silences both.
func (s *Server) UpdateNoticesDisabled(ctx context.Context) bool {
	return s.setSvc.Bool(ctx, settings.KeyUpdateNoticeDisabled)
}

// SetUpdateNoticesDisabled writes the shared update-notice switch (tray menu).
func (s *Server) SetUpdateNoticesDisabled(ctx context.Context, disabled bool) error {
	// "0" rather than "" because Set treats an empty value as a delete, and the
	// stored row is what the console's settings GET reads back.
	v := "0"
	if disabled {
		v = "1"
	}
	return s.setSvc.Set(ctx, settings.KeyUpdateNoticeDisabled, v)
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
			err = errors.New("server: workers did not stop within the shutdown deadline; DB left open to avoid use-after-close")
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

// agentStatusEventData marshals the "agent.status.changed" SSE payload. It is
// signal-only (just the affected site): the client always refetches the whole
// site's agent-status list, so there is nothing to drift.
func agentStatusEventData(siteID string) []byte {
	data, err := json.Marshal(map[string]any{"site_id": siteID})
	if err != nil {
		return []byte(`{"site_id":""}`)
	}
	return data
}

// incidentEventData marshals the "incident.changed" SSE payload: the affected
// site plus the incident id, so a console with that incident's detail open can
// refresh exactly it while the list refetches wholesale.
func incidentEventData(siteID, incidentID string) []byte {
	data, err := json.Marshal(map[string]any{"site_id": siteID, "incident_id": incidentID})
	if err != nil {
		return []byte(`{"site_id":"","incident_id":""}`)
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
		return errors.New("server: Addr is required")
	}
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return fmt.Errorf("server: invalid Addr %q: %w", cfg.Addr, err)
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return errors.New("server: TLSCert and TLSKey must be set together")
	}
	if cfg.Desktop != nil {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("server: desktop mode requires a loopback host, got %q", host)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			// Port 0 (OS-assigned) stays allowed for tests; the desktop app passes
			// the fixed default 12450.
			return fmt.Errorf("server: desktop mode requires a numeric port, got %q", port)
		}
		if cfg.TLSCert != "" {
			return errors.New("server: desktop mode does not use TLS")
		}
	}
	return nil
}
