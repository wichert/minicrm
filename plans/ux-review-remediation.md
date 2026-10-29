# UX Review Remediation — Plan Index

The remediation is split into independently executable plans, one per agent:

1. **[`plans/ux-draft-persistence.md`](./ux-draft-persistence.md)** — Workstream 1: reply draft persistence (P1).
2. **[`plans/ux-triage-workflow.md`](./ux-triage-workflow.md)** — Workstreams 2 & 3: complete new threads directly, bulk spam / multi-select triage (P1).
3. **[`plans/ux-queue-discoverability.md`](./ux-queue-discoverability.md)** — Workstreams 4, 5 & 6: customer history, queue assignment, pagination/search (P2). WS5 depends on the bulk-action UI from WS3 (plan 2).
4. **[`plans/ux-content-and-mobile.md`](./ux-content-and-mobile.md)** — Workstreams 7, 8 & 9: remote-image consent, reply formatting, mobile tab affordance (P2/P3). WS8's draft integration depends on plan 1.

---

Source: `docs/ux-review-user-stories.md`. Ordered per the review's recommended priority; each workstream is a shippable unit with its own tests. All work must respect the invariants in `AGENTS.md` (workflow state machine, IMAP folder lifecycle, immutable audit via `thread_events`/`audit_logs`, HTML sanitization/sandboxing, multi-account isolation, no `updated_at` columns).

## Workstream 1 — Draft persistence (P1, do first)

**Problem:** unsaved reply text is silently lost on navigation/refresh; no save control or indicator.

**Changes**
- Migration: new `reply_drafts` table — one draft per thread: `thread_id (PK, FK)`, `account_id`, `body`, `updated_by_session`, `occurred_at`/recorded timestamps (append-only intent: store drafts as rows in a draft table; supersede by insert, no `updated_at` on existing tables).
- Handler: `PUT /threads/{id}/draft` and `GET /threads/{id}/draft` with `HasAccountAccess` check; the draft belongs to the thread, not an individual author, so any authorized operator can pick up where another left off.
- Views: explicit **Save draft** button + autosave (debounced ~3 s after typing pause), visible **Saving / Saved / Could not save** feedback, and a "has draft" indicator in the inbox list.
- Restore draft into the textarea on thread open; **Discard draft** deletes it.
- `beforeunload` warning only when there are unsaved changes beyond the last successful autosave.
- On reply send or workflow transition that forbids replies, clear/supersede the draft; record the discard in `thread_events`.
- Concurrency: store a last-write session token; if another session saved since load, warn rather than silently overwrite.
- Draft storage is **MiniCRM-only** (no IMAP Drafts mirroring). One-way mirroring would desync as soon as a user edits the draft in an MUA — MUAs typically APPEND a brand-new draft message rather than update the existing one in place, making reliable two-way sync error-prone. Continuing a reply from a regular mail client is explicitly out of scope; users finish replies in MiniCRM.

**Tests:** save/restore across reload (web_test), access control (no cross-account draft read), send clears draft, save-failure path shows error.

## Workstream 2 — Complete a new thread directly (P1)

**Problem:** new threads require a misleading **Start working** step before completion.

**Changes**
- Workflow (`internal/workflow/service.go`): add audited composite op `CompleteFromNew` performing `new → in_progress → completed` atomically, writing both transitions to `thread_events`, enqueuing the normal `move_to_archive` IMAP job. State-machine invariants unchanged; this is a service-level composite, not a new direct transition.
- Views: show **Mark complete** on `new` threads (with the existing confirmation); keep **Start working** for genuinely starting work.

**Tests:** state machine unit tests for the composite; IMAP job enqueued exactly once; `thread_events` records both transitions.

## Workstream 3 — Bulk spam / multi-select triage (P1)

**Problem:** spam handling is one thread at a time with repeated navigation; no recovery out of `spam`.

**Changes**
- Views: accessible multi-select on desktop table and mobile cards (checkbox + "select all on page"), selected-count bar, bulk **Mark as spam** with confirmation showing count + scope + account.
- Handler: `POST /threads/bulk-spam` — server-side revalidation per thread: status must be `new` or `in_progress`, account access via `HasAccountAccess`; enqueue per-thread `move_to_junk` jobs on the owning account's connection. Return per-thread success/failure summary; do not claim IMAP move completion, only queueing.
- Redirect back to the same filtered list with a persistent summary notice (n succeeded, m failed).
- Spam recovery (prerequisite for reducing single-thread confirmation friction): deliberate `spam → in_progress` "Not spam" action via the same audited composite pattern, with `restore_from_junk` IMAP job. Only after this ships, downgrade the single-thread confirmation to a lighter two-step.
- Record every bulk transition as individual `thread_events` rows.

**Tests:** per-thread failure cases (already spam, wrong account, wrong status), job routing to correct account, state-machine legality, list filter preserved on redirect.

## Workstream 4 — Customer history discoverable (P2)

**Problem:** no way to see a customer's other threads from a conversation.

**Changes**
- Views: **Previous conversations** section in thread detail header — other threads matching the customer email address, showing subject, status, date, account; only accounts the user can access (filter by `allowed_accounts`); results link to those threads.
- Inbox: add customer/subject search box to filters.
- Search scope (multi-account): every search query unions across **all accounts in the user's `allowed_accounts`**, mirroring how the inbox list already filters; rows carry their account badge. Customers with the same address in several accounts appear grouped by account; users never see counts or rows from inaccessible accounts.
- Label the match plainly: "Other conversations from this email address" (do not imply a resolved customer identity; aliases noted in docs).
- Reply composer state (including draft) is untouched when consulting history — history opens in the same page or returns without loss (relies on Workstream 1).

**Tests:** matching by email only, inaccessible-account threads excluded, access-control test.

## Workstream 5 — Assignment from the queue + honest reply-assignment wording (P2)

**Depends on Workstream 3:** row-level checkboxes and the bulk-action bar (selection state, count display, per-item failure summary) are the components built in WS3; WS5 adds the assign action to that bar and reuses the per-row assignment controls.

**Problem:** assignment requires per-thread detail visits; the reply helper text overstates when assignment happens.

**Changes**
- Views: assignee dropdown (or **Assign to me**) directly on list rows/cards, plus optional bulk assignment with per-item failure feedback.
- Server: existing assignment endpoint reused per thread; enforce account access for each candidate assignee; record `assigned`/`reassigned` `thread_events`.
- `internal/mail/smtp_sender.go` + views helper text: change to "If this thread is unassigned, sending will assign it to you." (matches current behavior).

**Tests:** bulk assign with mixed accessible/inaccessible assignees; helper text behavior covered in smtp_sender_test.

## Workstream 6 — Pagination and search beyond 100 results (P2)

**Problem:** list hard-capped at 100 (`threadListLimit` in `internal/web/handlers_threads.go:75`) with no way to reach older work.

**Changes**
- Replace the cap with cursor-based pagination (`?before=<occurred_at>&<id>` keyset, stable under inserts) or load-more; keep a configurable page size.
- Show result-count/remaining context where cheap (count query).
- Add customer + subject full-text/ILIKE search to the inbox query.
- Preserve filters, search and page position when returning from a thread (return URL round-trip).

**Tests:** >100-thread fixture, keyset stability with concurrent inserts, filter+search combination.

## Workstream 7 — Blocked remote images: notice + consent to load (P2)

**Problem:** blocked remote images render as broken icons with no explanation or load option.

**Changes**
- Sanitizer (`internal/mail/mime_parser.go`): rewrite remote `<img src>` to a placeholder carrying the original URL in a data attribute; render an accessible notice: "Remote images blocked for your privacy" with **Load images for this message** and a note that the sender may learn the message was opened.
- Loading route: server-side image proxy (`/images/proxy?...`) — strictly allow-listed: SSRF protection (no private/loopback/link-local targets), redirects capped/disallowed to private ranges, size cap, content-type allow-list (image/*), timeout, no caching of arbitrary content; response served only to users with access to the owning email/account.
- Keep sanitizer, sandbox, CSP (`script-src 'none'`, no `allow-scripts`) and account checks fully intact; per-message consent only (no global opt-out).

**Tests:** SSRF unit tests (private IPs, redirects), size/type rejection, access control, sanitizer placeholder round-trip.

## Workstream 8 — Reply formatting: bold/italic with text fallback (P2)

**Problem:** replies are plain text only (`text/plain` in `internal/mail/smtp_sender.go`).

**Changes**
- Composer: **reuse an existing editor rather than building one** — integrate a small, maintained rich-text library (candidates: TipTap or a minimal ProseMirror setup) configured to only bold/italic, keyboard shortcuts Cmd/Ctrl+B/I, visible formatting state, and a plain-text fallback toggle. No new heavyweight dependency beyond that single editor package; evaluate bundle size and accessibility before committing.
- Send as `multipart/alternative`: `text/plain` part (existing content) + restricted HTML part generated server-side from the editor's limited node set (whitelist b/i/br/p/a only; escape everything else) — no raw user HTML.
- Sent copies stored/APPENDed as today and continue through existing sanitization/isolation when rendered.
- Draft storage extended to carry formatting state.

**Tests:** html-generation whitelist tests (XSS attempts reduced to text), multipart structure, rendering of received copies through sanitizer.

## Workstream 9 — Mobile status-tab overflow affordance (P3)

**Changes**
- Add scroll fade/arrow affordance on the horizontal status strip; ensure keyboard focus scrolls the active tab into view (`scrollIntoView` on focus, no touch-target shrink).
- Validate on real devices + keyboard/screen-reader before finalizing (per review's caveat).

**Tests:** manual + e2e check at 390 px; focus-visible test.

## Sequencing & validation

1. Ship 1 → 2 → 3 (P1 block: prevents lost work, then triage friction).
2. Ship 4 → 5 → 6 (P2 discoverability/queue).
3. Ship 7 → 8 → 9.
4. After each workstream: `golangci-lint run ./... && go test -race ./...`; e2e tests under `e2e/` for the P1 flows.
5. Final pass: run the review's suggested validation scenarios (mixed-queue triage timing, two-operator draft test, cross-account history visibility, 100+-result inbox) and update `docs/workflow-and-threading.md`, `docs/storage.md`, and `docs/security.md` for the composite transition, draft table, and image proxy respectively.

Each feature commit follows Conventional Commits (`feat(inbox): ...`, `fix(mail): ...`).