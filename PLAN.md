# MiniCRM — Detailed System Design & Architecture Plan

## Context

We are designing **MiniCRM**, a minimal, self-contained email CRM system built in **Go**. MiniCRM turns a shared IMAP/SMTP mailbox into a collaborative, stateful ticket/thread workflow where every incoming customer email is linked to a conversation thread, assigned to a team member, and tracked through a strict lifecycle (`new -> in-progress -> completed` or `new -> spam`).

### Core Functional Requirements
1. **Incoming Email (IMAP)**: Pull all incoming email from a single shared IMAP mailbox using basic `username + password` authentication.
2. **Outgoing Email (SMTP + IMAP Sent)**: Send all outgoing email through an SMTP server using `username + password` authentication, store the sent message in PostgreSQL immediately, and `APPEND` a copy to the IMAP `Sent` folder.
3. **Mandatory Assignment**: Every conversation thread (and by extension every email) must be assigned to a user. A configurable default assignee is used initially.
4. **Reply Linking (Threading)**: Link both incoming and outgoing replies to their original thread using:
   - **Primary**: `In-Reply-To` and `References` headers (`Message-ID` matching).
   - **Fallback**: Normalized subject + sender/participant matching.
   - **Completed-Thread Reply Handling**: If an incoming reply matches a thread that is already `completed`, MiniCRM creates a **new thread** (`state = new`) linked via `reopens_thread_id` to the completed thread, displays the completed thread in the UI, and offers a one-click **"Reopen & Merge into existing thread"** action (`completed -> in_progress`).
5. **Workflow State Machine**:
   - States: `new`, `in_progress`, `completed`, `spam`.
   - Standard transitions: `new -> in_progress -> completed`, or `new -> spam`.
   - Explicit merge/reopen transition: `completed -> in_progress` (only when a user chooses to merge a new follow-up thread into its referenced completed thread).
6. **IMAP Folder Lifecycle (`Archive` & `Junk`)**:
   - Marking a thread as `completed` (`in_progress -> completed`) moves all inbound IMAP messages in that thread from `INBOX` to the IMAP `Archive` folder. Deleting emails from the `Archive` folder on the IMAP server must be completely safe and must not break MiniCRM (all email content, headers, and attachments are retained independently in PostgreSQL and local disk).
   - Marking a thread as `spam` (`new -> spam`) moves all inbound IMAP messages in that thread from `INBOX` to the IMAP `Junk` folder.
7. **Storage & Timezone Handling**: PostgreSQL for all relational data, audit logs, metadata, and message bodies (timestamps stored in UTC `TIMESTAMPTZ` with a configurable IANA display timezone in system settings); local filesystem directory for email attachments.
8. **Authentication, Audit Trail & Structured Access/Mutation Logging**: OAuth2 / OpenID Connect (OIDC) against an existing identity server, supporting both auto-provisioning on login and admin pre-creation of users. All user creation, login/logout, settings changes, and thread updates are recorded in an immutable database audit trail rather than mutable `updated_at` columns, **and all data access (reads) and data changes (writes/updates/deletes) are logged using structured logging (`log/slog`)**.
9. **Untrusted HTML & HTTP Security Controls**: Use all available HTML and HTTP header security controls (strict server-side HTML sanitization, sandboxed `<iframe>` isolation for email bodies, strict `Content-Security-Policy`, `X-Content-Type-Options`, `Cross-Origin-*`, `Permissions-Policy`, `Referrer-Policy`, etc.) to minimize the risk of rendering untrusted email HTML.
10. **Code Quality & CI Enforcement**: Enforce `golangci-lint` and passing tests locally via `prek` git hooks and in CI via a GitHub Actions workflow.
11. **Future Extensibility**: The assignee resolution subsystem is designed around a pluggable strategy pipeline so manual routing rules and ML-based assignee prediction can be added without changing core ingestion or database schemas.

---

## Approach

MiniCRM is designed as a **single Go binary** that embeds both the HTTP server (Go SSR with `templ` + HTMX) and background workers (IMAP Synchronizer, IMAP Action Worker, and SMTP Outbox Dispatcher), backed by **PostgreSQL** and a **local attachment store**.

```
                        +-----------------------------------+
                        |   Existing OAuth2 / OIDC Server   |
                        +-----------------+-----------------+
                                          | (OIDC Auth Code + PKCE)
                                          v
+---------------+       +---------------------------------------------------+
|  Browser UI   | <---> |               MiniCRM Go Binary                   |
| (SSR + HTMX)  | HTTPS |                                                   |
+---------------+       |  +---------------------------------------------+  |
                        |  | HTTP Server (chi + templ + HTMX + SCS)      |  |
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
                        |  | Repository Layer (pgx/v5 + sqlc)            |  |
                        |  +------+---------------+---------------+------+  |
                        |         |               |               |         |
                        |         v               v               v         |
                        |  +------------+  +-------------+  +------------+  |
                        |  | IMAP Sync  |  | IMAP Action |  | SMTP +     |  |
                        |  | Worker     |  | Worker      |  | Sent Worker|  |
                        |  | (IDLE/Poll)|  | (Junk/Arch) |  | (Outbox)   |  |
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
                        | (Metadata/Body) | (/var/lib/...)  |
                        +-----------------------------------+
```

### Why Single Binary + Outbox/Job Pattern in Postgres?
- **Operational simplicity**: No Redis, RabbitMQ, or separate frontend build server is required.
- **Transactional consistency**: Using PostgreSQL transactional outbox tables (`imap_jobs`, `outbox_emails` with `SELECT ... FOR UPDATE SKIP LOCKED`) ensures that marking a thread as `spam` or sending a reply never loses state if the IMAP/SMTP server is momentarily unreachable.

---

## Technology & Library Choices

| Concern | Library / Tool | Rationale |
|---|---|---|
| **Language** | **Go 1.27** | Strong concurrency primitives, single static binary, fast startup, low memory footprint. |
| **HTTP Router & Middleware** | `github.com/go-chi/chi/v5` | Idiomatic, stdlib `net/http` compatible, composable middleware (including security headers & CSP middleware). |
| **Server-Side Templates & UI** | `github.com/a-h/templ` + **HTMX 2.x** + **PicoCSS / Tailwind** | Type-safe compiled Go HTML templates (`templ`) paired with HTMX partials for snappy inbox updates, state transitions, and inline reply forms without SPA complexity. Static assets embedded via Go `embed.FS`. |
| **PostgreSQL Driver & Queries** | `github.com/jackc/pgx/v5` + `sqlc` | Fastest pure-Go Postgres driver; `sqlc` generates type-safe Go code directly from SQL queries. |
| **Database Migrations** | `github.com/pressly/goose/v3` | Embedded SQL migrations run automatically on startup or via CLI flag. |
| **IMAP Client** | `github.com/emersion/go-imap/v2` | De-facto standard modern Go IMAPv4rev1/rev2 library with native support for `UID`, `MOVE` (RFC 6851), `IDLE` (RFC 2177), and `APPEND`. |
| **MIME Parsing & Composition** | `github.com/emersion/go-message` | Parses multipart MIME emails, charsets, attachments, and RFC 5322 headers (`Message-ID`, `In-Reply-To`, `References`). |
| **SMTP Client** | `github.com/wneessen/go-mail` | Modern, actively maintained Go SMTP library supporting STARTTLS/Implicit TLS, `PLAIN`/`LOGIN` auth, and streaming RFC 5322 serialization (so the exact same raw message bytes sent via SMTP can be `APPEND`ed to IMAP `Sent`). |
| **HTML Sanitization** | `github.com/microcosm-cc/bluemonday` | Strict allowlist sanitizer for untrusted HTML email bodies, paired with sandboxed `<iframe>` isolation and strict HTTP `Content-Security-Policy` headers. |
| **OAuth2 & OIDC** | `golang.org/x/oauth2` + `github.com/coreos/go-oidc/v3` | Standards-compliant OIDC discovery (`.well-known/openid-configuration`), ID token signature verification via JWKS, and OAuth2 Authorization Code + PKCE flow. |
| **Session Management** | `github.com/alexedwards/scs/v2` + `pgxstore` | Server-side HTTP-only, `SameSite=Lax`, Secure cookie sessions backed by PostgreSQL. |
| **Configuration & Logging** | `caarlos0/env/v11` + stdlib `log/slog` | Structured JSON/text logging and 12-factor environment variable config. |
| **Linting & Git Hooks / CI** | `golangci-lint` + `prek` + **GitHub Actions** | Static analysis (`golangci-lint`) and test execution enforced locally on commit/push via `prek` and in CI via GitHub Actions. |

---

## Storage & Data Model

### 1. PostgreSQL Schema

All database timestamps use `TIMESTAMPTZ` (stored in UTC). Mutable tables do **not** carry `updated_at` columns (nor `created_at` on `users`/`threads`); instead, every creation, login, configuration change, and thread update is recorded as an immutable row in `audit_logs` or `thread_events`. When rendering timestamps in the UI, MiniCRM converts UTC timestamps to the IANA timezone configured in `system_settings.display_timezone`.

```sql
-- 1. Users (Assignees & Operators)
-- Note: User creation, updates, and logins are recorded in audit_logs.
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    oidc_sub        TEXT UNIQUE,                -- Nullable until first OIDC login if pre-created by admin
    email           CITEXT NOT NULL UNIQUE,     -- Used to match pre-created users on first OIDC login
    display_name    TEXT NOT NULL,
    role            TEXT NOT NULL DEFAULT 'agent' CHECK (role IN ('agent', 'admin')),
    is_active       BOOLEAN NOT NULL DEFAULT TRUE
);

-- 2. System Settings (Singleton Configuration)
-- Note: Updates to system_settings are recorded in audit_logs.
CREATE TABLE system_settings (
    id                        INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    default_assignee_id       UUID NOT NULL REFERENCES users(id),
    display_timezone          TEXT NOT NULL DEFAULT 'UTC', -- IANA timezone (e.g. 'Europe/Amsterdam'), validated via time.LoadLocation
    imap_inbox_mailbox        TEXT NOT NULL DEFAULT 'INBOX',
    imap_archive_mailbox      TEXT NOT NULL DEFAULT 'Archive',
    imap_junk_mailbox         TEXT NOT NULL DEFAULT 'Junk',
    imap_sent_mailbox         TEXT NOT NULL DEFAULT 'Sent',
    subject_match_window_days INT NOT NULL DEFAULT 30
);

-- 3. Workflow State Enum
CREATE TYPE thread_status AS ENUM ('new', 'in_progress', 'completed', 'spam');

-- 4. Conversation Threads
-- Note: Thread creation and all state/assignment updates are recorded in thread_events.
CREATE TABLE threads (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_normalized      TEXT NOT NULL,              -- Subject stripped of Re:/Fwd: prefixes
    subject_display         TEXT NOT NULL,              -- Original subject of first email
    status                  thread_status NOT NULL DEFAULT 'new',
    assignee_id             UUID NOT NULL REFERENCES users(id),
    assignment_source       TEXT NOT NULL DEFAULT 'default', -- 'default', 'manual', 'rule', 'ml', 'reopen'
    customer_email          CITEXT NOT NULL,            -- Primary external sender email
    customer_name           TEXT,
    -- If this thread was created because a reply arrived for an already-completed thread,
    -- reopens_thread_id points to that completed thread so the UI can offer "Reopen & Merge".
    reopens_thread_id       UUID REFERENCES threads(id) ON DELETE SET NULL,
    last_message_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_threads_status_last_msg ON threads (status, last_message_at DESC);
CREATE INDEX idx_threads_assignee_status ON threads (assignee_id, status, last_message_at DESC);
CREATE INDEX idx_threads_subject_customer ON threads ( lower(subject_normalized), customer_email, last_message_at DESC );

-- 5. Emails (Both Incoming and Outgoing)
-- Emails are immutable once ingested/sent; their full body and attachments live in Postgres + local disk
-- so deleting archived messages on the IMAP server never breaks MiniCRM.
CREATE TABLE emails (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id           UUID NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    parent_email_id     UUID REFERENCES emails(id) ON DELETE SET NULL, -- Direct parent email in thread
    direction           TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    message_id          TEXT NOT NULL UNIQUE,   -- RFC 5322 Message-ID (normalized without < >)
    in_reply_to         TEXT,                   -- Normalized Message-ID from In-Reply-To header
    references_ids      TEXT[] NOT NULL DEFAULT '{}', -- Ordered array of Message-IDs from References header
    imap_mailbox        TEXT,                   -- Last known IMAP folder: 'INBOX', 'Archive', 'Junk', 'Sent'
    imap_uidvalidity    BIGINT,
    imap_uid            BIGINT,
    from_address        CITEXT NOT NULL,
    from_name           TEXT,
    to_addresses        JSONB NOT NULL DEFAULT '[]', -- [{name, email}]
    cc_addresses        JSONB NOT NULL DEFAULT '[]',
    subject             TEXT NOT NULL,
    subject_normalized  TEXT NOT NULL,
    body_text           TEXT,
    body_html_sanitized TEXT,
    raw_headers         JSONB NOT NULL DEFAULT '{}',
    sent_at             TIMESTAMPTZ NOT NULL,
    received_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_emails_thread_sent ON emails (thread_id, sent_at ASC);
CREATE INDEX idx_emails_message_id ON emails (message_id);
CREATE INDEX idx_emails_imap_uid ON emails (imap_mailbox, imap_uidvalidity, imap_uid);

-- 6. Attachments (Metadata in Postgres, binary payload on local disk)
CREATE TABLE attachments (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email_id        UUID NOT NULL REFERENCES emails(id) ON DELETE CASCADE,
    filename        TEXT NOT NULL,
    content_type    TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    sha256_hex      CHAR(64) NOT NULL,
    storage_relpath TEXT NOT NULL, -- e.g. 'ab/cd/<sha256_hex>' under ATTACHMENTS_DIR
    content_id      TEXT,          -- Optional CID for inline images
    is_inline       BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX idx_attachments_email_id ON attachments (email_id);

-- 7. IMAP Sync State (Tracks UID watermark for INBOX)
CREATE TABLE imap_sync_state (
    mailbox         TEXT PRIMARY KEY,
    uid_validity    BIGINT NOT NULL,
    last_synced_uid BIGINT NOT NULL DEFAULT 0
);

-- 8. Asynchronous IMAP Jobs (Moving completed emails to Archive, spam to Junk, appending Sent)
CREATE TABLE imap_jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_type        TEXT NOT NULL CHECK (job_type IN ('move_to_archive', 'move_to_junk', 'append_to_sent')),
    email_id        UUID NOT NULL REFERENCES emails(id) ON DELETE CASCADE,
    source_mailbox  TEXT,
    target_mailbox  TEXT NOT NULL,
    imap_uidvalidity BIGINT,
    imap_uid        BIGINT,
    raw_rfc822_path TEXT, -- Used for append_to_sent if IMAP APPEND retries after SMTP success
    attempts        INT NOT NULL DEFAULT 0,
    max_attempts    INT NOT NULL DEFAULT 10,
    next_run_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    enqueued_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_imap_jobs_runnable ON imap_jobs (status, next_run_at) WHERE status = 'pending';

-- 9. Outgoing Email Queue (Transactional SMTP Outbox)
CREATE TABLE outbox_emails (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email_id        UUID NOT NULL UNIQUE REFERENCES emails(id) ON DELETE CASCADE,
    sender_user_id  UUID NOT NULL REFERENCES users(id),
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sending', 'sent', 'failed')),
    attempts        INT NOT NULL DEFAULT 0,
    next_run_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    enqueued_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 10. Thread & Assignment Audit Events (Immutable audit trail for threads + future ML dataset)
CREATE TABLE thread_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id       UUID NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    actor_user_id   UUID REFERENCES users(id), -- NULL if system/automatic
    event_type      TEXT NOT NULL CHECK (event_type IN (
                        'created', 'status_changed', 'assigned', 'reassigned',
                        'reply_received', 'reply_sent', 'reopen_merged'
                    )),
    old_value       JSONB,
    new_value       JSONB,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_thread_events_thread ON thread_events (thread_id, occurred_at ASC);

-- 11. System & User Audit Trail (Immutable audit trail for logins, users, and settings)
CREATE TABLE audit_logs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_user_id   UUID REFERENCES users(id), -- NULL for system bootstrap or failed/unauthenticated events
    target_user_id  UUID REFERENCES users(id), -- Populated for user_* events
    event_type      TEXT NOT NULL CHECK (event_type IN (
                        'user_created', 'user_precreated', 'user_updated',
                        'user_login', 'user_login_failed', 'user_logout',
                        'settings_updated'
                    )),
    ip_address      INET,
    user_agent      TEXT,
    old_value       JSONB,
    new_value       JSONB,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_logs_occurred_at ON audit_logs (occurred_at DESC);
CREATE INDEX idx_audit_logs_target_user ON audit_logs (target_user_id, occurred_at DESC);
```

### 2. Local Disk Attachment Storage Layout
- Base directory configured via `MINICRM_ATTACHMENTS_DIR` (e.g., `/var/lib/minicrm/attachments`).
- Content-addressed storage by SHA-256 hash with 2-level fanout:
  `/var/lib/minicrm/attachments/<sha256[0:2]>/<sha256[2:4]>/<sha256>`
- **Benefits**:
  - Automatic deduplication of repeated company signatures/logos across thread replies.
  - Safe from path traversal vulnerabilities (original `filename` is stored only in PostgreSQL and emitted via `Content-Disposition: attachment; filename*=UTF-8''...` on download).
  - Atomic writes: stream attachment to `/var/lib/minicrm/attachments/tmp/<uuid>.part`, compute SHA-256 while streaming, `fsync`, and `os.Rename` to final path before committing the DB transaction.

---

## Core Subsystem Designs

### 1. Incoming Email Ingestion (IMAP Sync Worker)

- **Authentication**: Basic `username + password` over implicit TLS (port `993`) or `STARTTLS` (port `143`) using `go-imap/v2/imapclient`.
- **Sync Loop (`INBOX`)**:
  1. Connect and `SELECT INBOX`. Read server `UIDVALIDITY` and `UIDNEXT`.
  2. Compare `UIDVALIDITY` with `imap_sync_state.uid_validity`:
     - If `UIDVALIDITY` matches, fetch all messages with `UID > last_synced_uid` (`UID FETCH <last+1>:* (UID ENVELOPE BODY.PEEK[])`). Using `BODY.PEEK[]` avoids implicitly marking messages `\Seen` until ingested.
     - If `UIDVALIDITY` changed (rare mailbox rebuild), perform a full reconciliation using `Message-ID` deduplication against `emails.message_id`.
  3. For each fetched message (processed in ascending `UID` order):
     - Parse headers, plain-text body, HTML body (sanitized with `bluemonday`), and attachments via `emersion/go-message`.
     - If `Message-ID` is missing (malformed sender), synthesize a deterministic `Message-ID` (`sha256(raw_headers + sent_at)@synthetic.minicrm`).
     - If `emails.message_id` already exists in DB (e.g., an email we sent or a duplicate sync), update its `imap_uid` if needed and advance `last_synced_uid`.
     - Otherwise, run the **Reply Linking & Threading Algorithm** (below), persist the email/attachments, update `imap_sync_state.last_synced_uid`, and commit in a single PostgreSQL transaction.
  4. Enter **IMAP `IDLE`** mode to receive real-time push notifications from the IMAP server, re-issuing `IDLE` every 14 minutes (per RFC 2177 recommendation) with a fallback periodic poll every 2 minutes.

---

### 2. Reply Linking & Threading Algorithm

When an email (inbound or outbound) is processed:

#### Step A: Subject Normalization
Strip leading reply/forward prefixes iteratively (case-insensitive):
`^(?:\s*(?:re|fw|fwd|aw|wg|sv|antw)(?:\[\d+\])?:\s*)+` and collapse whitespace.
Example: `"Re: Fwd: [2]: Invoice #42"` -> `"Invoice #42"`.

#### Step B: Candidate Thread Lookup
1. **Header Match (Exact & Authoritative)**:
   - Extract `In-Reply-To` and all IDs in `References` (ordered from most recent to oldest).
   - Query `emails` where `message_id = ANY($candidate_ids)` ordered by `sent_at DESC LIMIT 1`.
   - If found, record `matched_email` and its `matched_thread`.
2. **Subject + Participant Fallback**:
   - If no header match is found AND the raw subject either had a reply prefix (`Re:`, `AW:`, etc.) OR matches an active conversation:
   - Query `threads` where:
     - `lower(subject_normalized) = lower($norm_subject)`
     - ` subject_normalized != ''` (never match empty subjects)
     - `customer_email = $external_party_email` (or exists in `emails.from_address` / `to_addresses` for that thread)
     - `last_message_at >= now() - ($subject_match_window_days || ' days')::interval`
     - `status != 'spam'`
   - Pick the thread with the most recent `last_message_at`.

#### Step C: Thread Resolution & Completed-Thread Handling
- **Case 1: No matching thread found (or matched thread is `spam`)**
  - Create a new `threads` row with `status = 'new'`, `assignee_id = AssignmentEngine.Resolve(email)`, and link the new `emails` row to it.
- **Case 2: Matched thread is `new` or `in_progress`**
  - Attach the new `emails` row to `matched_thread.id` (`parent_email_id = matched_email.id`).
  - Update `matched_thread.last_message_at = now()`.
  - Keep `matched_thread.status` and `matched_thread.assignee_id` unchanged.
- **Case 3: Matched thread is `completed` (User-Confirmed Reopen Pattern)**
  - Create a **new** `threads` row:
    - `status = 'new'`
    - `reopens_thread_id = matched_thread.id`
    - `assignee_id = AssignmentEngine.Resolve(email, matched_thread)` (defaults to the configured default assignee, or can optionally prefer the previous thread's assignee if still active).
  - Attach the incoming `emails` row to this new thread.
  - **In the UI**: When viewing this new thread, MiniCRM renders a contextual banner showing a summary and link to the completed thread (`matched_thread`), with a button:
    **"Reopen completed thread & move this email there"**.
  - **When the operator clicks "Reopen & Merge"**:
    Within a single DB transaction (`ReopenAndMergeThread`):
    1. Verify target thread is `completed` and current thread has `reopens_thread_id = target.id`.
    2. Re-parent all `emails` and `thread_events` from the new thread to `target.id`.
    3. Transition `target.status` from `completed` -> `in_progress` and update `target.last_message_at`.
    4. Record a `reopen_merged` event in `thread_events`.
    5. Delete the temporary follow-up thread row and redirect the browser to `/threads/{target.id}`.

---

### 3. Workflow State Machine & IMAP Folder Moves (`Junk` and `Archive`)

#### Valid State Transitions
```
                  +---> [   spam    ]  (triggers IMAP MOVE from INBOX to Junk)
                  |
[    new     ] ---+
                  |
                  +---> [ in_progress ] ---> [ completed ]  (triggers IMAP MOVE from INBOX to Archive)
                               ^                   |
                               +-------------------+
                         (via explicit "Reopen & Merge" action)
```

Enforced both in the Go domain service (`WorkflowService.Transition`) and via a PostgreSQL trigger/check:

| From State | To State | Allowed? | Side Effects |
|---|---|---|---|
| `new` | `in_progress` | Yes | Logs `status_changed` event in `thread_events`. (Also triggered automatically when an agent sends the first outbound reply on a `new` thread, or manually via UI button.) |
| `new` | `spam` | Yes | Logs `status_changed` event + enqueues `move_to_junk` jobs in `imap_jobs` for every inbound email in the thread currently in `INBOX`. |
| `in_progress` | `completed` | Yes | Logs `status_changed` event + enqueues `move_to_archive` jobs in `imap_jobs` for every inbound email in the thread currently in `INBOX`. |
| `completed` | `in_progress` | Only via Reopen & Merge | Merges the follow-up thread into the completed thread and sets state to `in_progress`, logging `reopen_merged` in `thread_events`. |
| Any other | Any other | **No (Rejected with 422)** | — |

#### Moving Spam to `Junk` and Completed Emails to `Archive`
1. When `Transition(threadID, "spam")` or `Transition(threadID, "completed")` executes in PostgreSQL, it inserts rows into `imap_jobs` (`job_type = 'move_to_junk'` with `target_mailbox = system_settings.imap_junk_mailbox`, or `job_type = 'move_to_archive'` with `target_mailbox = system_settings.imap_archive_mailbox`) for each inbound email in the thread that is still in `INBOX` (`imap_uid IS NOT NULL`).
2. The background **IMAP Action Worker** wakes up immediately (via an in-process Go channel notification + 5-second fallback ticker):
   - Selects pending jobs with `FOR UPDATE SKIP LOCKED`.
   - Ensures the target mailbox (`Junk` or `Archive`) exists on the IMAP server (`CREATE` if missing).
   - Opens/reuses the IMAP client connection, `SELECT`s `source_mailbox` (`INBOX`), verifies `UIDVALIDITY`, and executes `UID MOVE <uid> "<target_mailbox>"` (falling back to `UID COPY` + `UID STORE +FLAGS (\Deleted)` + `UID EXPUNGE` if the server lacks RFC 6851 `MOVE` capability).
   - Updates `emails.imap_mailbox = target_mailbox`, sets `emails.imap_uid = NULL` (since UIDs are mailbox-scoped), and marks the `imap_jobs` row `completed`.
   - If the message UID no longer exists in `source_mailbox` (e.g., already moved or deleted externally), the worker treats the move as a benign no-op and marks the job `completed`.
   - On transient network errors, increments `attempts` with exponential backoff (`next_run_at = now() + 2^attempts seconds`).

#### Why Deleting Emails from the IMAP `Archive` Folder is Safe
- **One-way ingestion from `INBOX` only**: The IMAP Sync Worker only monitors and ingests from `INBOX`. It never synchronizes deletions or expunges from `Archive`, `Junk`, or `Sent` back into PostgreSQL.
- **Self-contained storage**: Upon initial ingestion from `INBOX`, the complete email text, sanitized HTML, RFC 5322 headers (`Message-ID`, `In-Reply-To`, `References`), and all attachments are persisted in PostgreSQL (`emails`, `attachments`) and on local disk (`MINICRM_ATTACHMENTS_DIR`).
- **Reopen & Threading resilience**: Reply matching (`emails.message_id`), viewing historical completed threads, downloading attachments, and running **Reopen & Merge** operate 100% against PostgreSQL and local disk. Deleting or purging any or all messages from the IMAP `Archive` folder has zero impact on MiniCRM.

---

### 4. Outgoing Email (SMTP + IMAP `Sent`)

When an agent replies to a thread in the UI (`POST /threads/{id}/reply`):
1. **Compose & Persist (Transactional)**:
   - Generate a globally unique RFC 5322 `Message-ID`: `<uuid@configured-domain>`.
   - Set `In-Reply-To` to the latest inbound email's `<Message-ID>`.
   - Set `References` to the parent email's `References` chain + the parent email's `<Message-ID>` (capped at the last 20 IDs per RFC 5322 line length best practices).
   - Ensure `Subject` starts with `Re: <subject_display>` if not already prefixed.
   - Insert the `emails` row (`direction = 'outbound'`), save any uploaded attachments to local disk, and insert an `outbox_emails` row (`status = 'pending'`).
   - If the thread's status is `new`, optionally transition it to `in_progress`.
2. **SMTP Send + IMAP `APPEND` (Outbox Worker)**:
   - Picks up the `outbox_emails` entry.
   - Builds the RFC 5322 MIME message using `wneessen/go-mail` into a buffer/temp file.
   - Connects to the SMTP server (STARTTLS / Implicit TLS, `username + password` auth) and sends the message.
   - Marks `outbox_emails.status = 'sent'`.
   - Enqueues/executes `APPEND "Sent" (\Seen) {<size>}` on the IMAP server using the exact RFC 5322 bytes so the shared mailbox's `Sent` folder reflects all CRM replies.

---

### 5. OAuth2 / OIDC Authentication & Pre-Created Users

- **Identity Server Integration**:
  - Configured via `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, and `OIDC_REDIRECT_URL`.
  - Uses `coreos/go-oidc/v3` to discover endpoints from `{OIDC_ISSUER_URL}/.well-known/openid-configuration`.
- **Login Flow**:
  1. `GET /auth/login`: Generates cryptographic `state`, `nonce`, and PKCE `code_verifier`, stores them in a short-lived signed cookie, and redirects to the identity server's `authorization_endpoint` with scopes `openid profile email`.
  2. `GET /auth/callback`: Validates `state`, exchanges `code` + `code_verifier` at the `token_endpoint`, verifies the `id_token` signature and `nonce`, and extracts `sub`, `email`, and `name`.
  3. **User Resolution / Upsert & Audit Logging**:
     - First look up `users` by `oidc_sub = $sub`.
     - If not found, look up `users` by `email = $email` (matches users that were **pre-created by an admin** before their first login), bind `oidc_sub = $sub`, and record a `user_updated` event in `audit_logs`.
     - If still not found, insert a new `users` record (`role = 'agent'`, or `'admin'` if first user / matching bootstrap admin email) and record a `user_created` event in `audit_logs`.
     - If `user.is_active == false`, record `user_login_failed` in `audit_logs` and reject login with `403 Forbidden`.
     - On successful login, record `user_login` (with client IP and User-Agent) in `audit_logs`.
  4. Create an authenticated session via `scs` (stored in Postgres `sessions` table) and redirect to `/threads`.
- **Bootstrap Default Assignee & Display Timezone**:
  - On first startup, `MINICRM_BOOTSTRAP_ADMIN_EMAIL` seeds the initial admin user (recording `user_created` in `audit_logs`) and sets `system_settings.default_assignee_id` to that user and `system_settings.display_timezone` (from `MINICRM_DISPLAY_TIMEZONE`, defaulting to `'UTC'`) so incoming emails can be assigned immediately even before interactive logins occur.

---

### 6. Extensible Assignment Architecture (Ready for Rules & ML)

To support the requirement that *"all emails must be assigned to a person, the default assignee must be configurable, and in later versions we may want to add manual rules and learn from assignments to predict the right assignee"*, assignment resolution is decoupled behind a chain-of-responsibility interface:

```go
package assignment

type Candidate struct {
    AssigneeID uuid.UUID
    Source     string  // "rule", "ml", "default"
    Confidence float64 // 0.0 - 1.0
    Reason     string
}

type Resolver interface {
    Name() string
    Resolve(ctx context.Context, input EmailInput) (*Candidate, error)
}
```

#### V1 Implementation (`DefaultAssigneeResolver`)
- Reads `system_settings.default_assignee_id` (verifying `users.is_active = true`).
- Returns `Candidate{AssigneeID: defaultID, Source: "default", Confidence: 1.0}`.

#### Future V2 Extension (`RuleBasedResolver` -> `MLPredictorResolver` -> `DefaultAssigneeResolver`)
Because V1 already stores:
1. Normalized subjects, sender addresses/domains, sanitized bodies, and recipient headers in `emails`,
2. `assignment_source` on `threads`, and
3. Every manual reassignment (`event_type = 'reassigned'`, `old_value`, `new_value`, `actor_user_id`) in `thread_events`,

V2 can plug in two additional resolvers ahead of `DefaultAssigneeResolver` with zero changes to the ingestion pipeline:
- **`RuleBasedResolver` (V2)**: Evaluates ordered admin-defined rules (e.g., `from_domain == "acme.com"` or `subject CONTAINS "invoice"` -> assign to Bob).
- **`MLPredictorResolver` (V2)**: Trains a lightweight text classifier (e.g., TF-IDF / Naive Bayes or embeddings over `subject_normalized + from_address + body_text` trained on historical threads whose final `completed` assignee differs from initial `default` assignment) and assigns automatically when `Confidence >= threshold`.

---

### 7. Web UI & HTTP Routes (Go SSR + `templ` + HTMX)

All UI timestamps are formatted by converting PostgreSQL UTC `TIMESTAMPTZ` values into the configured `system_settings.display_timezone` (using `time.LoadLocation`).

| Method & Path | Description |
|---|---|
| `GET /auth/login`, `/auth/callback`, `POST /auth/logout` | OIDC login/callback and session logout (logged in `audit_logs`). |
| `GET /threads` | Filterable thread inbox (`?status=new\|in_progress\|completed\|spam&assignee=me\|all\|<uuid>`). Supports HTMX partial updates (`HX-Request`). |
| `GET /threads/{id}` | Thread detail view: displays all linked emails chronologically (each HTML body rendered inside a sandboxed `<iframe>`), attachment download links, status transition buttons, assignee dropdown, reply composer, audit timeline, and (if `reopens_thread_id != nil`) the **"Completed Thread Found — Reopen & Merge"** banner. |
| `GET /emails/{id}/body` | Dedicated isolated endpoint that serves the sanitized HTML body of a single email for rendering inside a sandboxed `<iframe>` with an ultra-restrictive `Content-Security-Policy`. |
| `POST /threads/{id}/transition` | Transitions thread state (`new -> in_progress`, `new -> spam`, `in_progress -> completed`). |
| `POST /threads/{id}/assign` | Changes `threads.assignee_id` and records a `reassigned` event in `thread_events`. |
| `POST /threads/{id}/reopen-merge` | Merges the current `new` follow-up thread into its `reopens_thread_id` (`completed -> in_progress`). |
| `POST /threads/{id}/reply` | Multipart form handler for sending a reply (with optional file attachments). |
| `GET /attachments/{id}/download` | Authenticated download streaming the file from local disk (`MINICRM_ATTACHMENTS_DIR`) with `Content-Disposition: attachment`. |
| `GET /settings`, `POST /settings` | Admin page to configure default assignee, `display_timezone` (IANA timezone), IMAP folder names (`INBOX`, `Archive`, `Junk`, `Sent`), and pre-create users (`POST /settings/users`). All changes logged in `audit_logs`. |

---

### 8. Untrusted HTML Rendering & HTTP Security Controls

Displaying untrusted HTML email bodies in a web application is a primary vector for XSS, CSRF, UI redressing (clickjacking), CSS exfiltration, and tracking-pixel privacy leaks. MiniCRM enforces **defense-in-depth** across four layers:

#### Layer 1: Strict Server-Side HTML Sanitization (`bluemonday`)
- On ingestion, `body_html_sanitized` is produced using a strict `bluemonday` allowlist policy:
  - **Allowed**: Basic structural and typography tags (`p`, `br`, `div`, `span`, `blockquote`, `pre`, `code`, `ul`, `ol`, `li`, `table`, `thead`, `tbody`, `tr`, `th`, `td`, `b`, `i`, `u`, `strong`, `em`, `a`, `img`, `h1`–`h6`, `hr`).
  - **Stripped**: `<script>`, `<object>`, `<embed>`, `<applet>`, `<iframe/frame>`, `<form>`, `<input>`, `<button>`, `<base>`, `<meta>`, `<link>`, `<style>`, `<svg>`, `<math>`, and all `on*` event attributes (`onclick`, `onerror`, `onload`, etc.).
  - **Link Hardening**: Only `http://`, `https://`, and `mailto:` URL schemes are permitted on `<a href>`. Every `<a>` tag is rewritten with `target="_blank"` and `rel="noopener noreferrer nofollow"`.
  - **Image / Media Privacy**: Remote images (`<img src="https://...">`) are blocked by default (rewritten to `data-blocked-src`) unless the operator clicks "Load remote images" for that email; inline `cid:` attachments resolved to `/attachments/{id}/inline` are permitted.

#### Layer 2: HTML `<iframe>` Sandbox Isolation
- Sanitized email HTML is **never** injected directly into the main application DOM.
- Instead, `/threads/{id}` embeds each email body inside an isolated, opaque-origin `<iframe credentialless>`:
  ```html
  <iframe
    src="/emails/{email_id}/body"
    sandbox="allow-popups allow-popups-to-escape-sandbox"
    referrerpolicy="no-referrer"
    credentialless
    loading="lazy">
  </iframe>
  ```
- **Why this sandbox configuration is maximally restrictive**:
  - `allow-scripts` is **omitted**: JavaScript execution inside the email is disabled at the browser engine level even if a sanitizer bypass ever existed.
  - `allow-same-origin` is **omitted**: The iframe runs in a unique opaque origin (`null`), so it cannot read cookies, `localStorage`, or the parent DOM.
  - `allow-forms` and `allow-top-navigation` are **omitted**: The email cannot submit forms or navigate the CRM tab.
  - `allow-popups allow-popups-to-escape-sandbox`: Clicking an `<a target="_blank" rel="noopener noreferrer">` link opens in a clean, un-sandboxed new tab without access to `window.opener`.

#### Layer 3: HTTP Security Headers on All Application Responses
Applied globally via `internal/web/security_headers.go` middleware:

| Header | Value on Main CRM Pages (`/threads`, `/settings`, etc.) | Value on Email Body Endpoint (`/emails/{id}/body`) |
|---|---|---|
| `Content-Security-Policy` | `default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'; object-src 'none'; upgrade-insecure-requests` | `default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'self'; sandbox allow-popups allow-popups-to-escape-sandbox; form-action 'none'; base-uri 'none'; object-src 'none'` |
| `X-Content-Type-Options` | `nosniff` | `nosniff` |
| `X-Frame-Options` | `DENY` | `SAMEORIGIN` |
| `Referrer-Policy` | `no-referrer` | `no-referrer` |
| `Cross-Origin-Opener-Policy` | `same-origin` | `same-origin` |
| `Cross-Origin-Resource-Policy` | `same-origin` | `same-origin` |
| `Cross-Origin-Embedder-Policy` | `require-corp` | `require-corp` |
| `Permissions-Policy` | `accelerometer=(), autoplay=(), camera=(), clipboard-read=(), clipboard-write=(), display-capture=(), fullscreen=(), geolocation=(), gyroscope=(), hid=(), idle-detection=(), magnetometer=(), microphone=(), midi=(), payment=(), picture-in-picture=(), publickey-credentials-get=(), screen-wake-lock=(), serial=(), usb=(), web-share=(), xr-spatial-tracking=()` | Same (all browser features disabled) |
| `Strict-Transport-Security` | `max-age=63072000; includeSubDomains; preload` | `max-age=63072000; includeSubDomains; preload` |
| `Cache-Control` | `no-store, max-age=0` | `private, no-store, max-age=0` |

#### Layer 4: Attachment Download & CSRF Protection
- **Attachment Downloads (`/attachments/{id}/download`)**:
  - Served with `Content-Disposition: attachment; filename*=UTF-8''<url-encoded-name>`, `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none'; sandbox`, and `X-Download-Options: noopen` so browsers never execute uploaded HTML/SVG/PDF attachments in the application origin.
- **CSRF Protection**:
  - Enforced via Go 1.25+ `http.CrossOriginProtection` (`Sec-Fetch-Site` / `Origin` verification) combined with `SameSite=Lax` + `__Host-` prefixed `Secure` `HttpOnly` session cookies.

---

### 9. Code Quality, `prek` Local Hooks & GitHub Actions CI

- **Linter (`golangci-lint`)**:
  - Configured via `.golangci.yml` at the repository root enabling strict linters (`errcheck`, `govet`, `staticcheck`, `gosimple`, `ineffassign`, `unused`, `gosec`, `gocritic`, `revive`, `bodyclose`, `sqlclosecheck`, `noctx`, `errorlint`).
- **Local Development Hooks (`prek`)**:
  - Configured via `.pre-commit-config.yaml` (used natively by `prek`) so every developer commit/push runs:
    1. `golangci-lint run ./...`
    2. `go test -race ./...`
- **Continuous Integration (`.github/workflows/ci.yml`)**:
  - Runs automatically on every `push` and `pull_request`:
    1. **Lint Job**: Sets up Go 1.27 and runs `golangci-lint`.
    2. **Test Job**: Sets up Go 1.27 + a `postgres:16-alpine` service container, runs database migrations, and executes unit and integration test suites via `go test -v -race ./...`.

---

### 10. Structured Logging for All Data Access & Changes (`log/slog`)

Every data access (read) and data mutation (insert/update/delete) across MiniCRM is emitted as a structured log entry using Go's `log/slog` (`slog.NewJSONHandler` in production):

- **Contextual Correlation Attributes**:
  - Every HTTP request and background worker job attaches a correlation `request_id` / `job_id`, `actor_user_id` (or `"system"` for background workers), `client_ip`, and `user_agent` into `context.Context`.
  - A custom `slog.Handler` wrapper (`internal/audit/slog_context.go`) automatically extracts these context attributes onto every `slog` record.
- **Data Access Logging (Reads)**:
  - Logged whenever threads, email bodies, attachments, user lists, or settings are read:
    ```json
    {
      "time": "2026-04-12T13:15:00Z",
      "level": "INFO",
      "msg": "data_access",
      "event_category": "data_access",
      "action": "read",
      "resource_type": "email_body",
      "resource_id": "019629a4-...",
      "thread_id": "019629a0-...",
      "actor_user_id": "01962900-...",
      "request_id": "req-8f92a1",
      "client_ip": "192.0.2.10"
    }
    ```
- **Data Change Logging (Mutations)**:
  - Logged alongside every database transaction commit that creates or mutates users, settings, threads, emails, attachments, or workflow states:
    ```json
    {
      "time": "2026-04-12T13:16:04Z",
      "level": "INFO",
      "msg": "data_change",
      "event_category": "data_change",
      "action": "update",
      "resource_type": "thread",
      "resource_id": "019629a0-...",
      "actor_user_id": "01962900-...",
      "request_id": "req-8f92b4",
      "old_value": {"status": "in_progress"},
      "new_value": {"status": "completed"}
    }
    ```
- **Database-Level Query Tracing (`pgx.QueryTracer`)**:
  - `pgxpool` is configured with a `pgx.QueryTracer` implementation that logs executed SQL statement names (`sqlc` query name), operation type (`SELECT` / `INSERT` / `UPDATE` / `DELETE`), row count, duration (`duration_ms`), and actor context via `slog`, ensuring no data access path can bypass structured logging.

---

## Proposed Project Directory Structure

```text
minicrm/
├── .github/
│   └── workflows/
│       └── ci.yml                  # GitHub Actions workflow: golangci-lint + go test -race
├── .golangci.yml                   # golangci-lint configuration
├── .pre-commit-config.yaml         # prek hook configuration (lint + tests)
├── cmd/
│   └── minicrm/
│       └── main.go                 # Entrypoint: config, DB migrations, workers, HTTP server
├── internal/
│   ├── config/
│   │   └── config.go               # Env var parsing (IMAP, SMTP, DB, OIDC, Disk paths)
│   ├── db/
│   │   ├── migrations/             # Goose SQL migrations (001_initial_schema.sql)
│   │   ├── queries/                # sqlc SQL definitions
│   │   └── sqlc/                   # Generated type-safe Go data access code
│   ├── audit/
│   │   └── logger.go               # Audit trail service for users, logins, and settings
│   ├── auth/
│   │   ├── oidc.go                 # OAuth2 + OIDC verifier, login & callback handlers
│   │   └── middleware.go           # SCS session & role authorization middleware
│   ├── assignment/
│   │   ├── engine.go               # Chain-of-responsibility assignment pipeline
│   │   └── default_resolver.go     # V1 configurable default assignee resolver
│   ├── threading/
│   │   ├── normalizer.go           # Subject normalization & Message-ID extraction
│   │   └── matcher.go              # Header match + Subject/Sender fallback + Reopen link
│   ├── workflow/
│   │   └── service.go              # Strict state machine transitions + Reopen & Merge
│   ├── mail/
│   │   ├── imap_sync.go            # IMAP IDLE + UID polling ingestion worker
│   │   ├── imap_actions.go         # IMAP MOVE to Archive/Junk & APPEND to Sent worker
│   │   ├── mime_parser.go          # RFC 5322 parser + bluemonday HTML sanitizer
│   │   └── smtp_sender.go          # SMTP dispatcher + RFC 5322 builder
│   ├── storage/
│   │   └── attachments.go          # Content-addressed SHA-256 local disk store
│   └── web/
│       ├── router.go               # chi routes
│       ├── security_headers.go     # CSP, HSTS, COOP/COEP/CORP, Permissions-Policy, CSRF
│       ├── handlers_threads.go     # Thread list, view, isolated email body, transitions, reply
│       ├── handlers_settings.go    # Default assignee, display timezone, folders, pre-created users
│       ├── static/                 # Embedded HTMX + CSS
│       └── views/                  # templ templates (layout, inbox, thread_detail, settings)
├── sqlc.yaml
└── go.mod
```

---

## Files to Modify
- `PLAN.md` (this design specification document — no implementation code is created in this planning phase per instructions).

## Reuse
- Greenfield repository (`/Users/wichert/Hack/minicrm` is currently empty).

## Steps (Implementation Checklist for Future Build Phase)
- [x] **Step 1: Project, Tooling & Database Foundation** — Initialize `go.mod` (Go 1.27), `.golangci.yml`, `.pre-commit-config.yaml` (`prek`), `.github/workflows/ci.yml`, `sqlc.yaml`, configuration loader (`internal/config`), and PostgreSQL schema migration (`001_initial_schema.sql` with `audit_logs`, `thread_events`, and `display_timezone`).
- [x] **Step 2: Local Disk Attachment Store** — Implement atomic temp-file write, SHA-256 hashing, deduplicated storage layout, and hardened streaming reader in `internal/storage/attachments.go`.
- [x] **Step 3: OAuth2 / OIDC Auth, User Management & Audit Logging** — Implement OIDC discovery, PKCE login/callback, SCS session middleware, bootstrap admin seeding, admin user pre-creation, and `audit_logs` recording for all login and user lifecycle events.
- [x] **Step 4: Assignment Engine & Workflow State Machine** — Implement `assignment.Engine` with `DefaultAssigneeResolver`, `workflow.Service` enforcing `new -> in_progress -> completed` (enqueueing `move_to_archive`) and `new -> spam` (enqueueing `move_to_junk`), and the transactional `ReopenAndMerge` operation.
- [x] **Step 5: Threading & MIME Ingestion** — Implement RFC 5322 parsing, strict `bluemonday` HTML sanitization, `In-Reply-To`/`References` matching, normalized subject+sender fallback, and `reopens_thread_id` linking when matching a `completed` thread.
- [x] **Step 6: IMAP Sync & Action Workers** — Implement `imap_sync.go` (`INBOX` UID tracking + `IDLE`) and `imap_actions.go` (`UID MOVE` to `Archive` on completion, `UID MOVE` to `Junk` on spam transition, and `APPEND` to `Sent` on outbound reply, safely tolerating externally deleted `Archive` messages).
- [x] **Step 7: Outbound SMTP Worker** — Implement `smtp_sender.go` reading from `outbox_emails`, sending via SMTP with username/password auth, and queueing the `Sent` folder `APPEND`.
- [x] **Step 8: Web UI & HTTP Security Layer (Go `templ` + HTMX)** — Build security headers middleware (`security_headers.go`), sandboxed email body iframe endpoint (`/emails/{id}/body`), inbox list, thread detail view, and settings view (including `display_timezone` configuration).

## Verification
- **Static Analysis & Local Hooks (`prek` + `golangci-lint`)**:
  - Run `prek run --all-files` locally to verify `golangci-lint run ./...` and `go test -race ./...` pass with zero warnings or errors.
- **GitHub Actions CI (`.github/workflows/ci.yml`)**:
  - Automated workflow running on every push and pull request:
    - Executes `golangci-lint` against Go 1.27.
    - Spins up a PostgreSQL 16 service container and runs all unit and integration tests with `-race`.
- **Unit Tests**:
  - `threading`: Verify `In-Reply-To`/`References` linking, subject prefix stripping (`Re:`, `Fwd:`, `AW:`), sender fallback within time window, and creation of a new thread with `reopens_thread_id` when matching a `completed` thread.
  - `workflow`: Verify allowed transitions (`new -> in_progress`, `new -> spam`, `in_progress -> completed`, and `completed -> in_progress` via merge), verify `move_to_archive` and `move_to_junk` job creation, and verify all invalid transitions return errors.
  - `security`: Verify `bluemonday` strips scripts/event handlers/unsafe URLs, verify `/emails/{id}/body` and main routes emit all required `Content-Security-Policy` and HTTP security headers, and verify `<iframe>` sandbox attributes.
  - `audit` & `timezone`: Verify user creation, login, settings updates, and thread transitions create immutable records in `audit_logs` / `thread_events`, and verify UTC timestamps render in the configured `display_timezone`.
- **Integration Tests**:
  - Run against a local test mail server (e.g., GreenMail / Dovecot+Postfix test container) and PostgreSQL:
    1. Inject inbound email into IMAP `INBOX` -> verify thread is created in `new` state with default assignee and attachments on disk.
    2. Mark thread as `spam` -> verify message is moved from `INBOX` to IMAP `Junk` folder.
    3. Send reply on another thread -> verify SMTP delivery, `In-Reply-To` header, DB record, and copy in IMAP `Sent`.
    4. Transition thread to `completed` -> verify inbound messages are moved from `INBOX` to IMAP `Archive` folder.
    5. Delete the moved messages from the IMAP `Archive` folder externally -> verify MiniCRM still displays the completed thread, bodies, and attachments, and can still execute `Reopen & Merge` when a new reply arrives.
