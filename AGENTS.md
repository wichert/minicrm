# AGENTS.md — MiniCRM Agent Guide

## Documentation Index

Read these documents before making architectural or functional changes:

- **[`README.md`](./README.md)** — Overview, environment variable reference, build/run instructions, and basic usage.
- **[`docs/architecture.md`](./docs/architecture.md)** — Detailed system architecture, technology stack, PostgreSQL schema, IMAP/SMTP worker mechanics, reply threading & reopen/merge flow, pluggable assignment engine, and HTML/HTTP security controls.
- **[`docs/authentication.md`](./docs/authentication.md)** — Production OAuth2/OIDC setup, PKCE authentication flow, pre-created user resolution, dummy OIDC server (`cmd/dev-oidc` / `internal/auth/devidm`), and E2E testing.

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
