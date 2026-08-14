---
name: hostr-upload
description: Publish generated artifacts — images, video, audio, HTML reports, data files, whole build directories — to a hostr site and return their public URLs. Use whenever a file you produced needs a link someone else can open, or when asked to upload, publish, host, or share an artifact. Also covers listing and deleting what was uploaded earlier.
---

# Uploading artifacts to hostr

hostr hosts static files on a domain. `hostrctl` is a single static binary that
talks to it. The whole job is: make sure the binary exists, make sure two
environment variables are set, push the file, print the URL.

## 1. Make sure `hostrctl` exists

```sh
hostrctl version
```

If that fails, download it. Releases are published per platform under stable
names, so `latest` always resolves:

```sh
# Set HOSTR_RELEASES if this project's binaries live somewhere else.
base="${HOSTR_RELEASES:-https://github.com/<owner>/<repo>/releases/latest/download}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')          # linux | darwin
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; esac

mkdir -p "$HOME/.local/bin"
curl -fsSL "$base/hostrctl_${os}_${arch}" -o "$HOME/.local/bin/hostrctl"
chmod +x "$HOME/.local/bin/hostrctl"
export PATH="$HOME/.local/bin:$PATH"
```

On Windows the asset is `hostrctl_windows_amd64.exe` (or `_arm64.exe`).

**If the download 404s**, no release has been cut yet. Build from a checkout of
the hostr repository instead — it needs nothing but Go:

```sh
go build -o "$HOME/.local/bin/hostrctl" ./cmd/hostrctl
```

Do not report failure until both the download and the source build have been
tried.

## 2. Credentials

```sh
export HOSTR_SERVER=https://admin.example.com   # the control panel
export HOSTR_TOKEN=hst_...                      # an API token
```

Both come from whoever runs the server; the token is minted in the control
panel or with `hostrctl token`. If they are already exported, or a previous
`hostrctl login` stored them, nothing more is needed — check with:

```sh
hostrctl whoami
hostrctl sites
```

If `hostrctl sites` lists exactly one site, use it and do not ask which.

## 3. Push a file

`push` uploads and deletes nothing else. This is the right command for
artifacts.

```sh
hostrctl push -site playground -scope my-website render.png
# https://playground.example.com/my-website/render.png
```

- `-scope DIR` puts the file in a directory. Use one scope per project so
  separate pieces of work never collide.
- `-as PATH` renames it on the site: `-as reports/2026-08-14/index.html`.
- Several files at once: `hostrctl push -site playground -scope demo a.png b.png`.
- `-q` prints only the URLs, which is what you want when piping.

The command prints one URL per file. **Report those URLs back verbatim** — they
are the deliverable.

Only content that actually changed is uploaded, so re-pushing an identical file
costs one request and no bytes.

## 4. Push a whole directory

For a built site or a folder of assets, sync the directory into a scope:

```sh
hostrctl deploy -site playground -scope my-website ./out
```

This makes `/my-website/` match `./out` exactly: files missing from `./out` are
deleted from that directory. **Everything outside the scope is untouched.**

Without `-scope` it replaces the entire site. Do not run an unscoped deploy
unless the whole site is yours to replace — check `hostrctl ls -site NAME`
first. Sites configured as scope-only reject unscoped deploys outright, and a
scoped token cannot perform one at all.

Preview before committing to it:

```sh
hostrctl deploy -site playground -scope my-website -dry-run ./out
```

## 5. Look at what is there

```sh
hostrctl ls -site playground                # top level
hostrctl ls -site playground my-website     # inside a scope
hostrctl status -site playground            # version, file count, size
```

## 6. Delete

Deletion is immediate and unrecoverable — the underlying content is garbage
collected as soon as the manifest is swapped.

```sh
hostrctl rm -site playground my-website/old.png
hostrctl rm -site playground -r my-website/drafts      # a whole directory
```

Without `-r`, a directory path is an error rather than a silent no-op. Ask
before deleting anything you did not upload in this session.

## 7. Making the uploads browsable

A scope with no `index.html` returns 404 by default. Turn on the built-in file
browser — two panes, keyboard navigation, inline preview of images, video and
audio — for the site:

```sh
hostrctl settings -site playground -listing=true
```

This makes every path on the site discoverable. Do not enable it on a site that
holds anything whose only protection is an unguessable URL.

Put a username and password in front of the whole site, listings included:

```sh
hostrctl settings -site playground -auth-user guest -auth-password 'something long'
hostrctl settings -site playground -no-auth        # remove it again
```

Basic auth credentials travel in the clear over plain HTTP. Only use them on a
site served over HTTPS.

`hostrctl settings -site playground` with no other flags prints the current
state.

## 8. Tokens for another agent

```sh
hostrctl token -name renderer -site playground -scope my-website
```

The token is printed once. It can write inside `my-website` and nowhere else:
not the rest of the site, not other sites, not the account. Prefer handing out
one of these over sharing an unrestricted token.

## Choosing where to put things

- One scope per project or per task: `-scope my-website`, `-scope report-2026-08`.
- Keep the file name meaningful; it becomes the URL.
- Group a run's output under a dated scope when the same artifact is regenerated
  often, so old versions stay reachable.

## When something fails

| Message | Cause |
| --- | --- |
| `not logged in` | `HOSTR_SERVER` or `HOSTR_TOKEN` is unset |
| `this token is not allowed to write ...` | the token is scoped elsewhere; push inside its scope |
| `this site only accepts scoped writes` | pass `-scope` |
| `this token is limited to file access within its scopes` | the operation needs a wider token; ask the site owner |
| `this token is limited to one site's files` | the token is bound to one site and cannot act on the account |
| `file exceeds the upload limit` | over the server's per-file ceiling, 100 MB by default |
| `413` from a proxy | the reverse proxy's body limit is below the server's |
