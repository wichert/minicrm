# Sent-Folder Sync: Capture Replies Sent From Other Mail Clients

## Context

MiniCRM only reads each account's `INBOX` (`IMAPSyncWorker.SyncOnce`, `internal/mail/imap_sync.go`). The `Sent` folder is write-only: after MiniCRM sends a reply via SMTP it queues `append_to_sent` to store a copy there.

When a team member answers a customer from another mail client (Thunderbird, Apple Mail, a phone, webmail) the reply lands in the shared mailbox's `Sent` folder, and MiniCRM never sees it. Consequences observed/expected today:

1. **The thread has a gap.** The customer's next message references a Message-ID MiniCRM doesn't know, so header matching fails and threading falls back to subject + sender. It usually still lands in the right thread, but the conversation is missing the team's answer.
2. **Status and attention indicators are wrong.** A `new` thread stays `new` and keeps showing a growing "Waiting 13d" badge even though somebody answered.
3. **The inbox snippet and `last_message_at` are stale**, because the team's answer isn't part of the thread.

Even if `Sent` were polled today, `Matcher.ResolveThread` could not handle it: it derives the *customer* from the message's `From`, creates `new` threads for unmatched messages, and `ingestOne` hard-codes `Direction: "inbound"`.

## Goals

- Replies sent from another client appear in the correct MiniCRM thread as **outbound** ("Team reply") messages, in chronological order.
- A reply sent externally counts as "somebody answered": it clears the "waiting" state of a `new` thread.
- Messages MiniCRM itself sent are **never duplicated**.
- The UI's age / last-activity indicators reflect replies sent from another client (see "Age & last-activity" below).
- If a customer replies to an outbound mail that started *outside* MiniCRM, the original outbound mail is pulled in so the new MiniCRM thread contains the whole exchange.
- Zero risk of spurious threads, spurious reopen/merge banners, or status regressions from `Sent` content.

## Non-Goals

- Importing arbitrary outbound mail that nobody replied to (new outbound conversations, newsletters, forwards). It stays in the mailbox and is ignored unless a customer replies to it (see "Customer replies to mail first sent outside MiniCRM").
- Syncing other folders (Drafts, Archive, Junk) or any other IMAP flag changes.
- Attributing external replies to a specific MiniCRM user (mail clients don't carry that identity reliably).

## Invariants To Preserve (from `AGENTS.md`)

- **Strict workflow state machine**: only `new -> in_progress`, `new -> spam`, `in_progress -> completed` as direct transitions; replies to `completed` threads never reopen directly. An external reply must go through the existing `Transition` path (so audit events, IMAP job enqueueing, and validation stay in one place) and must not add new transitions.
- **IMAP folder lifecycle**: Archive/Junk moves are driven only by status transitions. Ingesting a `Sent` message must not enqueue folder moves except via a legitimate `new -> in_progress` transition (which enqueues none).
- **Audit trail**: no `updated_at`; every status/assignment change is a `thread_events` row via `internal/audit`; ingestion logged via `slog` `data_change`.
- **Multi-account isolation**: every query is scoped by `account_id`; `emails` uniqueness is `(account_id, message_id)`; sync state is `(account_id, mailbox)`.
- **Untrusted HTML**: bodies are sanitized by the same `ParseRFC5322` path; no new rendering paths.

## Design

### 1. Sync the Sent folder per account (always on, INBOX first)

- Syncing `Sent` is **mandatory and not configurable**: MiniCRM is opinionated that the shared mailbox's `Sent` folder is part of the conversation. There is no `IMAP_SYNC_SENT` option.
- Extend `IMAPSyncWorker.SyncOnce` so that each pass **always completes the INBOX pass first and only then runs the Sent pass**, on the same connection. Reason: a sent reply is matched to the thread created from the customer's incoming mail, so the incoming mail must already be ingested (otherwise an outgoing reply could be ignored because its parent has not been seen yet). If the INBOX pass fails, the Sent pass is skipped for that poll (it would only produce spurious "no thread" ignores) and the error is logged as today.
- The safety net for the remaining race (reply sent, customer message not yet visible, or older than the window) is the on-demand lookup in "Customer replies to mail first sent outside MiniCRM" below, plus the fact that ignored Sent messages are always re-discoverable by Message-ID.
- Folder name goes through the existing `resolveMailboxName` logic but **without creating** the folder when reading. If it doesn't exist, skip the Sent pass (debug log). MiniCRM already creates `Sent` on its first `append_to_sent`.
- Reuse the existing machinery: `ListUIDsSince`, batched `FetchMessagesByUID`, per-batch watermark in `imap_sync_state(account_id, mailbox)` (a second row with `mailbox = <Sent name>`), poison-message skipping, `UIDVALIDITY` reset handling.
- **First-sync window**: same `IMAP_INITIAL_SYNC_DAYS` cutoff as INBOX (default 60 days). A new account only imports recent sent history.
- Sent pass failures are logged (`imap_sync_error` with `mailbox=Sent`, account/server/user) and do not affect the already-completed INBOX pass.

### 2. Direction-aware, attach-only ingestion

New method `IMAPSyncWorker.ingestSentOne` (separate from `ingestOne` to keep inbound logic untouched).

Steps for each Sent message:

1. Parse with `ParseRFC5322` (same sanitization). Unparseable -> skip + warn (existing behaviour).
2. **Dedupe by `(account_id, message_id)`** (`GetEmailByMessageID`). This is what stops MiniCRM's own replies from being duplicated: they were stored in PostgreSQL at send time with the same Message-ID and then `APPEND`ed to Sent. When the row exists, **backfill** `imap_mailbox`, `imap_uidvalidity` and `imap_uid` onto it if they are still NULL (new query `SetEmailIMAPLocation`, guarded by `WHERE imap_uid IS NULL`; logged as a `data_change`) and return. This lets later folder moves/cleanup address MiniCRM's own replies like any other message.
3. **Resolve the thread, attach-only** via a new `Matcher.ResolveOutboundThread(ctx, msg)`:
   - Header match: `In-Reply-To` / `References` -> `FindEmailByMessageIDs` (account-scoped). If found, use that email's thread.
   - Fallback (no headers, e.g. some clients strip them): `FindThreadBySubjectAndParticipant` using the **recipient** (first `To` address that equals a thread `customer_email`) instead of `From`, same subject window. Needs a small query variant or a parameter so the participant is the customer address, not the sender. Fallback is only used when the normalized subject is non-empty and distinct from "(No Subject)".
   - **No match -> ignore the message** (counted as `skipped_no_thread`, debug log). The Sent pass never creates a thread itself; ignored messages remain retrievable on demand (see below).
4. **Thread-status handling** (never reopen, never create):

   | Matched thread status | Action |
   |---|---|
   | `new` | Attach, then transition `new -> in_progress` through `workflow.Service.Transition` (actor = system). This also clears the "Waiting" badge, which is only rendered for `new` threads |
   | `in_progress` | Attach only |
   | `completed` | Attach silently as a historical message **without** changing status (the thread already moved to Archive; a late external reply must not resurrect it). No banner, no notice |
   | `spam` | Ignore (don't attach; spam threads are dead ends) |

5. Create the `emails` row: `Direction: "outbound"`, `ParentEmailID` from the header match, `FromAddress/FromName` from the message, `ToAddresses/CCAddresses`, `SentAt` from the `Date` header, `IMAPMailbox/UIDValidity/UID` set to the Sent folder location, attachments saved through the existing content-addressed store.
6. `TouchThreadLastMessageAt` only moves forward (`GREATEST` already) using the message's `SentAt`, clamped to "now" so a client with a skewed clock cannot push a thread into the future. This is what makes the age/last-activity UI reflect the external reply (see below).
7. Record a `thread_events` row `reply_sent_external` (new event type; no schema change since `event_type` is text) with `message_id` and `from`. `FormatThreadEventSummary` renders it as "A reply was sent from another mail client."
8. Emit a `slog` `data_change` (`resource_type=outbound_email`, `source=imap_sent`, account/server/user attrs).

### 3. Assignment

External replies do **not** assign the thread. All team members are likely to reply from the same shared mailbox address, so the `From`/`Sender` of a sent message cannot identify the person. If the thread is unassigned and moves to `in_progress`, it stays unassigned and shows "Unassigned" in the inbox, which is an honest signal that someone needs to claim it.

### 4. Matching hygiene for later inbound mail

Because external replies are now stored with their Message-ID, a customer's follow-up that references them now matches by header (step B.1) to the correct thread and parent email. This fixes the "gap" threading symptom with no extra code in the inbound path.

### 5. Age & last-activity reflect external replies

- The inbox's "Age" column shows relative `last_message_at` for every non-`new` thread; the thread header, the inbox ordering (`ORDER BY last_message_at DESC`) and the "You:" snippet (latest message's direction) all derive from the same data. Because step 6 above advances `last_message_at` and the latest-message snippet is read from the newest `emails` row, a reply sent from another MUA automatically refreshes the age, re-sorts the thread and flips the snippet to "You: ...".
- For `new` threads the "Waiting" badge is derived from the first inbound message and disappears when the thread leaves `new`, which the external reply triggers via the status transition.
- Tests assert all three (age text, ordering, snippet prefix, no waiting badge) after ingesting an external reply.
- Completed threads that receive a silent late reply also get a refreshed `last_message_at`, but remain in the Completed tab.

### 6. Customer replies to mail first sent outside MiniCRM

Scenario: someone starts a conversation from another client (no MiniCRM thread exists), the customer replies. Today the reply would create a thread containing only the customer's message.

- The Sent pass still ignores unmatched outbound mail (nothing creates threads from `Sent`), so no extra data is stored for it.
- When the **INBOX** pass ingests a message whose `In-Reply-To`/`References` match no known email in the account (and subject fallback also finds no thread), `ingestOne` performs an **on-demand lookup in the Sent folder** for the most recent candidate IDs (cap: 3 per message): `UID SEARCH HEADER Message-ID <id>`, fetch the match, and parse it.
- If found, the original outbound mail is ingested first as `Direction: "outbound"` into a **new thread** created by a new `Matcher.CreateThreadFromOutbound`: customer = the outbound's recipient, subject from the outbound, status `new` (the customer's reply is what needs attention, so the usual "needs a response" semantics apply), assignment via the normal default-assignee resolver, `created` thread event. Then the customer's reply is resolved normally and now header-matches the outbound mail, so the thread contains both messages in order.
- This lookup is not limited by `IMAP_INITIAL_SYNC_DAYS`; it runs per reply and is capped, so it cannot trigger a mailbox download. If the Sent folder doesn't exist, the lookup is skipped.
- Outbound mail with multiple recipients: the customer is the first `To` address that is not a configured account address; the other recipients remain stored in `to_addresses`/`cc_addresses`.
- One level only: the lookup does not recurse into the outbound mail's own `In-Reply-To`.

### 7. Requirement: Message-ID must be preserved (documented)

De-duplication of MiniCRM's own replies relies **solely on Message-ID**. This is documented as an operator requirement in `docs/imap.md` and `README.md`:

> The SMTP server must send MiniCRM replies with the `Message-ID` MiniCRM generated, and any copy stored in `Sent` (MiniCRM's own `APPEND` or a server-side copy) must carry the same `Message-ID`. Servers or relays that rewrite `Message-ID` will cause MiniCRM's own replies to be imported a second time from `Sent`.

No heuristic (body-hash/time-window) de-duplication is implemented.

### 8. UI

- Messages already render by `Direction`, so external replies show as "Team reply" with the existing outbound styling. Show the sender as the mail's `From` name/address (it will be the shared mailbox identity or an alias).
- Add a small muted "sent from another client" hint on messages whose email has no `outbox_emails` row (derivable without a schema change), so operators understand why no MiniCRM user is attached. Optional polish.
- Inbox snippet shows "You:" for the latest outbound message automatically (existing `LatestDirection`).

## Edge Cases

| Case | Behaviour |
|---|---|
| MiniCRM-sent reply copied to Sent by MiniCRM's `APPEND` | Dedupe by Message-ID; ignored |
| SMTP provider also saves its own copy to Sent with a **rewritten** Message-ID | Unsupported; documented requirement (section 7). Would import as a second outbound email |
| User replies from another client to a thread that is `completed` | Attached historically, status unchanged |
| User sends a brand-new mail (no matching thread) | Ignored by the Sent pass; pulled in later only if the customer replies (section 6) |
| Sent message processed before the matching customer mail | Prevented by INBOX-first ordering; residual cases resolved by the on-demand lookup |
| Reply to a customer who has two open threads with the same subject | Header match wins; subject fallback picks the most recent (existing behaviour) |
| `Sent` folder missing / unreadable | INBOX still syncs; Sent pass skipped with a throttled warning |
| `UIDVALIDITY` change on Sent | Reset watermark, rely on Message-ID dedupe (same as INBOX) |
| Huge historic Sent folder | Bounded by `IMAP_INITIAL_SYNC_DAYS` and batching |
| Reply drafted in MiniCRM *and* sent externally by a second person | Two outbound emails; accepted, both are real messages |

## Decisions (from review)

1. **Status on external reply**: move `new -> in_progress` **and** clear the waiting badge.
2. **Completed threads**: attach silently, no status change, no notice.
3. **Assignment**: leave unassigned; the sender address cannot identify the user because everyone replies from the same address.
4. **Duplicate protection**: Message-ID only, documented as a hard requirement (section 7).
5. **MiniCRM-sent emails**: backfill `imap_mailbox`/`imap_uid`/`imap_uidvalidity` when met in `Sent`.
6. **Configuration**: Sent syncing is mandatory; no option.
7. **Brand-new outbound mail**: ignored, but pulled in (with a new thread) if the customer replies (section 6).
8. **Sync order**: INBOX before Sent, always.
9. **Age/last-activity**: must reflect external replies (section 5).

## Files To Modify

- `internal/mail/imap_sync.go` — INBOX-then-Sent ordering in `SyncOnce`, Sent pass, `ingestSentOne`, on-demand `findSentByMessageID` lookup used by `ingestOne`, shared batch loop extracted from the INBOX path; `IMAPClient` interface gains `SearchByMessageID`.
- `internal/threading/matcher.go` (+ `matcher_test.go`) — `ResolveOutboundThread` (attach-only, status-aware) and `CreateThreadFromOutbound`.
- `internal/db/migrations/003_external_reply_event.sql` — allow the `reply_sent_external` thread event.
- `internal/db/queries/queries.sql`, `internal/db/sqlc/queries.go` — `SetEmailIMAPLocation` (backfill). The existing subject+participant query is reused with the recipient as participant.
- `internal/workflow/service.go` — expose/confirm a system-actor `new -> in_progress` path for external replies (reuse `Transition`; no new transitions).
- `internal/web/views/views.go` — `reply_sent_external` event text; optional "other client" hint. `internal/web/web_test.go` — age/ordering/snippet/badge assertions.
- `internal/mail/imap_workers_test.go` — fake IMAP server gets a `Sent` mailbox with fixtures.
- `docker/mail/seed/*/` — Sent-folder fixtures (Maildir `.Sent` or `INBOX.Sent` depending on the dovecot layout) with an external reply and a MiniCRM-style duplicate.
- Docs: `docs/imap.md` (incl. the Message-ID requirement), `docs/workflow-and-threading.md`, `README.md`, `AGENTS.md` index note.

## Steps

_Status: implemented (all steps). Verified against the dev Dovecot + PostgreSQL stack._

- [x] **Step 1: Plumbing** — `IMAPClient.SearchByMessageID`, Sent-mailbox resolution without creation, per-mailbox `imap_sync_state` row, `SetEmailIMAPLocation` query, INBOX-then-Sent ordering in `SyncOnce`.
- [x] **Step 2: Matcher** — `ResolveOutboundThread` (header match, recipient-based fallback, status table) and `CreateThreadFromOutbound`; unit tests per status and match path.
- [x] **Step 3: Sent ingestion** — `ingestSentOne`, Message-ID dedupe with IMAP-location backfill, attachments, `reply_sent_external` event, `new -> in_progress` via workflow service, clamped `last_message_at` update; extract the shared batch loop from the INBOX path.
- [x] **Step 4: Outbound-first threads** — on-demand Sent lookup in `ingestOne` for replies with unknown parents; create thread from the original outbound, then attach the customer's reply.
- [x] **Step 5: UI** — event summary, optional "sent from another client" hint, and tests for age text, ordering, "You:" snippet and absent waiting badge.
- [x] **Step 6: Fixtures** — fake IMAP Sent mailbox + dovecot seed fixtures: external reply to a `new` thread, reply to a `completed` thread, duplicate of a MiniCRM-sent message, unrelated outbound mail, outbound mail with a customer reply in INBOX.
- [x] **Step 7: Tests** — no duplicate of MiniCRM-sent mail (and IMAP location backfilled); external reply attaches and moves `new -> in_progress`; completed stays completed; spam ignored; no thread created from Sent alone; INBOX-first ordering; Sent failure does not affect INBOX; first-sync cutoff applies; outbound-first reply produces a two-message thread.
- [x] **Step 8: Docs & verification** — update docs including the Message-ID requirement; run `golangci-lint run ./...` and `go test -v -race ./...`; manual check against the dev IMAP container, then a real account with a reply sent from a phone/Thunderbird.

## Verification

1. Automated: `golangci-lint run ./...` and `go test -v -race ./...`.
2. Dev stack: seed an external reply in the Sent fixtures; confirm it appears as a "Team reply" in the right thread, the thread leaves the "Waiting" state, its age and position in the inbox update, and re-syncing creates no duplicates.
3. Regression: send a reply from MiniCRM itself and confirm that after the next Sent pass there is still exactly one outbound email for it.
4. Real mailbox: reply from another client to a `new` thread; expect status `in_progress`, the message in the thread, and the inbox snippet prefixed "You:".
