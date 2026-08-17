package main

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Guest links, several passwords per site, and the counters behind both.

// waitFor polls until cond holds. Traffic is recorded in a deferred call that
// runs after the response body has already reached the client, so a test
// reading a counter the instant its request returns is racing the server.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 400 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// newAlias mints a guest link and returns it.
func newAlias(t *testing.T, a *api, siteID, note string) Alias {
	t.Helper()
	var al Alias
	if code := a.call("POST", "/api/sites/"+siteID+"/aliases", map[string]any{"note": note}, &al); code != 201 {
		t.Fatalf("create guest link: got %d", code)
	}
	if al.ID == "" || !strings.HasSuffix(al.Domain, ".guest.test") {
		t.Fatalf("guest link came back malformed: %+v", al)
	}
	return al
}

func TestGuestLinkServesTheSameSiteWithoutNamingIt(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	index := put(t, alice, id, []byte("<h1>same content</h1>"))
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"docs/index.html": index}}, nil)
	alice.call("PATCH", "/api/sites/"+id, map[string]any{"listing": true}, nil)

	guest := newAlias(t, alice, id, "for bob")

	body, code, _ := fetch(t, ts.URL, guest.Domain, "/docs/", nil)
	if code != 200 || !strings.Contains(body, "same content") {
		t.Fatalf("guest link should serve the site: %d %q", code, body)
	}

	// The listing page names the host it was reached on. Naming the canonical
	// domain there would undo the point of handing out a throwaway one.
	body, code, _ = fetch(t, ts.URL, guest.Domain, "/", nil)
	if code != 200 {
		t.Fatalf("listing over a guest link: got %d", code)
	}
	if !strings.Contains(body, guest.Domain) {
		t.Fatalf("listing page does not name the guest domain: %q", body)
	}
	if strings.Contains(body, "play.test") {
		t.Fatal("listing page served over a guest link discloses the site's real domain")
	}
}

func TestGuestLinkAuthPromptDoesNotNameTheSite(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	alice.call("PATCH", "/api/sites/"+id, map[string]any{
		"auth_user": "guest", "auth_password": "long-enough-password",
	}, nil)
	guest := newAlias(t, alice, id, "")

	_, code, resp := fetch(t, ts.URL, guest.Domain, "/", nil)
	if code != 401 {
		t.Fatalf("a guest link is still behind the site password: got %d, want 401", code)
	}
	realm := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(realm, guest.Domain) || strings.Contains(realm, "play.test") {
		t.Fatalf("auth realm over a guest link: %q", realm)
	}
}

func TestTrafficIsCountedPerHostname(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	page := []byte("<h1>hello</h1>")
	index := put(t, alice, id, page)
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"index.html": index}}, nil)

	one := newAlias(t, alice, id, "bob")
	two := newAlias(t, alice, id, "carol")

	for range 3 {
		if _, code, _ := fetch(t, ts.URL, one.Domain, "/", nil); code != 200 {
			t.Fatalf("guest fetch: got %d", code)
		}
	}
	if _, code, _ := fetch(t, ts.URL, "play.test", "/", nil); code != 200 {
		t.Fatal("main domain fetch failed")
	}

	waitFor(t, "counters to settle", func() bool { return total(s.db.Site(id)).Count == 4 })

	site := s.db.Site(id)
	// The site's own counter is the site's own domain, and nothing else. Each
	// request moves exactly one hostname counter, so the total is a sum.
	if got := site.Stats; got.Count != 1 || got.Bytes != int64(len(page)) {
		t.Fatalf("main-domain traffic: %+v", got)
	}
	if site.Stats.First.IsZero() || site.Stats.Last.IsZero() {
		t.Fatalf("first/last never set: %+v", site.Stats)
	}

	byID := map[string]Stats{}
	for _, a := range site.Aliases {
		byID[a.ID] = a.Stats
	}
	if got := byID[one.ID]; got.Count != 3 || got.Bytes != int64(len(page))*3 {
		t.Fatalf("first guest link: %+v", got)
	}
	if got := byID[two.ID]; got.Count != 0 || got.Bytes != 0 {
		t.Fatalf("a guest link nobody used should be untouched: %+v", got)
	}
	if got := total(site); got.Bytes != int64(len(page))*4 {
		t.Fatalf("total bytes across every hostname: got %d, want %d", got.Bytes, len(page)*4)
	}
}

// total is what the panel shows as the site's traffic across every hostname:
// the site's own counter plus its guest links'. Adding rather than subtracting
// is what keeps it honest when one guest link is reset or revoked.
func total(site *Site) Stats {
	out := site.Stats
	for _, a := range site.Aliases {
		out.Count += a.Stats.Count
		out.Bytes += a.Stats.Bytes
	}
	return out
}

func TestRefusedRequestsAreNotCounted(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	index := put(t, alice, id, []byte("secret"))
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"index.html": index}}, nil)
	alice.call("PATCH", "/api/sites/"+id, map[string]any{
		"auth_user": "guest", "auth_password": "long-enough-password",
	}, nil)
	guest := newAlias(t, alice, id, "")

	for range 5 {
		fetch(t, ts.URL, guest.Domain, "/", nil)
		fetch(t, ts.URL, guest.Domain, "/", func(r *http.Request) { r.SetBasicAuth("guest", "wrong") })
	}
	// One request that does get through, so the assertion below is waiting on
	// something rather than on nothing.
	if _, code, _ := fetch(t, ts.URL, guest.Domain, "/", func(r *http.Request) {
		r.SetBasicAuth("guest", "long-enough-password")
	}); code != 200 {
		t.Fatal("correct credentials over a guest link should work")
	}
	waitFor(t, "the one allowed request", func() bool { return total(s.db.Site(id)).Count == 1 })

	if got := total(s.db.Site(id)).Count; got != 1 {
		t.Fatalf("rejected requests were counted: %d hits, want 1", got)
	}
}

func TestSeveralAccountsAreTrackedApart(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	index := put(t, alice, id, []byte("members only"))
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"index.html": index}}, nil)

	var anna, bob accountView
	if code := alice.call("POST", "/api/sites/"+id+"/accounts", map[string]any{
		"name": "anna", "user": "anna-login", "password": "long-enough-password",
	}, &anna); code != 201 {
		t.Fatalf("add first account: got %d", code)
	}
	if code := alice.call("POST", "/api/sites/"+id+"/accounts", map[string]any{
		"name": "bob", "user": "bob-login", "password": "another-long-password",
	}, &bob); code != 201 {
		t.Fatalf("add second account: got %d", code)
	}
	// A username can only mean one account, or the second would be unreachable
	// and its counters would never move.
	if code := alice.call("POST", "/api/sites/"+id+"/accounts", map[string]any{
		"name": "impostor", "user": "anna-login", "password": "yet-another-password",
	}, nil); code != 409 {
		t.Fatalf("duplicate username: got %d, want 409", code)
	}

	as := func(user, pass string) func(*http.Request) {
		return func(r *http.Request) { r.SetBasicAuth(user, pass) }
	}
	for range 2 {
		if _, code, _ := fetch(t, ts.URL, "play.test", "/", as("anna-login", "long-enough-password")); code != 200 {
			t.Fatal("anna cannot open the site")
		}
	}
	if _, code, _ := fetch(t, ts.URL, "play.test", "/", as("bob-login", "another-long-password")); code != 200 {
		t.Fatal("bob cannot open the site")
	}
	// One account's password is not another's.
	if _, code, _ := fetch(t, ts.URL, "play.test", "/", as("anna-login", "another-long-password")); code != 401 {
		t.Fatal("a password from a different account was accepted")
	}

	waitFor(t, "both accounts to be counted", func() bool { return s.db.Site(id).Stats.Count == 3 })
	byID := map[string]Stats{}
	for _, a := range s.db.Site(id).Accounts {
		byID[a.ID] = a.Stats
	}
	if got := byID[anna.ID].Count; got != 2 {
		t.Fatalf("anna's hits: got %d, want 2", got)
	}
	if got := byID[bob.ID].Count; got != 1 {
		t.Fatalf("bob's hits: got %d, want 1", got)
	}

	// Removing one account leaves the other working, and invalidates the memo
	// of every credential on the site rather than only its own.
	if code := alice.call("DELETE", "/api/sites/"+id+"/accounts/"+bob.ID, nil, nil); code != 200 {
		t.Fatalf("remove account: got %d", code)
	}
	if _, code, _ := fetch(t, ts.URL, "play.test", "/", as("bob-login", "another-long-password")); code != 401 {
		t.Fatal("a removed account still opens the site")
	}
	if _, code, _ := fetch(t, ts.URL, "play.test", "/", as("anna-login", "long-enough-password")); code != 200 {
		t.Fatal("removing one account locked out another")
	}
}

func TestAccountUsernamesNeverLeaveTheServer(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	alice.call("POST", "/api/sites/"+id+"/accounts", map[string]any{
		"name": "anna", "user": "anna-login", "password": "long-enough-password",
	}, nil)

	var raw map[string]any
	alice.call("GET", "/api/sites/"+id, nil, &raw)
	if raw["protected"] != true {
		t.Fatalf("a site with an account is protected: %v", raw["protected"])
	}
	accounts, _ := raw["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("site view should list one account: %v", raw["accounts"])
	}
	first, _ := accounts[0].(map[string]any)
	if first["name"] != "anna" {
		t.Fatalf("account label missing: %v", first)
	}
	for _, leak := range []string{"user", "hash"} {
		if _, found := first[leak]; found {
			t.Fatalf("site view leaks the account's %s", leak)
		}
	}
}

// The single-credential field is what hostrctl and the old API speak. It
// replaces whatever the site has, which is fine until "whatever the site has"
// is three other people's logins.
func TestLegacyPasswordFieldRefusesToWipeSeveralAccounts(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	for _, a := range []struct{ name, user string }{{"anna", "anna-login"}, {"bob", "bob-login"}} {
		alice.call("POST", "/api/sites/"+id+"/accounts", map[string]any{
			"name": a.name, "user": a.user, "password": "long-enough-password",
		}, nil)
	}
	if code := alice.call("PATCH", "/api/sites/"+id, map[string]any{
		"auth_user": "someone", "auth_password": "long-enough-password",
	}, nil); code != 409 {
		t.Fatalf("legacy password field on a multi-account site: got %d, want 409", code)
	}
	if n := len(s.db.Site(id).Accounts); n != 2 {
		t.Fatalf("a refused patch still changed the accounts: %d left", n)
	}
	// Clearing is still allowed: it is unambiguous about what it removes.
	if code := alice.call("PATCH", "/api/sites/"+id,
		map[string]any{"auth_user": "", "auth_password": ""}, nil); code != 200 {
		t.Fatalf("clearing every account: got %d", code)
	}
	if n := len(s.db.Site(id).Accounts); n != 0 {
		t.Fatalf("accounts survived a clear: %d left", n)
	}
}

// A password change through the legacy field is a change to that login, not a
// new one, so it keeps what that login has already earned.
func TestLegacyPasswordChangeKeepsItsCounters(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	index := put(t, alice, id, []byte("hi"))
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"index.html": index}}, nil)
	alice.call("PATCH", "/api/sites/"+id, map[string]any{
		"auth_user": "guest", "auth_password": "long-enough-password",
	}, nil)

	fetch(t, ts.URL, "play.test", "/", func(r *http.Request) {
		r.SetBasicAuth("guest", "long-enough-password")
	})
	waitFor(t, "the visit to be counted", func() bool {
		a := s.db.Site(id).Accounts
		return len(a) == 1 && a[0].Stats.Count == 1
	})

	alice.call("PATCH", "/api/sites/"+id, map[string]any{
		"auth_user": "guest", "auth_password": "a-brand-new-password",
	}, nil)
	acc := s.db.Site(id).Accounts
	if len(acc) != 1 || acc[0].Stats.Count != 1 {
		t.Fatalf("a password change reset the account's counters: %+v", acc)
	}
	if _, code, _ := fetch(t, ts.URL, "play.test", "/", func(r *http.Request) {
		r.SetBasicAuth("guest", "long-enough-password")
	}); code != 401 {
		t.Fatal("the old password still works after a change")
	}
}

func TestResettingCounters(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	index := put(t, alice, id, []byte("hi"))
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"index.html": index}}, nil)

	one := newAlias(t, alice, id, "bob")
	two := newAlias(t, alice, id, "carol")
	fetch(t, ts.URL, one.Domain, "/", nil)
	fetch(t, ts.URL, two.Domain, "/", nil)
	fetch(t, ts.URL, "play.test", "/", nil)
	waitFor(t, "three visits", func() bool { return total(s.db.Site(id)).Count == 3 })

	if code := alice.call("POST", "/api/sites/"+id+"/aliases/"+one.ID+"/reset", nil, nil); code != 200 {
		t.Fatalf("reset one guest link: got %d", code)
	}
	site := s.db.Site(id)
	byID := map[string]Stats{}
	for _, a := range site.Aliases {
		byID[a.ID] = a.Stats
	}
	if byID[one.ID].Count != 0 || !byID[one.ID].First.IsZero() {
		t.Fatalf("reset left counters behind: %+v", byID[one.ID])
	}
	if byID[two.ID].Count != 1 {
		t.Fatal("resetting one guest link zeroed another")
	}
	// The figures the panel shows must all still be true after a partial reset.
	// Nothing derived may go up because a counter went down.
	if site.Stats.Count != 1 {
		t.Fatalf("resetting a guest link changed the main-domain figure: %d", site.Stats.Count)
	}
	if got := total(site).Count; got != 2 {
		t.Fatalf("total after resetting a guest link with one visit: got %d, want 2", got)
	}

	// Revoking a guest link takes its traffic out of the total the same way,
	// with nothing left over to attribute to the main domain.
	if code := alice.call("DELETE", "/api/sites/"+id+"/aliases/"+two.ID, nil, nil); code != 200 {
		t.Fatalf("delete a guest link: got %d", code)
	}
	site = s.db.Site(id)
	if site.Stats.Count != 1 || total(site).Count != 1 {
		t.Fatalf("revoking a guest link moved its traffic onto the main domain: %+v", site.Stats)
	}

	if code := alice.call("POST", "/api/sites/"+id+"/reset", nil, nil); code != 200 {
		t.Fatalf("reset the site: got %d", code)
	}
	site = s.db.Site(id)
	if total(site).Count != 0 {
		t.Fatalf("counters survived their own reset: %+v", site.Stats)
	}
	for _, a := range site.Aliases {
		if a.Stats.Count != 0 {
			t.Fatalf("guest link %s survived the site reset: %+v", a.Domain, a.Stats)
		}
	}
}

// What counts as a visit, pinned. The bytes of every response are traffic; the
// hit count is per visit, and a visit's own follow-up requests must not each
// read as another visitor.
func TestWhatCountsAsAVisit(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	index := put(t, alice, id, []byte("<h1>hi</h1>"))
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"docs/index.html": index}}, nil)
	alice.call("PATCH", "/api/sites/"+id, map[string]any{"listing": true}, nil)

	hitsFor := func(f func()) int64 {
		before := total(s.db.Site(id)).Count
		bytesBefore := total(s.db.Site(id)).Bytes
		f()
		// Let the deferred record land, then read the delta. A wait on equality
		// would pass by default for the cases that record nothing, so this waits
		// for the bytes — every one of these responses has a body.
		waitFor(t, "the request to be recorded", func() bool {
			return total(s.db.Site(id)).Bytes > bytesBefore
		})
		return total(s.db.Site(id)).Count - before
	}

	for _, c := range []struct {
		what string
		path string
		want int64
	}{
		{"a page", "/docs/", 1},
		{"a missing file", "/nope.txt", 1},
		// The client asks again at the slashed URL, and that is the visit.
		{"the trailing-slash redirect", "/docs", 0},
		// The browser page's own feed, on a page already counted.
		{"the listing feed", "/?_hostr=ls", 0},
		{"a shared asset", "/" + assetPrefix + "md.css", 0},
	} {
		if got := hitsFor(func() {
			if _, code, _ := fetch(t, ts.URL, "play.test", c.path, nil); code == 0 {
				t.Fatalf("%s: request failed", c.what)
			}
		}); got != c.want {
			t.Errorf("%s: counted %d hits, want %d", c.what, got, c.want)
		}
	}
}

// requireOwner is applied by hand to each of these, so one forgotten call is a
// silent hole. A collaborator may deploy; deciding who can reach the site is
// not theirs, and neither is a scoped token's business.
func TestGuestLinksAndAccountsAreOwnerOnly(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	bob := tenant(t, s, ts, "bob")
	id := newSite(t, alice, "play", "play.test")
	alice.call("POST", "/api/sites/"+id+"/members", map[string]any{"name": "bob"}, nil)

	guest := newAlias(t, alice, id, "")
	var acc accountView
	alice.call("POST", "/api/sites/"+id+"/accounts", map[string]any{
		"name": "anna", "user": "anna-login", "password": "long-enough-password",
	}, &acc)

	// A scoped token is refused by the access tier before it reaches a handler,
	// so it gets 403 rather than the owner check's own answer.
	_, scoped, err := s.db.CreateToken(bob.user, "scoped", id, []string{"drafts"})
	if err != nil {
		t.Fatal(err)
	}
	scopedAPI := &api{t: t, base: ts.URL, token: scoped, user: bob.user}

	for _, c := range []struct {
		method, path string
	}{
		{"POST", "/api/sites/" + id + "/aliases"},
		{"DELETE", "/api/sites/" + id + "/aliases/" + guest.ID},
		{"POST", "/api/sites/" + id + "/aliases/" + guest.ID + "/reset"},
		{"POST", "/api/sites/" + id + "/accounts"},
		{"DELETE", "/api/sites/" + id + "/accounts/" + acc.ID},
		{"POST", "/api/sites/" + id + "/accounts/" + acc.ID + "/reset"},
		{"POST", "/api/sites/" + id + "/reset"},
	} {
		var body any
		if c.method != "DELETE" {
			body = map[string]any{}
		}
		if code := bob.call(c.method, c.path, body, nil); code != 403 {
			t.Errorf("%s %s as a collaborator: got %d, want 403", c.method, c.path, code)
		}
		if code := scopedAPI.call(c.method, c.path, body, nil); code != 403 {
			t.Errorf("%s %s with a scoped token: got %d, want 403", c.method, c.path, code)
		}
	}
	// Nothing was actually applied by any of that.
	site := s.db.Site(id)
	if len(site.Aliases) != 1 || len(site.Accounts) != 1 {
		t.Fatalf("a refused request changed the site anyway: %d links, %d accounts",
			len(site.Aliases), len(site.Accounts))
	}
}

// The guest namespace belongs to the server, not to whoever asks first. A
// tenant able to create a site under it could serve their own content — their
// own password prompt — under a name people were told to trust, and could pick
// up a guest link's name the moment its owner revoked it.
func TestNobodyCanSquatTheGuestNamespace(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	guest := newAlias(t, alice, id, "")

	for _, domain := range []string{
		"guest.test",                 // the parent itself
		"deadbeefcafe.guest.test",    // a name shaped exactly like a guest link
		"anything.deeper.guest.test", // and anywhere below it
		guest.Domain,                 // including one already handed out
	} {
		if code := alice.call("POST", "/api/sites",
			map[string]string{"slug": "grab", "domain": domain}, nil); code != 409 {
			t.Errorf("creating a site on %q: got %d, want 409", domain, code)
		}
		if code := alice.call("PATCH", "/api/sites/"+id,
			map[string]any{"domain": domain}, nil); code != 409 {
			t.Errorf("renaming a site onto %q: got %d, want 409", domain, code)
		}
	}
	if s.db.Site(id).Domain != "play.test" {
		t.Fatal("a refused rename moved the domain anyway")
	}
	// Revoking a link does not make its name available to anyone else either.
	alice.call("DELETE", "/api/sites/"+id+"/aliases/"+guest.ID, nil, nil)
	if code := alice.call("POST", "/api/sites",
		map[string]string{"slug": "grab", "domain": guest.Domain}, nil); code != 409 {
		t.Errorf("claiming a revoked guest name: got %d, want 409", code)
	}
}

func TestAccountsNeedARealUsername(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	// An empty username collides with the shorthand where empty means "remove
	// protection", and it matches every unparseable Authorization header,
	// because comparing two empty strings succeeds.
	if code := alice.call("POST", "/api/sites/"+id+"/accounts", map[string]any{
		"name": "blank", "user": "", "password": "long-enough-password",
	}, nil); code != 400 {
		t.Fatalf("account with an empty username: got %d, want 400", code)
	}
	if n := len(s.db.Site(id).Accounts); n != 0 {
		t.Fatalf("the site ended up with %d accounts", n)
	}
}

func TestGuestDomainsCannotCollide(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	bob := tenant(t, s, ts, "bob")
	id := newSite(t, alice, "play", "play.test")
	guest := newAlias(t, alice, id, "")

	// Nobody may claim a name a guest link already answers on — not another
	// tenant creating a site, and not this site renaming itself onto it.
	if code := bob.call("POST", "/api/sites",
		map[string]string{"slug": "evil", "domain": guest.Domain}, nil); code != 409 {
		t.Fatalf("creating a site on someone's guest domain: got %d, want 409", code)
	}
	if code := alice.call("PATCH", "/api/sites/"+id,
		map[string]any{"domain": guest.Domain}, nil); code != 409 {
		t.Fatalf("renaming a site onto its own guest domain: got %d, want 409", code)
	}
	if s.db.Site(id).Domain != "play.test" {
		t.Fatal("a refused rename moved the domain anyway")
	}

	// And a guest link keeps working after the site moves.
	if code := alice.call("PATCH", "/api/sites/"+id, map[string]any{"domain": "moved.test"}, nil); code != 200 {
		t.Fatalf("rename: got %d", code)
	}
	if site, al := s.db.Resolve(guest.Domain); site == nil || al == nil || al.ID != guest.ID {
		t.Fatal("a guest link stopped resolving after the site was renamed")
	}
}

func TestCountersSurviveARestart(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	index := put(t, alice, id, []byte("hi"))
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"index.html": index}}, nil)
	guest := newAlias(t, alice, id, "bob")

	fetch(t, ts.URL, guest.Domain, "/", nil)
	fetch(t, ts.URL, "play.test", "/", nil)
	waitFor(t, "both visits", func() bool { return total(s.db.Site(id)).Count == 2 })

	// Nothing is on disk until a flush; that is the whole point of the timer.
	if err := s.db.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	reopened, err := openDB(s.db.path, "guest.test")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	site := reopened.Site(id)
	if site.Stats.Count != 1 || site.Stats.Bytes == 0 {
		t.Fatalf("main-domain counters did not survive: %+v", site.Stats)
	}
	if len(site.Aliases) != 1 || site.Aliases[0].Stats.Count != 1 {
		t.Fatalf("guest link counters did not survive: %+v", site.Aliases)
	}
}

// A mutation whose write to disk fails is live in memory and missing from the
// file. The flush timer is what closes that gap; before the dirty flag was set
// on the way *into* save, a failed write left nothing to retry and the store
// silently diverged from what the server was enforcing.
func TestAFailedWriteIsRetriedByTheNextFlush(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")

	// Make the store unwritable by putting a directory where its temp file goes.
	if err := os.Mkdir(s.db.path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if code := alice.call("POST", "/api/sites/"+id+"/accounts", map[string]any{
		"name": "anna", "user": "anna-login", "password": "long-enough-password",
	}, nil); code == 201 {
		t.Fatal("a mutation that could not be written reported success")
	}
	// The server is enforcing it regardless: it is in memory.
	if len(s.db.Site(id).Accounts) != 1 {
		t.Fatal("the mutation was not applied in memory")
	}
	if err := os.Remove(s.db.path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Flush(); err != nil {
		t.Fatalf("flush after the write became possible again: %v", err)
	}
	reopened, err := openDB(s.db.path, "guest.test")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(reopened.Site(id).Accounts); n != 1 {
		t.Fatalf("the flush did not pick up the failed write: %d accounts on disk", n)
	}
}

func TestLegacySitePasswordBecomesAnAccount(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hostr.json"
	// A hand-written store from before accounts existed. The hash is not
	// verified by the migration, only carried across.
	raw := `{"users":{},"sites":{"s1":{"id":"s1","slug":"play","domain":"play.test",` +
		`"owner":"u1","members":[],"auth_user":"guest","auth_hash":"argon2id$c2FsdA$aGFzaA"}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := openDB(path, "")
	if err != nil {
		t.Fatalf("open a pre-accounts store: %v", err)
	}
	site := db.Site("s1")
	if len(site.Accounts) != 1 {
		t.Fatalf("the old credential did not become an account: %+v", site.Accounts)
	}
	if a := site.Accounts[0]; a.User != "guest" || a.Hash != "argon2id$c2FsdA$aGFzaA" || a.Name == "" {
		t.Fatalf("migrated account is wrong: %+v", a)
	}
	if site.AuthUser != "" || site.AuthHash != "" {
		t.Fatalf("the old fields were left behind: %q %q", site.AuthUser, site.AuthHash)
	}
	// Written back, so the migration does not run again on every start and the
	// file on disk matches what is being served.
	again, err := openDB(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again.Site("s1").Accounts); n != 1 {
		t.Fatalf("migration was not persisted: %d accounts after reopening", n)
	}
	if strings.Contains(string(mustRead(t, path)), "auth_user") {
		t.Fatal("the migrated store still carries auth_user")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Serving mutates the counters that live inside the site records, and the panel
// marshals those same records. Handing out a shallow copy would race here, and
// would let the view's own hash-blanking reach through into the live store.
func TestCountersDoNotRaceTheSiteView(t *testing.T) {
	s, ts := newTestServer(t)
	alice := tenant(t, s, ts, "alice")
	id := newSite(t, alice, "play", "play.test")
	index := put(t, alice, id, []byte("hi"))
	alice.call("POST", "/api/sites/"+id+"/deploy",
		map[string]any{"files": map[string]FileEntry{"index.html": index}}, nil)
	alice.call("POST", "/api/sites/"+id+"/accounts", map[string]any{
		"name": "anna", "user": "anna-login", "password": "long-enough-password",
	}, nil)
	guest := newAlias(t, alice, id, "bob")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				fetch(t, ts.URL, guest.Domain, "/", func(r *http.Request) {
					r.SetBasicAuth("anna-login", "long-enough-password")
				})
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				var v map[string]any
				alice.call("GET", "/api/sites/"+id, nil, &v)
			}
		}()
		// And the slices Record walks, being appended to and cut from
		// underneath it.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				var extra Alias
				alice.call("POST", "/api/sites/"+id+"/aliases", map[string]any{"note": "churn"}, &extra)
				alice.call("DELETE", "/api/sites/"+id+"/aliases/"+extra.ID, nil, nil)
			}
		}()
	}
	wg.Wait()

	// The credential the view blanks on its copy must still be the real one.
	if _, code, _ := fetch(t, ts.URL, "play.test", "/", func(r *http.Request) {
		r.SetBasicAuth("anna-login", "long-enough-password")
	}); code != 200 {
		t.Fatal("reading the site view destroyed the stored credential")
	}
}
