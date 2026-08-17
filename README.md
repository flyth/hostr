# hostr

A small static-site host you can share with friends. One container serves any
number of sites, each on its own domain, each owned by a different account.
`hostrctl` syncs a build directory to a site over plain HTTPS — only the files
whose contents actually changed get uploaded, and anything you deleted locally
gets deleted on the server.

```
hostrctl deploy -site blog ./build
./build -> blog.example.com
  128 files, 3 new, 2 changed, 1 to delete
  5 blobs to upload (412.0 KB), 123 already on server
  uploading 5/5
deployed v7: 128 files, 4.1 MB live, 3 stale blobs collected
```

## Running it

CI publishes a multi-arch image (amd64 + arm64) to GHCR on every push to main
and on every `v*` tag:

```sh
docker pull ghcr.io/<owner>/<repo>:latest     # or :v1.2.3, or :<full-sha>
```

Or build it yourself:

```sh
docker build -t hostr .
docker run -d --name hostr \
  -p 8080:8080 \
  -v hostr-data:/data \
  -e HOSTR_ADMIN_DOMAIN=admin.example.com \
  -e HOSTR_ADMIN_USER=you \
  -e HOSTR_ADMIN_PASSWORD='something long' \
  hostr
```

Put a TLS-terminating reverse proxy in front (Caddy, nginx, Traefik) and point
it at `:8080` for the admin domain and every site domain. `hostr` speaks plain
HTTP only — TLS, HTTP/2 and compression are the proxy's job, and it already
does them better.

With Caddy that is the entire config:

```
admin.example.com, blog.example.com, friend.example.org {
    reverse_proxy hostr:8080
}
```

Set `HOSTR_TRUST_PROXY=true` so login throttling sees real client addresses
rather than the proxy's.

| Variable | Default | Meaning |
| --- | --- | --- |
| `HOSTR_ADDR` | `:8080` | listen address |
| `HOSTR_DATA` | `/data` | blobs, manifests and the metadata file |
| `HOSTR_ADMIN_DOMAIN` | *(unset)* | domain the control panel answers on. Unset means "any hostname not bound to a site" — convenient locally, sloppy in production |
| `HOSTR_ALIAS_DOMAIN` | *(unset)* | parent domain for guest links. Unset means the feature is off |
| `HOSTR_ADMIN_USER` | `admin` | first account, created on an empty store |
| `HOSTR_ADMIN_PASSWORD` | *(generated)* | printed to the log once if unset |
| `HOSTR_MAX_UPLOAD` | `104857600` | per-file upload ceiling, bytes |
| `HOSTR_SECURE_COOKIES` | `true` | set `false` only for local HTTP |
| `HOSTR_TRUST_PROXY` | `false` | read the client address from `X-Forwarded-For` |

**One-way upgrade.** The first start on an existing store rewrites each site's
single basic-auth credential into the account list. An older binary reading that
store would find no credential at all and serve every protected site wide open,
so keep a copy of `hostr.json` if you might roll back.

## Adding people

The control panel's **Invites** tab (admin only) mints single-use codes. Hand
one over; the recipient redeems it on the sign-in screen under *Use an invite*.
No email, no SMTP, no OAuth app to register.

Each account creates its own sites. A site owner can add collaborators, who may
deploy to and read that site and nothing else.

## hostrctl

Every tagged release publishes a static binary for linux, macOS and Windows on
both amd64 and arm64, under names that do not change between releases:

```sh
curl -fsSL https://github.com/<owner>/<repo>/releases/latest/download/hostrctl_darwin_arm64 \
  -o /usr/local/bin/hostrctl && chmod +x /usr/local/bin/hostrctl
```

Or build it: `go build -o hostrctl ./cmd/hostrctl`, or copy it out of the
container.

```sh
hostrctl login    -server https://admin.example.com
hostrctl create   -slug blog -domain blog.example.com
hostrctl deploy   -site blog ./build
hostrctl deploy   -site blog ./build -dry-run
hostrctl status   -site blog
hostrctl share    -site blog -user friend
hostrctl sites
```

`login` exchanges your password for an API token and stores only the token
(`~/.config/hostrctl/config.json`, mode 0600). `HOSTR_SERVER` and `HOSTR_TOKEN`
override it, which is what you want in CI:

```yaml
- run: hostrctl deploy -site blog ./build
  env:
    HOSTR_SERVER: https://admin.example.com
    HOSTR_TOKEN: ${{ secrets.HOSTR_TOKEN }}
```

Dot-files are skipped unless you pass `-hidden`. Symlinks are never followed.

## Scopes

A scope is a directory on the site. `deploy -scope` syncs into it and leaves
everything outside it alone, which is what turns one site into a shelf of
independent projects rather than one thing you replace wholesale:

```sh
hostrctl deploy -site playground -scope my-website ./out
#   ./out -> playground.example.com/my-website/
```

Sites can refuse unscoped writes entirely:

```sh
hostrctl settings -site playground -scope-only=true
```

Every write must then name a scope. No deploy replaces the whole site, no
delete empties it, and nothing may write or remove a file at the top level.
Deleting one named scope is itself a scoped write and stays allowed.

Tokens can be limited two ways, which is the useful shape when the thing doing
the uploading is an agent rather than a person:

```sh
hostrctl token -name deploy-bot -site playground                     # one site
hostrctl token -name renderer   -site playground -scope my-website -scope drafts
```

A **site-bound** token is a full credential for that one site and nothing else
— it cannot reach another site, and it cannot reach the account: no minting
tokens, no password change, no creating sites.

A **scoped** token is narrower still. It may be reused across every scope it
was granted, and it can do nothing but move files inside them: not the rest of
the site, not the site's settings, not its collaborators. It cannot list, read,
or even confirm the existence of anything outside its scopes.

## Single files

`push` uploads without syncing anything. Nothing is deleted, and only content
the server does not already have is sent:

```sh
hostrctl push -site playground -scope my-website render.png
#   https://playground.example.com/my-website/render.png
hostrctl push -site playground -as reports/august/index.html ./report.html
```

`ls` and `rm` work on what is already there:

```sh
hostrctl ls -site playground my-website
hostrctl rm -site playground my-website/old.png
hostrctl rm -site playground -r my-website/drafts
```

Deletion is immediate: the blobs behind those paths are collected as soon as
the new manifest lands. `rm` resolves paths against the live manifest first, so
a typo is an error rather than a version bump that removed nothing.

The `skills/hostr-upload` directory holds a shareable skill that teaches an
agent this workflow, including fetching the binary if it is missing.

## Directory listing

Off by default. Switched on, any directory without an `index.html` serves a
two-pane file browser instead of a 404 — keyboard navigation, sizes, and inline
preview of images, video, audio and text:

```sh
hostrctl settings -site playground -listing=true
```

It is the same browser the control panel uses, which is why it exists once, in
`frontend/src/lib/browser.js`, embedded into the server for public listings and
imported by the panel for the version with delete controls.

Turning it on makes every path in the site enumerable. That is the entire point
of the feature and also the entire risk of it: anything that was only protected
by having an unguessable URL stops being protected.

The root can be held back from that:

```sh
hostrctl settings -site playground -no-root-listing=true
```

Every directory below it still lists, but `/` is a 404 again — so a visitor
browses from a path they were given, and nobody walks the site from the top.
This pairs with scoped tokens: each scope keeps its own browsable subtree
without the scope names themselves being on display at the root.

## Rendering markdown

Off by default. Switched on, a `.md` file opened in a browser is typeset as a
document instead of downloading:

```sh
hostrctl settings -site playground -markdown=true
```

`?raw` always returns the source as plain text, and a client that did not ask
for `text/html` — `curl`, a script, a feed reader — gets the source too, so
nothing that fetched a markdown file before starts receiving a page of
JavaScript instead.

Rendering happens in the browser. The server ships the shell and the document
arrives over `?raw`, which is what keeps relative links and images inside the
file working: the page is served at the file's own URL. GitHub-flavoured
markdown — tables, task lists, strikethrough, autolinks — comes from
[marked](https://github.com/markedjs/marked). There is no syntax highlighting;
code blocks are set in Fira Code and left alone.

With directory listing on as well, the file browser's preview pane renders
markdown the same way, from the same stylesheet.

**What turning this on changes.** A rendered document runs whatever HTML and
script the markdown contains, on the site's own origin. That is the same trust
a deployed `.html` file already carries, but it is *not* the trust a `.md` file
carried a moment earlier, and a scoped agent token that can write `docs/` can
write that markdown. The control panel is the one place this would be somebody
else's origin, so previews there render inside a sandboxed frame with scripting
off — a hostile file cannot reach the session around it.

### The `/_hostr/` namespace

The stylesheet, the three typefaces and the renderer are compiled into the
binary and served from `/_hostr/` on every site, matched before the site's own
files. Nothing you deploy under that path will ever be served — the name is
reserved on every site, listing or no listing, markdown or no markdown.

The typefaces are [Newsreader](https://github.com/productiontype/Newsreader) for
text, [Archivo](https://github.com/Omnibus-Type/Archivo) for headings and
[Fira Code](https://github.com/tonsky/FiraCode) for code, all under the SIL Open
Font License, latin subsets only, and each one's licence is served alongside it
at `/_hostr/fonts/`.

## Password protection

A site can sit behind a basic auth prompt, pages and listings alike. Give each
person their own account and you can see who has actually been reading:

```sh
hostrctl settings -site playground -auth-user guest -auth-password 'something long'
hostrctl settings -site playground -no-auth
```

The CLI speaks the one-credential shorthand, which replaces whatever the site
has. Once a site has more than one account, `-auth-user` is refused rather than
silently deleting other people's logins — add and remove accounts individually
in the panel. `-no-auth` is *not* refused: it says remove the protection, and it
removes all of it, however many accounts that is.

Each account carries a **label**, and the label is the only part a client ever
sees. The username stays on the server: it is half of a basic-auth credential,
and a site record is readable by every collaborator, not just its owner.

Passwords are stored as argon2id like an account password. Verifying one costs
64 MiB, so a successful check is memoised for an hour rather than repeated for
every image on a page, and verifications are serialised so one page load cannot
start forty of them at once. Adding an account, removing one or changing any
password invalidates the whole site's memo immediately. A login attempt runs
exactly one hash whether or not the username exists, so which accounts a site
has cannot be read off response timing.

Only *failed* attempts are throttled, per site and per client address, and a
credential that has already been verified skips the throttle entirely — behind
a reverse proxy every visitor shares one address, and someone else's guessing
must not sign them out. Success does not clear the counter: on a site with
several accounts, whoever holds one valid password could otherwise use it to
reset the budget between guesses at another.

Basic auth sends the password on every request, base64-encoded and not
encrypted. Only put it in front of a site that is served over HTTPS.

## Guest links

A guest link is a throwaway hostname serving the same site. Hand one to each
person and you can tell their visits apart, and revoke one without touching the
others.

```
3f9c1a2b7e4d.guest.example.com  ->  the same site as playground.example.com
```

Set `HOSTR_ALIAS_DOMAIN=guest.example.com` and point a wildcard at the server:

```
*.guest.example.com {
    reverse_proxy hostr:8080
}
```

The name is generated, never chosen — 48 random bits under that parent. Nothing
else may claim a name in there: a site created on `<something>.guest.example.com`
would serve a tenant's own content, and their own password prompt, under a
hostname people had been told to trust and a certificate that matches.

A guest link grants nothing the main domain does not. A site behind a password
is behind the same password here, and the prompt, the listing pages and the
markdown document titles all name the guest hostname rather than the site's real
one. What the server writes does not give away where the link points — but the
site's own pages can, and a canonical URL in your `404.html` or a `<link
rel=canonical>` in your HTML will still say so. That part is yours.

## Traffic

Every request a site serves is counted against the hostname it arrived on —
the site's own domain, or one guest link — and against the password account that
opened it. Each carries a hit count, the bytes actually sent, and a first and
last access. A site's traffic across every hostname is its own plus its guest
links': an addition, so revoking or resetting one guest link takes its traffic
out of the total by itself and leaves nothing to reattribute.

Anything refused is not counted — a 401 carries nothing worth attributing, and
counting it would let a stranger keep the server writing. Bytes are always
tallied, but the requests a visit makes on its own behalf do not each count as a
visit: the shared assets under `/_hostr/`, the file browser's own listing feed,
and the redirect that exists only to add a trailing slash. A 404 does count —
somebody asked this hostname for something.

Counters move in memory and are written out every ten seconds, and again on a
clean shutdown; a `kill -9` costs at most the last few seconds of counting. The
numbers a client reads always come from memory, so they are never stale. Each
count is one mutex acquisition and a few adds on a request that was about to
read a file off disk anyway.

## How a deploy works

1. `hostrctl` walks the directory and computes a SHA-256 per file.
2. It asks the server which of those hashes it does not already have
   (`POST /api/sites/{id}/check`).
3. It uploads only the missing content
   (`PUT /api/sites/{id}/blobs/{hash}`). The server re-hashes every byte it
   receives and rejects anything that does not match the declared hash, so a
   client cannot claim a blob it did not upload.
4. It posts the complete path → hash manifest
   (`POST /api/sites/{id}/deploy`). The swap is a single atomic rename.
   Files absent from the new manifest stop being served immediately, and their
   blobs are garbage collected.

Within a scope the manifest is the whole truth, which is why deletions need no
separate call: a deploy replaces everything under its scope, and an unscoped
deploy replaces the site. Re-deploying an unchanged directory uploads nothing.

Single-file uploads and deletes go through `PATCH /api/sites/{id}/files`
instead, which sets and removes exactly what it is given. Both paths
read-modify-write the manifest under the same per-site lock, so two writers
never merge onto a stale base, and both garbage collect only after the new
manifest is safely in place.

## Security model

- Blobs are namespaced **per site**, not globally deduplicated. A shared store
  would let any tenant test whether a given file exists anywhere on the server
  by uploading it and watching for a dedupe hit.
- Every per-site route runs through one access gate, and a caller without
  access gets `404` rather than `403` — site IDs cannot be enumerated.
- Sites are served on their own hostnames, so the session cookie (scoped to the
  admin domain, `HttpOnly`, `SameSite=Lax`) is never sent to tenant content, and
  the API is unreachable from a site's origin.
- Passwords: argon2id, 64 MiB, per-user salt. API tokens are stored as SHA-256
  and shown exactly once. Changing a password invalidates every session.
- Manifest paths are validated against traversal on write *and* the serving
  layer can only reach blobs listed in that site's manifest, so a hostile path
  has nothing to reach even if validation were bypassed.
- Domains are globally unique, and guest links share that one namespace; a
  tenant cannot claim a name already bound to someone else's site or guest link,
  and a site cannot rename itself onto one.
- A guest link is a handle, not a credential. It changes nothing about who may
  read the site — only which visits can be told apart — so a leaked one is worth
  exactly as much as the site's real domain.
- The guest-link namespace is reserved: no site may be created or renamed under
  `HOSTR_ALIAS_DOMAIN`, so a revoked link's name cannot be picked up by another
  tenant and served to whoever still has the bookmark.
- A token bound to one site is a full administrator *of that site*: it can add
  and remove that site's password accounts and guest links, as it could always
  clear its password. Give an agent a scoped token if it should only move files.
- Cookie-authenticated state changes require a same-origin `Origin` header.
  Token-authenticated calls skip that check — there is no ambient credential to
  forge.
- Limited tokens are gated by a whitelist rather than a blacklist, at two
  levels: a site-bound token cannot reach the account endpoints, and a scoped
  token cannot reach anything but file content within its scopes. The endpoints
  a limited token must not reach include the one that mints tokens, and a
  credential able to issue itself an unrestricted replacement is not limited at
  all.
- Blob existence is not an oracle. `POST /check` answers a scoped token only
  about content its own scopes already reference, so it cannot confirm that a
  guessed document exists elsewhere in the site — at worst it re-uploads bytes
  the server happens to have.
- File sizes in a manifest come from the blob on disk, never from the client
  that declared them.
- The panel previews a site's files through the API rather than from the site's
  own domain, so previews survive basic auth. Only images, video, audio and
  plain text are served inline there; everything else downloads, and every
  response carries `nosniff` and a sandbox CSP, because rendering a tenant's
  HTML on the admin origin would put it next to the session cookie.
- Site passwords are argon2id, verified once and then memoised for an hour keyed
  on a digest of every credential the site holds, so adding, removing or
  changing any account takes effect immediately.
- Admins are the server operator, who already has filesystem access to every
  blob, so they can see and delete any site. That is stated rather than
  pretended away.

## Development

```sh
make          # frontend + both binaries into bin/
make test
make run      # plain HTTP on :8080, data in ./data, panel on any hostname
```

For frontend work, run `make run` in one terminal and `npm run dev` in
`frontend/` in another — Vite proxies `/api` to the Go process.

`go build` works without Node: `web/` ships a placeholder page that explains
how to build the real console, and only that page is tracked in git.
