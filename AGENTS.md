# AGENTS.md — MiniCRM Developer & Agent Guide

## Overview

**MiniCRM** is a single-binary Go 1.27 email CRM system that synchronizes a shared IMAP mailbox (`INBOX`, `Archive`, `Junk`, `Sent`), sends replies via SMTP, links replies into conversation threads, assigns every thread to a user, and enforces a strict workflow state machine.

Detailed architectural specifications live in [`PLAN.md`](./PLAN.md).

---

## Core Domain Rules & Invariants

1. **Workflow State Machine (`internal/workflow/service.go`)**:
   - Thread states (`sqlc.ThreadStatus`): `new`, `in_progress`, `completed`, `spam`.
   - Permitted standard transitions:
     - `new -> in_progress` (manual or automatic when sending the first outbound reply on a `new` thread)
     - `new -> spam`
     - `in_progress -> completed`
   - All other direct transitions are rejected (`workflow.ErrInvalidTransition`).
   - **Completed-Thread Replies & Reopen/Merge (`reopens_thread_id`)**:
     - When an incoming email matches an already `completed` thread, `threading.Matcher` does **not** automatically reopen the completed thread. Instead, it creates a new thread (`status = 'new'`) with `reopens_thread_id` pointing to the completed thread.
     - In the UI (`/threads/{id}`), operators see a banner linking to the completed thread and can click **"Reopen & Merge"** (`POST /threads/{id}/reopen-merge`), which executes `workflow.Service.ReopenAndMerge` to move all emails/events into the completed thread, transition it from `completed -> in_progress`, and delete the temporary follow-up thread.

2. **IMAP Folder Lifecycle (`internal/mail/imap_sync.go`, `internal/mail/imap_actions.go`)**:
   - **Ingestion**: `IMAPSyncWorker` only syncs incoming emails from `INBOX` (using basic `username + password` authentication and tracking `uid_validity` and `last_synced_uid` in `imap_sync_state`).
   - **Spam (`new -> spam`)**: Enqueues `move_to_junk` jobs in `imap_jobs` to move the thread's inbound messages from `INBOX` to `Junk`.
   - **Completion (`in_progress -> completed`)**: Enqueues `move_to_archive` jobs in `imap_jobs` to move the thread's inbound messages from `INBOX` to `Archive`.
   - **Safe Deletion from `Archive`**: All email headers, bodies (`body_text`, `body_html_sanitized`), and attachments are stored durably in PostgreSQL and on local disk (`MINICRM_ATTACHMENTS_DIR`) upon initial ingestion. Deleting or purging messages from the IMAP `Archive` folder on the mail server is completely safe and must never break MiniCRM.
   - **Outbound (`POST /threads/{id}/reply`)**: `OutboxWorker` sends replies via SMTP (`username + password` auth), stores the email in PostgreSQL immediately, and enqueues an `append_to_sent` job in `imap_jobs` to `APPEND` the exact RFC 5322 message to the IMAP `Sent` folder.

3. **Reply Linking (`internal/threading/`)**:
   - Primary match: `In-Reply-To` and `References` RFC 5322 `Message-ID` headers (`FindEmailByMessageIDs`).
   - Fallback match: Normalized subject (`NormalizeSubject`, stripping `Re:`, `Fwd:`, `AW:`, `WG:`, `SV:`, `Antw:`) + customer email within `system_settings.subject_match_window_days`.

4. **Mandatory Assignment & Extensibility (`internal/assignment/`)**:
   - Every thread must have a non-null `assignee_id`.
   - Assignment resolution uses a chain-of-responsibility pipeline (`assignment.Engine` + `assignment.Resolver`).
   - V1 uses `DefaultAssigneeResolver` (reading `system_settings.default_assignee_id`). Future rule-based or ML resolvers should implement `assignment.Resolver` and be registered ahead of `DefaultAssigneeResolver` in `cmd/minicrm/main.go`.

5. **Database Timestamps, Display Timezone & Immutable Audit Trail**:
   - All timestamps in PostgreSQL are stored in UTC (`TIMESTAMPTZ`).
   - Mutable tables (`users`, `system_settings`, `threads`, `imap_sync_state`) intentionally do **not** have `updated_at` columns.
   - All user creations, pre-creations, OIDC logins/logouts, and settings changes must be recorded in `audit_logs` via `internal/audit/logger.go`.
   - All thread creations, state transitions, assignments/reassignments, replies, and reopen-merges must be recorded in `thread_events` via `internal/audit/logger.go`.
   - All SQL queries (`internal/db/sqlc/tracer.go`) and application-level reads/mutations emit structured JSON logs via `log/slog` (`event_category = "data_access"` or `"data_change"`).
   - UI views convert UTC timestamps into `system_settings.display_timezone` (validated via `time.LoadLocation`) using `views.FormatTimestamp`.

6. **Untrusted HTML & HTTP Security Controls (`internal/mail/mime_parser.go`, `internal/web/security_headers.go`)**:
   - Never render untrusted email HTML directly in the main DOM.
   - Incoming HTML is sanitized on ingestion via `mail.SanitizeEmailHTML` (`bluemonday` strict allowlist; remote tracking images rewritten to `data-blocked-src`; external links forced to `target="_blank" rel="nofollow noopener noreferrer"`).
   - Email bodies are served from the isolated `/emails/{id}/body` endpoint (`SetIsolatedEmailBodyHeaders`) and embedded via `<iframe sandbox="allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer" credentialless loading="lazy">` (no `allow-scripts`, no `allow-same-origin`).
   - Global security headers (`SecurityHeadersMiddleware`) and CSRF protection (`http.NewCrossOriginProtection()`) must remain enabled on all routes.

---

## Project Layout

```text
cmd/minicrm/main.go             # Binary entrypoint (DB pool, migrations, workers, HTTP server)
internal/
  config/                       # Environment variable configuration & validation
  db/
    migrations/                 # Embedded Goose SQL migrations (001_initial_schema.sql)
    queries/                    # SQL query definitions
    sqlc/                       # Typed database models, queries, and pgx StructuredQueryTracer
  audit/                        # Audit trail service (audit_logs, thread_events, slog data_access/data_change)
  auth/                         # OIDC provider (coreos/go-oidc + oauth2 PKCE), SCS sessions, user pre-creation
  assignment/                   # Pluggable assignee resolution engine & DefaultAssigneeResolver
  threading/                    # Subject/Message-ID normalizer and reply-to-thread matcher
  workflow/                     # Thread state machine, Archive/Junk move queueing, Reopen & Merge
  mail/                         # MIME parser, bluemonday sanitizer, IMAP sync/action workers, SMTP outbox worker
  storage/                      # Content-addressed SHA-256 local disk attachment store
  web/                          # chi router, security headers, handlers, embedded static assets, SSR views
```

---

## Development, Linting & Testing

Before committing any change, ensure both the linter and race-detector test suite pass with zero issues:

```bash
# Run unit and integration tests with race detector
go test -v -race ./...

# Run static analysis and security/style checks (must report 0 issues)
golangci-lint run ./...
```

- **Local Git Hooks**: `.pre-commit-config.yaml` is configured for `prek` (`prek run --all-files`) to enforce `golangci-lint run ./...` and `go test -race ./...`.
- **CI**: `.github/workflows/ci.yml` runs `golangci-lint` and `go test -v -race ./...` (with a PostgreSQL 16 service container) on every push and pull request.
