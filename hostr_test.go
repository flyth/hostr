package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// One end-to-end check covering the parts that would quietly ruin the product
// if they broke: the tenancy gate, the hash verification, deletion via GC, and
// path traversal.

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(dir+"/hostr.json", "guest.test")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:          Config{Data: dir, AdminDomain: "admin.test", AliasDomain: "guest.test", MaxUpload: 1 << 20},
		db:           db,
		storage:      newStorage(dir),
		sessions:     newSessions(),
		throttle:     newThrottle(100, time.Minute),
		siteThrottle: newThrottle(100, time.Minute),
		siteAuth:     newAuthCache(),
		log:          log.New(io.Discard, "", 0),
		spa:          os.DirFS(dir), // no SPA in tests
	}
	s.api = s.routes()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

type api struct {
	t     *testing.T
	base  string
	token string
	user  string // the account behind the token, for tests that mint their own
}

func (a *api) call(method, path string, body any, out any) int {
	a.t.Helper()
	var r io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		r, ct = bytes.NewReader(b), "application/octet-stream"
	default:
		raw, _ := json.Marshal(b)
		r, ct = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequest(method, a.base+path, r)
	if err != nil {
		a.t.Fatal(err)
	}
	req.Host = "admin.test"
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// tenant creates a user plus an API token without going through the HTTP login
// flow, which keeps the test from paying for extra argon2 rounds.
func tenant(t *testing.T, s *Server, ts *httptest.Server, name string) *api {
	t.Helper()
	u, err := s.db.CreateUser(name, "correct-horse-battery", false)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := s.db.CreateToken(u.ID, "test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return &api{t: t, base: ts.URL, token: secret, user: u.ID}
}

func TestDeployServeAndIsolation(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	bob := tenant(t, s, ts, "bob")

	var site struct{ ID string }
	if code := alice.call("POST", "/api/sites", map[string]string{"slug": "blog", "domain": "blog.test"}, &site); code != 201 {
		t.Fatalf("create site: got %d", code)
	}

	index := []byte("<h1>hello</h1>")
	stale := []byte("this file will be deleted")
	files := map[string]FileEntry{
		"index.html": {Hash: sum(index), Size: int64(len(index))},
		"old.txt":    {Hash: sum(stale), Size: int64(len(stale))},
	}

	// check -> upload -> deploy, the exact sequence hostrctl performs.
	var chk struct{ Missing []string }
	alice.call("POST", "/api/sites/"+site.ID+"/check",
		map[string]any{"hashes": []string{sum(index), sum(stale)}}, &chk)
	if len(chk.Missing) != 2 {
		t.Fatalf("expected both blobs missing, got %v", chk.Missing)
	}
	for _, b := range [][]byte{index, stale} {
		if code := alice.call("PUT", "/api/sites/"+site.ID+"/blobs/"+sum(b), b, nil); code != 201 {
			t.Fatalf("upload: got %d", code)
		}
	}
	if code := alice.call("POST", "/api/sites/"+site.ID+"/deploy", map[string]any{"files": files}, nil); code != 200 {
		t.Fatalf("deploy: got %d", code)
	}

	// Re-checking now reports nothing missing: this is what makes a second
	// deploy skip unchanged files.
	chk.Missing = nil
	alice.call("POST", "/api/sites/"+site.ID+"/check", map[string]any{"hashes": []string{sum(index)}}, &chk)
	if len(chk.Missing) != 0 {
		t.Fatalf("expected no missing blobs on redeploy, got %v", chk.Missing)
	}

	// The site is served on its own domain.
	if body, code := get(t, ts, "blog.test", "/"); code != 200 || body != string(index) {
		t.Fatalf("serve /: got %d %q", code, body)
	}
	if _, code := get(t, ts, "blog.test", "/old.txt"); code != 200 {
		t.Fatalf("serve /old.txt: got %d", code)
	}
	if _, code := get(t, ts, "nobody.test", "/"); code != 404 {
		t.Fatalf("unbound domain should 404, got %d", code)
	}

	// Dropping old.txt from the manifest removes it from the site and from disk.
	delete(files, "old.txt")
	var res struct{ Removed int }
	alice.call("POST", "/api/sites/"+site.ID+"/deploy", map[string]any{"files": files}, &res)
	if _, code := get(t, ts, "blog.test", "/old.txt"); code != 404 {
		t.Fatalf("deleted file still served: %d", code)
	}
	if res.Removed != 1 {
		t.Fatalf("expected 1 blob collected, got %d", res.Removed)
	}
	if s.storage.HasBlob(site.ID, sum(stale)) {
		t.Fatal("orphan blob survived garbage collection")
	}

	// Tenancy: bob knows the site ID and still gets nothing.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/sites/" + site.ID, nil},
		{"GET", "/api/sites/" + site.ID + "/manifest", nil},
		{"POST", "/api/sites/" + site.ID + "/deploy", map[string]any{"files": files}},
		{"PUT", "/api/sites/" + site.ID + "/blobs/" + sum(index), index},
	} {
		if code := bob.call(c.method, c.path, c.body, nil); code != 404 {
			t.Fatalf("%s %s as bob: got %d, want 404", c.method, c.path, code)
		}
	}
	var bobSites []map[string]any
	bob.call("GET", "/api/sites", nil, &bobSites)
	if len(bobSites) != 0 {
		t.Fatalf("bob should see no sites, got %v", bobSites)
	}

	// Bob cannot steal the domain either.
	if code := bob.call("POST", "/api/sites", map[string]string{"slug": "mine", "domain": "blog.test"}, nil); code != 409 {
		t.Fatalf("duplicate domain: got %d, want 409", code)
	}

	// A blob whose bytes do not match the declared hash is rejected.
	if code := alice.call("PUT", "/api/sites/"+site.ID+"/blobs/"+sum(index), []byte("tampered"), nil); code != 400 {
		t.Fatalf("hash mismatch: got %d, want 400", code)
	}

	// Anonymous callers get nothing.
	anon := &api{t: t, base: ts.URL}
	if code := anon.call("GET", "/api/sites", nil, nil); code != 401 {
		t.Fatalf("anonymous list: got %d, want 401", code)
	}
}

func get(t *testing.T, ts *httptest.Server, host, path string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func TestCleanPathRejectsTraversal(t *testing.T) {
	bad := []string{
		"../secret", "a/../../b", "/etc/passwd", "a//b", "a/./b",
		"", "a/", "a\\b", "a\x00b", "..",
	}
	for _, p := range bad {
		if _, ok := cleanPath(p); ok {
			t.Errorf("cleanPath(%q) accepted a path it should reject", p)
		}
	}
	for _, p := range []string{"index.html", "a/b/c.css", "_app/immutable/x.js"} {
		if _, ok := cleanPath(p); !ok {
			t.Errorf("cleanPath(%q) rejected a legitimate path", p)
		}
	}
}

func TestDeployRejectsIllegalPathsAndMissingBlobs(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	var site struct{ ID string }
	alice.call("POST", "/api/sites", map[string]string{"slug": "x", "domain": "x.test"}, &site)

	body := []byte("data")
	alice.call("PUT", "/api/sites/"+site.ID+"/blobs/"+sum(body), body, nil)

	// A path that escapes the site root is refused even with a valid blob.
	if code := alice.call("POST", "/api/sites/"+site.ID+"/deploy", map[string]any{
		"files": map[string]FileEntry{"../escape": {Hash: sum(body), Size: 4}},
	}, nil); code != 400 {
		t.Fatalf("traversal deploy: got %d, want 400", code)
	}
	// So is a manifest referencing content that was never uploaded.
	if code := alice.call("POST", "/api/sites/"+site.ID+"/deploy", map[string]any{
		"files": map[string]FileEntry{"a.html": {Hash: strings.Repeat("a", 64), Size: 1}},
	}, nil); code != 400 {
		t.Fatalf("dangling blob deploy: got %d, want 400", code)
	}
}

// Two deploys racing on one site must never leave a published manifest
// pointing at a blob that the other deploy's garbage collector removed. Run
// this with -race; without the per-site deploy lock it fails within a few
// iterations.
func TestConcurrentDeploysKeepManifestConsistent(t *testing.T) {
	st := newStorage(t.TempDir())
	const site = "site1"

	shared := []byte("shared")
	if _, err := st.PutBlob(site, sum(shared), bytes.NewReader(shared)); err != nil {
		t.Fatal(err)
	}
	small := FileEntry{Hash: sum(shared), Size: int64(len(shared))}

	for i := range 60 {
		extra := fmt.Appendf(nil, "extra-%d", i)
		if _, err := st.PutBlob(site, sum(extra), bytes.NewReader(extra)); err != nil {
			t.Fatal(err)
		}
		big := map[string]FileEntry{
			"a.html": small,
			"b.html": {Hash: sum(extra), Size: int64(len(extra))},
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			st.Deploy(site, "", map[string]FileEntry{"a.html": small}) //nolint:errcheck // losing the race is a valid outcome
		}()
		go func() {
			defer wg.Done()
			st.Deploy(site, "", big) //nolint:errcheck // as above
		}()
		wg.Wait()

		for p, e := range st.Manifest(site).Files {
			if !st.HasBlob(site, e.Hash) {
				t.Fatalf("iteration %d: manifest serves %q from blob %s, which is not on disk", i, p, e.Hash)
			}
		}
	}
}

// Login and register hand out a session cookie, so a cross-site POST is an
// attack even though the victim sends no credential of their own.
func TestLoginRejectsCrossOriginAndNonJSON(t *testing.T) {
	_, ts := newTestServer(t)
	body := `{"name":"alice","password":"correct-horse-battery"}`

	for _, c := range []struct {
		name, origin, contentType string
		want                      int
	}{
		{"cross-origin", "https://evil.example", "application/json", http.StatusForbidden},
		{"form text/plain", "", "text/plain", http.StatusUnsupportedMediaType},
		{"form urlencoded", "", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"no content type", "", "", http.StatusUnsupportedMediaType},
		// The legitimate shapes still work: same-origin browser, and hostrctl
		// which sends no Origin at all.
		{"same-origin", "http://admin.test", "application/json", http.StatusUnauthorized},
		{"no origin (cli)", "", "application/json", http.StatusUnauthorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest("POST", ts.URL+"/api/login", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "admin.test"
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			if c.contentType != "" {
				req.Header.Set("Content-Type", c.contentType)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("got %d, want %d", resp.StatusCode, c.want)
			}
			if len(resp.Cookies()) > 0 {
				t.Fatalf("a rejected login must not set a cookie, got %v", resp.Cookies())
			}
		})
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := hashPassword("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(h, "correct-horse-battery") {
		t.Fatal("valid password rejected")
	}
	if verifyPassword(h, "correct-horse-batterx") {
		t.Fatal("wrong password accepted")
	}
	if verifyPassword("garbage", "correct-horse-battery") {
		t.Fatal("malformed hash accepted")
	}
}
