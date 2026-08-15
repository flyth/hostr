package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// The stylesheet, typefaces and markdown renderer a rendered page depends on.
// They are compiled in rather than deployed, so turning markdown rendering on
// costs a tenant nothing and cannot half-work because someone forgot to upload
// a font.
//
//go:embed assets
var assetFiles embed.FS

// assetPrefix is reserved on every hostname this server answers on. It is
// matched before a site's own manifest, so a tenant who deploys a file at
// `_hostr/md.css` cannot shadow the real one — that path is simply not theirs
// to serve. The cost is that those names are unreachable on every site, which
// is the deal a reserved namespace always is.
const assetPrefix = "_hostr/"

// Go's built-in table knows the text types but not the font, and a woff2 served
// as octet-stream is a font the browser will not use.
var assetTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
}

// assets is the embedded tree rooted at the directory, plus a content hash per
// file. Both are built once at startup: the bytes never change while the
// process runs, so an ETag computed per request would be the same answer at a
// higher price.
var assets = func() (fs.FS, map[string]string) {
	sub, err := fs.Sub(assetFiles, "assets")
	if err != nil {
		panic(err) // the directory is compiled in; its absence is a build error
	}
	tags := map[string]string{}
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		tags[p] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	if err != nil {
		panic(err)
	}
	return sub, tags
}

var assetTree, assetTags = assets()

// serveAsset answers a request under assetPrefix. `name` is the path within the
// asset tree, already cleaned by the caller.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	tag, ok := assetTags[name]
	if !ok {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}
	f, err := assetTree.Open(name)
	if err != nil {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	// An embedded file seeks over the bytes already in the binary. Reading it
	// into a fresh buffer instead would copy 140 KB of font on every request,
	// including the revalidations that this cache policy makes the common case
	// and that answer with an empty 304.
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "500 asset error", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	if ct, ok := assetTypes[path.Ext(name)]; ok {
		h.Set("Content-Type", ct)
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", tag)
	// A rebuild can change these, so they revalidate within the day rather than
	// being pinned forever; the ETag makes that a 304 in practice.
	h.Set("Cache-Control", "public, max-age=86400")
	// The file browser renders markdown inside a sandboxed frame, which is an
	// opaque origin — its font requests are cross-origin and need this to be
	// allowed at all. These bytes are public and carry no credentials.
	h.Set("Access-Control-Allow-Origin", "*")
	http.ServeContent(w, r, name, time.Time{}, rs)
}

// assetRequest reports whether a cleaned request path belongs to the reserved
// namespace, and returns the name within the asset tree.
//
// The bare `_hostr` counts. Cleaning a path drops its trailing slash, so a
// request for `/_hostr/` arrives here as `_hostr` and would otherwise fall
// through to the manifest — where a tenant's own `_hostr/index.html` was happy
// to answer, on a prefix this server tells everyone it owns.
func assetRequest(req string) (string, bool) {
	if req == strings.TrimSuffix(assetPrefix, "/") {
		return "", true // no such asset, but not the tenant's to answer either
	}
	if !strings.HasPrefix(req, assetPrefix) {
		return "", false
	}
	return strings.TrimPrefix(req, assetPrefix), true
}

// handleAsset is the control panel's route into the same tree. The panel's file
// browser renders markdown previews with the same stylesheet as a public page,
// and its frame resolves those URLs against the panel's own origin.
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	// Normalized and matched exactly as it is on a tenant domain: two rules for
	// one namespace is how the two drift apart.
	name, ok := assetRequest(strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/"))
	if !ok {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}
	s.serveAsset(w, r, name)
}
