# Markdown Editor

A Markdown editor and organizer that works directly against your GitHub
repositories — including private ones. Browse the Markdown files in a repo, edit
them side by side with a live preview, reorganize them into folders, push your
changes back as ordinary commits, and export any document to HTML, Word, plain
text or PDF.

Written in Go, deployable to Vercel as a single serverless function. There is no
database and no server-side state.

```
┌─ Sidebar ──────────┐┌─ Editor ─────────────┬─ Preview ──────────┐
│ Account            ││ # Welcome            │ Welcome            │
│ Repositories       ││                      │ ────────────       │
│ Branch             ││ Type on the **left** │ Type on the left,  │
│ Documents (tree)   ││ …                    │ …                  │
│ New / Push / Delete││                      │                    │
└────────────────────┘└──────────────────────┴────────────────────┘
```

## What it does

**Repositories**
- Sign in with GitHub OAuth; private repositories are included.
- Connect as many repositories as you like. Each remembers its own branch.
- Switch branches from the sidebar.

**Editing and organizing**
- A tree of every Markdown file on the branch, with folders, a filter box and
  collapsible sections.
- Create documents and folders, rename them, and drag them between folders. A
  folder move rewrites every file underneath it in a single commit.
- Delete a file, or a folder and everything in it.
- **Bookmarks** pin the documents you are working on. Use the bookmark button
  next to the document title, the `+` in the Bookmarks header, or right-click a
  file in the tree. A bookmark remembers its repository and branch, so opening
  one switches to them if you are somewhere else.
- **Reload from GitHub** re-reads the open file with the button beside its title.
  Reopening a file after refreshing the tree also picks up remote changes on its
  own. Unsaved edits are never replaced without asking first.
- Links out to GitHub: the file, its commit history, the repository, the branch
  and the containing folder. They point at GitHub Enterprise Server correctly
  when `GITHUB_API_BASE_URL` is set.
- Unsaved work is kept in your browser, so a reload does not lose a draft — even
  for a document you created but have not pushed yet.
- A dot in the sidebar marks documents with unsaved changes.

**Pushing**
- **Commit & push** writes the current document as a normal commit. If other
  documents are also unsaved, it offers to include them in the same commit.
- Writes are guarded: a single-file save sends the blob SHA the edit was based
  on, and a multi-file commit fast-forwards the branch without `--force`. If
  somebody else pushed first, your push is refused instead of overwriting them.

**Preview and export**
- The preview is rendered server-side by the same code path every export uses,
  so what you see is what you get. GitHub Flavored Markdown: tables, task lists,
  strikethrough, footnotes, definition lists and syntax-highlighted code.
- Images stored next to a document in a private repository load in the preview
  through an authenticated proxy.
- Export to **Markdown**, **HTML**, **Styled HTML** (standalone, CSS inlined),
  **Plain text**, **Word (.docx)** and **PDF** (via your browser's print dialog).

**Importing**
- Import a `.md` file from your computer with the toolbar button, or drop it onto
  the editor.

## Security

This app holds a token that can read and write your private repositories, so the
handling is deliberately conservative:

- **The GitHub access token never reaches the browser.** It is sealed with
  AES-256-GCM into an `HttpOnly`, `Secure`, `SameSite=Lax` cookie that only the
  server can open. Page scripts cannot read it, and no API response contains it.
- **No database.** Session state lives entirely in that encrypted cookie, so
  there is no token store to leak. Tampering with the cookie fails GCM
  authentication and is rejected.
- **OAuth CSRF protection.** The `state` parameter is a random nonce pinned in a
  separate sealed, single-use cookie and compared in constant time.
- **Request CSRF protection.** Every mutating request must carry an
  `X-CSRF-Token` header matching a token bound to the session. Because the
  expected value lives inside the encrypted cookie, it cannot be forged from
  another site. Cross-origin and cross-site writes are refused outright via
  `Origin` and `Sec-Fetch-Site`.
- **Untrusted Markdown is sanitized.** A Markdown file in a repository is
  attacker-controlled input. Rendered HTML goes through a strict allow-list
  sanitizer (bluemonday) before it reaches the page, so `<script>`, event
  handlers, `javascript:` URLs, `<iframe>`, `<form>` and inline styles cannot
  survive into the preview.
- **Strict CSP.** `default-src 'self'` with no `unsafe-inline` and no
  `unsafe-eval`; there is no inline script or inline style anywhere in the page.
- **The image proxy only serves images.** It refuses any other content type and
  always sends a fixed `Content-Type` with `nosniff`, so it cannot be turned into
  a way to host active content on your own origin.
- **Path traversal is blocked.** Repository paths are normalized and rejected if
  they escape the repo or touch `.git`.
- **Post-login redirects are same-site only.**

Scope requested: `repo read:user`. `repo` is the narrowest GitHub scope that can
both read and push to private repositories. For public repositories only, set
`GITHUB_OAUTH_SCOPES=public_repo read:user`.

## Deploying to Vercel

**1. Create a GitHub OAuth app** — GitHub → Settings → Developer settings →
OAuth Apps → New OAuth App.

- Homepage URL: `https://your-app.vercel.app`
- Authorization callback URL: `https://your-app.vercel.app/api/auth/callback`

Note the client ID and generate a client secret.

**2. Generate a session secret**

```sh
openssl rand -base64 48
```

**3. Deploy**

```sh
npm i -g vercel
vercel                      # first deploy, creates the project
vercel env add GITHUB_CLIENT_ID
vercel env add GITHUB_CLIENT_SECRET
vercel env add SESSION_SECRET
vercel --prod
```

Or push the repository to GitHub and import it at
[vercel.com/new](https://vercel.com/new), then add the same three environment
variables in the project settings. No build configuration is needed — Vercel
detects the Go function in `api/` and serves `public/` statically.

If you use a custom domain, also set `APP_BASE_URL` (for example
`https://md.example.com`) so OAuth redirect URLs are pinned to it rather than
derived from the request. Remember to update the callback URL in the OAuth app.

Until the environment variables are set, the app still deploys and the sign-in
page explains exactly what is missing.

### Check the build before you deploy

`vercel build` runs the real builder locally and catches deployment problems
without a round trip:

```sh
vercel build
```

A successful run writes `.vercel/output/functions/api/index.func` — if that
directory is missing, the API would not have been deployed.

### Why `vercel.json` uses `builds` instead of `functions`

The obvious configuration is a `functions` block naming `api/index.go`, and it
relies on Vercel auto-detecting the Go file in `api/` as a Serverless Function.
That detection does not fire reliably for Go on the Vercel build environment, and
when it doesn't the deploy fails before compiling anything:

```
Error: The pattern "api/index.go" defined in `functions` doesn't match any
Serverless Functions inside the `api` directory.
```

So the build is declared explicitly instead: `builds` names the entrypoint and
the `@vercel/go` builder outright, which removes auto-detection from the picture.
That choice has consequences worth knowing:

- `builds` and `functions` are mutually exclusive, so per-function `memory` and
  `maxDuration` overrides are not available; Vercel's defaults apply.
- `builds` also replaces `rewrites`/`headers` with `routes`, which is why the
  security headers and the `/api/*` mapping are expressed as routes.
- With `@vercel/static`, the contents of `public/` are served from `/public/…`
  internally, so the final route rewrites `/<path>` to `/public/<path>`.

If you change `vercel.json`, run `vercel build` and confirm that
`.vercel/output/functions/api/index.go.func` exists and that
`.vercel/output/config.json` still maps `/api/(.*)` to `/api/index.go` before
the catch-all static route.

## Running locally

```sh
cp .env.example .env      # fill in the three required values
go run ./cmd/server       # http://127.0.0.1:3000
```

`cmd/server` uses the same handler Vercel does and additionally serves
`public/`. Cookies drop the `Secure` flag on localhost so plain HTTP works.

For the OAuth round trip to work locally, register a second OAuth app (or add a
second callback URL) pointing at `http://localhost:3000/api/auth/callback`, and
run with `APP_BASE_URL=http://localhost:3000`.

Environment variables are read from the process environment, not from `.env`
directly — export them or use a loader:

```sh
set -a && . ./.env && set +a && go run ./cmd/server
```

### Tests

```sh
go test ./...
```

The suite covers session sealing and tamper rejection, CSRF and cross-origin
enforcement on every mutating route, Markdown sanitization against a list of
injection attempts, path-traversal rejection, the GitHub client's request and
commit payloads (including the explicit `"sha": null` that deletes a tree entry),
and every export format — the `.docx` writer is checked for a well-formed OOXML
package.

## Configuration

| Variable | Required | Purpose |
|---|:---:|---|
| `GITHUB_CLIENT_ID` | yes | OAuth app client ID |
| `GITHUB_CLIENT_SECRET` | yes | OAuth app client secret |
| `SESSION_SECRET` | yes | Encrypts the session cookie; 32+ characters |
| `APP_BASE_URL` | no | Pins the public origin used for OAuth redirects |
| `GITHUB_OAUTH_SCOPES` | no | Defaults to `repo read:user` |
| `GITHUB_API_BASE_URL` | no | Point at GitHub Enterprise Server |

Rotating `SESSION_SECRET` invalidates every session, which is how you force all
users to sign in again.

## Layout

```
api/index.go              Vercel entrypoint; delegates to pkg/app
cmd/server/main.go        Local dev server (same handler + static files)
pkg/app/                  Router, handlers, path validation
pkg/auth/                 OAuth flow, encrypted cookie sessions, CSRF
pkg/config/               Environment configuration
pkg/github/               GitHub REST client (repos, trees, blobs, commits)
pkg/markdown/             goldmark rendering + HTML sanitization
pkg/export/               Markdown, HTML, styled HTML, text and .docx output
pkg/httpx/                JSON helpers, error mapping, security headers
public/                   Static frontend (no framework, no build step)
vercel.json               Explicit Go build, routes, security headers
```

These packages live under `pkg/` rather than `internal/` on purpose. The Vercel
Go builder compiles the entrypoint inside a generated module named `handler`, so
`handler/api` importing `yourmodule/internal/...` trips Go's `internal`
visibility rule and the build fails with:

```
imports handler/api
  index.go: use of internal package .../internal/app not allowed
```

If you move these packages back under `internal/`, deployment will break.

## API

All routes require a session except `/api/health`, `/api/config` and
`/api/me`. Mutating routes additionally require the `X-CSRF-Token` header.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/health`, `/api/config` | Liveness and setup state |
| `GET` | `/api/me` | Current user and CSRF token |
| `GET` | `/api/auth/login`, `/api/auth/callback` | OAuth flow |
| `POST` | `/api/auth/logout` | Clear the session |
| `GET` | `/api/repos` | List accessible repositories |
| `GET` | `/api/repos/{owner}/{repo}` | One repository |
| `GET` | `/api/repos/{owner}/{repo}/branches` | Branches |
| `GET` | `/api/repos/{owner}/{repo}/tree` | Markdown tree for a ref |
| `GET` | `/api/repos/{owner}/{repo}/file` | Read a file and its SHA |
| `PUT` | `/api/repos/{owner}/{repo}/file` | Create or update one file |
| `POST` | `/api/repos/{owner}/{repo}/commit` | Multi-file commit |
| `POST` | `/api/repos/{owner}/{repo}/move` | Move or rename a file or folder |
| `POST` | `/api/repos/{owner}/{repo}/delete` | Delete a file or folder |
| `GET` | `/api/repos/{owner}/{repo}/raw` | Image proxy for previews |
| `POST` | `/api/render` | Markdown → sanitized HTML |
| `POST` | `/api/export` | Markdown → downloadable file |
| `GET` | `/api/formats` | Available export formats |

## Limits

- Files are capped at 2 MiB.
- Folder moves and folder deletes read the whole tree, so they are refused on
  repositories large enough for GitHub to truncate the tree listing.
- The repository picker fetches the first 200 repositories; the search box
  filters that list.
- PDF export goes through the browser's print dialog rather than being rendered
  server-side, which keeps the function small and cold starts fast.
- Vercel functions are stateless, so every request that touches GitHub calls the
  GitHub API. Heavy use counts against your GitHub rate limit.
