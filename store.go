package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ponytail: one JSON file under an RWMutex instead of a database. This holds
// users, sites, tokens and invites for a handful of friends — a few kilobytes
// rewritten on every mutation. Swap in SQLite if the user count ever reaches
// the thousands, or if writes start showing up in a profile.

type User struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Hash    string    `json:"hash"` // argon2id, encoded
	Admin   bool      `json:"admin"`
	Created time.Time `json:"created"`
}

type Site struct {
	ID      string    `json:"id"`
	Slug    string    `json:"slug"`
	Domain  string    `json:"domain"`
	Owner   string    `json:"owner"`
	Members []string  `json:"members"` // user IDs with deploy + read access
	Created time.Time `json:"created"`

	// Listing turns the directory browser on for URLs that have no index.html.
	// Off by default: switching it on makes every path in the site
	// enumerable, which is a real disclosure for anyone who was relying on a
	// URL being unguessable.
	Listing bool `json:"listing"`
	// ListingNoRoot keeps the site root out of the browser while leaving every
	// directory below it listable. A visitor who already knows a path can browse
	// it; nobody can walk the whole site starting from `/`. Only meaningful
	// while Listing is on.
	ListingNoRoot bool `json:"listing_no_root"`
	// Markdown renders `.md` files as documents instead of handing over the
	// source. It is opt-in because it changes what a markdown file can do: a
	// rendered document runs whatever HTML and script the markdown contains, on
	// the site's own origin, which is the same trust a deployed `.html` file
	// already has but not the trust a `.md` file had a moment ago.
	Markdown bool `json:"markdown"`
	// ScopedOnly refuses any write that is not confined to a scope: no
	// whole-site deploy, no whole-site delete, and no writing or deleting a
	// file at the top level. Deleting one named scope is still a scoped write
	// and remains allowed.
	ScopedOnly bool `json:"scoped_only"`

	// Basic auth in front of the served site. AuthHash is argon2id like a user
	// password and must never leave the server; siteView shadows it.
	AuthUser string `json:"auth_user,omitempty"`
	AuthHash string `json:"auth_hash,omitempty"`
}

type Token struct {
	ID       string    `json:"id"`
	Hash     string    `json:"hash"` // sha256 hex of the secret; secret never stored
	User     string    `json:"user"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`

	// Site restricts the token to one site, Scopes to a set of path prefixes
	// within it. Both empty means the token can do whatever its user can. A
	// token with scopes may be reused across all of them, but cannot write
	// outside them and cannot write the site root.
	Site   string   `json:"site,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
}

// Invite codes are stored in the clear, unlike tokens. They are single-use,
// grant only the ability to create an account, and the admin needs to be able
// to re-read one to hand it over. A leak of this file already exposes password
// hashes, so hashing invites buys nothing.
type Invite struct {
	Code    string    `json:"code"`
	Creator string    `json:"creator"`
	Created time.Time `json:"created"`
	UsedBy  string    `json:"used_by,omitempty"`
	UsedAt  time.Time `json:"used_at,omitzero"`
}

type data struct {
	Users   map[string]*User   `json:"users"`
	Sites   map[string]*Site   `json:"sites"`
	Tokens  map[string]*Token  `json:"tokens"`
	Invites map[string]*Invite `json:"invites"`
}

type DB struct {
	mu   sync.RWMutex
	path string
	d    data
}

var (
	errNotFound = errors.New("not found")
	errConflict = errors.New("already exists")
)

func openDB(path string) (*DB, error) {
	db := &DB{path: path, d: data{
		Users:   map[string]*User{},
		Sites:   map[string]*Site{},
		Tokens:  map[string]*Token{},
		Invites: map[string]*Invite{},
	}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return db, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &db.d); err != nil {
		return nil, err
	}
	if db.d.Users == nil {
		db.d.Users = map[string]*User{}
	}
	if db.d.Sites == nil {
		db.d.Sites = map[string]*Site{}
	}
	if db.d.Tokens == nil {
		db.d.Tokens = map[string]*Token{}
	}
	if db.d.Invites == nil {
		db.d.Invites = map[string]*Invite{}
	}
	return db, nil
}

// save writes the whole store atomically. Callers must hold the write lock.
func (db *DB) save() error {
	b, err := json.MarshalIndent(db.d, "", "  ")
	if err != nil {
		return err
	}
	tmp := db.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, db.path)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is not a recoverable condition
	}
	return hex.EncodeToString(b)
}

// ---- users ----

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

func validName(s string) bool { return nameRe.MatchString(s) }

func (db *DB) CreateUser(name, password string, admin bool) (*User, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !validName(name) {
		return nil, errors.New("username must be 2-32 chars: a-z 0-9 . _ -")
	}
	if len(password) < 10 {
		return nil, errors.New("password must be at least 10 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, u := range db.d.Users {
		if u.Name == name {
			return nil, errConflict
		}
	}
	u := &User{ID: randHex(8), Name: name, Hash: hash, Admin: admin, Created: time.Now().UTC()}
	db.d.Users[u.ID] = u
	// Hand back a copy, like every read accessor does. Returning the stored
	// pointer lets the caller read fields that a later mutation is writing.
	return u.clone(), db.save()
}

func (db *DB) UserByName(name string) *User {
	db.mu.RLock()
	defer db.mu.RUnlock()
	for _, u := range db.d.Users {
		if u.Name == name {
			c := *u
			return &c
		}
	}
	return nil
}

func (db *DB) User(id string) *User {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if u, ok := db.d.Users[id]; ok {
		c := *u
		return &c
	}
	return nil
}

func (db *DB) Users() []*User {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]*User, 0, len(db.d.Users))
	for _, u := range db.d.Users {
		c := *u
		out = append(out, &c)
	}
	return out
}

func (db *DB) UserCount() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.d.Users)
}

func (db *DB) SetPassword(id, password string) error {
	if len(password) < 10 {
		return errors.New("password must be at least 10 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	u, ok := db.d.Users[id]
	if !ok {
		return errNotFound
	}
	u.Hash = hash
	return db.save()
}

// ---- invites ----

func (db *DB) CreateInvite(creator string) (*Invite, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	inv := &Invite{Code: randHex(16), Creator: creator, Created: time.Now().UTC()}
	db.d.Invites[inv.Code] = inv
	return inv, db.save()
}

func (db *DB) Invites() []*Invite {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]*Invite, 0, len(db.d.Invites))
	for _, i := range db.d.Invites {
		c := *i
		out = append(out, &c)
	}
	return out
}

func (db *DB) DeleteInvite(code string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.d.Invites[code]; !ok {
		return errNotFound
	}
	delete(db.d.Invites, code)
	return db.save()
}

// Redeem consumes an invite and creates the account in one critical section, so
// two people racing on the same code cannot both end up with an account.
func (db *DB) Redeem(code, name, password string) (*User, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !validName(name) {
		return nil, errors.New("username must be 2-32 chars: a-z 0-9 . _ -")
	}
	if len(password) < 10 {
		return nil, errors.New("password must be at least 10 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	inv, ok := db.d.Invites[code]
	if !ok || inv.UsedBy != "" {
		return nil, errNotFound
	}
	for _, u := range db.d.Users {
		if u.Name == name {
			return nil, errConflict
		}
	}
	u := &User{ID: randHex(8), Name: name, Hash: hash, Created: time.Now().UTC()}
	db.d.Users[u.ID] = u
	inv.UsedBy = u.ID
	inv.UsedAt = time.Now().UTC()
	return u.clone(), db.save()
}

// ---- tokens ----

func (db *DB) CreateToken(user, name, site string, scopes []string) (*Token, string, error) {
	for _, sc := range scopes {
		if _, ok := cleanPath(sc); !ok {
			return nil, "", errors.New("illegal scope: " + sc)
		}
	}
	secret := "hst_" + randHex(24)
	db.mu.Lock()
	defer db.mu.Unlock()
	if site != "" {
		if _, ok := db.d.Sites[site]; !ok {
			return nil, "", errNotFound
		}
	}
	t := &Token{
		ID: randHex(8), Hash: sha256hex(secret), User: user, Name: name,
		Created: time.Now().UTC(), Site: site, Scopes: scopes,
	}
	db.d.Tokens[t.ID] = t
	return t.clone(), secret, db.save()
}

// TokenUser resolves a bearer secret to its owner and the token itself; the
// token carries the site and scope limits the API has to enforce.
func (db *DB) TokenUser(secret string) (*User, *Token) {
	h := sha256hex(secret)
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, t := range db.d.Tokens {
		if constantTimeEq(t.Hash, h) {
			u, ok := db.d.Users[t.User]
			if !ok {
				return nil, nil
			}
			// Last-used is a coarse "is this token still in use" signal, so it
			// is only flushed once a minute. A deploy fires hundreds of
			// authenticated requests and none of them should rewrite the store.
			now := time.Now().UTC()
			if now.Sub(t.LastUsed) > time.Minute {
				t.LastUsed = now
				_ = db.save()
			}
			c := *u
			return &c, t.clone()
		}
	}
	return nil, nil
}

func (db *DB) Tokens(user string) []*Token {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := []*Token{}
	for _, t := range db.d.Tokens {
		if t.User == user {
			c := t.clone()
			c.Hash = ""
			out = append(out, c)
		}
	}
	return out
}

func (db *DB) DeleteToken(user, id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.d.Tokens[id]
	if !ok || t.User != user {
		return errNotFound
	}
	delete(db.d.Tokens, id)
	return db.save()
}

// ---- sites ----

var (
	slugRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

func normDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimSuffix(s, ".")
}

func (db *DB) CreateSite(owner, slug, domain string) (*Site, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	domain = normDomain(domain)
	if !slugRe.MatchString(slug) {
		return nil, errors.New("slug must be 1-63 chars: a-z 0-9 -")
	}
	if !domainRe.MatchString(domain) || len(domain) > 253 {
		return nil, errors.New("invalid domain")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, s := range db.d.Sites {
		// Domains are globally unique: without this any tenant could claim a
		// domain already bound to someone else's site and take over its traffic.
		if s.Domain == domain {
			return nil, errConflict
		}
		if s.Owner == owner && s.Slug == slug {
			return nil, errConflict
		}
	}
	s := &Site{ID: randHex(8), Slug: slug, Domain: domain, Owner: owner, Members: []string{}, Created: time.Now().UTC()}
	db.d.Sites[s.ID] = s
	return s.clone(), db.save()
}

func (db *DB) Site(id string) *Site {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if s, ok := db.d.Sites[id]; ok {
		return s.clone()
	}
	return nil
}

func (db *DB) SiteByDomain(domain string) *Site {
	db.mu.RLock()
	defer db.mu.RUnlock()
	for _, s := range db.d.Sites {
		if s.Domain == domain {
			return s.clone()
		}
	}
	return nil
}

// SitesFor returns the sites a user may deploy to and read.
func (db *DB) SitesFor(u *User) []*Site {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := []*Site{}
	for _, s := range db.d.Sites {
		if s.allows(u) {
			out = append(out, s.clone())
		}
	}
	return out
}

func (db *DB) SetDomain(id, domain string) error {
	domain = normDomain(domain)
	if !domainRe.MatchString(domain) || len(domain) > 253 {
		return errors.New("invalid domain")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.d.Sites[id]
	if !ok {
		return errNotFound
	}
	for _, o := range db.d.Sites {
		if o.ID != id && o.Domain == domain {
			return errConflict
		}
	}
	s.Domain = domain
	return db.save()
}

func (db *DB) AddMember(siteID, userID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.d.Sites[siteID]
	if !ok {
		return errNotFound
	}
	if _, ok := db.d.Users[userID]; !ok {
		return errNotFound
	}
	if userID == s.Owner {
		return errConflict
	}
	for _, m := range s.Members {
		if m == userID {
			return errConflict
		}
	}
	s.Members = append(s.Members, userID)
	return db.save()
}

func (db *DB) RemoveMember(siteID, userID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.d.Sites[siteID]
	if !ok {
		return errNotFound
	}
	out := s.Members[:0]
	for _, m := range s.Members {
		if m != userID {
			out = append(out, m)
		}
	}
	s.Members = out
	return db.save()
}

func (db *DB) DeleteSite(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.d.Sites[id]; !ok {
		return errNotFound
	}
	delete(db.d.Sites, id)
	return db.save()
}

// siteSettings carries the boolean site flags of a settings change. A nil
// pointer means the caller did not send that flag and it keeps its value. They
// travel as one struct rather than as positional arguments: they are all the
// same type, and swapping two of them would silently flip the wrong switch.
type siteSettings struct {
	Listing       *bool
	ListingNoRoot *bool
	Markdown      *bool
	ScopedOnly    *bool
}

func (set siteSettings) empty() bool {
	return set.Listing == nil && set.ListingNoRoot == nil && set.Markdown == nil && set.ScopedOnly == nil
}

// SetSiteSettings applies whichever flags the caller actually sent.
func (db *DB) SetSiteSettings(id string, set siteSettings) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.d.Sites[id]
	if !ok {
		return errNotFound
	}
	if set.Listing != nil {
		s.Listing = *set.Listing
	}
	if set.ListingNoRoot != nil {
		s.ListingNoRoot = *set.ListingNoRoot
	}
	if set.Markdown != nil {
		s.Markdown = *set.Markdown
	}
	if set.ScopedOnly != nil {
		s.ScopedOnly = *set.ScopedOnly
	}
	return db.save()
}

// checkSiteCredentials validates a basic-auth pair without doing the expensive
// part, so a caller can reject a bad request before committing anything else.
func checkSiteCredentials(user, password string) error {
	// A colon can never survive the round trip: basic auth joins the two
	// halves with one and the client splits on the first, so such a username
	// would leave the site permanently unopenable.
	if strings.ContainsAny(user, ":\x00") {
		return errors.New("site username cannot contain a colon")
	}
	if len(password) < 8 {
		return errors.New("site password must be at least 8 characters")
	}
	return nil
}

// CheckDomain reports whether a domain is well formed and unclaimed, changing
// nothing.
func (db *DB) CheckDomain(id, domain string) error {
	domain = normDomain(domain)
	if !domainRe.MatchString(domain) || len(domain) > 253 {
		return errors.New("invalid domain")
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	if _, ok := db.d.Sites[id]; !ok {
		return errNotFound
	}
	for _, o := range db.d.Sites {
		if o.ID != id && o.Domain == domain {
			return errConflict
		}
	}
	return nil
}

// SetSiteAuth sets or clears the basic-auth credentials in front of a site. An
// empty user removes protection; anything else needs both halves.
func (db *DB) SetSiteAuth(id, user, password string) error {
	user = strings.TrimSpace(user)
	hash := ""
	if user != "" {
		if err := checkSiteCredentials(user, password); err != nil {
			return err
		}
		var err error
		if hash, err = hashPassword(password); err != nil {
			return err
		}
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.d.Sites[id]
	if !ok {
		return errNotFound
	}
	s.AuthUser, s.AuthHash = user, hash
	return db.save()
}

func (u *User) clone() *User {
	c := *u
	return &c
}

func (s *Site) clone() *Site {
	c := *s
	c.Members = append([]string(nil), s.Members...)
	return &c
}

func (t *Token) clone() *Token {
	c := *t
	c.Scopes = append([]string(nil), t.Scopes...)
	return &c
}

// allowsSite reports whether a site-bound token may touch this site.
func (t *Token) allowsSite(id string) bool {
	return t == nil || t.Site == "" || t.Site == id
}

// restricted reports whether the token is limited to one site, with or without
// scopes. Such a token must not reach the account-level endpoints — above all
// the one that mints tokens, since a restricted token able to issue an
// unrestricted one is not restricted at all.
func (t *Token) restricted() bool {
	return t != nil && (t.Site != "" || len(t.Scopes) > 0)
}

// allowsScope reports whether the token may write the directory `scope`, where
// "" is the site root. A token with scopes is confined to them and to anything
// nested inside them, which is what stops a scoped agent from replacing a
// sibling project or emptying the site.
func (t *Token) allowsScope(scope string) bool {
	if t == nil || len(t.Scopes) == 0 {
		return true
	}
	if scope == "" {
		return false
	}
	for _, s := range t.Scopes {
		if scope == s || strings.HasPrefix(scope, s+"/") {
			return true
		}
	}
	return false
}

// allowsFile is allowsScope for an individual file, and is deliberately
// stricter: a file whose path *equals* the scope is not inside it, it sits
// beside it at the parent level. Serving resolves a bare path before a
// directory index, so allowing that would let a token scoped to `docs` write a
// root-level file that shadows every URL under `docs/`.
func (t *Token) allowsFile(p string) bool {
	if t == nil || len(t.Scopes) == 0 {
		return true
	}
	for _, s := range t.Scopes {
		if strings.HasPrefix(p, s+"/") {
			return true
		}
	}
	return false
}

// listable reports whether dir may be served as a directory listing, where dir
// is manifest-relative and "" is the site root.
func (s *Site) listable(dir string) bool {
	if !s.Listing {
		return false
	}
	return dir != "" || !s.ListingNoRoot
}

// allows reports whether u may read and deploy this site. Admins are the
// server operator and already have filesystem access to every blob, so
// pretending they are gated here would be theatre.
func (s *Site) allows(u *User) bool {
	if u == nil {
		return false
	}
	if u.Admin || s.Owner == u.ID {
		return true
	}
	for _, m := range s.Members {
		if m == u.ID {
			return true
		}
	}
	return false
}
