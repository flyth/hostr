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

// blocked reports whether a key is already over its limit, without counting a
// fresh attempt against it. Callers that only want to *charge* failures use
// this plus fail, so a correct credential is never spent on someone else's
// guessing.
func (t *throttle) blocked(key string) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[key]
	return ok && now.Before(e.until) && e.n >= t.limit
}

// ponytail: a hard cap rather than smarter eviction. Site auth keys include the
// submitted username, so a client can mint fresh keys at will; expired entries
// are swept on every call and this stops a flood from growing the map without
// bound in between. Dropping the lot only forgets failure counts, which the same
// client could achieve by changing username anyway. Make it an LRU if a server
// ever faces this at volume.
const throttleMax = 8192

func (t *throttle) fail(key string) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, e := range t.m {
		if now.After(e.until) {
			delete(t.m, k)
		}
	}
	if len(t.m) >= throttleMax {
		clear(t.m)
	}
	if e, ok := t.m[key]; ok {
		e.n++
		return
	}
	t.m[key] = &throttleEntry{n: 1, until: now.Add(t.window)}
}

func (t *throttle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, key)
}

// ---- site basic auth ----

// ponytail: verified credentials are memoised for an hour rather than
// re-hashed per request. Argon2id at 64 MiB is ~50 ms — fine once per visitor,
// ruinous on a page with forty images. The key mixes in a digest of every
// credential the site holds, so adding an account, removing one or changing any
// password invalidates its entries with no bookkeeping. Swap for a signed cookie
// if the memoisation window ever needs to survive a restart.
type authCache struct {
	mu sync.Mutex
	m  map[string]authEntry

	// verify serialises the argon2 work itself. A page with forty images
	// produces forty simultaneous requests carrying the same credentials, and
	// without this every one of them would run its own 64 MiB hash before the
	// first had a chance to fill the cache. Whoever wins re-checks the cache
	// after waiting, so the rest cost nothing.
	//
	// ponytail: one lock for the whole server rather than one per credential.
	// Verifications are ~50 ms and only happen on a cold cache, so the queue
	// is short; make it per-key if a server ever hosts enough protected sites
	// for that to matter.
	verify sync.Mutex
}

// authEntry remembers which account a memoised credential belongs to, so a
// cache hit still attributes its traffic to the right one rather than counting
// every returning visitor as anonymous.
type authEntry struct {
	account string
	expires time.Time
}

const siteAuthTTL = time.Hour

func newAuthCache() *authCache { return &authCache{m: map[string]authEntry{}} }

func (c *authCache) key(siteID, fingerprint, header string) string {
	return sha256hex(siteID + "\x00" + fingerprint + "\x00" + header)
}

func (c *authCache) ok(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expires) {
		delete(c.m, key)
		return "", false
	}
	return e.account, true
}

func (c *authCache) put(key, account string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, e := range c.m {
		if now.After(e.expires) {
			delete(c.m, k)
		}
	}
	// A hostile client can mint unlimited *failing* credentials, but only
	// successful ones land here, so the map is bounded by real passwords in
	// use. The cap is a backstop against a pathological number of sites.
	if len(c.m) > 4096 {
		clear(c.m)
	}
	c.m[key] = authEntry{account: account, expires: now.Add(siteAuthTTL)}
}

// checkSiteAuth gates a tenant site behind its basic-auth accounts, if it has
// any. It answers the request itself and returns false when the caller may not
// proceed; the string it returns on success is the account that opened the
// site, which is empty when the site has no password at all.
//
// realm is the hostname the request actually arrived on, not the site's
// canonical domain: a guest link must not disclose that, and a browser scopes
// stored credentials per host anyway.
func (s *Server) checkSiteAuth(w http.ResponseWriter, r *http.Request, site *Site, realm string) (string, bool) {
	if len(site.Accounts) == 0 {
		return "", true
	}
	deny := func(status int, msg string) (string, bool) {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`", charset="UTF-8"`)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, msg, status)
		return "", false
	}

	header := r.Header.Get("Authorization")
	if header == "" {
		return deny(http.StatusUnauthorized, "401 authentication required")
	}
	key := s.siteAuth.key(site.ID, site.authFingerprint(), header)
	// A credential that has already been verified never touches the throttle,
	// so a visitor who is signed in stays signed in while someone else is
	// guessing — which matters because behind a reverse proxy every visitor
	// may share one address.
	if id, ok := s.siteAuth.ok(key); ok {
		return id, true
	}

	user, pass, _ := r.BasicAuth()
	// The throttle is per site, per client address *and per submitted username*.
	// Without the username, thirty wrong guesses lock out every first-time
	// visitor to the site — behind a reverse proxy without HOSTR_TRUST_PROXY the
	// whole audience shares one address, and since a success no longer clears
	// the counter there would be nothing anyone could do but wait it out.
	// Guessing one account's password still costs the guesser their budget for
	// that account, which is the budget that matters; spraying usernames buys
	// nothing, because on this server a username is itself a secret.
	ip := s.clientIP(r)
	tkey := "site:" + site.ID + ":" + ip + ":" + sha256hex(user)
	if s.siteThrottle.blocked(tkey) {
		return deny(http.StatusTooManyRequests, "429 too many attempts, try again later")
	}

	s.siteAuth.verify.Lock()
	defer s.siteAuth.verify.Unlock()
	if id, ok := s.siteAuth.ok(key); ok {
		return id, true // another request verified the same credentials while we waited
	}

	// Every account is compared, without stopping at a hit, and exactly one
	// argon2 verification runs whether or not anything matched — against a hash
	// that can never verify when nothing did. A wrong username and a wrong
	// password therefore cost the same, and neither which accounts exist nor how
	// many of them there are is recoverable from response timing. Usernames are
	// unique per site, so at most one account can match.
	var matched *SiteAccount
	for _, a := range site.Accounts {
		if constantTimeEq(user, a.User) {
			matched = a
		}
	}
	encoded := dummyHash
	if matched != nil {
		encoded = matched.Hash
	}
	if !verifyPassword(encoded, pass) || matched == nil {
		s.siteThrottle.fail(tkey)
		return deny(http.StatusUnauthorized, "401 invalid credentials")
	}
	// Success deliberately does *not* clear the failure counter. With several
	// accounts on one site, whoever holds one valid credential could otherwise
	// spend the budget guessing at another, present their own to zero it, and
	// repeat — the throttle guarding everyone else's passwords would never fire.
	// A verified visitor is already exempt through the memo above, which is all
	// the reset was ever for.
	s.siteAuth.put(key, matched.ID)
	return matched.ID, true
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
	// token is set only for bearer callers. It carries the site and scope
	// limits the write handlers enforce; a cookie caller has none.
	token *Token
	kind  authKind
}

// authenticate resolves the caller from a bearer token or a session cookie, in
// that order. Bearer tokens are for hostrctl; cookies are for the SPA.
func (s *Server) authenticate(r *http.Request) caller {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		secret := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if u, t := s.db.TokenUser(secret); u != nil {
			return caller{user: u, token: t, kind: authToken}
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
