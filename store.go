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
}

type Token struct {
	ID       string    `json:"id"`
	Hash     string    `json:"hash"` // sha256 hex of the secret; secret never stored
	User     string    `json:"user"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`
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

func (db *DB) CreateToken(user, name string) (*Token, string, error) {
	secret := "hst_" + randHex(24)
	db.mu.Lock()
	defer db.mu.Unlock()
	t := &Token{ID: randHex(8), Hash: sha256hex(secret), User: user, Name: name, Created: time.Now().UTC()}
	db.d.Tokens[t.ID] = t
	c := *t
	return &c, secret, db.save()
}

func (db *DB) TokenUser(secret string) *User {
	h := sha256hex(secret)
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, t := range db.d.Tokens {
		if constantTimeEq(t.Hash, h) {
			u, ok := db.d.Users[t.User]
			if !ok {
				return nil
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
			return &c
		}
	}
	return nil
}

func (db *DB) Tokens(user string) []*Token {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := []*Token{}
	for _, t := range db.d.Tokens {
		if t.User == user {
			c := *t
			c.Hash = ""
			out = append(out, &c)
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

func (u *User) clone() *User {
	c := *u
	return &c
}

func (s *Site) clone() *Site {
	c := *s
	c.Members = append([]string(nil), s.Members...)
	return &c
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
