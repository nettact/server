package liteserver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
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
var ErrNotDesktop = errors.New("liteserver: login URLs require desktop mode")

// loginTokens is the in-memory store of live one-time login tokens. Only the
// SHA-256 of each token is kept (never the token bytes), keyed for constant-time
// map lookup and so a store dump can never leak a redeemable token. Never
// persisted, never logged.
type loginTokens struct {
	ttl time.Duration

	mu sync.Mutex
	m  map[[32]byte]time.Time // token hash → expiry
}

func newLoginTokens(ttl time.Duration) *loginTokens {
	return &loginTokens{ttl: ttl, m: map[[32]byte]time.Time{}}
}

// mint issues a fresh single-use token and returns its raw value. It first evicts
// expired entries and, if still at capacity, the entry closest to expiry (which,
// with a constant TTL, is the oldest) — so the store stays bounded regardless of
// how often the tray is clicked.
func (l *loginTokens) mint() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(token))
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()
	for h, exp := range l.m {
		if now.After(exp) {
			delete(l.m, h)
		}
	}
	for len(l.m) >= maxOutstandingLoginTokens {
		var oldestH [32]byte
		var oldestExp time.Time
		first := true
		for h, exp := range l.m {
			if first || exp.Before(oldestExp) {
				oldestH, oldestExp, first = h, exp, false
			}
		}
		delete(l.m, oldestH)
	}
	l.m[hash] = now.Add(l.ttl)
	return token, nil
}

// redeem atomically takes and deletes the token, returning true only if it was
// present and unexpired. Single-use by construction: a second redemption of the
// same token finds nothing. A hashed, expired, or unknown token all return false
// with no distinction, so nothing about validity leaks.
func (l *loginTokens) redeem(token string) bool {
	if token == "" {
		return false
	}
	hash := sha256.Sum256([]byte(token))
	l.mu.Lock()
	exp, ok := l.m[hash]
	if ok {
		delete(l.m, hash)
	}
	l.mu.Unlock()
	return ok && time.Now().Before(exp)
}

// MintLoginURL mints a fresh one-time browser-login URL. It is callable any time
// and any number of times (startup, every tray activation, second-instance
// activation); each call yields a new single-use token. The returned URL is
// handed straight to the browser and must never be logged, persisted, displayed,
// or copied.
func (s *Server) MintLoginURL() (string, error) {
	if s.login == nil {
		return "", ErrNotDesktop
	}
	token, err := s.login.mint()
	if err != nil {
		return "", err
	}
	return s.baseURL + "/desktop/login?token=" + token, nil
}

// handleDesktopLogin redeems a one-time token and establishes the normal
// authenticated session, then redirects to the token-free console root. Any
// failure (missing/expired/replayed token, or a non-loopback caller) also
// redirects to "/" with no detail — the SPA guard lands on /login at worst, and
// nothing about token validity is revealed. The handler never logs the token.
func (s *Server) handleDesktopLogin(w http.ResponseWriter, r *http.Request) {
	// These responses must never be cached or leak the token via Referer.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")

	// Defense in depth on top of the loopback-only bind: reject any non-loopback
	// peer outright.
	if !isLoopbackRemote(r.RemoteAddr) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if !s.login.redeem(r.URL.Query().Get("token")) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	sid, exp, err := s.idSvc.CreateSession(r.Context(), s.adminID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	api.SetSessionCookie(w, sid, exp, s.cfg.SecureCookie)
	http.Redirect(w, r, "/", http.StatusFound)
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
