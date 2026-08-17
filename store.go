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

// Stats is the traffic a site, a guest link or a password account has carried.
// Count is requests, Bytes is response bodies that actually went out. First and
// last answer the only question anyone really asks of a link they handed over:
// was it ever opened, and is it still being opened.
type Stats struct {
	Count int64     `json:"count,omitempty"`
	Bytes int64     `json:"bytes,omitempty"`
	First time.Time `json:"first,omitzero"`
	Last  time.Time `json:"last,omitzero"`
}

// hit folds one request into the counters. counted=false is for the requests a
// visit makes on its own behalf — the fonts, the stylesheet, the redirect that
// only exists to add a trailing slash. Their bytes are real traffic and count;
// they are not separate visits, so they move neither the hit count nor the
// first/last timestamps, which exist to answer "was this opened, and when".
func (s *Stats) hit(now time.Time, n int64, counted bool) {
	s.Bytes += n
	if !counted {
		return
	}
	s.Count++
	if s.First.IsZero() {
		s.First = now
	}
	s.Last = now
}

// Alias is a guest link: a second hostname serving the same site, meant to be
// handed to one person so that what they do with it can be told apart from
// everyone else. It grants nothing the main domain does not — a site behind a
// password is still behind that password on every alias.
type Alias struct {
	ID      string    `json:"id"`
	Domain  string    `json:"domain"`
	Note    string    `json:"note,omitempty"` // who you gave it to
	Created time.Time `json:"created"`
	Stats   Stats     `json:"stats"`
}

// SiteAccount is one basic-auth credential in front of a site. Name is a label
// for the owner's benefit and the only part a client ever sees: the username is
// half of the credential, and a site record is readable by every collaborator.
type SiteAccount struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	User    string    `json:"user"` // never leaves the server
	Hash    string    `json:"hash"` // argon2id, never leaves the server
	Created time.Time `json:"created"`
	Stats   Stats     `json:"stats"`
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

	// Aliases are the guest links pointing at this site, Accounts the basic-auth
	// credentials in front of it. Both live inside the site rather than in a
	// top-level map so that deleting a site takes them with it.
	Aliases  []*Alias       `json:"aliases,omitempty"`
	Accounts []*SiteAccount `json:"accounts,omitempty"`

	// Stats is the traffic that arrived on the site's *own* domain. Each guest
	// link keeps its own, so the total across every hostname is this plus
	// theirs — an addition, deliberately, rather than a site-wide total to
	// subtract from. Revoking or resetting one guest link then takes its traffic
	// out of the total by itself, with no second counter to keep in step.
	Stats Stats `json:"stats"`

	// The single basic-auth credential sites used to carry. Kept only so an
	// existing store still parses: openDB folds it into Accounts and clears it,
	// after which `omitempty` keeps it out of the file for good.
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
	// aliasBase is the parent of every guest link. The store needs it to keep
	// that namespace closed: only CreateAlias may mint a name under it.
	aliasBase string
	// dirty marks a change that is not on disk yet — counters that have moved,
	// or a mutation whose write failed. Traffic accounting deliberately does not
	// write per request; see stats.go.
	dirty bool
}

var (
	errNotFound = errors.New("not found")
	errConflict = errors.New("already exists")
	// errManyAccounts refuses the one-credential shorthand on a site that has
	// several, where obeying it would delete logins the caller never named.
	errManyAccounts = errors.New("this site has more than one password account; add and remove them individually")
)

// openDB loads the store. aliasDomain is the parent guest links hang under, or
// empty when the feature is off; the store holds it because it is the store
// that has to refuse anyone else claiming a name in there.
func openDB(path, aliasDomain string) (*DB, error) {
	db := &DB{path: path, aliasBase: normDomain(aliasDomain), d: data{
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
	if db.migrateAuth() {
		// Written back straight away so the file matches what the server is
		// serving from. A store that has been through this cannot be read by an
		// older binary — it would see no `auth_user` and serve every protected
		// site wide open — so the migration is one way on purpose.
		db.mu.Lock()
		err := db.save()
		db.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	return db, nil
}

// migrateAuth folds the single basic-auth credential a site used to carry into
// the account list, and reports whether anything moved.
func (db *DB) migrateAuth() bool {
	moved := false
	for _, s := range db.d.Sites {
		if s.AuthUser == "" && s.AuthHash == "" {
			continue
		}
		if s.AuthUser != "" && s.AuthHash != "" && len(s.Accounts) == 0 {
			s.Accounts = []*SiteAccount{{
				ID: randHex(8), Name: "default",
				User: s.AuthUser, Hash: s.AuthHash, Created: s.Created,
			}}
		}
		s.AuthUser, s.AuthHash = "", ""
		moved = true
	}
	return moved
}

// save writes the whole store atomically. Callers must hold the write lock.
func (db *DB) save() error {
	// Marked before the write, not after: a mutation whose write fails is live
	// in memory and missing from disk, and the flush timer is what gets it
	// there. Clearing this only on success is what makes a transient ENOSPC
	// heal itself instead of leaving the server enforcing rules the file has
	// never heard of.
	db.dirty = true
	b, err := json.MarshalIndent(db.d, "", "  ")
	if err != nil {
		return err
	}
	tmp := db.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, db.path); err != nil {
		return err
	}
	db.dirty = false
	return nil
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

// reservedDomain reports whether a hostname belongs to the guest-link
// namespace, which only CreateAlias may mint into. Without this any tenant could
// create a site on a name shaped exactly like a guest link and serve their own
// content — their own password prompt, even — under the operator's wildcard
// certificate. Worse, a revoked guest link's name is free the instant it is
// deleted, so whoever grabs it inherits everyone still holding that bookmark.
func (db *DB) reservedDomain(domain string) bool {
	if db.aliasBase == "" {
		return false
	}
	return domain == db.aliasBase || strings.HasSuffix(domain, "."+db.aliasBase)
}

// domainClaimed reports whether a hostname is already in use, by a site's own
// domain or by anybody's guest link. The two share one namespace on purpose:
// they resolve the same way, so letting a site claim a name an alias already
// answers on — or the reverse — would hand one tenant another tenant's traffic.
// exceptSite is the site being renamed, which may of course keep its own name.
// Callers hold at least the read lock.
func (db *DB) domainClaimed(domain, exceptSite string) bool {
	for _, s := range db.d.Sites {
		if s.ID != exceptSite && s.Domain == domain {
			return true
		}
		// A site's own aliases still block its rename: two names resolving to
		// the same site is fine, but the same name meaning two things is not.
		for _, a := range s.Aliases {
			if a.Domain == domain {
				return true
			}
		}
	}
	return false
}

// domainTaken is what every path but guest-link minting asks: is this name
// unavailable, either because something already answers on it or because it is
// not ours to hand out.
func (db *DB) domainTaken(domain, exceptSite string) bool {
	return db.reservedDomain(domain) || db.domainClaimed(domain, exceptSite)
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
	if db.domainTaken(domain, "") {
		return nil, errConflict
	}
	for _, s := range db.d.Sites {
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

// Resolve maps a hostname onto the site that answers on it, plus the guest link
// it arrived through when it was not the site's own domain. Site domains are
// matched first so a guest link can never shadow the real one, even though
// domainTaken already makes that collision impossible to create.
func (db *DB) Resolve(domain string) (*Site, *Alias) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	for _, s := range db.d.Sites {
		if s.Domain == domain {
			return s.clone(), nil
		}
	}
	for _, s := range db.d.Sites {
		for _, a := range s.Aliases {
			if a.Domain == domain {
				return s.clone(), a.clone()
			}
		}
	}
	return nil, nil
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
	if db.domainTaken(domain, id) {
		return errConflict
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
	// An empty username is not a credential. It also collides with the
	// one-credential shorthand, where an empty user means "remove protection",
	// and it matches every unparseable Authorization header, since comparing two
	// empty strings succeeds.
	if user == "" {
		return errors.New("site username cannot be empty")
	}
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
	if db.domainTaken(domain, id) {
		return errConflict
	}
	return nil
}

// SetSiteAuth is the one-credential path the CLI and the old API speak: it
// replaces whatever accounts a site has with a single one, and an empty user
// removes protection entirely. Callers must refuse it on a site with more than
// one account — silently deleting other people's logins is not a settings
// change. The multi-account API is CreateAccount/DeleteAccount.
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
	if user == "" {
		s.Accounts = nil
		return db.save()
	}
	// Re-checked here, under the write lock, and not only in the handler: the
	// handler works from a snapshot taken before the request body was read, and
	// an account added in that window would be deleted by a call that was
	// supposed to be refused.
	if len(s.Accounts) > 1 {
		return errManyAccounts
	}
	acc := &SiteAccount{ID: randHex(8), Name: "default", User: user, Hash: hash, Created: time.Now().UTC()}
	if len(s.Accounts) == 1 && s.Accounts[0].User == user {
		// A new password for the same login is a password change, not a new
		// account, so it keeps the counters that login has already earned.
		acc = s.Accounts[0]
		acc.Hash = hash
	}
	s.Accounts = []*SiteAccount{acc}
	return db.save()
}

// ---- guest links and site accounts ----

// Caps, not policy. A site with a thousand guest links is a script that got
// away from someone, and every one of them is scanned on each request.
const (
	maxAliases  = 100
	maxAccounts = 32
	maxNote     = 200
	maxLabel    = 64
)

// CreateAlias mints a guest link: a random name under the server's alias
// domain, serving this site. The name is generated and claimed inside one
// critical section, so two requests racing cannot both take it. reserved is the
// control panel's own domain, which nothing may ever shadow.
func (db *DB) CreateAlias(siteID, reserved, note string) (*Alias, error) {
	note = strings.TrimSpace(note)
	if len(note) > maxNote {
		return nil, errors.New("note must be at most 200 characters")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	// The same value the namespace is reserved against, read from the same
	// place, so the two can never disagree about what a guest link looks like.
	base := db.aliasBase
	if base == "" {
		return nil, errors.New("guest links are not configured on this server")
	}
	s, ok := db.d.Sites[siteID]
	if !ok {
		return nil, errNotFound
	}
	if len(s.Aliases) >= maxAliases {
		return nil, errors.New("this site already has the maximum number of guest links")
	}
	// 48 random bits under a domain nobody enumerates. A retry loop rather than
	// bare trust because the check is one map walk and a silent collision would
	// hand someone else's guest another tenant's site.
	for range 8 {
		domain := randHex(6) + "." + base
		if len(domain) > 253 || !domainRe.MatchString(domain) {
			return nil, errors.New("invalid guest domain: " + domain)
		}
		// domainClaimed, not domainTaken: this is the one caller allowed to mint
		// inside the guest namespace.
		if domain == reserved || db.domainClaimed(domain, "") {
			continue
		}
		a := &Alias{ID: randHex(8), Domain: domain, Note: note, Created: time.Now().UTC()}
		s.Aliases = append(s.Aliases, a)
		return a.clone(), db.save()
	}
	return nil, errors.New("could not allocate a free guest domain")
}

func (db *DB) DeleteAlias(siteID, aliasID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.d.Sites[siteID]
	if !ok {
		return errNotFound
	}
	for i, a := range s.Aliases {
		if a.ID == aliasID {
			s.Aliases = append(s.Aliases[:i], s.Aliases[i+1:]...)
			return db.save()
		}
	}
	return errNotFound
}

// CreateAccount adds one basic-auth credential to a site. Usernames are unique
// within a site: basic auth identifies a caller by username alone, so a
// duplicate would be permanently unreachable and its counters would never move
// — a silent failure rather than an error.
func (db *DB) CreateAccount(siteID, name, user, password string) (*SiteAccount, error) {
	name = strings.TrimSpace(name)
	user = strings.TrimSpace(user)
	if name == "" || len(name) > maxLabel {
		return nil, errors.New("label must be 1-64 characters")
	}
	if err := checkSiteCredentials(user, password); err != nil {
		return nil, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.d.Sites[siteID]
	if !ok {
		return nil, errNotFound
	}
	if len(s.Accounts) >= maxAccounts {
		return nil, errors.New("this site already has the maximum number of accounts")
	}
	for _, a := range s.Accounts {
		if a.User == user {
			return nil, errConflict
		}
	}
	a := &SiteAccount{ID: randHex(8), Name: name, User: user, Hash: hash, Created: time.Now().UTC()}
	s.Accounts = append(s.Accounts, a)
	return a.clone(), db.save()
}

func (db *DB) DeleteAccount(siteID, accountID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.d.Sites[siteID]
	if !ok {
		return errNotFound
	}
	for i, a := range s.Accounts {
		if a.ID == accountID {
			s.Accounts = append(s.Accounts[:i], s.Accounts[i+1:]...)
			return db.save()
		}
	}
	return errNotFound
}

// ResetStats zeroes one set of counters. Resetting the site zeroes its guest
// links and accounts too: main-domain traffic is read as the site total minus
// the aliases', and that subtraction stops meaning anything — and can go
// negative on screen — the moment the two are zeroed at different times.
func (db *DB) ResetStats(siteID, kind, id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.d.Sites[siteID]
	if !ok {
		return errNotFound
	}
	switch kind {
	case "site":
		s.Stats = Stats{}
		for _, a := range s.Aliases {
			a.Stats = Stats{}
		}
		for _, a := range s.Accounts {
			a.Stats = Stats{}
		}
	case "alias":
		for _, a := range s.Aliases {
			if a.ID == id {
				a.Stats = Stats{}
				return db.save()
			}
		}
		return errNotFound
	case "account":
		for _, a := range s.Accounts {
			if a.ID == id {
				a.Stats = Stats{}
				return db.save()
			}
		}
		return errNotFound
	default:
		return errNotFound
	}
	return db.save()
}

func (u *User) clone() *User {
	c := *u
	return &c
}

func (s *Site) clone() *Site {
	c := *s
	c.Members = append([]string(nil), s.Members...)
	// Aliases and accounts hold pointers, so copying the slices alone would hand
	// the caller the live records. That is not a style point: the site view
	// blanks a password hash on what it believes is its own copy, and through a
	// shared pointer that blanks the real credential and the next flush writes
	// the damage to disk. Traffic counters have the same problem from the other
	// side — they move while an encoder is reading them.
	if s.Aliases != nil {
		c.Aliases = make([]*Alias, len(s.Aliases))
		for i, a := range s.Aliases {
			c.Aliases[i] = a.clone()
		}
	}
	if s.Accounts != nil {
		c.Accounts = make([]*SiteAccount, len(s.Accounts))
		for i, a := range s.Accounts {
			c.Accounts[i] = a.clone()
		}
	}
	return &c
}

func (a *Alias) clone() *Alias {
	c := *a
	return &c
}

func (a *SiteAccount) clone() *SiteAccount {
	c := *a
	return &c
}

// authFingerprint digests every credential the site currently holds. It goes
// into the memoisation key for verified basic auth, so adding an account,
// removing one or changing any password invalidates the site's cached entries
// with no bookkeeping of its own.
func (s *Site) authFingerprint() string {
	var b strings.Builder
	for _, a := range s.Accounts {
		// The username is in here as well as the hash. Nothing renames an
		// account today, but this digest is the whole of the invalidation
		// mechanism, and the day something does, a stale memo would keep letting
		// the old username in.
		b.WriteString(a.ID)
		b.WriteByte(0)
		b.WriteString(a.User)
		b.WriteByte(0)
		b.WriteString(a.Hash)
		b.WriteByte(0)
	}
	return sha256hex(b.String())
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
