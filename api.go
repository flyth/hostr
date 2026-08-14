package main

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
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
	*Site
	OwnerName     string         `json:"owner_name"`
	Collaborators []collaborator `json:"collaborators"`
	Files         int            `json:"files"`
	Bytes         int64          `json:"bytes"`
	Version       int            `json:"version"`
	Deployed      time.Time      `json:"deployed,omitzero"`
	Mine          bool           `json:"mine"`
}

func (s *Server) view(site *Site, u *User) siteView {
	m := s.storage.Manifest(site.ID)
	members := make([]collaborator, 0, len(site.Members))
	for _, id := range site.Members {
		c := collaborator{ID: id, Name: id}
		if mu := s.db.User(id); mu != nil {
			c.Name = mu.Name
		}
		members = append(members, c)
	}
	owner := ""
	if ou := s.db.User(site.Owner); ou != nil {
		owner = ou.Name
	}
	return siteView{
		Site: site, OwnerName: owner, Collaborators: members,
		Files: len(m.Files), Bytes: m.Usage(), Version: m.Version, Deployed: m.Updated,
		Mine: site.Owner == u.ID,
	}
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/login", s.public(s.handleLogin))
	mux.HandleFunc("POST /api/logout", s.public(s.handleLogout))
	mux.HandleFunc("POST /api/register", s.public(s.handleRegister))

	mux.HandleFunc("GET /api/me", s.auth(s.handleMe))
	mux.HandleFunc("POST /api/password", s.auth(s.handlePassword))

	mux.HandleFunc("GET /api/sites", s.auth(s.handleListSites))
	mux.HandleFunc("POST /api/sites", s.auth(s.handleCreateSite))
	mux.HandleFunc("GET /api/sites/{id}", s.site(s.handleGetSite))
	mux.HandleFunc("PATCH /api/sites/{id}", s.site(s.handlePatchSite))
	mux.HandleFunc("DELETE /api/sites/{id}", s.site(s.handleDeleteSite))
	mux.HandleFunc("GET /api/sites/{id}/manifest", s.site(s.handleManifest))
	mux.HandleFunc("POST /api/sites/{id}/check", s.site(s.handleCheck))
	mux.HandleFunc("PUT /api/sites/{id}/blobs/{hash}", s.site(s.handlePutBlob))
	mux.HandleFunc("POST /api/sites/{id}/deploy", s.site(s.handleDeploy))
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
type siteHandler func(http.ResponseWriter, *http.Request, *User, *Site)

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

func (s *Server) auth(next handler) http.HandlerFunc {
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
		next(w, r, c.user)
	}
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
// without access gets 404 rather than 403 so site IDs cannot be enumerated.
func (s *Server) site(next siteHandler) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, u *User) {
		site := s.db.Site(r.PathValue("id"))
		if site == nil || !site.allows(u) {
			writeErr(w, http.StatusNotFound, "no such site")
			return
		}
		next(w, r, u, site)
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

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request, u *User) {
	sites := s.db.SitesFor(u)
	out := make([]siteView, 0, len(sites))
	for _, site := range sites {
		out = append(out, s.view(site, u))
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
	writeJSON(w, http.StatusCreated, s.view(site, u))
}

func (s *Server) handleGetSite(w http.ResponseWriter, r *http.Request, u *User, site *Site) {
	writeJSON(w, http.StatusOK, s.view(site, u))
}

func (s *Server) handlePatchSite(w http.ResponseWriter, r *http.Request, u *User, site *Site) {
	if site.Owner != u.ID && !u.Admin {
		writeErr(w, http.StatusForbidden, "only the owner can change the domain")
		return
	}
	var req struct{ Domain string }
	if !readJSON(w, r, &req) {
		return
	}
	if normDomain(req.Domain) == s.cfg.AdminDomain {
		writeErr(w, http.StatusConflict, "that domain is reserved for the control panel")
		return
	}
	if err := s.db.SetDomain(site.ID, req.Domain); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(s.db.Site(site.ID), u))
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request, u *User, site *Site) {
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

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request, u *User, site *Site) {
	writeJSON(w, http.StatusOK, s.storage.Manifest(site.ID))
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request, u *User, site *Site) {
	var req struct {
		Hashes []string `json:"hashes"`
	}
	if !readJSON(w, r, &req) {
		return
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
		if !s.storage.HasBlob(site.ID, h) {
			missing = append(missing, h)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"missing": missing})
}

func (s *Server) handlePutBlob(w http.ResponseWriter, r *http.Request, u *User, site *Site) {
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

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request, u *User, site *Site) {
	var req struct {
		Files map[string]FileEntry `json:"files"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	m, removed, err := s.storage.Deploy(site.ID, req.Files)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Printf("deploy site=%s domain=%s by=%s files=%d version=%d gc=%d",
		site.Slug, site.Domain, u.Name, len(m.Files), m.Version, removed)
	writeJSON(w, http.StatusOK, map[string]any{
		"version": m.Version, "files": len(m.Files), "bytes": m.Usage(), "removed": removed,
	})
}

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request, u *User, site *Site) {
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
	writeJSON(w, http.StatusOK, s.view(s.db.Site(site.ID), u))
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request, u *User, site *Site) {
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
	var req struct{ Name string }
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
	t, secret, err := s.db.CreateToken(u.ID, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not create token")
		return
	}
	// The secret is shown exactly once; only its hash is kept.
	writeJSON(w, http.StatusCreated, map[string]any{"id": t.ID, "name": t.Name, "token": secret})
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
