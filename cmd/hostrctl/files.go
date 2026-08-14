package main

// Commands that work on individual files rather than a whole directory sync:
// push, ls, rm, settings and token. They all go through the incremental
// `PATCH /files` endpoint, which never deletes anything it was not asked to.

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// stringList collects a flag that may be repeated.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// joinScope places a site-relative path inside a scope.
func joinScope(scope, p string) string {
	p = strings.Trim(filepath.ToSlash(p), "/")
	scope = strings.Trim(scope, "/")
	if scope == "" {
		return p
	}
	return scope + "/" + p
}

type listing struct {
	Path    string      `json:"path"`
	Entries []listEntry `json:"entries"`
}

type listEntry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Files int    `json:"files"`
	Hash  string `json:"hash"`
}

type writeResult struct {
	Version int   `json:"version"`
	Files   int   `json:"files"`
	Bytes   int64 `json:"bytes"`
	Removed int   `json:"removed"`
}

// ---- push ----

func cmdPush(args []string) error {
	fset := flag.NewFlagSet("push", flag.ExitOnError)
	name := fset.String("site", "", "site slug, domain or id")
	scope := fset.String("scope", "", "directory on the site to place the files in")
	as := fset.String("as", "", "path on the site for a single file")
	quiet := fset.Bool("q", false, "print only the resulting URLs")
	_ = fset.Parse(args)

	local := fset.Args()
	if len(local) == 0 {
		return errors.New("usage: hostrctl push -site NAME [-scope DIR] FILE...")
	}
	if *as != "" && len(local) != 1 {
		return errors.New("-as names the destination of a single file")
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

	set := map[string]fileEntry{}
	source := map[string]string{} // site path -> local path
	for _, p := range local {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if st.IsDir() {
			into := ""
			if sc := strings.Trim(*scope, "/"); sc != "" {
				into = " -scope " + sc
			}
			return fmt.Errorf("%s is a directory; use `hostrctl deploy -site %s%s %s` to sync one", p, *name, into, p)
		}
		h, err := hashFile(p)
		if err != nil {
			return err
		}
		dest := *as
		if dest == "" {
			dest = filepath.Base(p)
		}
		dest = joinScope(*scope, dest)
		set[dest] = fileEntry{Hash: h, Size: st.Size()}
		source[dest] = p
	}

	// Same content-addressed shortcut as deploy: ask what is missing rather
	// than re-uploading a file the server already has.
	hashes := make([]string, 0, len(set))
	for _, e := range set {
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
	sent := map[string]bool{}
	for dest, e := range set {
		if !missing[e.Hash] || sent[e.Hash] {
			continue
		}
		sent[e.Hash] = true
		if err := putBlob(c, s.ID, e.Hash, source[dest]); err != nil {
			return fmt.Errorf("upload %s: %w", source[dest], err)
		}
	}

	var res writeResult
	if err := c.patchJSON("/api/sites/"+s.ID+"/files", map[string]any{"set": set}, &res); err != nil {
		return err
	}

	dests := make([]string, 0, len(set))
	for dest := range set {
		dests = append(dests, dest)
	}
	sort.Strings(dests)
	for _, dest := range dests {
		fmt.Printf("https://%s/%s\n", s.Domain, dest)
	}
	if !*quiet {
		fmt.Printf("pushed %d file(s) as v%d: %d files, %s live\n",
			len(set), res.Version, res.Files, humanBytes(res.Bytes))
	}
	return nil
}

// ---- ls ----

func cmdLs(args []string) error {
	fset := flag.NewFlagSet("ls", flag.ExitOnError)
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
	dir := strings.Trim(fset.Arg(0), "/")
	var res listing
	if err := c.getJSON("/api/sites/"+s.ID+"/files?path="+url.QueryEscape(dir), &res); err != nil {
		return err
	}
	if len(res.Entries) == 0 {
		fmt.Println("(empty)")
		return nil
	}
	for _, e := range res.Entries {
		if e.Dir {
			fmt.Printf("%10s  %-40s %d files\n", humanBytes(e.Size), e.Name+"/", e.Files)
			continue
		}
		fmt.Printf("%10s  %s\n", humanBytes(e.Size), e.Name)
	}
	return nil
}

// ---- rm ----

func cmdRm(args []string) error {
	fset := flag.NewFlagSet("rm", flag.ExitOnError)
	name := fset.String("site", "", "site slug, domain or id")
	recursive := fset.Bool("r", false, "delete a directory and everything under it")
	yes := fset.Bool("yes", false, "skip the confirmation prompt")
	_ = fset.Parse(args)

	targets := fset.Args()
	if len(targets) == 0 {
		return errors.New("usage: hostrctl rm -site NAME [-r] PATH...")
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

	// Resolve against the live manifest first: deleting is unrecoverable, so a
	// typo should be an error rather than a version bump that removed nothing.
	var m manifest
	if err := c.getJSON("/api/sites/"+s.ID+"/manifest", &m); err != nil {
		return err
	}
	del := make([]string, 0, len(targets))
	total := 0
	for _, t := range targets {
		clean := strings.Trim(filepath.ToSlash(t), "/")
		if clean == "" {
			return errors.New("refusing to delete the site root; use `hostrctl delete -site NAME`")
		}
		if *recursive {
			n := 0
			for p := range m.Files {
				if strings.HasPrefix(p, clean+"/") {
					n++
				}
			}
			if n == 0 {
				if _, ok := m.Files[clean]; ok {
					return fmt.Errorf("%s is a file, not a directory; drop -r", clean)
				}
				return fmt.Errorf("%s: no such directory on %s", clean, s.Slug)
			}
			del = append(del, clean+"/")
			total += n
			continue
		}
		if _, ok := m.Files[clean]; !ok {
			hint := ""
			for p := range m.Files {
				if strings.HasPrefix(p, clean+"/") {
					hint = "; it is a directory, pass -r"
					break
				}
			}
			return fmt.Errorf("%s: no such file on %s%s", clean, s.Slug, hint)
		}
		del = append(del, clean)
		total++
	}

	if !*yes {
		fmt.Printf("Delete %d file(s) from %s (%s)? This cannot be undone. [y/N] ", total, s.Slug, s.Domain)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "y" {
			return errors.New("aborted")
		}
	}
	var res writeResult
	if err := c.patchJSON("/api/sites/"+s.ID+"/files", map[string]any{"delete": del}, &res); err != nil {
		return err
	}
	fmt.Printf("deleted %d file(s), now v%d: %d files, %s live, %d blobs collected\n",
		total, res.Version, res.Files, humanBytes(res.Bytes), res.Removed)
	return nil
}

// ---- settings ----

func cmdSettings(args []string) error {
	fset := flag.NewFlagSet("settings", flag.ExitOnError)
	name := fset.String("site", "", "site slug, domain or id")
	listingOn := fset.Bool("listing", false, "serve a file browser where a directory has no index.html")
	scopeOnly := fset.Bool("scope-only", false, "refuse any write that is not confined to a scope")
	authUser := fset.String("auth-user", "", "username for the basic auth prompt in front of the site")
	authPass := fset.String("auth-password", "", "password for basic auth; prompted for if omitted")
	noAuth := fset.Bool("no-auth", false, "remove basic auth from the site")
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

	// Only the flags actually typed go into the patch — everything else keeps
	// its current value rather than being reset to a zero.
	body := map[string]any{}
	fset.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "listing":
			body["listing"] = *listingOn
		case "scope-only":
			body["scoped_only"] = *scopeOnly
		}
	})
	switch {
	case *noAuth:
		body["auth_user"] = ""
		body["auth_password"] = ""
	case *authUser != "":
		pass := *authPass
		if pass == "" {
			if pass, err = readPassword("Site password: "); err != nil {
				return err
			}
		}
		body["auth_user"] = *authUser
		body["auth_password"] = pass
	}

	if len(body) == 0 {
		fmt.Printf("%s (%s)\n", s.Slug, s.Domain)
		fmt.Printf("  listing     %s\n", onOff(s.Listing))
		fmt.Printf("  scope-only  %s\n", onOff(s.ScopedOnly))
		fmt.Printf("  basic auth  %s\n", onOff(s.Protected))
		return nil
	}
	var out site
	if err := c.patchJSON("/api/sites/"+s.ID, body, &out); err != nil {
		return err
	}
	fmt.Printf("%s: listing %s, scope-only %s, basic auth %s\n",
		out.Slug, onOff(out.Listing), onOff(out.ScopedOnly), onOff(out.Protected))
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ---- token ----

func cmdToken(args []string) error {
	fset := flag.NewFlagSet("token", flag.ExitOnError)
	label := fset.String("name", "", "label for the token")
	name := fset.String("site", "", "restrict the token to one site")
	var scopes stringList
	fset.Var(&scopes, "scope", "restrict the token to a directory; repeatable")
	_ = fset.Parse(args)

	if len(scopes) > 0 && *name == "" {
		return errors.New("-scope needs -site: a scope only means something within one site")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	c := newClient(cfg)
	body := map[string]any{"name": *label, "scopes": []string(scopes)}
	if *name != "" {
		s, err := resolveSite(c, *name)
		if err != nil {
			return err
		}
		body["site"] = s.ID
	}
	var res struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
		Token  string   `json:"token"`
	}
	if err := c.postJSON("/api/tokens", body, &res); err != nil {
		return err
	}
	fmt.Println(res.Token)
	fmt.Fprintf(os.Stderr, "token %s created; it is shown once and stored only as a hash\n", res.ID)
	if len(res.Scopes) > 0 {
		fmt.Fprintf(os.Stderr, "limited to %s within that site, and to file access only\n", strings.Join(res.Scopes, ", "))
	}
	return nil
}
