# MiniCRM Architecture & Technical Design

## 1. System Overview

**MiniCRM** is a single-binary Go 1.27 application that turns a shared IMAP/SMTP mailbox into a collaborative, stateful ticket/thread workflow. Every incoming email is linked to a conversation thread, assigned to an operator, and tracked through a strict lifecycle (`new -> in_progress -> completed` or `new -> spam`).

```text
                        +-----------------------------------+
                        |   Existing OAuth2 / OIDC Server   |
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

## 2. Technology Stack

| Layer | Package / Tool | Purpose |
|---|---|---|
| **Runtime** | Go 1.27 | Single static binary containing web server, embedded assets/migrations, and background workers. |
| **HTTP Router** | `github.com/go-chi/chi/v5` | Routing, security headers middleware, CSRF protection (`http.CrossOriginProtection`). |
| **Database** | PostgreSQL 16+ (`github.com/jackc/pgx/v5`) | Relational storage, transactional outboxes (`imap_jobs`, `outbox_emails`), and query tracing (`pgx.QueryTracer`). |
| **Migrations** | `github.com/pressly/goose/v3` | Embedded SQL schema migrations executed automatically on startup. |
| **IMAP** | `github.com/emersion/go-imap/v2` | `INBOX` synchronization, `UID MOVE` to `Archive` and `Junk`, and `APPEND` to `Sent`. |
| **MIME & HTML** | `github.com/emersion/go-message` + `github.com/microcosm-cc/bluemonday` | RFC 5322 multipart parsing and strict allowlist HTML sanitization. |
| **SMTP** | `github.com/wneessen/go-mail` | Outbound SMTP delivery with basic `username + password` authentication and RFC 5322 serialization. |
| **Authentication** | `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` + `github.com/alexedwards/scs/v2` | OIDC Authorization Code flow with PKCE and PostgreSQL-backed HTTP-only sessions. |
| **Logging** | `log/slog` | Structured JSON logging of all application events, data reads (`data_access`), and mutations (`data_change`). |

---

## 3. Data Model, Storage & Audit Trail

### PostgreSQL Schema (`internal/db/migrations/001_initial_schema.sql`)

All timestamps are stored in UTC (`TIMESTAMPTZ`). Mutable tables (`users`, `system_settings`, `threads`, `imap_sync_state`) intentionally omit `updated_at` columns; every creation, login, configuration update, and workflow transition is recorded as an immutable audit event in `audit_logs` or `thread_events`.

- **`users`**: CRM operators and assignees (`id`, `oidc_sub`, `email`, `display_name`, `role`, `is_active`). `oidc_sub` is nullable so admins can pre-create users by email before their first OIDC login.
- **`system_settings`**: Singleton configuration row (`id = 1`) holding:
  - `default_assignee_id`
  - `display_timezone` (IANA timezone name, e.g., `Europe/Amsterdam`, used to render UTC timestamps in the UI)
  - `imap_inbox_mailbox` (`INBOX`), `imap_archive_mailbox` (`Archive`), `imap_junk_mailbox` (`Junk`), `imap_sent_mailbox` (`Sent`)
  - `subject_match_window_days` (default `30`)
- **`threads`**: Conversation threads (`id`, `subject_normalized`, `subject_display`, `status`, `assignee_id`, `assignment_source`, `customer_email`, `customer_name`, `reopens_thread_id`, `last_message_at`).
- **`emails`**: Individual inbound and outbound messages (`id`, `thread_id`, `parent_email_id`, `direction`, `message_id`, `in_reply_to`, `references_ids`, `imap_mailbox`, `imap_uidvalidity`, `imap_uid`, `from_address`, `to_addresses`, `cc_addresses`, `subject`, `body_text`, `body_html_sanitized`, `raw_headers`, `sent_at`, `received_at`).
- **`attachments`**: Attachment metadata (`id`, `email_id`, `filename`, `content_type`, `size_bytes`, `sha256_hex`, `storage_relpath`, `content_id`, `is_inline`).
- **`imap_sync_state`**: Tracks `(mailbox, uid_validity, last_synced_uid)` for `INBOX`.
- **`imap_jobs`**: Transactional queue (`FOR UPDATE SKIP LOCKED`) for asynchronous IMAP operations (`move_to_archive`, `move_to_junk`, `append_to_sent`).
- **`outbox_emails`**: Transactional queue (`FOR UPDATE SKIP LOCKED`) for outbound SMTP delivery.
- **`thread_events`**: Immutable thread audit trail (`created`, `status_changed`, `assigned`, `reassigned`, `reply_received`, `reply_sent`, `reopen_merged`).
- **`audit_logs`**: Immutable system and user audit trail (`user_created`, `user_precreated`, `user_updated`, `user_login`, `user_login_failed`, `user_logout`, `settings_updated`).
- **`sessions`**: PostgreSQL session store for `scs`.

### Local Disk Attachment Storage (`internal/storage/attachments.go`)
- Binary attachment payloads are stored on local disk under `MINICRM_ATTACHMENTS_DIR` using content-addressed SHA-256 paths with two-level fanout: `<sha256[0:2]>/<sha256[2:4]>/<sha256>`.
- Writes stream to `<baseDir>/tmp/<uuid>.part`, compute SHA-256, `fsync`, and atomically `os.Rename` into place.
- Reads verify path confinement within `baseDir` to prevent path traversal.

### Structured Data Access & Change Logging (`log/slog`)
- **SQL Layer (`internal/db/sqlc/tracer.go`)**: `StructuredQueryTracer` implements `pgx.QueryTracer` and logs every SQL query execution with `event_category` (`data_access` for `SELECT`, `data_change` for `INSERT`/`UPDATE`/`DELETE`), `operation`, `rows_affected`, `duration_ms`, `actor_user_id`, `request_id`, and `client_ip`.
- **Domain/Handler Layer (`internal/audit/logger.go`)**: Emits structured `data_access` and `data_change` log records with resource type/ID alongside immutable rows in `audit_logs` and `thread_events`.

---

## 4. Core Subsystems

### 4.1 Incoming Email Ingestion & IMAP Folder Lifecycle (`internal/mail/`)

1. **`IMAPSyncWorker` (`imap_sync.go`)**:
   - Authenticates to the IMAP server using basic `username + password` over TLS/STARTTLS.
   - Selects `INBOX`, verifies `UIDVALIDITY` against `imap_sync_state`, and fetches messages with `UID > last_synced_uid` using `BODY.PEEK[]`.
   - Parses MIME parts (`mime_parser.go`), deduplicates by `Message-ID`, resolves the conversation thread (`threading.Matcher`), saves attachments to local disk, stores the email in PostgreSQL, and advances `last_synced_uid`.
2. **`IMAPActionWorker` (`imap_actions.go`)**:
   - Claims pending jobs from `imap_jobs`:
     - **`move_to_junk`**: Triggered when a thread transitions `new -> spam`. Moves all inbound emails in the thread from `INBOX` to `Junk`.
     - **`move_to_archive`**: Triggered when a thread transitions `in_progress -> completed`. Moves all inbound emails in the thread from `INBOX` to `Archive`.
     - **`append_to_sent`**: Triggered after an outbound reply is sent via SMTP. Appends the exact RFC 5322 message to `Sent`.
3. **Resilience to External Deletion in `Archive`**:
   - `IMAPSyncWorker` only reads from `INBOX` and never propagates external folder deletions back to PostgreSQL.
   - Because full message bodies, headers, and attachments are stored in PostgreSQL and local disk upon ingestion, deleting or purging emails from the IMAP `Archive` folder on the mail server is completely safe and never breaks thread viewing or reopening.

### 4.2 Reply Linking & Completed-Thread Reopen (`internal/threading/`)

When an email is ingested, `Matcher.ResolveThread` links it to a conversation thread in three steps:

1. **Header Match (Primary)**: Extracts normalized `Message-ID`s from `In-Reply-To` and `References` (most recent first) and queries `emails.message_id`.
2. **Subject + Participant Fallback**: Strips reply/forward prefixes (`Re:`, `Fwd:`, `AW:`, `WG:`, `SV:`, `Antw:`) via `NormalizeSubject` and looks up the most recent non-spam thread with matching `subject_normalized` and `customer_email` within `subject_match_window_days`.
3. **Thread Resolution**:
   - **No match (or `spam` match)**: Creates a new thread (`status = 'new'`) assigned via `assignment.Engine`.
   - **Matched thread is `new` or `in_progress`**: Attaches the email to that thread and updates `last_message_at`.
   - **Matched thread is `completed`**: Creates a **new** follow-up thread (`status = 'new'`) with `reopens_thread_id` pointing to the completed thread. In the UI, a banner highlights the completed thread and offers a one-click **"Reopen & Merge"** action (`POST /threads/{id}/reopen-merge`). When clicked, `workflow.Service.ReopenAndMerge` transactionally moves the emails and events into the existing thread, transitions it from `completed -> in_progress`, and deletes the temporary follow-up thread.

### 4.3 Workflow State Machine (`internal/workflow/service.go`)

```text
                  +---> [   spam    ]  (enqueues IMAP MOVE from INBOX to Junk)
                  |
[    new     ] ---+
                  |
                  +---> [ in_progress ] ---> [ completed ]  (enqueues IMAP MOVE from INBOX to Archive)
                               ^                   |
                               +-------------------+
                         (via explicit "Reopen & Merge" action)
```

All transitions outside `new -> in_progress`, `new -> spam`, `in_progress -> completed`, and explicit `ReopenAndMerge` (`completed -> in_progress`) are rejected with `workflow.ErrInvalidTransition` (`HTTP 422`).

### 4.4 Outgoing Email (`internal/mail/smtp_sender.go`)

1. `OutboxWorker.ComposeAndQueueReply` creates an `outbound` `emails` record with a unique `<uuid@mailDomain>` `Message-ID`, sets `In-Reply-To` and `References` from the latest email in the thread, prefixes `Re:` on the subject if needed, stores uploaded attachments on disk, queues an `outbox_emails` row, and auto-transitions a `new` thread to `in_progress`.
2. `OutboxWorker.ProcessPending` serializes the RFC 5322 message via `wneessen/go-mail`, delivers it through the SMTP server (`username + password` auth), marks the outbox item `sent`, and enqueues an `append_to_sent` job in `imap_jobs`.

### 4.5 OAuth2 / OIDC Authentication & Local Dummy IDM (`internal/auth/`, `cmd/dev-oidc`)

See **[`docs/authentication.md`](./authentication.md)** for the complete OAuth2/OIDC setup guide, sequence diagram, and `cmd/dev-oidc` reference.

- Uses OIDC discovery (`.well-known/openid-configuration`) and OAuth2 Authorization Code flow with PKCE (`S256`).
- On callback (`/auth/callback`), verifies the ID Token signature and `nonce`, then resolves the user:
  1. Matches by `oidc_sub`.
  2. If not found, matches by `email` (binding `oidc_sub` to users **pre-created by an admin** via `/settings/users`).
  3. Otherwise auto-provisions a new `agent` user (or `admin` if matching `MINICRM_BOOTSTRAP_ADMIN_EMAIL`).
- All user creations, pre-creations, updates, logins, failed logins, and logouts are recorded in `audit_logs`.
- **Local Dummy OIDC Identity Server (`internal/auth/devidm`, `cmd/dev-oidc`)**:
  - For local development and CI end-to-end testing without an external identity server, `internal/auth/devidm` implements a standards-compliant OIDC provider (`/.well-known/openid-configuration`, `/jwks`, `/authorize`, `/token`) using an ephemeral 2048-bit RSA keypair and `github.com/go-jose/v4`.
  - `/authorize` validates the PKCE `S256` challenge and immediately redirects back to `redirect_uri` with a single-use authorization code for the configured fixed identity (`sub`, `email`, `name`).
  - `/token` verifies the PKCE `code_verifier` and returns an RS256-signed `id_token`.
  - In CI (`.github/workflows/ci.yml`, `e2e` job), `cmd/dev-oidc` and `cmd/minicrm` are started as background services against PostgreSQL 16, and `e2e/auth_e2e_test.go` verifies the complete HTTP redirect chain, session cookie authentication, and `user_login` audit trail.

### 4.6 Pluggable Assignment Engine (`internal/assignment/`)

`assignment.Engine` evaluates an ordered slice of `assignment.Resolver` implementations:
- **V1**: `DefaultAssigneeResolver` returns `system_settings.default_assignee_id`.
- **V2 Extensibility**: Additional resolvers (such as a manual `RuleBasedResolver` or an `MLPredictorResolver` trained on `emails` text and `thread_events` `reassigned` history) can be prepended in `cmd/minicrm/main.go` without schema or ingestion changes.

---

## 5. Untrusted HTML & HTTP Security Controls

To safely display untrusted HTML emails, MiniCRM applies four defense-in-depth layers:

1. **Server-Side Sanitization (`internal/mail/mime_parser.go`)**:
   - `SanitizeEmailHTML` uses a strict `bluemonday` allowlist stripping `<script>`, `<style>`, `<iframe>`, `<object>`, `<form>`, `<svg>`, `<base>`, `<meta>`, and all `on*` event attributes.
   - External links are restricted to `http`, `https`, and `mailto` and rewritten with `target="_blank" rel="nofollow noopener noreferrer"`.
   - Remote tracking images (`http://`/`https://` `<img src>`) are rewritten to `data-blocked-src`.
2. **Sandboxed `<iframe>` Isolation (`internal/web/views/views.go`)**:
   - Email HTML is never rendered inline in the main CRM DOM. Each message is isolated in:
     ```html
     <iframe
       src="/emails/{id}/body"
       sandbox="allow-popups allow-popups-to-escape-sandbox"
       referrerpolicy="no-referrer"
       credentialless
       loading="lazy">
     </iframe>
     ```
   - Omitting `allow-scripts` and `allow-same-origin` disables script execution at the browser engine level and isolates the frame in an opaque origin.
3. **Strict HTTP Security Headers (`internal/web/security_headers.go`)**:
   - Main pages emit `Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'; object-src 'none'; upgrade-insecure-requests`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cross-Origin-Opener-Policy: same-origin`, `Cross-Origin-Resource-Policy: same-origin`, `Cross-Origin-Embedder-Policy: require-corp`, `Permissions-Policy` (all features disabled), and `Strict-Transport-Security`.
   - `/emails/{id}/body` emits `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'self'; sandbox allow-popups allow-popups-to-escape-sandbox; form-action 'none'; base-uri 'none'; object-src 'none'` and `X-Frame-Options: SAMEORIGIN`.
4. **Hardened Attachment Downloads & CSRF Protection**:
   - `/attachments/{id}/download` serves files with `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none'; sandbox`, and `X-Download-Options: noopen`.
   - State-changing requests are protected by Go's `http.CrossOriginProtection` and `SameSite=Lax`, `HttpOnly` session cookies.

---

## 6. Package Structure

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
