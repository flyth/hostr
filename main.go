package main

import (
	"context"
	"embed"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The management SPA is compiled in. `web/` holds only a committed placeholder
// until `make frontend` drops the SvelteKit build on top of it, so `go build`
// works on a fresh clone without Node installed.
//
//go:embed all:web
var webFiles embed.FS

type Config struct {
	Addr          string
	Data          string
	AdminDomain   string
	MaxUpload     int64
	SecureCookies bool
	TrustProxy    bool
}

type Server struct {
	cfg      Config
	db       *DB
	storage  *Storage
	sessions *sessions
	throttle *throttle
	// siteThrottle counts failed basic-auth attempts per site and client
	// address. It is separate from the login throttle: that one guards one
	// account, this one guards a whole site's audience.
	siteThrottle *throttle
	siteAuth     *authCache
	log          *log.Logger
	api          http.Handler
	spa          fs.FS
}

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)

	cfg := Config{
		Addr:          env("HOSTR_ADDR", ":8080"),
		Data:          env("HOSTR_DATA", "/data"),
		AdminDomain:   normDomain(env("HOSTR_ADMIN_DOMAIN", "")),
		MaxUpload:     envInt("HOSTR_MAX_UPLOAD", 100<<20),
		SecureCookies: env("HOSTR_SECURE_COOKIES", "true") != "false",
		TrustProxy:    env("HOSTR_TRUST_PROXY", "") == "true",
	}

	if err := os.MkdirAll(cfg.Data, 0o700); err != nil {
		logger.Fatalf("data directory %s: %v", cfg.Data, err)
	}
	db, err := openDB(path.Join(cfg.Data, "hostr.json"))
	if err != nil {
		logger.Fatalf("open store: %v", err)
	}
	spa, err := fs.Sub(webFiles, "web")
	if err != nil {
		logger.Fatalf("embedded assets: %v", err)
	}

	s := &Server{
		cfg:          cfg,
		db:           db,
		storage:      newStorage(cfg.Data),
		sessions:     newSessions(),
		throttle:     newThrottle(10, 15*time.Minute),
		siteThrottle: newThrottle(30, 5*time.Minute),
		siteAuth:     newAuthCache(),
		log:          logger,
		spa:          spa,
	}
	s.api = s.routes()

	if err := s.bootstrap(); err != nil {
		logger.Fatalf("bootstrap: %v", err)
	}
	if cfg.AdminDomain == "" {
		logger.Print("WARNING: HOSTR_ADMIN_DOMAIN is unset — the control panel answers on any hostname that is not bound to a site. Set it in production.")
	} else {
		logger.Printf("control panel on https://%s", cfg.AdminDomain)
	}
	if !cfg.SecureCookies {
		logger.Print("WARNING: HOSTR_SECURE_COOKIES=false — session cookies will be sent over plain HTTP. Local development only.")
	}

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: s,
		// No Read/WriteTimeout: a 100 MB upload or download on a slow link is
		// legitimate. The header timeout plus MaxBytesReader bound the abuse.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Printf("shutdown: %v", err)
		}
		close(idle)
	}()

	logger.Printf("listening on %s, data in %s", cfg.Addr, cfg.Data)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("listen: %v", err)
	}
	<-idle
}

// ServeHTTP routes by Host: the control panel on its own domain, every other
// domain to whichever tenant site claims it. A request can only ever reach one
// tenant's files, and the panel is never reachable from a site's origin.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)

	if s.cfg.AdminDomain != "" && host == s.cfg.AdminDomain {
		s.serveAdmin(w, r)
		return
	}
	if site := s.db.SiteByDomain(host); site != nil {
		s.serveSite(w, r, site)
		return
	}
	if s.cfg.AdminDomain == "" {
		s.serveAdmin(w, r)
		return
	}
	http.Error(w, "404 no site is bound to "+host, http.StatusNotFound)
}

func (s *Server) serveAdmin(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	s.api.ServeHTTP(w, r)
}

// serveSPA hands out the embedded SvelteKit build, falling back to index.html
// so client-side routes survive a refresh or a deep link.
func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	f, err := s.spa.Open(name)
	if err != nil {
		switch {
		case name == "index.html":
			// Nothing was built into web/ — say so instead of 404ing.
			name = "placeholder.html"
		case path.Ext(name) != "":
			http.NotFound(w, r) // a missing asset stays a missing asset
			return
		default:
			name = "index.html" // client-side route
		}
		if f, err = s.spa.Open(name); err != nil {
			if name == "index.html" {
				name = "placeholder.html"
				f, err = s.spa.Open(name)
			}
			if err != nil {
				http.Error(w, "control panel not built; run `make frontend`", http.StatusServiceUnavailable)
				return
			}
		}
	}
	defer f.Close()

	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "500 asset error", http.StatusInternalServerError)
		return
	}
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		// Vite fingerprints asset filenames, so they are safe to pin.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, name, time.Time{}, rs)
}

// bootstrap creates the first admin account. A fresh server with no users is
// unusable otherwise, and shipping a fixed default password would be worse.
func (s *Server) bootstrap() error {
	if s.db.UserCount() > 0 {
		return nil
	}
	name := env("HOSTR_ADMIN_USER", "admin")
	pw, generated := os.Getenv("HOSTR_ADMIN_PASSWORD"), false
	if pw == "" {
		pw, generated = randHex(12), true
	}
	if _, err := s.db.CreateUser(name, pw, true); err != nil {
		return err
	}
	if generated {
		s.log.Printf("created admin user %q with generated password: %s", name, pw)
		s.log.Print("change it after first login; it is not stored anywhere in recoverable form")
	} else {
		s.log.Printf("created admin user %q from HOSTR_ADMIN_PASSWORD", name)
	}
	return nil
}

func requestHost(r *http.Request) string {
	h := r.Host
	if i := strings.LastIndex(h, ":"); i != -1 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return normDomain(strings.Trim(h, "[]"))
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}
