# Plan 1 — Reply Draft Persistence (P1)

Source: `docs/ux-review-user-stories.md`, Workstream 1. Execute independently of the other UX plans, though Plan 4 (reply formatting) extends this draft storage later.

**Problem:** unsaved reply text is silently lost on navigation/refresh; no save control or draft indicator.

**Constraints (from `AGENTS.md`):** immutable audit trail (`thread_events` via `internal/audit/logger.go`), no `updated_at` columns, multi-account isolation (`HasAccountAccess` on every read/write), UTC `TIMESTAMPTZ` timestamps.

**Changes**
- Migration: new `reply_drafts` table — one draft per thread: `thread_id (PK, FK)`, `account_id`, `body`, `updated_by_session`, `occurred_at`/recorded timestamps (append-only intent: store drafts as rows in a draft table; supersede by insert, no `updated_at` on existing tables).
- Handler: `PUT /threads/{id}/draft` and `GET /threads/{id}/draft` with `HasAccountAccess` check; the draft belongs to the thread, not an individual author, so any authorized operator can pick up where another left off.
- Views (`internal/web/views/views.go`): explicit **Save draft** button + autosave (debounced ~3 s after typing pause), visible **Saving / Saved / Could not save** feedback, and a "has draft" indicator in the inbox list.
- Restore draft into the textarea on thread open; **Discard draft** deletes it.
- `beforeunload` warning only when there are unsaved changes beyond the last successful autosave.
- On reply send or workflow transition that forbids replies, clear/supersede the draft; record the discard in `thread_events`.
- Concurrency: store a last-write session token; if another session saved since load, warn rather than silently overwrite.
- Draft storage is **MiniCRM-only** (no IMAP Drafts mirroring). One-way mirroring would desync as soon as a user edits the draft in an MUA — MUAs typically APPEND a brand-new draft message rather than update the existing one in place, making reliable two-way sync error-prone. Continuing a reply from a regular mail client is explicitly out of scope; users finish replies in MiniCRM.

**Tests:** save/restore across reload (`internal/web/web_test.go`), access control (no cross-account draft read), send clears draft, save-failure path shows error.

**Verification:** `golangci-lint run ./... && go test -race ./...`; update `docs/storage.md` for the `reply_drafts` table.

**Commit style:** Conventional Commits, e.g. `feat(drafts): persist reply drafts per thread`.