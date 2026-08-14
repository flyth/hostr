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

// Deploy replaces a site's file set wholesale. Every referenced blob must
// already be uploaded; anything the new manifest drops is garbage collected,
// which is how deletions propagate.
func (st *Storage) Deploy(siteID string, files map[string]FileEntry) (*Manifest, int, error) {
	// Path and hash shapes are pure checks, so do them before queueing on the
	// lock — a malformed request should not wait behind someone else's deploy.
	clean := make(map[string]FileEntry, len(files))
	for p, e := range files {
		cp, ok := cleanPath(p)
		if !ok {
			return nil, 0, errors.New("illegal path: " + p)
		}
		if !validHash(e.Hash) {
			return nil, 0, errBadHash
		}
		clean[cp] = e
	}

	lk := st.siteLock(siteID)
	lk.Lock()
	defer lk.Unlock()

	// Blob presence is checked *inside* the lock. A deploy that raced with
	// another one and lost its uploads fails here rather than publishing a
	// manifest that points at collected files.
	for p, e := range clean {
		if !st.HasBlob(siteID, e.Hash) {
			return nil, 0, errors.New("missing uploaded content for " + p + "; re-run the deploy")
		}
	}

	st.mu.RLock()
	prev := st.cache[siteID]
	st.mu.RUnlock()
	if prev == nil {
		prev = st.readManifest(siteID)
	}
	m := &Manifest{Version: prev.Version + 1, Updated: time.Now().UTC(), Files: clean}

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
	return m, st.gc(siteID, m), nil
}

func (st *Storage) gc(siteID string, m *Manifest) int {
	live := make(map[string]bool, len(m.Files))
	for _, e := range m.Files {
		live[e.Hash] = true
	}
	blobs := filepath.Join(st.siteDir(siteID), "blobs")
	removed := 0
	_ = filepath.WalkDir(blobs, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a partially readable store still GCs what it can
		}
		if strings.HasPrefix(d.Name(), ".upload-") {
			return nil // an upload in flight, not an orphan
		}
		if !live[d.Name()] {
			if os.Remove(p) == nil {
				removed++
			}
		}
		return nil
	})
	return removed
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
