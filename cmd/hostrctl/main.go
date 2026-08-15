// hostrctl uploads static sites to a hostr server.
//
//	hostrctl login   -server https://admin.example.com
//	hostrctl sites
//	hostrctl create  -slug blog -domain blog.example.com
//	hostrctl deploy  -site blog ./build
//	hostrctl status  -site blog
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// version is stamped by the release build with -X main.version.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"login":    cmdLogin,
		"logout":   cmdLogout,
		"whoami":   cmdWhoami,
		"sites":    cmdSites,
		"create":   cmdCreate,
		"deploy":   cmdDeploy,
		"push":     cmdPush,
		"ls":       cmdLs,
		"rm":       cmdRm,
		"settings": cmdSettings,
		"token":    cmdToken,
		"status":   cmdStatus,
		"delete":   cmdDelete,
		"share":    cmdShare,
		"version": func([]string) error {
			fmt.Println(version)
			return nil
		},
	}
	run, ok := cmds[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "hostrctl: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `hostrctl - deploy static sites to a hostr server

  login    -server URL [-user NAME]  authenticate and store an API token
  logout                             forget the stored token
  whoami                             show the current account
  sites                              list sites you can deploy to
  create   -slug S -domain D         create a new site
  deploy   -site S [-scope DIR] [-dry-run] DIR   sync DIR to a site or a scope
  push     -site S [-scope DIR] [-as PATH] FILE...  upload files, delete nothing
  ls       -site S [PATH]            list one directory of a site
  rm       -site S [-r] PATH...      delete files or a whole directory
  settings -site S [-listing] [-no-root-listing] [-markdown] [-scope-only] [-auth-user U] [-no-auth]
  token    -name N [-site S] [-scope DIR]...  mint an API token
  status   -site S                   show a site's current deployment
  share    -site S -user NAME [-remove]  manage collaborators
  delete   -site S                   delete a site and all its files
  version                            print the binary's version

A scope is a directory on the site: "deploy -scope docs" replaces everything
under /docs/ and leaves the rest of the site alone. "push" never deletes.

Environment: HOSTR_SERVER and HOSTR_TOKEN override the stored config.
`)
}

// ---- config ----

type config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	User   string `json:"user,omitempty"`
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hostrctl", "config.json"), nil
}

func loadConfig() (config, error) {
	var c config
	p, err := configPath()
	if err == nil {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &c)
		}
	}
	if v := os.Getenv("HOSTR_SERVER"); v != "" {
		c.Server = v
	}
	if v := os.Getenv("HOSTR_TOKEN"); v != "" {
		c.Token = v
	}
	c.Server = strings.TrimSuffix(c.Server, "/")
	if c.Server == "" || c.Token == "" {
		return c, errors.New("not logged in; run `hostrctl login -server https://...`")
	}
	return c, nil
}

func saveConfig(c config) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600) // contains an API token
}

// ---- HTTP ----

type client struct {
	server string
	token  string
	http   *http.Client
}

func newClient(c config) *client {
	return &client{server: c.Server, token: c.Token, http: &http.Client{Timeout: 0}}
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status) }

func (c *client) do(method, p string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequest(method, c.server+p, body)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &apiError{Status: resp.StatusCode, Message: msg}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *client) getJSON(p string, out any) error { return c.do("GET", p, nil, "", out) }

func (c *client) postJSON(p string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do("POST", p, bytes.NewReader(b), "application/json", out)
}

func (c *client) patchJSON(p string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do("PATCH", p, bytes.NewReader(b), "application/json", out)
}

// ---- commands ----

func cmdLogin(args []string) error {
	fset := flag.NewFlagSet("login", flag.ExitOnError)
	server := fset.String("server", "", "base URL of the hostr control panel")
	user := fset.String("user", "", "username")
	name := fset.String("name", "", "label for the API token")
	_ = fset.Parse(args)

	if *server == "" {
		return errors.New("-server is required")
	}
	if *user == "" {
		fmt.Print("username: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		*user = strings.TrimSpace(line)
	}
	pw, err := readPassword("password: ")
	if err != nil {
		return err
	}

	base := strings.TrimSuffix(*server, "/")
	// Log in with a cookie jar just long enough to mint a token; the token is
	// what gets stored, so the password never touches disk.
	jar := &cookieJar{}
	c := &client{server: base, http: &http.Client{Jar: jar}}
	var me struct {
		Name  string `json:"name"`
		Admin bool   `json:"admin"`
	}
	if err := c.postJSON("/api/login", map[string]string{"name": *user, "password": pw}, &me); err != nil {
		return err
	}
	label := *name
	if label == "" {
		h, _ := os.Hostname()
		label = "hostrctl@" + h
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := c.postJSON("/api/tokens", map[string]string{"name": label}, &tok); err != nil {
		return err
	}
	if err := saveConfig(config{Server: base, Token: tok.Token, User: me.Name}); err != nil {
		return err
	}
	_ = c.postJSON("/api/logout", struct{}{}, nil)
	fmt.Printf("logged in as %s; token %q saved\n", me.Name, label)
	return nil
}

func cmdLogout(args []string) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Println("local token removed (revoke it in the web console to be sure)")
	return nil
}

func cmdWhoami(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var me struct {
		Name  string `json:"name"`
		Admin bool   `json:"admin"`
	}
	if err := newClient(cfg).getJSON("/api/me", &me); err != nil {
		return err
	}
	role := ""
	if me.Admin {
		role = " (admin)"
	}
	fmt.Printf("%s%s on %s\n", me.Name, role, cfg.Server)
	return nil
}

type site struct {
	ID         string    `json:"id"`
	Slug       string    `json:"slug"`
	Domain     string    `json:"domain"`
	Owner      string    `json:"owner"`
	OwnerNam   string    `json:"owner_name"`
	Files      int       `json:"files"`
	Bytes      int64     `json:"bytes"`
	Version    int       `json:"version"`
	Deployed   time.Time `json:"deployed"`
	Mine       bool      `json:"mine"`
	Listing    bool      `json:"listing"`
	NoRootList bool      `json:"listing_no_root"`
	Markdown   bool      `json:"markdown"`
	ScopedOnly bool      `json:"scoped_only"`
	Protected  bool      `json:"protected"`
}

func cmdSites(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var sites []site
	if err := newClient(cfg).getJSON("/api/sites", &sites); err != nil {
		return err
	}
	if len(sites) == 0 {
		fmt.Println("no sites yet — `hostrctl create -slug NAME -domain DOMAIN`")
		return nil
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Slug < sites[j].Slug })
	for _, s := range sites {
		owner := "shared by " + s.OwnerNam
		if s.Mine {
			owner = "owner"
		}
		fmt.Printf("%-20s %-32s v%-4d %5d files  %8s  %s\n",
			s.Slug, s.Domain, s.Version, s.Files, humanBytes(s.Bytes), owner)
	}
	return nil
}

func cmdCreate(args []string) error {
	fset := flag.NewFlagSet("create", flag.ExitOnError)
	slug := fset.String("slug", "", "short name")
	domain := fset.String("domain", "", "domain to serve it on")
	_ = fset.Parse(args)
	if *slug == "" || *domain == "" {
		return errors.New("-slug and -domain are required")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var s site
	if err := newClient(cfg).postJSON("/api/sites", map[string]string{"slug": *slug, "domain": *domain}, &s); err != nil {
		return err
	}
	fmt.Printf("created %s -> %s\n", s.Slug, s.Domain)
	return nil
}

func cmdStatus(args []string) error {
	fset := flag.NewFlagSet("status", flag.ExitOnError)
	name := fset.String("site", "", "site slug, domain or id")
	_ = fset.Parse(args)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	c := newClient(cfg)
	s, err := resolveSite(c, *name)
	if err != nil {
		return err
	}
	var m manifest
	if err := c.getJSON("/api/sites/"+s.ID+"/manifest", &m); err != nil {
		return err
	}
	fmt.Printf("%s (%s)\nversion %d, %d files, %s\n", s.Slug, s.Domain, m.Version, len(m.Files), humanBytes(s.Bytes))
	if !m.Updated.IsZero() {
		fmt.Printf("last deploy %s\n", m.Updated.Local().Format(time.RFC1123))
	}
	return nil
}

func cmdShare(args []string) error {
	fset := flag.NewFlagSet("share", flag.ExitOnError)
	name := fset.String("site", "", "site slug, domain or id")
	user := fset.String("user", "", "username to add or remove")
	remove := fset.Bool("remove", false, "remove instead of add")
	_ = fset.Parse(args)
	if *user == "" {
		return errors.New("-user is required")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	c := newClient(cfg)
	s, err := resolveSite(c, *name)
	if err != nil {
		return err
	}
	if *remove {
		var full struct {
			Collaborators []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"collaborators"`
		}
		if err := c.getJSON("/api/sites/"+s.ID, &full); err != nil {
			return err
		}
		for _, m := range full.Collaborators {
			if m.Name == *user {
				if err := c.do("DELETE", "/api/sites/"+s.ID+"/members/"+m.ID, nil, "", nil); err != nil {
					return err
				}
				fmt.Printf("removed %s from %s\n", *user, s.Slug)
				return nil
			}
		}
		return fmt.Errorf("%s is not a collaborator on %s", *user, s.Slug)
	}
	if err := c.postJSON("/api/sites/"+s.ID+"/members", map[string]string{"name": *user}, nil); err != nil {
		return err
	}
	fmt.Printf("added %s to %s\n", *user, s.Slug)
	return nil
}

func cmdDelete(args []string) error {
	fset := flag.NewFlagSet("delete", flag.ExitOnError)
	name := fset.String("site", "", "site slug, domain or id")
	yes := fset.Bool("yes", false, "skip the confirmation prompt")
	_ = fset.Parse(args)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	c := newClient(cfg)
	s, err := resolveSite(c, *name)
	if err != nil {
		return err
	}
	if !*yes {
		fmt.Printf("Delete site %q (%s) and all %d of its files? This cannot be undone. [y/N] ", s.Slug, s.Domain, s.Files)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "y" {
			return errors.New("aborted")
		}
	}
	if err := c.do("DELETE", "/api/sites/"+s.ID, nil, "", nil); err != nil {
		return err
	}
	fmt.Printf("deleted %s\n", s.Slug)
	return nil
}

// ---- deploy ----

type fileEntry struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

type manifest struct {
	Version int                  `json:"version"`
	Updated time.Time            `json:"updated"`
	Files   map[string]fileEntry `json:"files"`
}

func cmdDeploy(args []string) error {
	fset := flag.NewFlagSet("deploy", flag.ExitOnError)
	name := fset.String("site", "", "site slug, domain or id")
	scopeFlag := fset.String("scope", "", "directory on the site to sync into; the rest is untouched")
	dry := fset.Bool("dry-run", false, "show what would change, upload nothing")
	hidden := fset.Bool("hidden", false, "include dot-files and dot-directories")
	workers := fset.Int("j", 4, "parallel uploads")
	_ = fset.Parse(args)
	scope := strings.Trim(*scopeFlag, "/")

	dir := fset.Arg(0)
	if dir == "" {
		return errors.New("usage: hostrctl deploy -site NAME DIR")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	c := newClient(cfg)
	s, err := resolveSite(c, *name)
	if err != nil {
		return err
	}

	local, err := scan(dir, *hidden)
	if err != nil {
		return err
	}
	if len(local) == 0 {
		return fmt.Errorf("%s contains no files", dir)
	}

	var remote manifest
	if err := c.getJSON("/api/sites/"+s.ID+"/manifest", &remote); err != nil {
		return err
	}
	// With a scope, only that subtree is being replaced, so it is the only
	// part the diff — and in particular the delete count — may consider.
	compare := remote.Files
	if scope != "" {
		compare = map[string]fileEntry{}
		for p, e := range remote.Files {
			if rest, ok := strings.CutPrefix(p, scope+"/"); ok {
				compare[rest] = e
			}
		}
	}
	added, changed, removed := diff(local, compare)

	// The server is the authority on what it already stores: ask it which
	// content hashes are missing rather than guessing from the old manifest.
	hashes := make([]string, 0, len(local))
	for _, e := range local {
		hashes = append(hashes, e.Hash)
	}
	var check struct {
		Missing []string `json:"missing"`
	}
	if err := c.postJSON("/api/sites/"+s.ID+"/check", map[string]any{"hashes": hashes}, &check); err != nil {
		return err
	}
	missing := map[string]bool{}
	for _, h := range check.Missing {
		missing[h] = true
	}

	var upload []string // local paths whose content must be sent
	seen := map[string]bool{}
	var uploadBytes int64
	for p, e := range local {
		if missing[e.Hash] && !seen[e.Hash] {
			seen[e.Hash] = true
			upload = append(upload, p)
			uploadBytes += e.Size
		}
	}
	sort.Strings(upload)

	target := s.Domain
	if scope != "" {
		target += "/" + scope + "/"
	}
	fmt.Printf("%s -> %s\n", dir, target)
	fmt.Printf("  %d files, %d new, %d changed, %d to delete\n", len(local), len(added), len(changed), len(removed))
	fmt.Printf("  %d blobs to upload (%s), %d already on server\n",
		len(upload), humanBytes(uploadBytes), len(local)-len(upload))
	if *dry {
		printList("  + ", added)
		printList("  ~ ", changed)
		printList("  - ", removed)
		return nil
	}

	if err := uploadAll(c, s.ID, dir, local, upload, *workers); err != nil {
		return err
	}

	var res struct {
		Version int   `json:"version"`
		Files   int   `json:"files"`
		Bytes   int64 `json:"bytes"`
		Removed int   `json:"removed"`
	}
	if err := c.postJSON("/api/sites/"+s.ID+"/deploy", map[string]any{"scope": scope, "files": local}, &res); err != nil {
		return err
	}
	fmt.Printf("deployed v%d: %d files, %s live, %d stale blobs collected\n",
		res.Version, res.Files, humanBytes(res.Bytes), res.Removed)
	return nil
}

func uploadAll(c *client, siteID, dir string, local map[string]fileEntry, upload []string, workers int) error {
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	done := 0

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				e := local[p]
				err := putBlob(c, siteID, e.Hash, filepath.Join(dir, filepath.FromSlash(p)))
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("upload %s: %w", p, err)
				}
				done++
				fmt.Printf("\r  uploading %d/%d", done, len(upload))
				mu.Unlock()
			}
		}()
	}
	for _, p := range upload {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		jobs <- p
	}
	close(jobs)
	wg.Wait()
	if len(upload) > 0 {
		fmt.Println()
	}
	return firstErr
}

func putBlob(c *client, siteID, hash, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	req, err := http.NewRequest("PUT", c.server+"/api/sites/"+siteID+"/blobs/"+hash, f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &apiError{Status: resp.StatusCode, Message: msg}
	}
	return nil
}

// scan hashes every file under dir. Symlinks are not followed: a link pointing
// outside the directory would otherwise smuggle unrelated files into a deploy.
func scan(dir string, hidden bool) (map[string]fileEntry, error) {
	out := map[string]fileEntry{}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		base := d.Name()
		if !hidden && strings.HasPrefix(base, ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlink, socket, device: skipped deliberately
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := hashFile(p)
		if err != nil {
			return err
		}
		out[rel] = fileEntry{Hash: h, Size: info.Size()}
		return nil
	})
	return out, err
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func diff(local map[string]fileEntry, remote map[string]fileEntry) (added, changed, removed []string) {
	for p, e := range local {
		r, ok := remote[p]
		switch {
		case !ok:
			added = append(added, p)
		case r.Hash != e.Hash:
			changed = append(changed, p)
		}
	}
	for p := range remote {
		if _, ok := local[p]; !ok {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	return
}

// ---- small helpers ----

func resolveSite(c *client, name string) (*site, error) {
	var sites []site
	if err := c.getJSON("/api/sites", &sites); err != nil {
		return nil, err
	}
	if name == "" {
		if len(sites) == 1 {
			return &sites[0], nil
		}
		return nil, errors.New("-site is required (you have access to more than one)")
	}
	name = strings.ToLower(name)
	var hits []site
	for _, s := range sites {
		if s.ID == name || s.Domain == name || s.Slug == name {
			hits = append(hits, s)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no site named %q that you can deploy to", name)
	case 1:
		return &hits[0], nil
	default:
		return nil, fmt.Errorf("%q is ambiguous; use the domain or id", name)
	}
}

func readPassword(prompt string) (string, error) {
	if pw := os.Getenv("HOSTR_PASSWORD"); pw != "" {
		return pw, nil
	}
	fmt.Fprint(os.Stderr, prompt)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

func printList(prefix string, items []string) {
	for _, i := range items {
		fmt.Println(prefix + i)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// cookieJar is the minimum http.CookieJar needed to carry a session cookie
// through login -> create token. Nothing here needs domain or path matching.
type cookieJar struct {
	mu      sync.Mutex
	cookies []*http.Cookie
}

func (j *cookieJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cookies = append(j.cookies, cookies...)
}

func (j *cookieJar) Cookies(_ *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cookies
}
