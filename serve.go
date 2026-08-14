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
func (s *Server) serveSite(w http.ResponseWriter, r *http.Request, site *Site) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	m := s.storage.Manifest(site.ID)
	req := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")

	// A directory-ish URL without its trailing slash gets a redirect rather
	// than a body, so relative links inside the page resolve correctly.
	if req != "" && !strings.HasSuffix(r.URL.Path, "/") {
		if _, direct := m.Files[req]; !direct {
			if _, isDir := m.Files[req+"/index.html"]; isDir {
				u := *r.URL
				u.Path = "/" + req + "/"
				http.Redirect(w, r, u.RequestURI(), http.StatusMovedPermanently)
				return
			}
		}
	}

	name, entry, ok := resolve(m, req)
	if !ok {
		s.serveNotFound(w, r, site, m)
		return
	}
	s.writeBlob(w, r, site, name, entry, http.StatusOK)
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

	ct := mime.TypeByExtension(path.Ext(name))
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
