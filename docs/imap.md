# IMAP & SMTP Mail Server Integration

This document specifies the requirements MiniCRM places on the mail server and details how MiniCRM synchronizes incoming mail, moves messages across IMAP folders, and sends outbound replies.

---

## 1. Mail Server Requirements

MiniCRM connects to one or more shared mailboxes (accounts) over standard **IMAP** and **SMTP** protocols. Accounts are configured via numbered `MINICRM_ACCOUNT_<N>_*` environment variables (`ID`, `NAME`, `OIDC_GROUP`, `IMAP_*`, `SMTP_*`). Each account has its own `INBOX` synchronization worker, its own `imap_sync_state` watermark (`PRIMARY KEY (account_id, mailbox)`), its own outbound SMTP identity, and its own `Archive`/`Junk`/`Sent` folder lifecycle.

Because `emails` uniqueness is `UNIQUE (account_id, message_id)` and thread matching is scoped by `account_id`, an email delivered to two accounts is ingested into both and produces two independent threads (one per account).

### IMAP Server Requirements
- **Protocol**: IMAPv4rev1 or IMAPv4rev2 (`github.com/emersion/go-imap/v2`).
- **Transport Security**:
  - Implicit TLS on port `993` (`MINICRM_IMAP_IMPLICIT_TLS=true`, default), or
  - `STARTTLS` on port `143` (`MINICRM_IMAP_IMPLICIT_TLS=false`).
  - Minimum TLS version: **TLS 1.2**.
- **Authentication**: Basic `username + password` (`LOGIN`).
- **Required IMAP Capabilities & Commands**:
  - `SELECT`, `CREATE`
  - `UID FETCH` with `BODY.PEEK[]` (to read raw RFC 5322 messages without implicitly mutating `\Seen` flags before database commit)
  - `UID MOVE` (RFC 6851) for moving messages from `INBOX` to `Archive` and `Junk`
  - `APPEND` for saving copies of outbound replies into `Sent`
- **Mailboxes (Folders)**:
  - Default folder names (all configurable via `/settings` in `system_settings`):
    - **`INBOX`** (`imap_inbox_mailbox`): Source folder for all incoming emails.
    - **`Archive`** (`imap_archive_mailbox`): Target folder for emails belonging to `completed` threads.
    - **`Junk`** (`imap_junk_mailbox`): Target folder for emails belonging to `spam` threads.
    - **`Sent`** (`imap_sent_mailbox`): Target folder where outbound SMTP replies are `APPEND`ed.
  - MiniCRM automatically issues an idempotent `CREATE <mailbox>` before moving or appending to `Archive`, `Junk`, or `Sent`.

### SMTP Server Requirements
- **Protocol**: SMTP / ESMTP (`github.com/wneessen/go-mail`).
- **Transport Security**: Configurable via `MINICRM_SMTP_TLS_MODE`:
  - `starttls` (default, port `587` with mandatory `STARTTLS`)
  - `implicit` (implicit TLS, typically port `465`)
  - `none` (unencrypted, only for local loopback test servers)
- **Authentication**: Basic `username + password` (`AUTH PLAIN`).

---

## 2. Incoming Email Synchronization (`IMAPSyncWorker`)

Implemented in `internal/mail/imap_sync.go`.

```text
IMAP Server (INBOX)                      IMAPSyncWorker                         PostgreSQL + Disk
        |                                       |                                       |
        |  1. SELECT "INBOX"                    |                                       |
        |<--------------------------------------|                                       |
        |     Returns UIDVALIDITY               |                                       |
        |-------------------------------------->|                                       |
        |                                       | 2. Read imap_sync_state("INBOX")      |
        |                                       |-------------------------------------->|
        |  3. UID FETCH <last_uid+1>:*          |                                       |
        |     (UID BODY.PEEK[])                 |                                       |
        |<--------------------------------------|                                       |
        |     Streams raw RFC 5322 messages     |                                       |
        |-------------------------------------->|                                       |
        |                                       | 4. For each message (ascending UID):  |
        |                                       |    - Parse MIME + sanitize HTML       |
        |                                       |    - Deduplicate by Message-ID        |
        |                                       |    - Resolve/create Thread            |
        |                                       |    - Save attachments to local disk   |
        |                                       |    - Insert emails & attachments      |
        |                                       |    - Update last_synced_uid           |
        |                                       |-------------------------------------->|
```

### Initial Sync Window, Batching & Bad Messages
- **Recent history only by default**: when an account has no watermark yet (or `UIDVALIDITY` changed), `SyncOnce` issues `UID SEARCH SINCE <today - N days>` and only ingests those messages. `N` is `MINICRM_ACCOUNT_<N>_IMAP_INITIAL_SYNC_DAYS` (default `60`). Set it to `0` to backfill the complete mailbox history (can take very long on large mailboxes). Once a watermark exists the cutoff is no longer applied.
- **Batched download**: matching UIDs are fetched in batches of 20 (`UID FETCH <uids> BODY.PEEK[]`), ingested, and the `imap_sync_state` watermark is saved after every batch, so memory stays bounded, progress is visible (`imap_initial_sync_started` / `imap_sync_completed` logs), and a failure never re-downloads completed work.
- **Poison messages**: a message that cannot be parsed (or is empty) is logged as `imap_message_skipped` (with account, server, username, UID and reason) and skipped; the watermark advances past it so it cannot block the mailbox. Truncated IMAP literals fail the fetch instead of being parsed.

### Replies Sent From Other Mail Clients (`Sent` Folder Sync)
Each sync pass first completes the `INBOX` pass and **then** reads the `Sent` folder (resolved with the same namespace logic as Archive/Junk, but never created while reading; a missing `Sent` folder is skipped). The `Sent` folder has its own `imap_sync_state` row and the same batching, first-sync window (`IMAP_INITIAL_SYNC_DAYS`) and bad-message skipping. This is always on and not configurable.

For every message found in `Sent` (`ingestSentOne`):
1. **Already known (MiniCRM's own replies)**: deduplicated by `(account_id, Message-ID)`. The existing row gets its missing `imap_mailbox`/`imap_uidvalidity`/`imap_uid` backfilled; nothing else changes.
2. **Attach-only matching**: `In-Reply-To`/`References` header match, else normalized subject + a recipient that is a thread's customer. If no thread matches (or the thread is `spam`) the message is ignored. `Sent` never creates a thread.
3. **Attach**: stored as an `outbound` email ("Team reply"), `last_message_at` advances (never beyond *now*), a `reply_sent_external` thread event is recorded. A `new` thread moves to `in_progress` through the normal workflow transition (so the waiting badge clears); `in_progress` and `completed` threads keep their status (a `completed` thread is never reopened). The thread is **not** assigned: all team members typically send from the same address.

**Customer replies to mail first sent outside MiniCRM**: when an incoming message references a Message-ID that no thread knows, the worker searches the `Sent` folder for it (`SEARCH HEADER Message-ID`, up to 3 candidates, not limited by the initial-sync window). If found and addressed to the replying customer, it is ingested as the first (`outbound`) message of a new `new` thread (`created` event with `origin: outbound`), and the customer's reply then threads under it.

> **Requirement: Message-ID must be preserved.** De-duplication of MiniCRM's own replies relies solely on `Message-ID`. The SMTP server must send MiniCRM replies with the `Message-ID` MiniCRM generated, and every copy in `Sent` (MiniCRM's own `APPEND` or a server-side copy) must carry the same `Message-ID`. Servers or relays that rewrite `Message-ID` cause MiniCRM's own replies to be imported a second time from `Sent`.

### UID Watermark & `UIDVALIDITY` Handling
- MiniCRM stores `(mailbox, uid_validity, last_synced_uid)` in the `imap_sync_state` table for `INBOX`.
- On each sync pass (`SyncOnce`):
  1. **Normal Incremental Sync (`uid_validity` unchanged)**:
     MiniCRM fetches only messages with `UID > last_synced_uid` (`UID FETCH <last_synced_uid+1>:* (UID BODY.PEEK[])`).
  2. **Mailbox Rebuild (`uid_validity` changed)**:
     If the IMAP server rebuilt the mailbox and assigned a new `UIDVALIDITY`, MiniCRM resets `afterUID = 0` and scans from the start of `INBOX`. Because every message is deduplicated against `emails.message_id` (`UNIQUE`), previously ingested emails are skipped without creating duplicate threads or messages.
- If a malformed incoming email lacks a `Message-ID` header, `ParseRFC5322` synthesizes a deterministic ID (`sha256(sent_at | from | subject)@synthetic.minicrm`) so re-syncs remain idempotent.

---

## 3. Asynchronous Folder Actions (`IMAPActionWorker`)

Implemented in `internal/mail/imap_actions.go`.

All IMAP folder mutations triggered by user actions are decoupled from HTTP requests through the PostgreSQL `imap_jobs` table (`SELECT ... FOR UPDATE SKIP LOCKED`), ensuring that temporary IMAP server downtime never blocks or loses a workflow state change.

| Job Type | Trigger | IMAP Operation | Database Update |
|---|---|---|---|
| `move_to_junk` | Thread transitions `new -> spam` | `SELECT "INBOX"` -> `UID MOVE <uid> "Junk"` | Sets `emails.imap_mailbox = 'Junk'`, `emails.imap_uid = NULL`, `imap_jobs.status = 'completed'`. |
| `move_to_archive` | Thread transitions `in_progress -> completed` | `SELECT "INBOX"` -> `UID MOVE <uid> "Archive"` | Sets `emails.imap_mailbox = 'Archive'`, `emails.imap_uid = NULL`, `imap_jobs.status = 'completed'`. |
| `append_to_sent` | Outbound reply delivered via SMTP | `APPEND "Sent" (\Seen) {<size>}` | Sets `emails.imap_mailbox = 'Sent'`, `imap_jobs.status = 'completed'`. |

### Retry & Backoff
- If an IMAP job encounters a transient network or server error, `FailIMAPJob` increments `attempts` and reschedules the job with exponential backoff (`next_run_at = now() + 2^attempts seconds`, up to `max_attempts = 10`).

### Why Deleting Emails from `Archive` is Safe
- **One-way `INBOX` sync**: `IMAPSyncWorker` only reads from `INBOX`. It never inspects `Archive`, `Junk`, or `Sent` for deletions or expunges.
- **Complete local persistence**: Every email's headers, plain-text body, sanitized HTML body, and attachments are stored in PostgreSQL and on local disk as soon as the message is ingested from `INBOX`.
- **Missing UID tolerance**: If an email was already moved or deleted externally on the IMAP server before a `move_to_archive` or `move_to_junk` job runs (`ErrMessageNotFound` or `UIDVALIDITY` mismatch), `IMAPActionWorker` treats the missing UID as a benign no-op and marks the job `completed`.
- Therefore, operators or mail retention policies can freely delete or purge emails from the IMAP `Archive` folder without breaking thread history, attachment downloads, or the **Reopen & Merge** workflow.

---

## 4. Outgoing SMTP & `Sent` Folder Synchronization (`OutboxWorker`)

Implemented in `internal/mail/smtp_sender.go`.

1. **Transactional Outbox Queueing (`ComposeAndQueueReply`)**:
   - Generates a unique `Message-ID` (`<uuid@MINICRM_MAIL_DOMAIN>`).
   - Populates `In-Reply-To` with the latest email's `Message-ID` in the thread and appends to the `References` chain (capped at the last 20 IDs).
   - Prefixes `Subject` with `Re: ` if not already present.
   - Writes uploaded attachments to local disk (`storage.Store.Save`), inserts the `outbound` row into `emails`, and inserts a `pending` row into `outbox_emails`.
2. **SMTP Delivery & IMAP `APPEND` (`ProcessPending`)**:
   - Claims the next pending `outbox_emails` row using `FOR UPDATE SKIP LOCKED`.
   - Serializes the complete RFC 5322 MIME message (headers, body, and attachments) into a byte buffer using `wneessen/go-mail`.
   - Sends the message via the configured SMTP server (`AUTH PLAIN`).
   - Marks `outbox_emails.status = 'sent'`.
   - Saves the exact serialized RFC 5322 bytes to the local blob store and enqueues an `append_to_sent` job in `imap_jobs` so `IMAPActionWorker` appends the identical message to the IMAP `Sent` folder.
