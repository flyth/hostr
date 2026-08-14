package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Blobs are content-addressed but namespaced *per site*, not globally. A shared
// store would dedupe a little better across a user's sites, and would also let
// any tenant probe whether a given file exists anywhere on the server by
// uploading it and watching for a dedupe hit. Per-site keeps the interesting
// dedupe (redeploys of the same site) and removes the oracle.
//
//	<data>/sites/<siteID>/blobs/<hh>/<sha256>
//	<data>/sites/<siteID>/manifest.json

type FileEntry struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

type Manifest struct {
	Version int                  `json:"version"`
	Updated time.Time            `json:"updated"`
	Files   map[string]FileEntry `json:"files"`
}

type Storage struct {
	root string

	mu    sync.RWMutex
	cache map[string]*Manifest   // siteID -> manifest, populated on read
	locks map[string]*sync.Mutex // siteID -> deploy lock, see siteLock
}

func newStorage(root string) *Storage {
	return &Storage{root: root, cache: map[string]*Manifest{}, locks: map[string]*sync.Mutex{}}
}

// siteLock serialises everything that mutates one site's files. Deploy has to
// validate blobs, swap the manifest and garbage collect as one indivisible
// step: without that, one deploy's GC can delete a blob another deploy has
// already validated, leaving a live manifest pointing at a file that no longer
// exists. Per site rather than global so two tenants never wait on each other.
func (st *Storage) siteLock(siteID string) *sync.Mutex {
	st.mu.Lock()
	defer st.mu.Unlock()
	lk, ok := st.locks[siteID]
	if !ok {
		lk = &sync.Mutex{}
		st.locks[siteID] = lk
	}
	return lk
}

var errBadHash = errors.New("hash must be 64 lowercase hex characters")

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// cleanPath rejects anything that could escape a site's directory or confuse
// the serving layer. Paths must already be clean and relative — the client is
// expected to send `a/b.html`, not `./a/../a/b.html`.
func cleanPath(p string) (string, bool) {
	if p == "" || len(p) > 1024 {
		return "", false
	}
	if strings.ContainsAny(p, "\x00\\") {
		return "", false
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return "", false
	}
	if path.Clean(p) != p {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return p, true
}

func (st *Storage) siteDir(siteID string) string {
	return filepath.Join(st.root, "sites", siteID)
}

func (st *Storage) blobPath(siteID, hash string) string {
	return filepath.Join(st.siteDir(siteID), "blobs", hash[:2], hash)
}

func (st *Storage) manifestPath(siteID string) string {
	return filepath.Join(st.siteDir(siteID), "manifest.json")
}

func (st *Storage) HasBlob(siteID, hash string) bool {
	if !validHash(hash) {
		return false
	}
	_, err := os.Stat(st.blobPath(siteID, hash))
	return err == nil
}

// PutBlob streams r to disk, verifying that its content really hashes to
// `hash`. A mismatch is discarded — the server never takes the client's word
// for what it uploaded, which is what makes the "skip what's already there"
// protocol safe.
func (st *Storage) PutBlob(siteID, hash string, r io.Reader) (int64, error) {
	if !validHash(hash) {
		return 0, errBadHash
	}
	dst := st.blobPath(siteID, hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return 0, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hash {
		return 0, errBadHash
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return 0, err
	}
	return n, os.Rename(tmp.Name(), dst)
}

func (st *Storage) OpenBlob(siteID, hash string) (*os.File, error) {
	if !validHash(hash) {
		return nil, errBadHash
	}
	return os.Open(st.blobPath(siteID, hash))
}

func (st *Storage) Manifest(siteID string) *Manifest {
	st.mu.RLock()
	m, ok := st.cache[siteID]
	st.mu.RUnlock()
	if ok {
		return m
	}
	m = st.readManifest(siteID)
	st.mu.Lock()
	st.cache[siteID] = m
	st.mu.Unlock()
	return m
}

func (st *Storage) readManifest(siteID string) *Manifest {
	empty := &Manifest{Files: map[string]FileEntry{}}
	b, err := os.ReadFile(st.manifestPath(siteID))
	if err != nil {
		return empty
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return empty
	}
	if m.Files == nil {
		m.Files = map[string]FileEntry{}
	}
	return &m
}

// scopePrefix turns a scope into the path prefix its files live under. The
// empty scope is the site root and yields an empty prefix, which every
// HasPrefix test below then matches — that is exactly the whole-site
// behaviour deploy has always had.
func scopePrefix(scope string) (string, bool) {
	if scope == "" {
		return "", true
	}
	sc, ok := cleanPath(scope)
	if !ok {
		return "", false
	}
	return sc + "/", true
}

// Deploy replaces a site's file set, or — with a scope — just the subtree under
// it, leaving everything else untouched. Every referenced blob must already be
// uploaded; anything the new manifest drops is garbage collected, which is how
// deletions propagate. Paths in `files` are relative to the scope.
func (st *Storage) Deploy(siteID, scope string, files map[string]FileEntry) (*Manifest, int, error) {
	prefix, ok := scopePrefix(scope)
	if !ok {
		return nil, 0, errors.New("illegal scope: " + scope)
	}
	// Path and hash shapes are pure checks, so do them before queueing on the
	// lock — a malformed request should not wait behind someone else's deploy.
	// Cleaning the *joined* path is what stops `../` in a scoped upload from
	// landing outside its subtree.
	clean := make(map[string]FileEntry, len(files))
	for p, e := range files {
		cp, ok := cleanPath(prefix + p)
		if !ok || !strings.HasPrefix(cp, prefix) {
			return nil, 0, errors.New("illegal path: " + p)
		}
		if !validHash(e.Hash) {
			return nil, 0, errBadHash
		}
		clean[cp] = e
	}
	return st.commit(siteID, func(cur map[string]FileEntry) error {
		for p := range cur {
			if strings.HasPrefix(p, prefix) {
				delete(cur, p)
			}
		}
		// A file whose path is exactly the scope name is not under the prefix,
		// but serving resolves a bare path before a directory index, so
		// leaving it behind would let it shadow every URL in the scope this
		// deploy just published.
		delete(cur, scope)
		for p, e := range clean {
			cur[p] = e
		}
		return nil
	})
}

// Patch adds or replaces the listed files and deletes the listed paths, and
// touches nothing else. A delete entry ending in "/" removes that whole
// subtree; "/" on its own is every file in the site.
func (st *Storage) Patch(siteID string, set map[string]FileEntry, del []string) (*Manifest, int, error) {
	clean := make(map[string]FileEntry, len(set))
	for p, e := range set {
		cp, ok := cleanPath(p)
		if !ok {
			return nil, 0, errors.New("illegal path: " + p)
		}
		if !validHash(e.Hash) {
			return nil, 0, errBadHash
		}
		clean[cp] = e
	}
	prefixes := make([]string, 0, len(del))
	exact := make([]string, 0, len(del))
	for _, d := range del {
		if d == "/" {
			prefixes = append(prefixes, "")
			continue
		}
		if strings.HasSuffix(d, "/") {
			cp, ok := cleanPath(strings.TrimSuffix(d, "/"))
			if !ok {
				return nil, 0, errors.New("illegal path: " + d)
			}
			prefixes = append(prefixes, cp+"/")
			continue
		}
		cp, ok := cleanPath(d)
		if !ok {
			return nil, 0, errors.New("illegal path: " + d)
		}
		exact = append(exact, cp)
	}
	return st.commit(siteID, func(cur map[string]FileEntry) error {
		for _, p := range exact {
			delete(cur, p)
		}
		for _, pre := range prefixes {
			for p := range cur {
				if strings.HasPrefix(p, pre) {
					delete(cur, p)
				}
			}
		}
		for p, e := range clean {
			cur[p] = e
		}
		return nil
	})
}

// commit applies mutate to a copy of the live file set and publishes the
// result as the next version. Read-modify-write has to happen inside the site
// lock or two concurrent scoped deploys would each merge onto a stale base and
// the later one would silently drop the earlier one's files.
func (st *Storage) commit(siteID string, mutate func(map[string]FileEntry) error) (*Manifest, int, error) {
	lk := st.siteLock(siteID)
	lk.Lock()
	defer lk.Unlock()

	st.mu.RLock()
	prev := st.cache[siteID]
	st.mu.RUnlock()
	if prev == nil {
		prev = st.readManifest(siteID)
	}

	next := make(map[string]FileEntry, len(prev.Files))
	for p, e := range prev.Files {
		next[p] = e
	}
	if err := mutate(next); err != nil {
		return nil, 0, err
	}

	// Blob presence is checked *inside* the lock. A deploy that raced with
	// another one and lost its uploads fails here rather than publishing a
	// manifest that points at collected files. Entries carried over unchanged
	// were live a moment ago and cannot have been collected under this lock,
	// so only the new ones need a stat.
	changed := false
	for p, e := range next {
		if pe, ok := prev.Files[p]; ok && pe.Hash == e.Hash {
			continue
		}
		changed = true
		info, err := os.Stat(st.blobPath(siteID, e.Hash))
		if err != nil || !validHash(e.Hash) {
			return nil, 0, errors.New("missing uploaded content for " + p + "; re-run the deploy")
		}
		// The size comes from disk, never from the client. A declared size is
		// only ever used for display and quota arithmetic, and taking it on
		// trust let a caller report a negative total or a one-byte file that
		// is really two hundred megabytes.
		e.Size = info.Size()
		next[p] = e
	}
	if !changed && len(next) == len(prev.Files) {
		// Nothing actually moved: no new version, no rewrite, no sweep. A
		// no-op push or a delete that matched nothing should not churn the
		// manifest or make the site look freshly deployed.
		return prev, 0, nil
	}

	m := &Manifest{Version: prev.Version + 1, Updated: time.Now().UTC(), Files: next}

	if err := os.MkdirAll(st.siteDir(siteID), 0o700); err != nil {
		return nil, 0, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, 0, err
	}
	tmp := st.manifestPath(siteID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return nil, 0, err
	}
	if err := os.Rename(tmp, st.manifestPath(siteID)); err != nil {
		return nil, 0, err
	}
	st.mu.Lock()
	st.cache[siteID] = m
	st.mu.Unlock()

	// GC runs after the manifest swap: a crash mid-sweep leaves orphan blobs
	// (harmless, retried next deploy) rather than a manifest pointing at a
	// file that is already gone.
	return m, st.gc(siteID, m, prev), nil
}

// gcGrace protects blobs no manifest has ever referenced: content that has
// been uploaded for a deploy which has not landed yet. Collecting those breaks
// every other writer on the site — one agent's deploy would sweep away a
// second agent's staged uploads, and the second agent's deploy would then fail
// its own presence check, forever.
const gcGrace = time.Hour

// gc removes a site's unreferenced blobs. prev is the manifest being replaced:
// anything it referenced and the new one does not is unambiguously garbage and
// goes immediately, while a blob neither version knows about is either a
// staged upload or the remains of an abandoned one, and only the second is
// safe to touch.
func (st *Storage) gc(siteID string, m, prev *Manifest) int {
	live := make(map[string]bool, len(m.Files))
	for _, e := range m.Files {
		live[e.Hash] = true
	}
	orphaned := map[string]bool{}
	for _, e := range prev.Files {
		if !live[e.Hash] {
			orphaned[e.Hash] = true
		}
	}
	// ponytail: nothing became garbage, so skip the walk entirely rather than
	// scan the whole blob tree on every single-file push. Abandoned uploads
	// wait for the next commit that does orphan something; they are bounded by
	// the upload limit and invisible to the site either way.
	if len(orphaned) == 0 {
		return 0
	}

	cutoff := time.Now().Add(-gcGrace)
	blobs := filepath.Join(st.siteDir(siteID), "blobs")
	removed := 0
	_ = filepath.WalkDir(blobs, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a partially readable store still GCs what it can
		}
		if strings.HasPrefix(d.Name(), ".upload-") {
			return nil // an upload in flight, not an orphan
		}
		if live[d.Name()] {
			return nil
		}
		if !orphaned[d.Name()] {
			info, err := d.Info()
			if err != nil || info.ModTime().After(cutoff) {
				return nil //nolint:nilerr // staged for a deploy that has not happened yet
			}
		}
		if os.Remove(p) == nil {
			removed++
		}
		return nil
	})
	return removed
}

// Entry is one row of a directory listing: a file, or a directory summarised
// by what it holds.
type Entry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir,omitempty"`
	Size  int64  `json:"size"`
	Files int    `json:"files,omitempty"` // directories only
	Hash  string `json:"hash,omitempty"`  // files only
}

// List returns the immediate children of a directory, directories first and
// each group by name. The manifest is a flat path map, so a listing is a
// prefix scan plus a split on the next separator — no tree to keep in sync.
// The bool reports whether the directory exists at all.
func (m *Manifest) List(dir string) ([]Entry, bool) {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	dirs := map[string]*Entry{}
	files := []Entry{}
	// The root always exists, even on a site with nothing in it yet —
	// otherwise a fresh site's browser opens on an error instead of an empty
	// folder.
	found := dir == ""
	for p, e := range m.Files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		found = true
		rest := p[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			d, ok := dirs[rest[:i]]
			if !ok {
				d = &Entry{Name: rest[:i], Dir: true}
				dirs[rest[:i]] = d
			}
			d.Size += e.Size
			d.Files++
			continue
		}
		files = append(files, Entry{Name: rest, Size: e.Size, Hash: e.Hash})
	}
	out := make([]Entry, 0, len(dirs)+len(files))
	for _, d := range dirs {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return append(out, files...), found
}

// HasDir reports whether any file lives under a directory, without building
// the listing. Serving asks this on every miss, so it stops at the first hit
// rather than walking a manifest that may hold a hundred thousand paths.
func (m *Manifest) HasDir(dir string) bool {
	if dir == "" {
		return true
	}
	prefix := dir + "/"
	for p := range m.Files {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// Usage reports the on-disk byte total of a site's live files.
func (m *Manifest) Usage() int64 {
	var n int64
	for _, e := range m.Files {
		n += e.Size
	}
	return n
}

func (st *Storage) DeleteSite(siteID string) error {
	lk := st.siteLock(siteID)
	lk.Lock()
	defer lk.Unlock()

	st.mu.Lock()
	delete(st.cache, siteID)
	st.mu.Unlock()
	// ponytail: the lock entry stays behind — dropping it would hand a
	// concurrent deploy a different mutex and undo the serialisation. It is
	// one pointer per site ever created.
	return os.RemoveAll(st.siteDir(siteID))
}
