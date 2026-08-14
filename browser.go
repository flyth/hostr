package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html"
)

// The file browser lives in the frontend tree because the control panel
// imports it as an ordinary module; the server embeds the same source so a
// public listing needs no build step and no second implementation.
//
//go:embed frontend/src/lib/browser.js
var browserJS string

//go:embed frontend/src/lib/browser.css
var browserCSS string

// listingPage is the shell for a public directory listing: the shared browser
// module inlined and nothing else. Every name arrives afterwards as JSON over
// fetch rather than being interpolated into markup, so a file called
// `<script>` is inert here.
func listingPage(dir, domain string) []byte {
	start, _ := json.Marshal(dir) // json escapes <, > and & by default

	var b bytes.Buffer
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>`)
	b.WriteString(html.EscapeString(domain))
	b.WriteString(`</title>
<style>
html, body { margin: 0; height: 100%; background: #0b0d10; }
@media (prefers-color-scheme: light) { html, body { background: #fff; } }
#hostr-browser { height: 100vh; }
`)
	b.WriteString(browserCSS)
	b.WriteString(`</style>
</head>
<body>
<div id="hostr-browser"></div>
<script type="module">
`)
	b.WriteString(browserJS)
	b.WriteString(`
const encode = (p) => p.split('/').map(encodeURIComponent).join('/');
const urlFor = (dir) => '/' + (dir ? encode(dir) + '/' : '');
const fromURL = () => location.pathname.split('/').filter(Boolean).map(decodeURIComponent).join('/');

const browser = mount(document.getElementById('hostr-browser'), {
  path: `)
	b.Write(start)
	b.WriteString(`,
  list: async (dir) => {
    const res = await fetch(urlFor(dir) + '?_hostr=ls', { headers: { accept: 'application/json' } });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    return res.json();
  },
  href: (p) => '/' + encode(p),
  onNavigate: (dir) => {
    if (urlFor(dir) !== location.pathname) history.pushState(null, '', urlFor(dir));
  },
});

addEventListener('popstate', () => browser.navigate(fromURL()));
</script>
</body>
</html>
`)
	return b.Bytes()
}
