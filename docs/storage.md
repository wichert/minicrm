# Storage Architecture: PostgreSQL & Local Filesystem

MiniCRM uses **PostgreSQL 16+** for relational metadata, email bodies, sessions, job queues, and immutable audit trails, paired with a **content-addressed local filesystem store** (`MINICRM_ATTACHMENTS_DIR`) for binary email attachments and raw RFC 822 message blobs.

---

## 1. Storage Design Principles

1. **UTC Timestamps + Configurable Display Timezone**:
   - Every timestamp column in PostgreSQL uses `TIMESTAMPTZ` and is stored in **UTC**.
   - `system_settings.display_timezone` stores an IANA timezone name (e.g., `UTC`, `Europe/Amsterdam`, `America/New_York`), seeded on startup from `MINICRM_DISPLAY_TIMEZONE` and editable by admins in `/settings`.
   - All UI templates convert UTC timestamps into `display_timezone` at render time using `views.FormatTimestamp`.
2. **Immutable Audit Trail Instead of `updated_at` Columns**:
   - Mutable tables (`users`, `system_settings`, `threads`, `imap_sync_state`) intentionally do **not** have `updated_at` columns (nor `created_at` on `users`/`threads`).
   - Instead, every creation, login, configuration update, assignment change, and workflow transition is recorded as an append-only row in `audit_logs` or `thread_events`.
3. **Structured Logging for All Data Access & Changes (`log/slog`)**:
   - Every database query and application-level read or mutation is logged in structured JSON format with `event_category = "data_access"` (reads) or `event_category = "data_change"` (writes/updates/deletes).

---

## 2. PostgreSQL Schema (`internal/db/migrations/001_initial_schema.sql`)

Schema migrations are embedded into the Go binary (`internal/db/migrations/embed.go`) and applied automatically on startup using `github.com/pressly/goose/v3`.

### Table Reference

| Table | Purpose | Key Columns & Constraints |
|---|---|---|
| `users` | Operators & assignees | `id (UUID PK)`, `oidc_sub (TEXT UNIQUE, nullable)`, `email (CITEXT UNIQUE)`, `display_name`, `role ('agent'\|'admin')`, `is_active (BOOL)`. |
| `system_settings` | Singleton CRM configuration (`id = 1`) | `default_assignee_id (FK users)`, `display_timezone`, `imap_inbox_mailbox`, `imap_archive_mailbox`, `imap_junk_mailbox`, `imap_sent_mailbox`, `subject_match_window_days`. |
| `threads` | Conversation threads | `id (UUID PK)`, `subject_normalized`, `subject_display`, `status (thread_status: 'new'\|'in_progress'\|'completed'\|'spam')`, `assignee_id (FK users)`, `assignment_source`, `customer_email (CITEXT)`, `customer_name`, `reopens_thread_id (FK threads, nullable)`, `last_message_at (TIMESTAMPTZ)`. |
| `emails` | Inbound & outbound emails | `id (UUID PK)`, `thread_id (FK threads)`, `parent_email_id (FK emails)`, `direction ('inbound'\|'outbound')`, `message_id (TEXT UNIQUE)`, `in_reply_to`, `references_ids (TEXT[])`, `imap_mailbox`, `imap_uidvalidity`, `imap_uid`, `from_address (CITEXT)`, `to_addresses (JSONB)`, `cc_addresses (JSONB)`, `subject`, `subject_normalized`, `body_text`, `body_html_sanitized`, `raw_headers (JSONB)`, `sent_at`, `received_at`. |
| `attachments` | Attachment metadata | `id (UUID PK)`, `email_id (FK emails)`, `filename`, `content_type`, `size_bytes`, `sha256_hex (CHAR(64))`, `storage_relpath`, `content_id`, `is_inline`. |
| `imap_sync_state` | `INBOX` UID watermark | `mailbox (TEXT PK)`, `uid_validity (BIGINT)`, `last_synced_uid (BIGINT)`. |
| `imap_jobs` | Async IMAP action queue | `id (UUID PK)`, `job_type ('move_to_archive'\|'move_to_junk'\|'append_to_sent')`, `email_id (FK emails)`, `source_mailbox`, `target_mailbox`, `imap_uidvalidity`, `imap_uid`, `raw_rfc822_path`, `attempts`, `max_attempts`, `next_run_at`, `last_error`, `status`, `enqueued_at`. |
| `outbox_emails` | Transactional SMTP outbox | `id (UUID PK)`, `email_id (UUID UNIQUE FK emails)`, `sender_user_id (FK users)`, `status ('pending'\|'sending'\|'sent'\|'failed')`, `attempts`, `next_run_at`, `last_error`, `enqueued_at`. |
| `thread_events` | Immutable thread audit trail | `id (UUID PK)`, `thread_id (FK threads)`, `actor_user_id (FK users, nullable)`, `event_type ('created'\|'status_changed'\|'assigned'\|'reassigned'\|'reply_received'\|'reply_sent'\|'reopen_merged')`, `old_value (JSONB)`, `new_value (JSONB)`, `occurred_at (TIMESTAMPTZ)`. |
| `audit_logs` | Immutable system & user audit trail | `id (UUID PK)`, `actor_user_id (FK users, nullable)`, `target_user_id (FK users, nullable)`, `event_type ('user_created'\|'user_precreated'\|'user_updated'\|'user_login'\|'user_login_failed'\|'user_logout'\|'settings_updated')`, `ip_address`, `user_agent`, `old_value (JSONB)`, `new_value (JSONB)`, `occurred_at (TIMESTAMPTZ)`. |
| `sessions` | HTTP session store (`scs/pgxstore`) | `token (TEXT PK)`, `data (BYTEA)`, `expiry (TIMESTAMPTZ)`. |

### Indexes
- `idx_threads_status_last_msg ON threads (status, last_message_at DESC)` — Powers the filtered thread inbox.
- `idx_threads_assignee_status ON threads (assignee_id, status, last_message_at DESC)` — Powers per-assignee filtering.
- `idx_threads_subject_customer ON threads (lower(subject_normalized), customer_email, last_message_at DESC)` — Powers fallback reply threading by normalized subject and customer email.
- `idx_emails_thread_sent ON emails (thread_id, sent_at ASC)` — Orders messages chronologically within a thread.
- `idx_imap_jobs_runnable ON imap_jobs (status, next_run_at) WHERE status = 'pending'` — Partial index for fast `FOR UPDATE SKIP LOCKED` job claiming.

---

## 3. Local Disk Attachment Store (`internal/storage/attachments.go`)

Email attachments (and temporary raw RFC 822 copies queued for `append_to_sent`) are stored on the local filesystem under `MINICRM_ATTACHMENTS_DIR` (default `./data/attachments`).

### Directory Layout
Files are content-addressed by their lowercase hex **SHA-256** digest using a two-level fanout directory hierarchy:

```text
<MINICRM_ATTACHMENTS_DIR>/
├── tmp/
│   └── <uuid>.part                                # In-flight atomic upload temp files
└── <sha256[0:2]>/
    └── <sha256[2:4]>/
        └── <full_64_char_sha256_hex>              # Immutable content-addressed blob
```

Example: A file with SHA-256 `7ed95043...` is stored at relative path `7e/d9/7ed95043...`.

### Atomic Write & Deduplication (`Store.Save`)
1. Creates an exclusive temporary file `<baseDir>/tmp/<uuid>.part` (`0600` permissions).
2. Streams the input `io.Reader` through `io.MultiWriter(file, sha256Hasher)`.
3. Calls `file.Sync()` (`fsync`) and `file.Close()`.
4. Computes the final path `<baseDir>/<sha256[0:2]>/<sha256[2:4]>/<sha256>`, creates parent directories (`0750`), and atomically renames the temp file into place (`os.Rename`).
5. **Automatic Deduplication**: Identical attachments (such as repeated email signature logos across a thread) resolve to the same SHA-256 blob on disk while each email retains its own `attachments` metadata row in PostgreSQL.

### Path Traversal Protection (`Store.Open`)
- Original untrusted filenames from MIME headers are stored **only** in `attachments.filename` in PostgreSQL and never used to construct filesystem paths.
- Before opening any `storage_relpath`, `resolveSafePath` cleans the path, rejects absolute paths or `..` segments, and verifies via `filepath.Rel(baseDir, fullPath)` that the target file resides strictly inside `baseDir`.

---

## 4. Structured Data Access & Change Logging

MiniCRM enforces structured logging (`log/slog`) at both the SQL driver layer and the domain/storage layer:

1. **PostgreSQL Driver Tracing (`internal/db/sqlc/tracer.go`)**:
   - `StructuredQueryTracer` implements `pgx.QueryTracer` and is attached to the `pgxpool.Config`.
   - Every SQL statement is classified (`SELECT` -> `event_category="data_access", operation="read"`; `INSERT`/`UPDATE`/`DELETE` -> `event_category="data_change"`) and logged with `rows_affected`, `duration_ms`, `actor_user_id`, `request_id`, and `client_ip`.
2. **Application & Blob Store Logging (`internal/audit/logger.go`, `internal/storage/attachments.go`)**:
   - Reading threads, email bodies, settings, or attachment files emits a `data_access` log entry with `resource_type`, `resource_id`, and `actor_user_id`.
   - Creating or updating users, settings, threads, emails, or attachment blobs emits a `data_change` log entry alongside the database audit record.
