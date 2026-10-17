# AGENTS.md — MiniCRM Agent Guide

## Documentation Index

Read these documents before making architectural or functional changes:

- **[`README.md`](./README.md)** — Overview, environment variable reference, build/run instructions, and basic usage.
- **[`docs/architecture.md`](./docs/architecture.md)** — System overview, component diagram, technology stack, and package layout.
- **[`docs/authentication.md`](./docs/authentication.md)** — Production OAuth2/OIDC setup, PKCE authentication flow, pre-created user resolution, and dummy OIDC server (`cmd/dev-oidc` / `internal/auth/devidm`).
- **[`docs/imap.md`](./docs/imap.md)** — IMAP & SMTP requirements, `INBOX` UID synchronization, `Archive`/`Junk`/`Sent` folder lifecycle, and safe external deletion from `Archive`.
- **[`docs/storage.md`](./docs/storage.md)** — PostgreSQL schema, UTC `TIMESTAMPTZ` + `display_timezone`, immutable `audit_logs`/`thread_events`, `pgx.QueryTracer` structured logging, and content-addressed local disk attachment store.
- **[`docs/workflow-and-threading.md`](./docs/workflow-and-threading.md)** — Reply linking, completed-thread **Reopen & Merge**, workflow state machine, and pluggable assignment engine.
- **[`docs/security.md`](./docs/security.md)** — Defense-in-depth HTML sanitization, sandboxed `<iframe>` isolation, HTTP security headers, attachment download hardening, and CSRF protection.
- **[`docs/testing.md`](./docs/testing.md)** — Unit, integration, and end-to-end (`e2e/`) test setup, `prek` + `golangci-lint`, and GitHub Actions CI.

---

## Non-Negotiable Invariants for Agents

When modifying MiniCRM, preserve the following invariants (documented in full in [`docs/architecture.md`](./docs/architecture.md)):

1. **Strict Workflow State Machine (`internal/workflow/service.go`)**:
   - Only `new -> in_progress`, `new -> spam`, and `in_progress -> completed` are permitted as direct transitions.
   - Replies to `completed` threads must create a new thread (`status = 'new'`) with `reopens_thread_id` set; reopening (`completed -> in_progress`) happens only via explicit `ReopenAndMerge`.
2. **IMAP Folder Lifecycle (`internal/mail/`)**:
   - `new -> spam` enqueues `move_to_junk` (`INBOX -> Junk`).
   - `in_progress -> completed` enqueues `move_to_archive` (`INBOX -> Archive`). Deleting messages from `Archive` on the IMAP server must never break MiniCRM.
   - Outbound replies sent via SMTP must be stored in PostgreSQL and queued via `append_to_sent` (`APPEND` to `Sent`).
3. **No `updated_at` Columns — Immutable Audit Trail & Structured Logging**:
   - Do not add `updated_at` columns to tables. Record all user/login/settings changes in `audit_logs` and all thread changes in `thread_events` via `internal/audit/logger.go`.
   - Every data read (`data_access`) and mutation (`data_change`) must be logged via `log/slog` (`internal/db/sqlc/tracer.go` and `internal/audit/logger.go`).
   - Store all timestamps in UTC (`TIMESTAMPTZ`) and format them in the UI using `system_settings.display_timezone` (`views.FormatTimestamp`).
4. **Untrusted HTML & HTTP Security (`internal/mail/mime_parser.go`, `internal/web/security_headers.go`)**:
   - Never render email HTML directly in the main DOM; always sanitize with `mail.SanitizeEmailHTML` and serve inside the isolated `<iframe sandbox="allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer" credentialless>` via `/emails/{id}/body`.
   - Keep `SecurityHeadersMiddleware`, `http.NewCrossOriginProtection()`, and `http.MaxBytesReader` form bounds enabled on all routes.

---

## Verification Commands

Always run both commands before finishing work or committing:

```bash
golangci-lint run ./...
go test -v -race ./...
```
