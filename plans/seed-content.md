# Plan — Go seed CLI (`cmd/seed`) with declarative YAML demo content

**Goal:** replace fragile SQL-based content seeding with a standalone Go CLI that owns the whole
flow: generate RFC 822 from YAML, APPEND to Dovecot, ingest through the real sync/matcher code,
then apply demo state (assignments, completed, spam, drafts) via the real workflow service — so
queued `move_to_archive`/`move_to_junk` jobs really move mail on the IMAP server.

**Demo requirements (user-confirmed):**
- ~60 threads, >100 messages total
- multiple threads from the same sender
- enough open conversations for bulk assign / bulk spam demos (batching = bulk inbox actions only)
- existing assignments (manual + default-source), completed threads, spam markings
- completed/spam threads queue real IMAP folder moves
- keep demonstrating attachments, HTML mail, external Sent replies, reopen candidate

## 1. New package `internal/seed` + `cmd/seed`

- `cmd/seed/main.go`: flags `-config <yaml>` plus the standard connection flags/env
  (`MINICRM_DATABASE_URL`, IMAP host/port/user/password per account — same env schema as the app,
  so the compose `seed` service just reuses the account env vars).
  - Runs migrations check (expects schema to exist; errors if not — it never migrates).
  - Phases below; exits non-zero on failure; logs structured slog lines like the app.
- No changes to `cmd/minicrm` itself.

## 2. YAML schema (`internal/seed/config.go`, parsed with `gopkg.in/yaml.v3`)

```yaml
accounts:            # which configured account IDs participate
  - support
  - sales
users:               # pre-created assignees (folds seed-users SQL in, ON CONFLICT DO NOTHING)
  - email: alex@example.com
    display_name: Alex Agent
    accounts: [support, sales]
  - email: sam@example.com
    display_name: Sam Support
    accounts: [support]
settings:
  timezone: Europe/Amsterdam
threads:
  - account: support
    customer: alice@customer.example
    customer_name: Alice Example
    subject: Cannot log in to my account
    status: completed          # new | in_progress | completed | spam (default new)
    assigned_to: alex@example.com
    assigned_source: manual    # manual | default (affects assignment_source)
    draft: "..."               # optional shared reply draft
    messages:                  # chronological; direction defaults to inbound
      - from: alice@customer.example
        date: 2026-10-05T09:15:00Z
        body: |
          Hi support team, ...
      - direction: outbound    # team reply, APPENDed to Sent
        date: 2026-10-05T11:40:00Z
        body: |
          Hi Alice, ...
      - from: alice@customer.example   # follow-up reply; references previous message
        date: 2026-10-05T12:10:00Z
        body: |
          Thanks! One more question...
    attach:                    # optional per-message attachments, files on disk
      - filename: receipt.pdf
        content_type: application/pdf
        file: fixtures/receipt.pdf
```

Rules enforced by the loader (fail fast, clear errors):
- `account` must be a configured account ID; `assigned_to` must have access to that account.
- Message dates strictly increasing; first message inbound; Message-IDs generated deterministically
  (`<seed-<thread-slug>-<n>@minicrm.dev>`) so reruns dedupe cleanly.
- Thread families: the same `customer` may appear in many threads (same-sender demo) — including a
  deliberate pair where a later thread references a *completed* earlier thread, creating the
  `Reopen & Merge` candidate.

## 3. Seed content file `docker/seed/dev-seed.yaml` (~60 threads, >100 messages)

Written by hand but compact (bodies 1–3 sentences). Composition across both accounts:

- **Support (~40 threads):**
  - Port the 8 existing fixture threads' *content* into YAML (plain bodies; the attachment-bearing
    messages keep their fixtures — see §5), including the login-issue thread (Alice, 3 messages),
    invoice question (Bob, HTML body variant), Gina outbound-first thread.
  - One sender with 4 separate threads (`frank@customer.example` — same-sender demo).
  - 1 completed thread with a later follow-up referencing it (Reopen & Merge demo).
  - 6 spam threads (obvious junk subjects) → `move_to_junk` queued for real.
  - 5 completed threads (mix of directly-completed and in_progress→completed) → `move_to_archive`.
  - ~12 in_progress with mixed manual/default assignment to Alex/Sam; 1 with an active draft.
  - Remaining ~15 `new`/unassigned — enough open rows for bulk assign/spam selection.
- **Sales (~20 threads):** Dave pricing/PO family (3 threads from Dave), Mia partnership family,
  plus ~16 generated threads (quotes, renewals, trial questions) with 2 more senders owning
  multiple threads each; 3 completed, 2 spam, several assigned.

## 4. Execution phases in `internal/seed/seed.go`

1. **Users & settings** — create pre-created users (audit `user_precreated`, same as today's
   `seed-users`), set `system_settings.display_timezone` if not set, and set
   `account_settings.default_assignee_id` (support → sam, sales → alex) so *default-sourced*
   assignments exist alongside manual ones.
2. **APPEND** — for every YAML message, generate RFC 822 (`internal/seed/rfc822.go`, ~120 lines:
   plain or multipart/alternative, optional attachment parts, deterministic Message-ID/Date) and
   APPEND to the account's INBOX (or Sent mailbox for `direction: outbound`) via
   `mail.GoIMAPClient`. Mail now looks exactly like real mail on the server.
3. **Ingest** — run one `IMAPSyncWorker.SyncOnce` pass per account against the real DB. Threads,
   dedup by Message-ID, default assignment, `created`/`reply_received` events and
   Sent-folder attach semantics are all produced by production code — no SQL duplication.
4. **Demo state** — for each YAML thread, via `workflow.Service`:
   - `assigned_to` + `assigned_source: manual` → `Reassign` (audited `assigned` event);
     `assigned_source: default` is left to phase 1's account defaults instead.
   - `status: in_progress` → `Transition(new→in_progress)`;
     `completed` → `CompleteFromNew` (or Transition if already in_progress);
     `spam` → `Transition(→spam)`. These enqueue real `move_to_archive`/`move_to_junk` jobs.
   - `draft` → `AppendReplyDraft` (shared-draft demo).
5. **Drain jobs** — loop `imap action` worker `ProcessPending` until no pending jobs remain, so
   Dovecot's Archive/Junk folders are actually populated before the CLI exits (with a timeout).
6. **Idempotency** — before phase 2, check whether Message-IDs already exist
   (`GetEmailByMessageID` on the first thread's first message): if seeded, skip phases 2–5 and
   exit 0. State changes are still safe to reapply (workflow no-ops / audited re-assign).
7. **Verification** — final summary log: threads/messages/jobs counts per account + status.

## 5. Attachment fixtures

- Keep the existing hand-crafted `.eml` fixtures in `docker/mail/seed/` **for the attachment
  messages only** (05, 06, 07 support; 03, 04 sales): they are the Maildir-seeded messages and
  remain covered by `internal/mail/seed_fixtures_test.go`.
- All other existing fixture `.eml` files stay too (they're already in the YAML as well? **No —**
  to avoid double-seeding, the non-attachment fixture `.eml` files are removed from
  `docker/mail/seed/` and their content lives only in the YAML; entrypoint keeps seeding
  `Sent/` fixtures and the 5 attachment `.eml` files).
- YAML threads that should demo attachments reference small files under `docker/seed/fixtures/`
  (checked in; generated once by the existing python script or committed directly).

## 6. Compose wiring

- `compose.yaml`: add one-shot service `seed` (`<<: *app`, `command: [seed, -config,
  /seed/dev-seed.yaml, ...]`, mount `./docker/seed:/seed:ro`, reuse account env vars via YAML
  anchor or explicit env), `depends_on: {minicrm: service_healthy, mail: service_healthy}`.
- Remove the `seed-users` SQL service (its users/settings move into the YAML); update the
  compose header comment.
- `compose.liethorp.yaml` untouched.

## 7. Tests

- Unit: YAML loader validation (bad account, non-increasing dates, assignee without access);
  RFC 822 generation (parse back with `mail.ParseRFC5322`, verify Message-ID/headers/attachments).
- Integration (fake IMAP server already in `internal/mail`): run the full pipeline end-to-end —
  APPEND → sync → transitions → jobs — asserting thread counts, statuses, assignment sources,
  queued/completed move jobs.
- `seed_fixtures_test.go` updated for the slimmed fixture set.

## 8. Docs

- README dev-stack section: replace `seed-users` paragraph + seeded-threads table with the `seed`
  service description and how to customize `docker/seed/dev-seed.yaml`.
- `docs/testing.md`: mention the integration test; `AGENTS.md` unchanged.

## 9. Verification

- `golangci-lint run ./... && go test -race ./...`
- Manual smoke: `docker compose down -v && docker compose up --build -d` → open
  <http://localhost:8080/threads>, confirm volume/statuses/assignments/Archive/Junk on the IMAP
  server, bulk-select demos, Reopen & Merge candidate, draft thread.

## Task breakdown

1. `internal/seed` config loader + validation (+ unit tests)
2. `internal/seed/rfc822.go` generator (+ tests)
3. `internal/seed/seed.go` pipeline phases 1–7 (+ integration test with fake IMAP server)
4. `cmd/seed/main.go` CLI wiring
5. `docker/seed/dev-seed.yaml` content + attachment fixtures; slim `docker/mail/seed/`
6. compose.yaml rewiring; README + docs updates
