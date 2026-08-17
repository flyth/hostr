package main

import (
	"bytes"
	"html"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// Markdown is rendered in the browser rather than here. The alternative is a
// markdown library in the server, and this project has no third-party Go
// dependencies worth breaking for a feature a 43 KB script does on the client —
// where the same code also renders the file browser's preview pane.

// markdownPage is the shell served in place of a `.md` file. It carries no
// document content at all: the script fetches the file from its own URL with
// `?raw` and renders it there, which is also why relative links and images in
// the document resolve exactly as they would in any other page at that path.
func markdownPage(name, domain string) []byte {
	var b bytes.Buffer
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>`)
	b.WriteString(html.EscapeString(path.Base(name)))
	if domain != "" {
		b.WriteString(" · ")
		b.WriteString(html.EscapeString(domain))
	}
	b.WriteString(`</title>
<link rel="stylesheet" href="/` + assetPrefix + `md.css">
</head>
<body>
<main class="md" id="md" hidden></main>
<noscript>
<div class="md"><p class="md-error">This page renders markdown in the browser.
Enable JavaScript, or read <a href="?raw">the source</a>.</p></div>
</noscript>
<script src="/` + assetPrefix + `vendor/marked.umd.js"></script>
<script src="/` + assetPrefix + `md.js"></script>
</body>
</html>
`)
	return b.Bytes()
}

// isMarkdown reports whether a manifest name should be rendered as a document.
func isMarkdown(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown":
		return true
	}
	return false
}

// wantsDocument distinguishes a browser opening a page from a script fetching a
// file. `curl site/readme.md` should still get markdown rather than a shell
// full of JavaScript it cannot run, and only a browser navigation asks for
// text/html by name — `*/*` deliberately does not count.
//
// Each entry is matched whole rather than by substring: `text/html;q=0` is a
// client saying it will not take HTML, and a type called `notext/htmlish` is
// not text/html at all.
func wantsDocument(r *http.Request) bool {
	for _, entry := range strings.Split(r.Header.Get("Accept"), ",") {
		media, params, _ := strings.Cut(entry, ";")
		if !strings.EqualFold(strings.TrimSpace(media), "text/html") {
			continue
		}
		if quality(params) == 0 {
			return false // named it only to refuse it
		}
		return true
	}
	return false
}

// quality reads the q parameter of one Accept entry. Anything unparseable is
// treated as the default of 1: a malformed q is not a refusal.
func quality(params string) float64 {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 1
		}
		return q
	}
	return 1
}

func (s *Server) serveMarkdownPage(w http.ResponseWriter, r *http.Request, domain, name string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(markdownPage(name, domain))
}
