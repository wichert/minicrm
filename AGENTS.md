# AGENTS.md — MiniCRM Agent Guide

## Documentation Index

Read these documents before making architectural or functional changes:

- **[`README.md`](./README.md)** — Overview, environment variable reference, build/run instructions, and basic usage.
- **[`docs/architecture.md`](./docs/architecture.md)** — System overview, component diagram, technology stack, and package layout.
- **[`docs/authentication.md`](./docs/authentication.md)** — Production OAuth2/OIDC setup, PKCE authentication flow, OIDC group-based mail-account access, Kanidm setup guide, pre-created user resolution, and dummy OIDC server (`cmd/dev-oidc` / `internal/auth/devidm`).
- **[`docs/imap.md`](./docs/imap.md)** — IMAP & SMTP requirements, `INBOX`/`Sent` UID synchronization (incl. replies sent from other mail clients), `Archive`/`Junk`/`Sent` folder lifecycle, and safe external deletion from `Archive`.
- **[`docs/storage.md`](./docs/storage.md)** — PostgreSQL schema, UTC `TIMESTAMPTZ` + `display_timezone`, immutable `audit_logs`/`thread_events`, `pgx.QueryTracer` structured logging, and content-addressed local disk attachment store.
- **[`docs/workflow-and-threading.md`](./docs/workflow-and-threading.md)** — Reply linking, completed-thread **Reopen & Merge**, workflow state machine, and pluggable assignment engine.
- **[`docs/security.md`](./docs/security.md)** — Defense-in-depth HTML sanitization, sandboxed `<iframe>` isolation, HTTP security headers, attachment download hardening, and CSRF protection.
- **[`docs/testing.md`](./docs/testing.md)** — Unit, integration, and end-to-end (`e2e/`) test setup, `prek` + `golangci-lint`, and GitHub Actions CI.
- **[`docs/release.md`](./docs/release.md)** — Commit message convention (Conventional Commits), release process via release-please, and Docker image publishing to GitHub Packages.
- **[`plans/multi-account.md`](./plans/multi-account.md)** — Design record for multi-account IMAP/SMTP support: `MINICRM_ACCOUNT_<N>_*` configuration, OIDC group → account mapping, per-account threading/attachments/UI, and unassigned-thread handling.

---

## Commit Message Convention

All commits must follow [Conventional Commits](https://www.conventionalcommits.org/):
`<type>(<scope>)!: <description>` with types such as `feat`, `fix`, `docs`,
`refactor`, `test`, `chore`, `ci`. `feat` bumps the minor version, `fix` the
patch, and `!`/`BREAKING CHANGE:` the major. Messages are enforced locally by
the commitlint hook in `.pre-commit-config.yaml` (via `prek`) and in CI on pull
requests. Releases are automated: see [`docs/release.md`](./docs/release.md).

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
   - The `Sent` folder is synchronized after `INBOX` (attach-only; never creates threads, never reopens `completed` threads); MiniCRM's own replies are de-duplicated by `Message-ID`, which SMTP servers must preserve.
   - Every `imap_jobs` row carries the `account_id` whose IMAP server must execute it; never route a job to a different account's connection.
3. **No `updated_at` Columns — Immutable Audit Trail & Structured Logging**:
   - Do not add `updated_at` columns to tables. Record all user/login/settings changes in `audit_logs` and all thread changes in `thread_events` via `internal/audit/logger.go`.
   - Every data read (`data_access`) and mutation (`data_change`) must be logged via `log/slog` (`internal/db/sqlc/tracer.go` and `internal/audit/logger.go`).
   - Store all timestamps in UTC (`TIMESTAMPTZ`) and format them in the UI using `system_settings.display_timezone` (`views.FormatTimestamp`).
4. **Untrusted HTML & HTTP Security (`internal/mail/mime_parser.go`, `internal/web/security_headers.go`)**:
   - Never render email HTML directly in the main DOM; always sanitize with `mail.SanitizeEmailHTML` and serve inside the isolated `<iframe sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer">` via `/emails/{id}/body` (with `allow-scripts` omitted and `script-src 'none'`).
   - Keep `SecurityHeadersMiddleware`, `http.NewCrossOriginProtection()`, and `http.MaxBytesReader` form bounds enabled on all routes.
5. **Multi-Account Isolation (`internal/db/migrations/002_multi_account.sql`, `internal/web/`)**:
   - Mail accounts are configured via numbered `MINICRM_ACCOUNT_<N>_*` env vars; each account maps an OIDC group (`OIDC_GROUP`) to its `ID`, and `users.allowed_accounts` is derived from the ID token's groups claim on every login.
   - Every `threads`/`emails`/`imap_jobs`/`imap_sync_state` row is scoped by `account_id`, and `emails` uniqueness is `(account_id, message_id)` so one email delivered to several accounts yields one thread per account. Thread matching (`internal/threading`) must always filter by `account_id`.
   - All thread, email-body (`/emails/{id}/body`), and attachment (`/attachments/{id}/view`, `/attachments/{id}/download`) access must verify `sqlc.User.HasAccountAccess(...)` of the owning account (`404` otherwise), even when attachments share a content-addressed blob.
   - A thread may only be assigned to (or default-assigned to) a user with access to its account; threads without a configured default assignee stay unassigned (`assignee_id IS NULL`) until one is set or an operator takes them.

---

## Verification Commands

Always run both commands before finishing work or committing:

```bash
golangci-lint run ./...
go test -v -race ./...
```
