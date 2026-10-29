# Plan 2 — Triage Workflow: Direct Completion + Bulk Spam (P1)

Source: `docs/ux-review-user-stories.md`, Workstreams 2 & 3.

**Constraints (from `AGENTS.md`):** strict workflow state machine in `internal/workflow/service.go` (only `new → in_progress`, `new → spam`, `in_progress → completed` as direct transitions); `new → spam` enqueues `move_to_junk` (INBOX → Junk); every `imap_jobs` row carries the owning `account_id`; all transitions recorded in `thread_events`.

## Workstream 2 — Complete a new thread directly (P1)

**Problem:** new threads require a misleading **Start working** step before completion (`internal/web/views/views.go`, `internal/workflow/service.go`).

**Changes**
- Workflow (`internal/workflow/service.go`): add audited composite op `CompleteFromNew` performing `new → in_progress → completed` atomically, writing both transitions to `thread_events`, enqueuing the normal `move_to_archive` IMAP job. State-machine invariants unchanged; this is a service-level composite, not a new direct transition.
- Views: show **Mark complete** on `new` threads (with the existing confirmation); keep **Start working** for genuinely starting work.

**Tests:** state machine unit tests for the composite; IMAP job enqueued exactly once; `thread_events` records both transitions.

## Workstream 3 — Bulk spam / multi-select triage (P1)

**Problem:** spam handling is one thread at a time with repeated navigation; no recovery out of `spam`.

**Changes**
- Views (`internal/web/views/views.go`, `internal/web/handlers_threads.go`): accessible multi-select on desktop table and mobile cards (checkbox + "select all on page"), selected-count bar, bulk **Mark as spam** with confirmation showing count + scope + account.
- Handler: `POST /threads/bulk-spam` — server-side revalidation per thread: status must be `new` or `in_progress`, account access via `HasAccountAccess`; enqueue per-thread `move_to_junk` jobs on the owning account's connection. Return per-thread success/failure summary; do not claim IMAP move completion, only queueing.
- Redirect back to the same filtered list with a persistent summary notice (n succeeded, m failed).
- Spam recovery (prerequisite for reducing single-thread confirmation friction): deliberate `spam → in_progress` "Not spam" action via the same audited composite pattern as `CompleteFromNew`, with `restore_from_junk` IMAP job. Only after this ships, downgrade the single-thread confirmation to a lighter two-step.
- Record every bulk transition as individual `thread_events` rows.

**Tests:** per-thread failure cases (already spam, wrong account, wrong status), job routing to correct account, state-machine legality, list filter preserved on redirect.

**Verification:** `golangci-lint run ./... && go test -race ./...`; e2e test for the bulk flow under `e2e/`; update `docs/workflow-and-threading.md` for the composite operations.

**Commit style:** Conventional Commits, e.g. `feat(workflow): complete new thread as one action`, `feat(inbox): bulk mark-as-spam with multi-select`.