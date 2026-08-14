package main

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Scopes, incremental writes, listings and site passwords. The invariant worth
// protecting here is that a scoped write touches its subtree and nothing else —
// deploy used to be a wholesale replace, and every path below is a way that
// could quietly go wrong.

// fetch is get with a hook for headers, and without redirect following so the
// directory redirect can be observed rather than resolved away.
func fetch(t *testing.T, base, host, path string, mod func(*http.Request)) (string, int, *http.Response) {
	t.Helper()
	req, err := http.NewRequest("GET", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if mod != nil {
		mod(req)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, resp
}

// put uploads one blob and returns its manifest entry.
func put(t *testing.T, a *api, siteID string, body []byte) FileEntry {
	t.Helper()
	if code := a.call("PUT", "/api/sites/"+siteID+"/blobs/"+sum(body), body, nil); code != 201 {
		t.Fatalf("upload blob: got %d", code)
	}
	return FileEntry{Hash: sum(body), Size: int64(len(body))}
}

// newSite creates a site owned by a and returns its ID.
func newSite(t *testing.T, a *api, slug, domain string) string {
	t.Helper()
	var site struct{ ID string }
	if code := a.call("POST", "/api/sites", map[string]string{"slug": slug, "domain": domain}, &site); code != 201 {
		t.Fatalf("create site: got %d", code)
	}
	return site.ID
}

func manifestOf(t *testing.T, a *api, siteID string) Manifest {
	t.Helper()
	var m Manifest
	if code := a.call("GET", "/api/sites/"+siteID+"/manifest", nil, &m); code != 200 {
		t.Fatalf("manifest: got %d", code)
	}
	return m
}

func TestScopedDeployReplacesOnlyItsSubtree(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	root := put(t, alice, id, []byte("root index"))
	oldOne := put(t, alice, id, []byte("scope file one"))
	other := put(t, alice, id, []byte("a different project"))

	if code := alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{"files": map[string]FileEntry{
		"index.html":            root,
		"my-website/index.html": oldOne,
		"other/index.html":      other,
	}}, nil); code != 200 {
		t.Fatalf("initial deploy: got %d", code)
	}

	// A scoped deploy carrying a single file replaces the whole of that scope.
	fresh := put(t, alice, id, []byte("replacement"))
	if code := alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{
		"scope": "my-website",
		"files": map[string]FileEntry{"page.html": fresh},
	}, nil); code != 200 {
		t.Fatalf("scoped deploy: got %d", code)
	}

	m := manifestOf(t, alice, id)
	for _, want := range []string{"index.html", "other/index.html", "my-website/page.html"} {
		if _, ok := m.Files[want]; !ok {
			t.Fatalf("scoped deploy lost %s; manifest is %v", want, m.Files)
		}
	}
	if _, ok := m.Files["my-website/index.html"]; ok {
		t.Fatal("scoped deploy kept a file the new scope did not contain")
	}
	// The blob behind the replaced file is collected; the untouched project's
	// blob must survive, which is the part a naive whole-site GC would break.
	if s.storage.HasBlob(id, oldOne.Hash) {
		t.Fatal("replaced blob survived collection")
	}
	if !s.storage.HasBlob(id, other.Hash) {
		t.Fatal("a scoped deploy collected another scope's blob")
	}
}

func TestScopedDeployCannotEscapeItsScope(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	e := put(t, alice, id, []byte("payload"))

	for _, p := range []string{"../escaped.html", "../../etc/passwd", "/absolute.html", "a/../../b"} {
		code := alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{
			"scope": "my-website",
			"files": map[string]FileEntry{p: e},
		}, nil)
		if code != 400 {
			t.Fatalf("deploy of %q into a scope: got %d, want 400", p, code)
		}
	}
	if code := alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{
		"scope": "../elsewhere",
		"files": map[string]FileEntry{"a.html": e},
	}, nil); code != 400 {
		t.Fatalf("deploy with a traversing scope: got %d, want 400", code)
	}
}

func TestScopeOnlyRefusesWholeSiteWrites(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	e := put(t, alice, id, []byte("payload"))

	if code := alice.call("PATCH", "/api/sites/"+id, map[string]any{"scoped_only": true}, nil); code != 200 {
		t.Fatalf("enable scoped_only: got %d", code)
	}
	if code := alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"index.html": e}}, nil); code != 403 {
		t.Fatalf("unscoped deploy on a scope-only site: got %d, want 403", code)
	}
	if code := alice.call("PATCH", "/api/sites/"+id+"/files",
		map[string]any{"delete": []string{"/"}}, nil); code != 403 {
		t.Fatalf("whole-site delete on a scope-only site: got %d, want 403", code)
	}
	// A scoped write still goes through.
	if code := alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{
		"scope": "my-website",
		"files": map[string]FileEntry{"index.html": e},
	}, nil); code != 200 {
		t.Fatalf("scoped deploy on a scope-only site: got %d, want 200", code)
	}
}

func TestPatchAddsAndDeletesWithoutTouchingTheRest(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	keep := put(t, alice, id, []byte("keep me"))
	doomed := put(t, alice, id, []byte("delete me"))
	nested := put(t, alice, id, []byte("nested"))
	alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{"files": map[string]FileEntry{
		"index.html":       keep,
		"assets/gone.txt":  doomed,
		"drafts/one/a.txt": nested,
	}}, nil)

	// A single-file push: one entry set, nothing else disturbed.
	added := put(t, alice, id, []byte("brand new"))
	if code := alice.call("PATCH", "/api/sites/"+id+"/files", map[string]any{
		"set": map[string]FileEntry{"assets/new.png": added},
	}, nil); code != 200 {
		t.Fatalf("patch set: got %d", code)
	}
	m := manifestOf(t, alice, id)
	if len(m.Files) != 4 {
		t.Fatalf("push changed the file count to %d, want 4: %v", len(m.Files), m.Files)
	}

	// Exact delete, then a recursive one.
	if code := alice.call("PATCH", "/api/sites/"+id+"/files", map[string]any{
		"delete": []string{"assets/gone.txt"},
	}, nil); code != 200 {
		t.Fatalf("patch delete: got %d", code)
	}
	if code := alice.call("PATCH", "/api/sites/"+id+"/files", map[string]any{
		"delete": []string{"drafts/"},
	}, nil); code != 200 {
		t.Fatalf("recursive delete: got %d", code)
	}
	m = manifestOf(t, alice, id)
	if len(m.Files) != 2 {
		t.Fatalf("after deletes: %v", m.Files)
	}
	if _, ok := m.Files["index.html"]; !ok {
		t.Fatal("delete took an unrelated file with it")
	}
	if _, ok := m.Files["assets/new.png"]; !ok {
		t.Fatal("recursive delete of drafts/ reached into assets/")
	}
	if s.storage.HasBlob(id, nested.Hash) {
		t.Fatal("recursively deleted blob survived collection")
	}
}

func TestScopedTokenIsConfined(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	other := newSite(t, alice, "blog", "blog.test")

	mine := put(t, alice, id, []byte("in scope"))
	theirs := put(t, alice, id, []byte("out of scope"))
	alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{"files": map[string]FileEntry{
		"my-website/index.html": mine,
		"secrets/keys.txt":      theirs,
	}}, nil)

	_, secret, err := s.db.CreateToken(alice.user, "agent", id, []string{"my-website"})
	if err != nil {
		t.Fatal(err)
	}
	agent := &api{t: t, base: ts.URL, token: secret}

	// Inside the scope: allowed.
	fresh := put(t, agent, id, []byte("agent output"))
	if code := agent.call("POST", "/api/sites/"+id+"/deploy", map[string]any{
		"scope": "my-website",
		"files": map[string]FileEntry{"render.png": fresh},
	}, nil); code != 200 {
		t.Fatalf("scoped token writing its own scope: got %d, want 200", code)
	}
	if code := agent.call("PATCH", "/api/sites/"+id+"/files", map[string]any{
		"set": map[string]FileEntry{"my-website/extra.png": fresh},
	}, nil); code != 200 {
		t.Fatalf("scoped token pushing into its own scope: got %d, want 200", code)
	}

	// Everything else: refused.
	for _, c := range []struct {
		what         string
		method, path string
		body         any
		want         int
	}{
		{"whole-site deploy", "POST", "/api/sites/" + id + "/deploy",
			map[string]any{"files": map[string]FileEntry{"index.html": fresh}}, 403},
		{"another scope", "POST", "/api/sites/" + id + "/deploy",
			map[string]any{"scope": "secrets", "files": map[string]FileEntry{"a.txt": fresh}}, 403},
		{"push outside the scope", "PATCH", "/api/sites/" + id + "/files",
			map[string]any{"set": map[string]FileEntry{"secrets/new.txt": fresh}}, 403},
		{"delete outside the scope", "PATCH", "/api/sites/" + id + "/files",
			map[string]any{"delete": []string{"secrets/keys.txt"}}, 403},
		{"empty the site", "PATCH", "/api/sites/" + id + "/files",
			map[string]any{"delete": []string{"/"}}, 403},
		{"another site", "GET", "/api/sites/" + other, nil, 404},
		{"change settings", "PATCH", "/api/sites/" + id, map[string]any{"listing": true}, 403},
		{"delete the site", "DELETE", "/api/sites/" + id, nil, 403},
		{"mint a token", "POST", "/api/tokens", map[string]any{"name": "escape"}, 403},
		{"change the password", "POST", "/api/password",
			map[string]any{"old": "correct-horse-battery", "new": "another-long-password"}, 403},
	} {
		if code := agent.call(c.method, c.path, c.body, nil); code != c.want {
			t.Fatalf("%s with a scoped token: got %d, want %d", c.what, code, c.want)
		}
	}

	// It cannot even see the rest of the site.
	m := manifestOf(t, agent, id)
	if _, ok := m.Files["secrets/keys.txt"]; ok {
		t.Fatal("a scoped token can enumerate files outside its scope")
	}
	if _, ok := m.Files["my-website/render.png"]; !ok {
		t.Fatalf("a scoped token cannot see its own files: %v", m.Files)
	}
	var sites []struct{ ID string }
	agent.call("GET", "/api/sites", nil, &sites)
	if len(sites) != 1 || sites[0].ID != id {
		t.Fatalf("a site-bound token sees %d sites, want just its own", len(sites))
	}
}

func TestDirectoryListing(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	pic := put(t, alice, id, []byte("not really a png"))
	alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{"files": map[string]FileEntry{
		"gallery/one.png":       pic,
		"gallery/sub/two.png":   pic,
		"gallery/notes/why.txt": pic,
	}}, nil)

	// Off by default: a directory with no index is still a 404, and the JSON
	// feed is not reachable either.
	if _, code, _ := fetch(t, ts.URL, "play.test", "/gallery/", nil); code != 404 {
		t.Fatalf("listing is on without being enabled: got %d", code)
	}
	if _, code, _ := fetch(t, ts.URL, "play.test", "/gallery/?_hostr=ls", nil); code != 404 {
		t.Fatalf("listing JSON served while listing is off: got %d", code)
	}

	if code := alice.call("PATCH", "/api/sites/"+id, map[string]any{"listing": true}, nil); code != 200 {
		t.Fatalf("enable listing: got %d", code)
	}

	body, code, _ := fetch(t, ts.URL, "play.test", "/gallery/", nil)
	if code != 200 {
		t.Fatalf("listing page: got %d", code)
	}
	// The shell must not carry any file name into markup; names arrive as JSON.
	if !strings.Contains(body, "hostr-browser") {
		t.Fatal("listing page did not render the browser shell")
	}
	if strings.Contains(body, "one.png") {
		t.Fatal("listing page interpolated a file name into its markup")
	}

	var res struct {
		Path    string
		Entries []Entry
	}
	if code := alice.call("GET", "/api/sites/"+id+"/files?path=gallery", nil, &res); code != 200 {
		t.Fatalf("file listing API: got %d", code)
	}
	if len(res.Entries) != 3 || !res.Entries[0].Dir || res.Entries[0].Name != "notes" {
		t.Fatalf("directories should sort first: %+v", res.Entries)
	}
	if res.Entries[2].Name != "one.png" || res.Entries[2].Size == 0 {
		t.Fatalf("file rows need names and sizes: %+v", res.Entries)
	}

	// A directory URL without its slash redirects rather than 404s.
	if _, code, resp := fetch(t, ts.URL, "play.test", "/gallery", nil); code != 301 {
		t.Fatalf("directory redirect: got %d (%s)", code, resp.Header.Get("Location"))
	}
	// An index.html still wins over the listing.
	page := put(t, alice, id, []byte("<h1>gallery</h1>"))
	alice.call("PATCH", "/api/sites/"+id+"/files",
		map[string]any{"set": map[string]FileEntry{"gallery/index.html": page}}, nil)
	if body, code, _ := fetch(t, ts.URL, "play.test", "/gallery/", nil); code != 200 || !strings.Contains(body, "<h1>gallery</h1>") {
		t.Fatalf("index.html should win over the listing: %d %q", code, body)
	}
}

func TestSiteBasicAuth(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	index := put(t, alice, id, []byte("secret page"))
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"index.html": index}}, nil)

	if code := alice.call("PATCH", "/api/sites/"+id, map[string]any{
		"auth_user": "guest", "auth_password": "long-enough-password",
	}, nil); code != 200 {
		t.Fatalf("set site password: got %d", code)
	}

	_, code, resp := fetch(t, ts.URL, "play.test", "/", nil)
	if code != 401 {
		t.Fatalf("unauthenticated visitor: got %d, want 401", code)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("401 without a WWW-Authenticate header will not prompt a browser")
	}
	for _, c := range []struct{ user, pass string }{
		{"guest", "wrong"}, {"wrong", "long-enough-password"}, {"", ""},
	} {
		if _, code, _ := fetch(t, ts.URL, "play.test", "/", func(r *http.Request) {
			r.SetBasicAuth(c.user, c.pass)
		}); code != 401 {
			t.Fatalf("credentials %q/%q: got %d, want 401", c.user, c.pass, code)
		}
	}
	body, code, _ := fetch(t, ts.URL, "play.test", "/", func(r *http.Request) {
		r.SetBasicAuth("guest", "long-enough-password")
	})
	if code != 200 || body != "secret page" {
		t.Fatalf("correct credentials: got %d %q", code, body)
	}

	// The password hash must never reach a client through the site view.
	var raw map[string]any
	alice.call("GET", "/api/sites/"+id, nil, &raw)
	if _, leaked := raw["auth_hash"]; leaked {
		t.Fatal("site view leaks the password hash")
	}
	if raw["protected"] != true {
		t.Fatalf("site view should report protection: %v", raw["protected"])
	}

	// Clearing the username removes protection.
	alice.call("PATCH", "/api/sites/"+id, map[string]any{"auth_user": "", "auth_password": ""}, nil)
	if _, code, _ := fetch(t, ts.URL, "play.test", "/", nil); code != 200 {
		t.Fatalf("after removing the password: got %d", code)
	}
}

// A token bound to a site is not a scoped token, and the first cut of the
// access rules let it walk straight out through POST /api/tokens.
func TestSiteBoundTokenCannotReachTheAccount(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	other := newSite(t, alice, "blog", "blog.test")

	_, secret, err := s.db.CreateToken(alice.user, "site-bound", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	bound := &api{t: t, base: ts.URL, token: secret, user: alice.user}

	for _, c := range []struct {
		what         string
		method, path string
		body         any
		want         int
	}{
		{"mint another token", "POST", "/api/tokens", map[string]any{"name": "escape"}, 403},
		{"list tokens", "GET", "/api/tokens", nil, 403},
		{"change the password", "POST", "/api/password",
			map[string]any{"old": "correct-horse-battery", "new": "another-long-password"}, 403},
		{"create a site", "POST", "/api/sites", map[string]string{"slug": "new", "domain": "new.test"}, 403},
		{"touch the other site", "GET", "/api/sites/" + other, nil, 404},
		{"delete the other site", "DELETE", "/api/sites/" + other, nil, 404},
	} {
		if code := bound.call(c.method, c.path, c.body, nil); code != c.want {
			t.Fatalf("%s with a site-bound token: got %d, want %d", c.what, code, c.want)
		}
	}
	// It is still a full credential for the site it names.
	if code := bound.call("PATCH", "/api/sites/"+id, map[string]any{"listing": true}, nil); code != 200 {
		t.Fatalf("site-bound token managing its own site: got %d, want 200", code)
	}
}

func TestScopeOnlyBlocksPiecemealWrites(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	e := put(t, alice, id, []byte("payload"))
	alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{"files": map[string]FileEntry{
		"index.html":  e,
		"blog/a.html": e,
		"docs/b.html": e,
	}}, nil)
	alice.call("PATCH", "/api/sites/"+id, map[string]any{"scoped_only": true}, nil)

	// Emptying a site scope by scope is the same act as emptying it at once.
	if code := alice.call("PATCH", "/api/sites/"+id+"/files", map[string]any{
		"delete": []string{"blog/", "docs/", "index.html"},
	}, nil); code != 403 {
		t.Fatalf("piecewise wipe on a scope-only site: got %d, want 403", code)
	}
	// So is replacing the landing page from outside any scope.
	if code := alice.call("PATCH", "/api/sites/"+id+"/files", map[string]any{
		"set": map[string]FileEntry{"index.html": e},
	}, nil); code != 403 {
		t.Fatalf("root-level push on a scope-only site: got %d, want 403", code)
	}
	// Work inside a scope is untouched by the rule.
	if code := alice.call("PATCH", "/api/sites/"+id+"/files", map[string]any{
		"set":    map[string]FileEntry{"blog/c.html": e},
		"delete": []string{"blog/a.html"},
	}, nil); code != 200 {
		t.Fatalf("scoped patch on a scope-only site: got %d, want 200", code)
	}
	if m := manifestOf(t, alice, id); len(m.Files) != 3 {
		t.Fatalf("scope-only site lost files it should have kept: %v", m.Files)
	}
}

// The whole point of scopes is several writers on one site, and a garbage
// collector that cannot tell a staged upload from an orphan makes that
// impossible: one agent's deploy sweeps away another's uploads.
func TestDeployDoesNotCollectAnotherWritersStagedUploads(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	staged := put(t, alice, id, []byte("agent A's artifact, not deployed yet"))
	landed := put(t, alice, id, []byte("agent B's artifact"))

	if code := alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{
		"scope": "b", "files": map[string]FileEntry{"index.html": landed},
	}, nil); code != 200 {
		t.Fatalf("B's deploy: got %d", code)
	}
	if !s.storage.HasBlob(id, staged.Hash) {
		t.Fatal("a deploy collected another writer's staged upload")
	}
	if code := alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{
		"scope": "a", "files": map[string]FileEntry{"index.html": staged},
	}, nil); code != 200 {
		t.Fatalf("A's deploy after B's: got %d, want 200", code)
	}
}

func TestSizeComesFromDiskNotTheClient(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	body := []byte("twenty-nine bytes of content!")
	put(t, alice, id, body)
	if code := alice.call("PATCH", "/api/sites/"+id+"/files", map[string]any{
		"set": map[string]FileEntry{"a.txt": {Hash: sum(body), Size: -1 << 40}},
	}, nil); code != 200 {
		t.Fatalf("patch: got %d", code)
	}
	m := manifestOf(t, alice, id)
	if got := m.Files["a.txt"].Size; got != int64(len(body)) {
		t.Fatalf("declared size was trusted: manifest says %d, blob is %d", got, len(body))
	}
}

func TestScopedTokenCannotShadowItsOwnScope(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	e := put(t, alice, id, []byte("hijack"))
	_, secret, err := s.db.CreateToken(alice.user, "agent", id, []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	agent := &api{t: t, base: ts.URL, token: secret, user: alice.user}

	// A file named exactly `docs` sits beside the scope, not inside it, and
	// serving would resolve it before docs/index.html.
	if code := agent.call("PATCH", "/api/sites/"+id+"/files", map[string]any{
		"set": map[string]FileEntry{"docs": e},
	}, nil); code != 403 {
		t.Fatalf("scoped token writing a file named after its scope: got %d, want 403", code)
	}

	// The owner can create that collision; a later scoped deploy must clear it.
	alice.call("PATCH", "/api/sites/"+id+"/files", map[string]any{
		"set": map[string]FileEntry{"docs": e},
	}, nil)
	page := put(t, alice, id, []byte("<h1>docs</h1>"))
	alice.call("POST", "/api/sites/"+id+"/deploy", map[string]any{
		"scope": "docs", "files": map[string]FileEntry{"index.html": page},
	}, nil)
	if _, ok := manifestOf(t, alice, id).Files["docs"]; ok {
		t.Fatal("a scoped deploy left a file shadowing the scope it just published")
	}
	if body, code, _ := fetch(t, ts.URL, "play.test", "/docs/", nil); code != 200 || body != "<h1>docs</h1>" {
		t.Fatalf("scope is shadowed by a same-named file: %d %q", code, body)
	}
}

func TestScopedTokenGetsNoContentOracle(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	secretDoc := []byte("the memo the agent must not confirm")
	e := put(t, alice, id, secretDoc)
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"secrets/memo.txt": e}}, nil)

	_, token, err := s.db.CreateToken(alice.user, "agent", id, []string{"my-website"})
	if err != nil {
		t.Fatal(err)
	}
	agent := &api{t: t, base: ts.URL, token: token, user: alice.user}

	var chk struct{ Missing []string }
	if code := agent.call("POST", "/api/sites/"+id+"/check",
		map[string]any{"hashes": []string{sum(secretDoc)}}, &chk); code != 200 {
		t.Fatalf("check: got %d", code)
	}
	if len(chk.Missing) != 1 {
		t.Fatal("check confirmed the existence of content outside the token's scopes")
	}
}

// One page load is not one request. A visitor opening a gallery fires every
// asset at once on a cold cache, and if each of those verifications counts
// against the guessing budget — or runs its own 64 MiB hash — the page half
// loads and the site looks broken.
func TestSiteAuthSurvivesAPageFullOfAssets(t *testing.T) {
	s, ts := newTestServer(t)
	// A budget far smaller than the number of requests the page makes.
	s.siteThrottle = newThrottle(3, time.Minute)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	index := put(t, alice, id, []byte("members only"))
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"index.html": index}}, nil)
	alice.call("PATCH", "/api/sites/"+id, map[string]any{
		"auth_user": "guest", "auth_password": "long-enough-password",
	}, nil)

	hit := func() int {
		req, err := http.NewRequest("GET", ts.URL+"/", nil)
		if err != nil {
			return 0
		}
		req.Host = "play.test"
		req.SetBasicAuth("guest", "long-enough-password")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	codes := make([]int, 12)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = hit()
		}()
	}
	wg.Wait()
	for i, code := range codes {
		if code != 200 {
			t.Fatalf("asset %d of a single page load: got %d, want 200", i, code)
		}
	}

	// And a visitor who is already through stays through while someone else
	// guesses from the same address — which behind a reverse proxy is every
	// visitor's address.
	for range 5 {
		fetch(t, ts.URL, "play.test", "/", func(r *http.Request) { r.SetBasicAuth("guest", "wrong") })
	}
	if code := hit(); code != 200 {
		t.Fatalf("valid credentials after someone else's guessing: got %d, want 200", code)
	}
}

func TestSiteAuthRejectsUnusableUsername(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	// A colon cannot survive basic auth's encoding, so storing one would make
	// the site permanently unopenable.
	if code := alice.call("PATCH", "/api/sites/"+id, map[string]any{
		"auth_user": "gu:est", "auth_password": "long-enough-password",
	}, nil); code != 400 {
		t.Fatalf("username with a colon: got %d, want 400", code)
	}
	// A rejected settings patch must not have applied its other half either.
	if code := alice.call("PATCH", "/api/sites/"+id, map[string]any{
		"domain": "moved.test", "auth_user": "guest", "auth_password": "short",
	}, nil); code != 400 {
		t.Fatalf("short site password: got %d, want 400", code)
	}
	var v map[string]any
	alice.call("GET", "/api/sites/"+id, nil, &v)
	if v["domain"] != "play.test" {
		t.Fatalf("a failed settings patch still moved the domain to %v", v["domain"])
	}
}

func TestEmptySiteListsAsEmpty(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	var res struct{ Entries []Entry }
	if code := alice.call("GET", "/api/sites/"+id+"/files?path=", nil, &res); code != 200 {
		t.Fatalf("listing a site with no files yet: got %d, want 200", code)
	}
	if len(res.Entries) != 0 {
		t.Fatalf("empty site listed %d entries", len(res.Entries))
	}
}

func TestManifestListGroupsDirectories(t *testing.T) {
	m := &Manifest{Files: map[string]FileEntry{
		"a.txt":         {Size: 1},
		"dir/b.txt":     {Size: 2},
		"dir/c.txt":     {Size: 3},
		"dir/sub/d.txt": {Size: 4},
	}}
	entries, found := m.List("")
	if !found || len(entries) != 2 {
		t.Fatalf("root listing: %+v", entries)
	}
	if !entries[0].Dir || entries[0].Name != "dir" || entries[0].Files != 3 || entries[0].Size != 9 {
		t.Fatalf("directory row should summarise everything beneath it: %+v", entries[0])
	}
	if entries[1].Dir || entries[1].Name != "a.txt" {
		t.Fatalf("files come after directories: %+v", entries[1])
	}
	if _, found := m.List("a.txt"); found {
		t.Fatal("a file is not a directory")
	}
	if _, found := m.List("nope"); found {
		t.Fatal("listing a directory that does not exist should report so")
	}
}
