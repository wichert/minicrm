# MiniCRM System Architecture

## 1. Overview

**MiniCRM** is a single-binary Go 1.27 application that turns one or more shared IMAP/SMTP mailboxes (accounts) into a collaborative, stateful ticket/thread workflow. Every incoming email is linked to a conversation thread, assigned to an operator, and tracked through a strict lifecycle (`new -> in_progress -> completed` or `new -> spam`).

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
| (SSR + HTMX)  | HTTPS |                                                   |
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
| **Logging** | `log/slog` | Structured JSON logging of all application events, data reads (`data_access`), and mutations (`data_change`). |

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
  web/                          # chi router, security headers, handlers, embedded static assets, SSR views
```
