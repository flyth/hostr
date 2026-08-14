package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	sessionCookie = "hostr_session"
	sessionTTL    = 30 * 24 * time.Hour
)

// Argon2id parameters. 64 MiB / 1 pass / 4 lanes is the RFC 9106 second
// recommended profile — comfortable on a small container, far past the point
// where an offline attacker gets cheap guesses.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
)

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func constantTimeEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding.EncodeToString
	return "argon2id$" + b64(salt) + "$" + b64(key), nil
}

func verifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 || parts[0] != "argon2id" {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---- sessions ----

// ponytail: sessions live in memory, so a restart logs everyone out. That is
// the whole cost, and it buys automatic expiry plus nothing sensitive on disk.
// Persist them only if restarts become frequent enough to annoy.
type sessions struct {
	mu sync.Mutex
	m  map[string]sessionEntry
}

type sessionEntry struct {
	user    string
	expires time.Time
}

func newSessions() *sessions { return &sessions{m: map[string]sessionEntry{}} }

func (s *sessions) create(userID string) string {
	tok := randHex(32)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.m[sha256hex(tok)] = sessionEntry{user: userID, expires: time.Now().Add(sessionTTL)}
	return tok
}

func (s *sessions) lookup(tok string) (string, bool) {
	k := sha256hex(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[k]
	if !ok || time.Now().After(e.expires) {
		delete(s.m, k)
		return "", false
	}
	return e.user, true
}

func (s *sessions) drop(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, sha256hex(tok))
}

// dropUser invalidates every session belonging to a user (password change).
func (s *sessions) dropUser(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.m {
		if e.user == userID {
			delete(s.m, k)
		}
	}
}

func (s *sessions) sweepLocked() {
	now := time.Now()
	for k, e := range s.m {
		if now.After(e.expires) {
			delete(s.m, k)
		}
	}
}

// ---- login throttle ----

// ponytail: fixed-window counter per IP, no cleanup goroutine — the map is
// swept lazily. Replace with a real limiter only if this ever faces the open
// internet at volume.
type throttle struct {
	mu     sync.Mutex
	m      map[string]*throttleEntry
	limit  int
	window time.Duration
}

type throttleEntry struct {
	n     int
	until time.Time
}

func newThrottle(limit int, window time.Duration) *throttle {
	return &throttle{m: map[string]*throttleEntry{}, limit: limit, window: window}
}

func (t *throttle) allow(key string) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, e := range t.m {
		if now.After(e.until) {
			delete(t.m, k)
		}
	}
	e, ok := t.m[key]
	if !ok {
		t.m[key] = &throttleEntry{n: 1, until: now.Add(t.window)}
		return true
	}
	e.n++
	return e.n <= t.limit
}

func (t *throttle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, key)
}

// ---- request authentication ----

type authKind int

const (
	authNone authKind = iota
	authCookie
	authToken
)

type caller struct {
	user *User
	kind authKind
}

// authenticate resolves the caller from a bearer token or a session cookie, in
// that order. Bearer tokens are for hostrctl; cookies are for the SPA.
func (s *Server) authenticate(r *http.Request) caller {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		secret := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if u := s.db.TokenUser(secret); u != nil {
			return caller{user: u, kind: authToken}
		}
		return caller{}
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return caller{}
	}
	id, ok := s.sessions.lookup(c.Value)
	if !ok {
		return caller{}
	}
	if u := s.db.User(id); u != nil {
		return caller{user: u, kind: authCookie}
	}
	return caller{}
}

// checkCSRF guards cookie-authenticated state changes. Token callers are
// exempt: an attacker who cannot read the token cannot forge the header, so
// there is nothing to protect against.
func (s *Server) checkCSRF(r *http.Request, c caller) error {
	if c.kind != authCookie {
		return nil
	}
	return s.checkOrigin(r)
}

// checkOrigin rejects a state-changing request whose Origin is some other
// site. Unauthenticated endpoints need this too: login and register *set* the
// session cookie rather than send one, so SameSite does not cover them, and a
// forged login silently drops the victim into the attacker's account.
func (s *Server) checkOrigin(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// A browser always sends Origin on a cross-origin state change, so an
		// absent header means a non-browser client (hostrctl, curl) with no
		// ambient cookie to abuse.
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(u.Host, r.Host) {
		return errors.New("cross-origin request rejected")
	}
	return nil
}

func (s *Server) setSessionCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// clientIP is used only for login throttling. With HOSTR_TRUST_PROXY set we
// read the *last* X-Forwarded-For entry: a reverse proxy appends the peer it
// actually saw, so trailing entries are the ones it vouches for. Earlier
// entries are attacker-controlled and deliberately ignored.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
