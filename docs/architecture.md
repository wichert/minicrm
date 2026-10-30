# MiniCRM System Architecture

## 1. Overview

**MiniCRM** is a single-binary Go 1.27 application that turns one or more shared IMAP/SMTP mailboxes (accounts) into a collaborative, stateful ticket/thread workflow. Every incoming email is linked to a conversation thread, assigned to an operator, and tracked through a strict lifecycle (`new -> in_progress -> completed`, or `new`/`in_progress -> spam` with explicit spam recovery).

Mail accounts are configured with numbered `MINICRM_ACCOUNT_<N>_*` environment variables. Each account is ingested, threaded, and replied from independently: an email delivered to several accounts produces one thread per account. Access is constrained by OIDC group claims (`MINICRM_ACCOUNT_<N>_OIDC_GROUP`), so users only see threads, email bodies, and attachments for accounts they may access.

```text
                        +-----------------------------------+
                        |   Existing OAuth2 / OIDC Server   |
                        |      (or local cmd/dev-oidc)      |
                        +-----------------+-----------------+
                                          | (OIDC Auth Code + PKCE)
                                          v
+---------------+       +---------------------------------------------------+
|  Browser UI   | <---> |               MiniCRM Go Binary                   |
| (SSR + htmx)  | HTTPS |                                                   |
+---------------+       |  +---------------------------------------------+  |
                        |  | HTTP Server (chi + html/template + SCS)     |  |
                        |  +------+---------------+---------------+------+  |
                        |         |               |               |         |
                        |         v               v               v         |
                        |  +------------+  +-------------+  +------------+  |
                        |  | Workflow & |  | Assignment  |  | Threading  |  |
                        |  | Merge Svc  |  | Engine      |  | Matcher    |  |
                        |  +------+-----+  +------+------+  +-----+------+  |
                        |         |               |               |         |
                        |         +---------------+---------------+         |
                        |                         |                         |
                        |                         v                         |
                        |  +---------------------------------------------+  |
                        |  | Repository Layer (pgx/v5 + QueryTracer)     |  |
                        |  +------+---------------+---------------+------+  |
                        |         |               |               |         |
                        |         v               v               v         |
                        |  +------------+  +-------------+  +------------+  |
                        |  | IMAP Sync  |  | IMAP Action |  | SMTP +     |  |
                        |  | Worker     |  | Worker      |  | Sent Worker|  |
                        |  | (INBOX)    |  | (Junk/Arch) |  | (Outbox)   |  |
                        |  +------+-----+  +------+------+  +-----+------+  |
                        +---------|---------------|---------------|---------+
                                  |               |               |
                     IMAP (TLS)   |               |  IMAP MOVE    | SMTP + IMAP APPEND
                                  v               v               v
                        +-----------------------------------+     |
                        |      Mail Server (IMAP/SMTP)      | <---+
                        | [INBOX]  [Archive]  [Junk] [Sent] |
                        +-----------------------------------+

                        +-----------------------------------+
                        | PostgreSQL 16+  | Local Disk      |
                        | (Metadata/Body) | (Attachments)   |
                        +-----------------------------------+
```

---

## 2. Detailed Documentation Map

| Document | Topics Covered |
|---|---|
| **[`docs/authentication.md`](./authentication.md)** | OAuth2 / OIDC configuration, PKCE authorization flow, pre-created user resolution, and the standalone dummy IDM (`cmd/dev-oidc`). |
| **[`docs/imap.md`](./imap.md)** | Mail server requirements (IMAP & SMTP), `INBOX` UID synchronization, asynchronous `Archive` / `Junk` / `Sent` folder jobs, and safe deletion from `Archive`. |
| **[`docs/storage.md`](./storage.md)** | PostgreSQL schema reference, UTC `TIMESTAMPTZ` + `display_timezone`, immutable audit trails (`audit_logs` & `thread_events`), `pgx.QueryTracer` structured logging, and content-addressed local disk attachment storage. |
| **[`docs/workflow-and-threading.md`](./workflow-and-threading.md)** | Reply linking (`In-Reply-To`/`References` + subject/sender fallback), completed-thread replies & **Reopen & Merge**, workflow state machine, and pluggable assignment engine. |
| **[`docs/security.md`](./security.md)** | Defense-in-depth untrusted HTML email rendering (`bluemonday` allowlist, remote image blocking, `<iframe sandbox>`), HTTP security headers (`Content-Security-Policy`, `Permissions-Policy`, COOP/COEP/CORP, HSTS), and CSRF protection. |
| **[`docs/testing.md`](./testing.md)** | Unit, integration, and end-to-end (`e2e/`) test suites, local hooks (`prek` + `golangci-lint`), and GitHub Actions CI workflows. |

---

## 3. Technology Stack

| Layer | Package / Tool | Purpose |
|---|---|---|
| **Runtime** | Go 1.27 | Single static binary containing web server, embedded assets/migrations, and background workers. |
| **HTTP Router** | `github.com/go-chi/chi/v5` | Routing, security headers middleware, CSRF protection (`http.CrossOriginProtection`). |
| **Database** | PostgreSQL 16+ (`github.com/jackc/pgx/v5`) | Relational storage, transactional outboxes (`imap_jobs`, `outbox_emails`), and query tracing (`pgx.QueryTracer`). |
| **Migrations** | `github.com/pressly/goose/v3` | Embedded SQL schema migrations executed automatically on startup. |
| **IMAP** | `github.com/emersion/go-imap/v2` | Per-account `INBOX` synchronization, `UID MOVE` to `Archive` and `Junk`, and `APPEND` to `Sent`. |
| **MIME & HTML** | `github.com/emersion/go-message` + `github.com/microcosm-cc/bluemonday` | RFC 5322 multipart parsing and strict allowlist HTML sanitization. |
| **SMTP** | `github.com/wneessen/go-mail` | Outbound SMTP delivery with basic `username + password` authentication and RFC 5322 serialization. |
| **Authentication** | `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` + `github.com/alexedwards/scs/v2` | OIDC Authorization Code flow with PKCE and PostgreSQL-backed HTTP-only sessions. |
| **Health** | `net/http` (`/healthz`) | Unauthenticated liveness/readiness probe; `cmd/minicrm` plugs in `pgxpool.Pool.Ping` via `web.Server.WithHealthCheck` |
| **Logging** | `log/slog` | Structured JSON logging of all application events, data reads (`data_access`), and mutations (`data_change`). |
| **Frontend** | `html/template` + htmx 2.0.4 (vendored) + `esbuild`-bundled ES modules | Server-rendered HTML, hypermedia partial swaps, and progressive-enhancement JavaScript. See [§5](#5-frontend-rendering-and-hypermedia). |

---

## 4. Package Structure

```text
cmd/
  minicrm/main.go               # Main CRM binary entrypoint (DB pool, migrations, workers, HTTP server)
  dev-oidc/main.go              # Standalone dummy OIDC identity server for local dev & CI E2E
e2e/
  auth_e2e_test.go              # End-to-end authentication tests against MiniCRM + dev-oidc
internal/
  config/                       # Environment variable configuration & validation
  db/
    migrations/                 # Embedded Goose SQL migrations (001_initial_schema.sql)
    queries/                    # SQL query definitions
    sqlc/                       # Typed database models, queries, and pgx StructuredQueryTracer
  audit/                        # Audit trail service (audit_logs, thread_events, slog data_access/data_change)
  auth/                         # OIDC provider (coreos/go-oidc + oauth2 PKCE), SCS sessions, user pre-creation
    devidm/                     # Reusable dummy OIDC + PKCE identity provider (RS256 JWT + JWKS)
  assignment/                   # Pluggable assignee resolution engine & DefaultAssigneeResolver
  threading/                    # Subject/Message-ID normalizer and reply-to-thread matcher
  workflow/                     # Thread state machine, Archive/Junk move queueing, Reopen & Merge
  mail/                         # MIME parser, bluemonday sanitizer, IMAP sync/action workers, SMTP outbox worker
  storage/                      # Content-addressed SHA-256 local disk attachment store
  web/                          # chi router, security headers, /healthz probe, handlers
    static/                     # Embedded assets: app.js bundle, editor.js, app.css, sw.js, vendored htmx.min.js
    views/                      # SSR views: view models, template funcs, go:embed template sets
      templates/                # .html template files (embedded via templates/*.html)
frontend/                       # Frontend sources & toolchain (not served; built into internal/web/static)
  src/                          # ES modules bundled into static/app.js (see §5)
  editor.js                     # ProseMirror reply editor entrypoint → static/editor.js
  test/                         # vitest + jsdom unit tests (draft autosave)
```

---

## 5. Frontend Rendering and Hypermedia

The UI is **server-rendered HTML with progressive enhancement**. There is no SPA, no client-side router, and no JSON API for page data: every route returns HTML, and every interactive control degrades to a plain form or link.

**Template layer (`internal/web/views/`).** View models and template funcs live in `views.go`; markup lives in `internal/web/views/templates/*.html`, embedded with `//go:embed templates/*.html`. `mustTemplates` concatenates the named files into one `html/template` set per page (deliberately not `ParseFS`, which would make each file an associated template and break the page skeleton). Pages are composed from shared partials — `layout_header.html` / `layout_footer.html`, `inbox_table.html`, `inbox_filter_form.html`, `inbox_tabs.html`, `inbox_results.html`, `settings_audit_table.html` — so the inbox and the split detail pane render the list from a single definition.

**Hypermedia layer (htmx 2.0.4).** htmx is a vendored static file (`internal/web/static/htmx.min.js`, SHA-256 pinned in `static/vendor/README.md`), not a build dependency. It owns exactly two behaviours:

1. **List region swaps.** The results region carries its own polling contract:
   ```html
   <div data-inbox-results id="inbox-results-region"
        hx-get="/threads?..." hx-trigger="every 60s, refresh from:body"
        hx-target="this" hx-swap="outerHTML" hx-push-url="false">
   ```
   The fragment response is the region plus out-of-band replacements for the results count and the status tab strip, which live in the toolbar outside the swapped element.
2. **Filter selects.** They submit with `hx-get`/`hx-include="closest form"`/`hx-push-url="true"`; the plain `<form method="get">` and its `<noscript>` submit button remain, so filtering works without JS.

Mutating actions (transition, assign, reopen & merge, reply, bulk actions) stay ordinary `POST` forms with redirect-with-flash — server-side redirects, the `data-confirm` `<dialog>` flow, and CSRF protection all keep working unchanged.

**CSP constraint.** `script-src 'self'` forbids inline script and `eval`. htmx compiles trigger *filters* (`every 60s [cond]`) with `new Function`, so triggers are unconditional and behavioural gating is done in a `htmx:beforeRequest` listener that calls `event.preventDefault()` (see `frontend/src/inbox-refresh.js`, `isPaused()`). `htmx.config.allowEval` therefore stays at its default `false`.

**JavaScript layer (`frontend/src/` → `internal/web/static/app.js`).** Written as ES modules and bundled/minified by `esbuild` (`cd frontend && npm run build`). The bundle is committed and CI fails if it is stale. Modules: `inbox-refresh.js` (pause logic, selection restore across swaps, pull-to-refresh), `draft.js` (reply draft autosave with revision/409 conflict handling), `selection.js` (bulk selection), `rows.js`, `tabs.js`, `chrome.js`, `frames.js` (sandboxed email iframe sizing), `split-pane.js`, `keys.js` (keyboard navigation), `reply.js` (reply sheet). `draft.js` is unit-tested with vitest + jsdom; see [`docs/testing.md`](./testing.md).

The reply editor is the one deliberate exception: a ProseMirror bundle (`frontend/editor.js` → `static/editor.js`) that hides a `<textarea>` and dispatches an `editorchange` event on the form, so `draft.js` and the reply form stay plain HTML controls.
