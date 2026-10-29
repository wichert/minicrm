# Plan 3 — Queue Discoverability: Customer History, Assignment, Pagination (P2)

Source: `docs/ux-review-user-stories.md`, Workstreams 4, 5 & 6.

**Constraints (from `AGENTS.md`):** multi-account isolation — every thread query filters by `account_id` and users only see accounts in their `allowed_accounts`; assignment changes recorded in `thread_events`.

**Dependency:** Workstream 5 depends on the row-checkbox / bulk-action bar UI built in Plan 2 (`plans/ux-triage-workflow.md`, Workstream 3). Workstreams 4 and 6 are independent of it.

## Workstream 4 — Customer history discoverable (P2)

**Problem:** no way to see a customer's other threads from a conversation.

**Changes**
- Views (`internal/web/views/views.go`): **Previous conversations** section in thread detail header — other threads matching the customer email address, showing subject, status, date, account; only accounts the user can access (filter by `allowed_accounts`); results link to those threads.
- Inbox: add customer/subject search box to filters.
- Search scope (multi-account): every search query unions across **all accounts in the user's `allowed_accounts`**, mirroring how the inbox list already filters; rows carry their account badge. Customers with the same address in several accounts appear grouped by account; users never see counts or rows from inaccessible accounts.
- Label the match plainly: "Other conversations from this email address" (do not imply a resolved customer identity; aliases noted in docs).
- Reply composer state is untouched when consulting history — history opens in the same page or returns without loss (relies on Plan 1 draft persistence).

**Tests:** matching by email only, inaccessible-account threads excluded, access-control test.

## Workstream 5 — Assignment from the queue + honest reply-assignment wording (P2)

**Depends on Plan 2 (WS3):** row-level checkboxes and the bulk-action bar (selection state, count display, per-item failure summary); WS5 adds the assign action to that bar and reuses the per-row assignment controls.

**Problem:** assignment requires per-thread detail visits; the reply helper text overstates when assignment happens (`internal/mail/smtp_sender.go`).

**Changes**
- Views: assignee dropdown (or **Assign to me**) directly on list rows/cards, plus optional bulk assignment with per-item failure feedback.
- Server: existing assignment endpoint reused per thread; enforce account access for each candidate assignee; record `assigned`/`reassigned` `thread_events`.
- `internal/mail/smtp_sender.go` + views helper text: change to "If this thread is unassigned, sending will assign it to you." (matches current behavior).

**Tests:** bulk assign with mixed accessible/inaccessible assignees; helper text behavior covered in `internal/mail/smtp_sender_test.go`.

## Workstream 6 — Pagination and search beyond 100 results (P2)

**Problem:** list hard-capped at 100 (`threadListLimit` in `internal/web/handlers_threads.go:75`) with no way to reach older work.

**Changes**
- Replace the cap with cursor-based pagination (`?before=<occurred_at>&<id>` keyset, stable under inserts) or load-more; keep a configurable page size.
- Show result-count/remaining context where cheap (count query).
- Add customer + subject full-text/ILIKE search to the inbox query (shared with WS4's search box).
- Preserve filters, search and page position when returning from a thread (return URL round-trip).

**Tests:** >100-thread fixture, keyset stability with concurrent inserts, filter+search combination.

**Verification:** `golangci-lint run ./... && go test -race ./...`; update `docs/workflow-and-threading.md` (assignment wording) if behavior text lives there.

**Commit style:** Conventional Commits, e.g. `feat(inbox): customer history panel`, `feat(inbox): keyset pagination and search`.