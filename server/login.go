package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nettact/server-core/api"
)

// maxOutstandingLoginTokens bounds the in-memory token store so rapid tray
// clicking or repeated failed browser launches can never grow memory. The oldest
// token is evicted once the cap is reached; abandoned tokens also expire on TTL.
const maxOutstandingLoginTokens = 16

// ErrNotDesktop is returned by MintLoginURL when the server was not started in
// desktop mode (the one-time-login surface only exists then).
var ErrNotDesktop = errors.New("server: login URLs require desktop mode")

// LoginTargetKind names the in-console destination a one-time login lands on.
// The zero value is the console root, which is what a plain tray activation
// wants.
type LoginTargetKind int

const (
	TargetConsole LoginTargetKind = iota
	TargetIncident
	TargetStorm
)

// LoginTarget is where a one-time login should land. Only a kind and a resource
// id cross this API: the redirect path itself is built here, server-side, so a
// caller can never steer the post-login redirect at an arbitrary URL. That is
// the difference between a login endpoint and an open redirect, and it is why
// there is deliberately no "next" or "redirect" parameter anywhere below.
type LoginTarget struct {
	Kind LoginTargetKind
	ID   string
}

// Resource ids are minted as a fixed prefix plus a lowercase uuid (see
// fault.Engine and notifypolicy storm handling), so anything else in a target is
// either a bug or an injection attempt and gets no redirect at all.
var (
	incidentIDRe = regexp.MustCompile(`^inc_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	stormIDRe    = regexp.MustCompile(`^stm_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// redirectPath renders the target as a same-origin console path. An unknown kind
// or a malformed id degrades to the console root instead of failing the mint:
// the user clicked a notification and expects a signed-in console, so landing on
// the root beats an error page, and the incident they wanted is one click away.
func (t LoginTarget) redirectPath() string {
	switch t.Kind {
	case TargetIncident:
		if incidentIDRe.MatchString(t.ID) {
			return "/incidents?incident=" + url.QueryEscape(t.ID)
		}
	case TargetStorm:
		if stormIDRe.MatchString(t.ID) {
			return "/incidents?storm=" + url.QueryEscape(t.ID)
		}
	}
	return "/"
}

// loginGrant is what a live token buys: a deadline and the one path it may land
// on. Binding the destination at mint time (rather than reading it from the
// redemption request) is what keeps the endpoint from becoming an open redirect.
type loginGrant struct {
	expiry   time.Time
	redirect string
}

// loginTokens is the in-memory store of live one-time login tokens. Only the
// SHA-256 of each token is kept (never the token bytes), keyed for constant-time
// map lookup and so a store dump can never leak a redeemable token. Never
// persisted, never logged.
type loginTokens struct {
	ttl time.Duration

	mu sync.Mutex
	m  map[[32]byte]loginGrant // token hash → grant
}

func newLoginTokens(ttl time.Duration) *loginTokens {
	return &loginTokens{ttl: ttl, m: map[[32]byte]loginGrant{}}
}

// mint issues a fresh single-use token bound to redirect and returns its raw
// value. It first evicts expired entries and, if still at capacity, the entry
// closest to expiry (which, with a constant TTL, is the oldest) — so the store
// stays bounded regardless of how often the tray is clicked.
func (l *loginTokens) mint(redirect string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(token))
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()
	for h, g := range l.m {
		if now.After(g.expiry) {
			delete(l.m, h)
		}
	}
	for len(l.m) >= maxOutstandingLoginTokens {
		var oldestH [32]byte
		var oldestExp time.Time
		first := true
		for h, g := range l.m {
			if first || g.expiry.Before(oldestExp) {
				oldestH, oldestExp, first = h, g.expiry, false
			}
		}
		delete(l.m, oldestH)
	}
	l.m[hash] = loginGrant{expiry: now.Add(l.ttl), redirect: redirect}
	return token, nil
}

// redeem atomically takes and deletes the token, returning its bound redirect
// and true only if it was present and unexpired. Single-use by construction: a
// second redemption of the same token finds nothing. A hashed, expired, or
// unknown token all return false with no distinction, so nothing about validity
// leaks.
func (l *loginTokens) redeem(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	hash := sha256.Sum256([]byte(token))
	l.mu.Lock()
	g, ok := l.m[hash]
	if ok {
		delete(l.m, hash)
	}
	l.mu.Unlock()
	if !ok || !time.Now().Before(g.expiry) {
		return "", false
	}
	return g.redirect, true
}

// MintLoginURL mints a fresh one-time browser-login URL that lands on target. It
// is callable any time and any number of times (startup, every tray activation,
// second-instance activation, every notification deep link); each call yields a
// new single-use token. The returned URL is handed straight to the browser and
// must never be logged, persisted, displayed, or copied.
//
// The URL is built on the loopback origin this server is actually listening on,
// never on console_base_url — that setting may name a LAN address or a reverse
// proxy, which is a different browser origin and therefore a different cookie
// jar from the one this session lands in.
func (s *Server) MintLoginURL(target LoginTarget) (string, error) {
	if s.login == nil {
		return "", ErrNotDesktop
	}
	token, err := s.login.mint(target.redirectPath())
	if err != nil {
		return "", err
	}
	return s.baseURL + "/desktop/login?token=" + token, nil
}

// handleDesktopLogin redeems a one-time token and establishes the normal
// authenticated session, then redirects to the token-free path the token was
// minted for. Any failure (missing/expired/replayed token, or a non-loopback
// caller) instead redirects to "/" with no detail — the SPA guard lands on
// /login at worst, and nothing about token validity is revealed. The handler
// never logs the token.
func (s *Server) handleDesktopLogin(w http.ResponseWriter, r *http.Request) {
	// These responses must never be cached or leak the token via Referer.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")

	// Defense in depth on top of the loopback-only bind: reject any non-loopback
	// peer outright. This runs before redemption so a rejected probe cannot burn
	// a token the legitimate browser is about to use.
	if !isLoopbackRemote(r.RemoteAddr) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	redirect, ok := s.login.redeem(r.URL.Query().Get("token"))
	if !ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	sid, exp, err := s.idSvc.CreateSession(r.Context(), s.adminID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	api.SetSessionCookie(w, sid, exp, s.cfg.SecureCookie)
	// The store only ever holds paths built by redirectPath, but this is the last
	// point before a redirect leaves an endpoint that just handed out a session
	// cookie: anything that is not a same-origin absolute path goes to the root.
	if !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/"
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
