package main

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// collaborator pairs an ID with its name. Two parallel arrays would drift the
// moment one of them skipped an entry, and both clients index into them to
// decide who to remove.
type collaborator struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type siteView struct {
	// The embedded site is a copy with its password hash cleared; see view.
	*Site
	Protected     bool           `json:"protected"`
	OwnerName     string         `json:"owner_name"`
	Collaborators []collaborator `json:"collaborators"`
	Files         int            `json:"files"`
	Bytes         int64          `json:"bytes"`
	Version       int            `json:"version"`
	Deployed      time.Time      `json:"deployed,omitzero"`
	Mine          bool           `json:"mine"`
}

func (s *Server) view(c caller, site *Site) siteView {
	u := c.user
	// Counts follow what the caller may see, so a scoped token does not learn
	// the size of the rest of the site from its own site record.
	m := visible(c, s.storage.Manifest(site.ID))
	members := make([]collaborator, 0, len(site.Members))
	owner := ""
	if !c.scoped() {
		for _, id := range site.Members {
			m := collaborator{ID: id, Name: id}
			if mu := s.db.User(id); mu != nil {
				m.Name = mu.Name
			}
			members = append(members, m)
		}
		if ou := s.db.User(site.Owner); ou != nil {
			owner = ou.Name
		}
	}
	// The view embeds the site wholesale, so secrets are stripped from a copy
	// rather than suppressed with a tag — a field added to Site later inherits
	// the safe default of being visible, and a secret one has to be cleared
	// here on purpose. The basic-auth username goes too: it is half of that
	// credential, and `protected` is all a client needs to render.
	safe := site.clone()
	safe.AuthHash = ""
	safe.AuthUser = ""
	return siteView{
		Site: safe, Protected: site.AuthUser != "", OwnerName: owner, Collaborators: members,
		Files: len(m.Files), Bytes: m.Usage(), Version: m.Version, Deployed: m.Updated,
		Mine: site.Owner == u.ID,
	}
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/login", s.public(s.handleLogin))
	mux.HandleFunc("POST /api/logout", s.public(s.handleLogout))
	mux.HandleFunc("POST /api/register", s.public(s.handleRegister))

	mux.HandleFunc("GET /api/me", s.anyToken(s.handleMe))
	mux.HandleFunc("POST /api/password", s.auth(s.handlePassword))

	mux.HandleFunc("GET /api/sites", s.authenticated(files, s.handleListSites))
	mux.HandleFunc("POST /api/sites", s.auth(s.handleCreateSite))
	mux.HandleFunc("GET /api/sites/{id}", s.siteFiles(s.handleGetSite))
	mux.HandleFunc("PATCH /api/sites/{id}", s.site(s.handlePatchSite))
	mux.HandleFunc("DELETE /api/sites/{id}", s.site(s.handleDeleteSite))
	mux.HandleFunc("GET /api/sites/{id}/manifest", s.siteFiles(s.handleManifest))
	mux.HandleFunc("GET /api/sites/{id}/files", s.siteFiles(s.handleListFiles))
	mux.HandleFunc("PATCH /api/sites/{id}/files", s.siteFiles(s.handlePatchFiles))
	mux.HandleFunc("GET /api/sites/{id}/raw", s.siteFiles(s.handleRawBlob))
	mux.HandleFunc("POST /api/sites/{id}/check", s.siteFiles(s.handleCheck))
	mux.HandleFunc("PUT /api/sites/{id}/blobs/{hash}", s.siteFiles(s.handlePutBlob))
	mux.HandleFunc("POST /api/sites/{id}/deploy", s.siteFiles(s.handleDeploy))
	mux.HandleFunc("POST /api/sites/{id}/members", s.site(s.handleAddMember))
	mux.HandleFunc("DELETE /api/sites/{id}/members/{user}", s.site(s.handleRemoveMember))

	mux.HandleFunc("GET /api/tokens", s.auth(s.handleListTokens))
	mux.HandleFunc("POST /api/tokens", s.auth(s.handleCreateToken))
	mux.HandleFunc("DELETE /api/tokens/{id}", s.auth(s.handleDeleteToken))

	mux.HandleFunc("GET /api/invites", s.admin(s.handleListInvites))
	mux.HandleFunc("POST /api/invites", s.admin(s.handleCreateInvite))
	mux.HandleFunc("DELETE /api/invites/{code}", s.admin(s.handleDeleteInvite))
	mux.HandleFunc("GET /api/users", s.admin(s.handleListUsers))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "no such endpoint")
	})
	mux.HandleFunc("/", s.serveSPA)
	return mux
}

// ---- middleware ----

type handler func(http.ResponseWriter, *http.Request, *User)
type siteHandler func(http.ResponseWriter, *http.Request, caller, *Site)

// access is how far into the API a limited token may reach. It is a whitelist
// rather than a blacklist because the endpoints a limited token must not reach
// include the one that mints tokens: anything able to issue itself a fresh
// unrestricted credential is not limited at all.
type access int

const (
	// account acts on the account itself — tokens, password, invites, users,
	// creating sites. Closed to any token that carries a site or scopes.
	account access = iota
	// siteAdmin administers one site: its settings, its collaborators, its
	// deletion. A site-bound token belongs here; a scoped one does not.
	siteAdmin
	// files moves file content, and is the only level a scoped token reaches.
	files
)

// scoped reports whether the caller is confined to a set of path prefixes.
func (c caller) scoped() bool { return c.token != nil && len(c.token.Scopes) > 0 }

func (c caller) allowed(l access) bool {
	switch l {
	case account:
		return !c.token.restricted()
	case siteAdmin:
		return !c.scoped()
	default:
		return true
	}
}

func (c caller) limitedTo() string {
	if c.scoped() {
		return "this token is limited to file access within its scopes"
	}
	return "this token is limited to one site's files"
}

// public wraps the endpoints that run without authentication. They still need
// the origin check: login and register hand out a session, so a forged
// cross-site POST is a real attack even though there is no cookie to steal.
func (s *Server) public(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.checkOrigin(r); err != nil {
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
		next(w, r)
	}
}

// authenticated resolves the caller, applies the CSRF rule, and refuses a
// limited token anything above the level of the endpoint it is calling.
func (s *Server) authenticated(l access, next func(http.ResponseWriter, *http.Request, caller)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := s.authenticate(r)
		if c.user == nil {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if err := s.checkCSRF(r, c); err != nil {
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
		if !c.allowed(l) {
			writeErr(w, http.StatusForbidden, c.limitedTo())
			return
		}
		next(w, r, c)
	}
}

func (s *Server) auth(next handler) http.HandlerFunc {
	return s.authenticated(account, func(w http.ResponseWriter, r *http.Request, c caller) { next(w, r, c.user) })
}

// anyToken wraps the account-wide reads a limited token still needs — it has
// to be able to find the site it is allowed to write to.
func (s *Server) anyToken(next handler) http.HandlerFunc {
	return s.authenticated(files, func(w http.ResponseWriter, r *http.Request, c caller) { next(w, r, c.user) })
}

func (s *Server) admin(next handler) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, u *User) {
		if !u.Admin {
			writeErr(w, http.StatusForbidden, "admin only")
			return
		}
		next(w, r, u)
	})
}

// site is the tenancy gate. Every per-site route goes through it, and a caller
// without access gets 404 rather than 403 so site IDs cannot be enumerated. A
// token bound to another site is treated exactly like no access at all.
func (s *Server) site(next siteHandler) http.HandlerFunc {
	return s.siteGate(siteAdmin, next)
}

// siteFiles is site for the endpoints a scoped token may use.
func (s *Server) siteFiles(next siteHandler) http.HandlerFunc {
	return s.siteGate(files, next)
}

func (s *Server) siteGate(l access, next siteHandler) http.HandlerFunc {
	return s.authenticated(l, func(w http.ResponseWriter, r *http.Request, c caller) {
		site := s.db.Site(r.PathValue("id"))
		if site == nil || !site.allows(c.user) || !c.token.allowsSite(site.ID) {
			writeErr(w, http.StatusNotFound, "no such site")
			return
		}
		next(w, r, c, site)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	// Insisting on application/json is what stops HTML-form CSRF: a <form> can
	// only send text/plain, urlencoded or multipart, and text/plain in
	// particular can be shaped into a body Go's decoder would otherwise
	// accept. Cross-origin fetch with this type triggers a preflight we never
	// answer.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "expected Content-Type: application/json")
		return false
	}
	// A deploy manifest for a large site is still only metadata; 16 MiB covers
	// roughly 200k files and nothing legitimate comes close.
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, errConflict):
		writeErr(w, http.StatusConflict, "already exists")
	default:
		writeErr(w, http.StatusBadRequest, err.Error())
	}
}

// dummyHash lets a login against an unknown username burn the same argon2 time
// as a real one, so response timing does not leak which accounts exist.
var dummyHash = mustHash("password-that-is-never-valid")

func mustHash(s string) string {
	h, err := hashPassword(s)
	if err != nil {
		panic(err)
	}
	return h
}

// ---- auth handlers ----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Password string }
	if !readJSON(w, r, &req) {
		return
	}
	ip := s.clientIP(r)
	if !s.throttle.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	u := s.db.UserByName(strings.ToLower(strings.TrimSpace(req.Name)))
	// Hash against a dummy encoding when the user is absent so a missing
	// account and a wrong password take the same time.
	encoded := dummyHash
	if u != nil {
		encoded = u.Hash
	}
	if !verifyPassword(encoded, req.Password) || u == nil {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	s.throttle.reset(ip)
	s.setSessionCookie(w, s.sessions.create(u.ID))
	writeJSON(w, http.StatusOK, publicUser(u))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.drop(c.Value)
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code, Name, Password string }
	if !readJSON(w, r, &req) {
		return
	}
	if !s.throttle.allow("register:" + s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	u, err := s.db.Redeem(strings.TrimSpace(req.Code), req.Name, req.Password)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeErr(w, http.StatusForbidden, "invalid or already-used invite code")
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.setSessionCookie(w, s.sessions.create(u.ID))
	writeJSON(w, http.StatusOK, publicUser(u))
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, http.StatusOK, publicUser(u))
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request, u *User) {
	var req struct{ Old, New string }
	if !readJSON(w, r, &req) {
		return
	}
	if !verifyPassword(u.Hash, req.Old) {
		writeErr(w, http.StatusForbidden, "current password is wrong")
		return
	}
	if err := s.db.SetPassword(u.ID, req.New); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Changing a password should end every other session.
	s.sessions.dropUser(u.ID)
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func publicUser(u *User) map[string]any {
	return map[string]any{"id": u.ID, "name": u.Name, "admin": u.Admin, "created": u.Created}
}

// ---- site handlers ----

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request, c caller) {
	sites := s.db.SitesFor(c.user)
	out := make([]siteView, 0, len(sites))
	for _, site := range sites {
		// A site-bound token sees only its own site, so handing one to an
		// agent does not also hand over an inventory of everything else the
		// account hosts.
		if !c.token.allowsSite(site.ID) {
			continue
		}
		out = append(out, s.view(c, site))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateSite(w http.ResponseWriter, r *http.Request, u *User) {
	var req struct{ Slug, Domain string }
	if !readJSON(w, r, &req) {
		return
	}
	if normDomain(req.Domain) == s.cfg.AdminDomain {
		writeErr(w, http.StatusConflict, "that domain is reserved for the control panel")
		return
	}
	site, err := s.db.CreateSite(u.ID, req.Slug, req.Domain)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.view(caller{user: u}, site))
}

func (s *Server) handleGetSite(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	writeJSON(w, http.StatusOK, s.view(c, site))
}

// handlePatchSite carries the site's settings as well as its domain. Every
// field is optional and a nil pointer means "leave this alone", so the panel
// can flip one toggle without resending the rest.
func (s *Server) handlePatchSite(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	u := c.user
	if site.Owner != u.ID && !u.Admin {
		writeErr(w, http.StatusForbidden, "only the owner can change site settings")
		return
	}
	var req struct {
		Domain     *string `json:"domain"`
		Listing    *bool   `json:"listing"`
		ScopedOnly *bool   `json:"scoped_only"`
		AuthUser   *string `json:"auth_user"`
		AuthPass   *string `json:"auth_password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// Everything is checked before anything is written. These are three
	// separate store mutations, and failing the third after committing the
	// first leaves the caller with a half-applied settings change they never
	// asked for.
	if req.Domain != nil {
		if normDomain(*req.Domain) == s.cfg.AdminDomain {
			writeErr(w, http.StatusConflict, "that domain is reserved for the control panel")
			return
		}
		if err := s.db.CheckDomain(site.ID, *req.Domain); err != nil {
			writeStoreErr(w, err)
			return
		}
	}
	if req.AuthUser != nil && strings.TrimSpace(*req.AuthUser) != "" {
		pass := ""
		if req.AuthPass != nil {
			pass = *req.AuthPass
		}
		if err := checkSiteCredentials(strings.TrimSpace(*req.AuthUser), pass); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if req.Domain != nil {
		if err := s.db.SetDomain(site.ID, *req.Domain); err != nil {
			writeStoreErr(w, err)
			return
		}
	}
	if req.Listing != nil || req.ScopedOnly != nil {
		if err := s.db.SetSiteSettings(site.ID, req.Listing, req.ScopedOnly); err != nil {
			writeStoreErr(w, err)
			return
		}
	}
	if req.AuthUser != nil {
		pass := ""
		if req.AuthPass != nil {
			pass = *req.AuthPass
		}
		if err := s.db.SetSiteAuth(site.ID, *req.AuthUser, pass); err != nil {
			writeStoreErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.view(c, s.db.Site(site.ID)))
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	u := c.user
	if site.Owner != u.ID && !u.Admin {
		writeErr(w, http.StatusForbidden, "only the owner can delete a site")
		return
	}
	if err := s.db.DeleteSite(site.ID); err != nil {
		writeStoreErr(w, err)
		return
	}
	if err := s.storage.DeleteSite(site.ID); err != nil {
		s.log.Printf("site %s deleted from index but files remain: %v", site.ID, err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// visible narrows a manifest to what the caller is allowed to see. A
// scope-limited token gets its own subtrees and nothing else, so it can neither
// read nor even enumerate a sibling project.
func visible(c caller, m *Manifest) *Manifest {
	if !c.scoped() {
		return m
	}
	out := &Manifest{Version: m.Version, Updated: m.Updated, Files: map[string]FileEntry{}}
	for p, e := range m.Files {
		if c.token.allowsFile(p) {
			out.Files[p] = e
		}
	}
	return out
}

// writeTarget is one path a write touches: an individual file, or a whole
// directory — a deploy scope, or the subtree a recursive delete removes. The
// distinction matters twice over, because a directory may be named by a token
// scope while a file of the same name sits beside it rather than inside it.
type writeTarget struct {
	path string
	dir  bool
}

func fileTarget(p string) writeTarget { return writeTarget{path: p} }
func dirTarget(p string) writeTarget  { return writeTarget{path: p, dir: true} }

// inScope reports whether the target is confined to some directory, which is
// what a scope-only site demands. The site root fails it either way: as a
// directory it is unnamed, and as a file it sits at the top level.
func (t writeTarget) inScope() bool {
	if t.dir {
		return t.path != ""
	}
	return strings.Contains(t.path, "/")
}

// authoriseWrite vets every path a write is about to touch before any of it is
// applied, so a request that is partly out of bounds changes nothing at all.
func authoriseWrite(c caller, site *Site, targets ...writeTarget) error {
	for _, t := range targets {
		where := t.path
		if where == "" {
			where = "the site root"
		}
		if site.ScopedOnly && !t.inScope() {
			return errors.New("this site only accepts scoped writes, and " + where + " is not inside a scope")
		}
		ok := c.token.allowsFile(t.path)
		if t.dir {
			ok = c.token.allowsScope(t.path)
		}
		if !ok {
			return errors.New("this token is not allowed to write " + where)
		}
	}
	return nil
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	writeJSON(w, http.StatusOK, visible(c, s.storage.Manifest(site.ID)))
}

// handleListFiles backs the file browser: one directory at a time rather than
// the whole manifest, so a site with a hundred thousand files still opens.
func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	dir := strings.Trim(r.URL.Query().Get("path"), "/")
	if dir != "" {
		if _, ok := cleanPath(dir); !ok {
			writeErr(w, http.StatusBadRequest, "illegal path")
			return
		}
	}
	entries, found := visible(c, s.storage.Manifest(site.ID)).List(dir)
	if !found {
		writeErr(w, http.StatusNotFound, "no such directory")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": dir, "entries": entries})
}

// handlePatchFiles adds or replaces the named files and deletes the named
// paths, touching nothing else. This is what a single-file upload uses, and
// what the browser's delete controls call; a delete ending in "/" takes the
// whole subtree with it.
func (s *Server) handlePatchFiles(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	var req struct {
		Set    map[string]FileEntry `json:"set"`
		Delete []string             `json:"delete"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	targets := make([]writeTarget, 0, len(req.Set)+len(req.Delete))
	for p := range req.Set {
		cp, ok := cleanPath(p)
		if !ok {
			writeErr(w, http.StatusBadRequest, "illegal path: "+p)
			return
		}
		targets = append(targets, fileTarget(cp))
	}
	for _, d := range req.Delete {
		if d == "/" {
			targets = append(targets, dirTarget("")) // the whole site
			continue
		}
		recursive := strings.HasSuffix(d, "/")
		cp, ok := cleanPath(strings.TrimSuffix(d, "/"))
		if !ok {
			writeErr(w, http.StatusBadRequest, "illegal path: "+d)
			return
		}
		if recursive {
			targets = append(targets, dirTarget(cp))
			continue
		}
		targets = append(targets, fileTarget(cp))
	}
	if err := authoriseWrite(c, site, targets...); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	m, removed, err := s.storage.Patch(site.ID, req.Set, req.Delete)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Printf("patch site=%s domain=%s by=%s set=%d delete=%d version=%d gc=%d",
		site.Slug, site.Domain, c.user.Name, len(req.Set), len(req.Delete), m.Version, removed)
	writeJSON(w, http.StatusOK, map[string]any{
		"version": m.Version, "files": len(m.Files), "bytes": m.Usage(), "removed": removed,
	})
}

// inlineType lists what may be rendered in place on the *control panel's*
// origin. Anything else is sent as a download: an uploaded HTML file opened
// inline here would run with the panel's session cookie in reach.
func inlineType(ct string) bool {
	switch {
	case strings.HasPrefix(ct, "image/"), strings.HasPrefix(ct, "video/"), strings.HasPrefix(ct, "audio/"):
		return true
	case strings.HasPrefix(ct, "text/plain"):
		return true
	}
	return false
}

// handleRawBlob serves one of a site's files back through the panel, so the
// browser can preview content without leaving the admin origin and without
// tripping over the site's own basic auth.
func (s *Server) handleRawBlob(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	p := strings.TrimPrefix(r.URL.Query().Get("path"), "/")
	// One map lookup plus a prefix test, rather than filtering the whole
	// manifest to answer a question about a single path — this runs once per
	// previewed file.
	e, ok := s.storage.Manifest(site.ID).Files[p]
	if !ok || !c.token.allowsFile(p) {
		writeErr(w, http.StatusNotFound, "no such file")
		return
	}
	f, err := s.storage.OpenBlob(site.ID, e.Hash)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such file")
		return
	}
	defer f.Close()

	ct := mime.TypeByExtension(path.Ext(p))
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	// An opaque origin for anything that does end up rendered — an SVG opened
	// in its own tab cannot then reach the panel's cookies or storage.
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if inlineType(ct) {
		h.Set("Content-Disposition", "inline")
	} else {
		h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(path.Base(p)))
	}
	h.Set("Cache-Control", "private, max-age=300")
	h.Set("ETag", `"`+e.Hash+`"`)
	http.ServeContent(w, r, path.Base(p), time.Time{}, f)
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	var req struct {
		Hashes []string `json:"hashes"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// Blobs are stored per site, not per scope, so answering this honestly for
	// a scoped token would turn it into an oracle: upload nothing, submit the
	// hash of a guessed document, and learn whether that exact content exists
	// somewhere the token cannot read. A scoped caller is told about content
	// its own scopes already reference and nothing else, so at worst it
	// re-uploads bytes the server happens to have.
	var mine map[string]bool
	if c.scoped() {
		mine = map[string]bool{}
		for p, e := range s.storage.Manifest(site.ID).Files {
			if c.token.allowsFile(p) {
				mine[e.Hash] = true
			}
		}
	}

	missing := []string{}
	seen := map[string]bool{}
	for _, h := range req.Hashes {
		if seen[h] {
			continue
		}
		seen[h] = true
		if !validHash(h) {
			writeErr(w, http.StatusBadRequest, errBadHash.Error())
			return
		}
		have := s.storage.HasBlob(site.ID, h)
		if mine != nil && !mine[h] {
			have = false
		}
		if !have {
			missing = append(missing, h)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"missing": missing})
}

func (s *Server) handlePutBlob(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	hash := r.PathValue("hash")
	if !validHash(hash) {
		writeErr(w, http.StatusBadRequest, errBadHash.Error())
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload)
	n, err := s.storage.PutBlob(site.ID, hash, body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "file exceeds the upload limit")
			return
		}
		if errors.Is(err, errBadHash) {
			writeErr(w, http.StatusBadRequest, "content does not match the declared hash")
			return
		}
		s.log.Printf("blob write for site %s: %v", site.ID, err)
		writeErr(w, http.StatusInternalServerError, "could not store file")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"hash": hash, "size": n})
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	var req struct {
		Scope string               `json:"scope"`
		Files map[string]FileEntry `json:"files"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	scope := strings.Trim(req.Scope, "/")
	if scope != "" {
		if _, ok := cleanPath(scope); !ok {
			writeErr(w, http.StatusBadRequest, "illegal scope: "+req.Scope)
			return
		}
	}
	// A deploy replaces everything under its scope, so the scope itself is the
	// thing being written — that is what has to clear the token and the
	// site's scoped-only rule.
	if err := authoriseWrite(c, site, dirTarget(scope)); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	m, removed, err := s.storage.Deploy(site.ID, scope, req.Files)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Printf("deploy site=%s domain=%s scope=%q by=%s files=%d version=%d gc=%d",
		site.Slug, site.Domain, scope, c.user.Name, len(m.Files), m.Version, removed)
	writeJSON(w, http.StatusOK, map[string]any{
		"version": m.Version, "files": len(m.Files), "bytes": m.Usage(), "removed": removed,
	})
}

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	u := c.user
	if site.Owner != u.ID && !u.Admin {
		writeErr(w, http.StatusForbidden, "only the owner can manage collaborators")
		return
	}
	var req struct{ Name string }
	if !readJSON(w, r, &req) {
		return
	}
	target := s.db.UserByName(strings.ToLower(strings.TrimSpace(req.Name)))
	if target == nil {
		writeErr(w, http.StatusNotFound, "no such user")
		return
	}
	if err := s.db.AddMember(site.ID, target.ID); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(c, s.db.Site(site.ID)))
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request, c caller, site *Site) {
	u := c.user
	target := r.PathValue("user")
	// The owner can remove anyone; a collaborator can only remove themselves.
	if site.Owner != u.ID && !u.Admin && target != u.ID {
		writeErr(w, http.StatusForbidden, "only the owner can manage collaborators")
		return
	}
	if err := s.db.RemoveMember(site.ID, target); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- tokens ----

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, http.StatusOK, s.db.Tokens(u.ID))
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request, u *User) {
	var req struct {
		Name   string   `json:"name"`
		Site   string   `json:"site"`
		Scopes []string `json:"scopes"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "hostrctl"
	}
	if len(name) > 64 {
		writeErr(w, http.StatusBadRequest, "name too long")
		return
	}
	// Scopes only mean anything against one site, and a token that could pick
	// its site at call time would not be limited to much.
	if len(req.Scopes) > 0 && req.Site == "" {
		writeErr(w, http.StatusBadRequest, "a scoped token must name a site")
		return
	}
	if req.Site != "" {
		// Same 404 as everywhere else: no probing for site IDs.
		if site := s.db.Site(req.Site); site == nil || !site.allows(u) {
			writeErr(w, http.StatusNotFound, "no such site")
			return
		}
	}
	scopes := make([]string, 0, len(req.Scopes))
	for _, sc := range req.Scopes {
		scopes = append(scopes, strings.Trim(sc, "/"))
	}
	t, secret, err := s.db.CreateToken(u.ID, name, req.Site, scopes)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	// The secret is shown exactly once; only its hash is kept.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": t.ID, "name": t.Name, "site": t.Site, "scopes": t.Scopes, "token": secret,
	})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request, u *User) {
	if err := s.db.DeleteToken(u.ID, r.PathValue("id")); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- admin ----

func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, http.StatusOK, s.db.Invites())
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request, u *User) {
	inv, err := s.db.CreateInvite(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not create invite")
		return
	}
	writeJSON(w, http.StatusCreated, inv)
}

func (s *Server) handleDeleteInvite(w http.ResponseWriter, r *http.Request, u *User) {
	if err := s.db.DeleteInvite(r.PathValue("code")); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request, u *User) {
	users := s.db.Users()
	out := make([]map[string]any, 0, len(users))
	for _, x := range users {
		out = append(out, publicUser(x))
	}
	writeJSON(w, http.StatusOK, out)
}
