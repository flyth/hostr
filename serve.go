package main

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// serveSite answers a request for a tenant's static site. Everything it can
// reach is bounded by that site's manifest, so a lookup can never cross into
// another tenant's blobs even if the path is hostile.
//
// alias is the guest link the request arrived through, or nil for the site's own
// domain. Nothing about what gets served depends on it — a guest link is the
// same site, behind the same password — but everything the response *names* does:
// a link handed to one person must not disclose where it really points.
func (s *Server) serveSite(w http.ResponseWriter, r *http.Request, site *Site, alias *Alias) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The hostname this request came in on, taken from the stored record rather
	// than from r.Host: it ends up in an auth realm and in page titles, and a
	// value out of the request is not one this server ever validated.
	domain, aliasID := site.Domain, ""
	if alias != nil {
		domain, aliasID = alias.Domain, alias.ID
	}

	// Before anything is disclosed, including whether a path exists.
	account, ok := s.checkSiteAuth(w, r, site, domain)
	if !ok {
		return
	}

	// Past the gate, so this is a real visit and gets counted. A 401 is not: it
	// carries no traffic worth attributing, and counting it would let any
	// stranger dirty the store at will. Bytes are always tallied; the hit count
	// is what a visit costs *once*, so the requests a visit makes on its own
	// behalf clear this flag as they go — the shared assets, the browser's own
	// listing feed, and the redirect whose only job is to add a trailing slash
	// before the client asks again.
	cw := &countingWriter{ResponseWriter: w}
	w, counted := http.ResponseWriter(cw), true
	defer func() { s.db.Record(site.ID, aliasID, account, cw.n, counted) }()

	m := s.storage.Manifest(site.ID)
	req := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")

	// Reserved, and matched before the manifest so no deployed file can take
	// these names. Behind the site's own auth: a private site should not double
	// as an open font host.
	if name, ok := assetRequest(req); ok {
		counted = false
		s.serveAsset(w, r, name)
		return
	}

	// The browser page fetches its rows from the same URL. A query parameter
	// rather than a reserved path prefix, so no filename in a site can ever
	// collide with it.
	if r.URL.Query().Get("_hostr") == "ls" {
		counted = false
		s.serveListingJSON(w, site, m, req)
		return
	}

	// A directory-ish URL without its trailing slash gets a redirect rather
	// than a body, so relative links inside the page resolve correctly.
	if req != "" && !strings.HasSuffix(r.URL.Path, "/") {
		if _, direct := m.Files[req]; !direct {
			_, isDir := m.Files[req+"/index.html"]
			if !isDir && site.listable(req) {
				isDir = m.HasDir(req)
			}
			if isDir {
				u := *r.URL
				u.Path = "/" + req + "/"
				// The client immediately asks again at the slashed URL, and that
				// request is the visit. Counting both would double every
				// navigation that omitted a trailing slash.
				counted = false
				http.Redirect(w, r, u.RequestURI(), http.StatusMovedPermanently)
				return
			}
		}
	}

	if name, entry, ok := resolve(m, req); ok {
		if site.Markdown && isMarkdown(name) {
			// This URL now has two representations, and the source one is
			// cacheable. Without this a shared cache stores whichever was asked
			// for first and hands a navigating browser an octet-stream to
			// download — the exact thing the flag exists to stop.
			w.Header().Set("Vary", "Accept")
			// `?raw` is the way back to the source, and the renderer's own way
			// of reading it. Plain text rather than text/markdown: the point is
			// that a browser shows it instead of downloading it.
			if r.URL.Query().Has("raw") {
				s.writeBlobAs(w, r, site, name, entry, "text/plain; charset=utf-8", http.StatusOK)
				return
			}
			if wantsDocument(r) {
				s.serveMarkdownPage(w, r, domain, name)
				return
			}
		}
		s.writeBlob(w, r, site, name, entry, http.StatusOK)
		return
	}
	// A real directory with no index of its own. Only once the site has opted
	// in — listing turns every path into something a stranger can enumerate.
	if site.listable(req) && m.HasDir(req) {
		s.serveListingPage(w, r, site, domain, req)
		return
	}
	s.serveNotFound(w, r, site, m)
}

// serveListingJSON feeds the browser page. It is the same disclosure as the
// page itself, so it is gated on the same rule — including the root, which the
// page would otherwise be denied and the feed would hand over anyway.
func (s *Server) serveListingJSON(w http.ResponseWriter, site *Site, m *Manifest, dir string) {
	if !site.listable(dir) {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}
	entries, found := m.List(dir)
	if !found {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSON(w, http.StatusOK, map[string]any{"path": dir, "entries": entries})
}

func (s *Server) serveListingPage(w http.ResponseWriter, r *http.Request, site *Site, domain, dir string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	// No path or filename is interpolated into markup — the shell is static
	// and every name arrives later as JSON, so there is nothing here for a
	// hostile filename to break out of.
	_, _ = w.Write(listingPage(dir, domain, site.ListingNoRoot, site.Markdown))
}

// resolve maps a request path onto a manifest entry, trying the file itself,
// then its directory index, then the `.html` extensionless form.
func resolve(m *Manifest, req string) (string, FileEntry, bool) {
	if req == "" {
		req = "index.html"
	}
	for _, cand := range []string{req, req + "/index.html", req + ".html"} {
		if e, ok := m.Files[cand]; ok {
			return cand, e, true
		}
	}
	return "", FileEntry{}, false
}

func (s *Server) serveNotFound(w http.ResponseWriter, r *http.Request, site *Site, m *Manifest) {
	if e, ok := m.Files["404.html"]; ok {
		s.writeBlob(w, r, site, "404.html", e, http.StatusNotFound)
		return
	}
	// A single-page app ships one index.html and routes client side; falling
	// back to it keeps deep links working. Only done when there is no 404.html,
	// so a site can opt out by shipping one.
	if e, ok := m.Files["index.html"]; ok && looksLikeRoute(r.URL.Path) {
		s.writeBlob(w, r, site, "index.html", e, http.StatusOK)
		return
	}
	http.Error(w, "404 not found", http.StatusNotFound)
}

// looksLikeRoute distinguishes `/settings/profile` from `/app.abc123.js`. A
// missing asset should stay a 404 instead of silently returning HTML.
func looksLikeRoute(p string) bool {
	return path.Ext(p) == ""
}

func (s *Server) writeBlob(w http.ResponseWriter, r *http.Request, site *Site, name string, e FileEntry, status int) {
	s.writeBlobAs(w, r, site, name, e, "", status)
}

// writeBlobAs is writeBlob with the content type decided by the caller. An
// empty ct means "whatever the extension says", which is every case but the
// markdown source, where the extension would have the browser download the file
// the reader just asked to look at.
func (s *Server) writeBlobAs(w http.ResponseWriter, r *http.Request, site *Site, name string, e FileEntry, ct string, status int) {
	f, err := s.storage.OpenBlob(site.ID, e.Hash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// A redeploy collected this blob between the manifest read and
			// here. The file is gone, not broken, so say 404. No recursion
			// into serveNotFound: its 404.html could be missing too.
			http.Error(w, "404 not found", http.StatusNotFound)
			return
		}
		s.log.Printf("serving %s from site %s: %v", name, site.ID, err)
		http.Error(w, "500 storage error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(name))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")

	if status != http.StatusOK {
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			io.Copy(w, f) //nolint:errcheck // client hung up; nothing to report
		}
		return
	}

	// Content is immutable per hash but the URL is not, so revalidate every
	// time. The ETag turns that into a 304 in practice. ServeContent handles
	// If-None-Match and Range from here.
	h.Set("ETag", `"`+e.Hash+`"`)
	h.Set("Cache-Control", "public, max-age=0, must-revalidate")
	http.ServeContent(w, r, name, time.Time{}, f)
}
