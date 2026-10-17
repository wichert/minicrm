# Multi-Account IMAP & SMTP Support Plan

## Context

MiniCRM currently monitors a single IMAP mailbox and sends replies through a single SMTP account, with all authenticated users able to view and act on all threads and attachments.

We need to support monitoring and sending from **multiple IMAP & SMTP accounts** (two initially, but designed generally to support any number `N >= 1` of configured accounts) with strict OAuth2/OIDC-driven access control and account isolation:
1. **Per-user account access via OIDC claims**: Not all users have access to all accounts. OAuth2/OIDC group/role claims determine which account(s) a user can access.
2. **Thread & assignee access control**: A user can only list, view, transition, assign, reopen/merge, or reply to a thread if they have access to the IMAP account the thread belongs to. Furthermore, **neither the UI nor the backend may allow assigning a thread to a user who does not have access to that thread's IMAP account**.
3. **Attachment & email body access control + single-copy storage**: A user cannot access an attachment (`/attachments/{id}/download`) or isolated email body (`/emails/{id}/body`) from an account they do not have access to. Identical attachments sent to multiple accounts are stored only once on disk via content-addressed SHA-256 storage.
4. **Independent threads when sent to multiple accounts**: If a single email is sent to multiple configured IMAP accounts simultaneously, each account ingests it independently and creates its own thread (one thread per account).
5. **Account-aware UI**: Users with access to multiple accounts see a clear account badge/column in the inbox table, an account filter dropdown in the inbox, and an account indicator in the thread detail view. All account indicators are hidden for users who only have access to a single account.

---

## Approach

### 1. N-Account Numbered Environment Variable Configuration (`internal/config/config.go`)
- Support an arbitrary number `N >= 1` of configured accounts (`Accounts []MailAccountConfig` on `Config`) by iterating numbered environment variable prefixes `MINICRM_ACCOUNT_1_*`, `MINICRM_ACCOUNT_2_*`, `MINICRM_ACCOUNT_3_*`, ... until `MINICRM_ACCOUNT_<N>_ID` is no longer present in the environment (requiring at least 1 configured account).
- Each `MailAccountConfig` contains:
  - `ID string` (`env:"ID,required"` — **mandatory** unique account identifier, e.g. `"support"` or `"sales"`, with no arbitrary default)
  - `Name string` (`env:"NAME,required"` — human-readable display name shown in the UI, e.g. `"Customer Support"` or `"Sales"`)
  - `OIDCGroup string` (`env:"OIDC_GROUP,required"` — the exact OIDC group/role value that grants a user access to this account, e.g. `"crm-support"`)
  - `IMAPHost string` (`env:"IMAP_HOST,required"`), `IMAPPort int` (`env:"IMAP_PORT" envDefault:"993"`), `IMAPUsername string` (`env:"IMAP_USERNAME,required"`), `IMAPPassword string` (`env:"IMAP_PASSWORD,required"`), `IMAPImplicitTLS bool` (`env:"IMAP_IMPLICIT_TLS" envDefault:"true"`), `IMAPPollInterval time.Duration` (`env:"IMAP_POLL_INTERVAL" envDefault:"2m"`)
  - `SMTPHost string` (`env:"SMTP_HOST,required"`), `SMTPPort int` (`env:"SMTP_PORT" envDefault:"587"`), `SMTPUsername string` (`env:"SMTP_USERNAME,required"`), `SMTPPassword string` (`env:"SMTP_PASSWORD,required"`), `SMTPFromAddress string` (`env:"SMTP_FROM_ADDRESS,required"`), `SMTPFromName string` (`env:"SMTP_FROM_NAME" envDefault:"MiniCRM Support"`), `SMTPTLSMode string` (`env:"SMTP_TLS_MODE" envDefault:"starttls"`)
- Add `OIDCGroupsClaim string` (`env:"MINICRM_OIDC_GROUPS_CLAIM" envDefault:"groups"`) to `Config` to configure which claim in the OIDC ID Token carries the user's group memberships.
- `Config.Validate()` verifies that `len(c.Accounts) >= 1` and that every account has a unique non-empty `ID`, non-empty `Name`, non-empty `OIDCGroup`, valid IMAP/SMTP ports, and valid `SMTPTLSMode`.

### 2. OIDC Group-to-Account Mapping & User Account Access (`internal/auth/`)
- **How an OIDC Group Links to an Account**:
  - Each configured account in `Config.Accounts` explicitly defines its mandatory `ID` (`MINICRM_ACCOUNT_<N>_ID`) and its required OIDC group (`MINICRM_ACCOUNT_<N>_OIDC_GROUP`). For example:
    ```env
    MINICRM_OIDC_GROUPS_CLAIM=groups

    MINICRM_ACCOUNT_1_ID=support
    MINICRM_ACCOUNT_1_NAME=Support
    MINICRM_ACCOUNT_1_OIDC_GROUP=crm-support

    MINICRM_ACCOUNT_2_ID=sales
    MINICRM_ACCOUNT_2_NAME=Sales
    MINICRM_ACCOUNT_2_OIDC_GROUP=crm-sales
    ```
  - During OIDC login (`OIDCProvider.ExchangeAndVerify`), `idToken.Claims(&rawClaims)` extracts the claim named by `MINICRM_OIDC_GROUPS_CLAIM` (e.g. `"groups": ["crm-support", "staff"]`) into `VerifiedIdentity.Groups []string`.
  - `auth.Service` holds the configured slice of `AccountAccessRule{AccountID, AccountName, OIDCGroup}` constructed from `cfg.Accounts`.
  - In `ResolveAndLoginUser`, `auth.Service` iterates over the configured rules and checks whether `VerifiedIdentity.Groups` contains `rule.OIDCGroup`:
    ```go
    var allowedAccounts []string
    for _, rule := range s.accountRules {
        if slices.Contains(id.Groups, rule.OIDCGroup) {
            allowedAccounts = append(allowedAccounts, rule.AccountID)
        }
    }
    ```
    In the example above, a user with `"groups": ["crm-support"]` resolves to `allowedAccounts = ["support"]`; a user with `["crm-support", "crm-sales"]` resolves to `["support", "sales"]`.
  - If `len(allowedAccounts) == 0`, login is rejected: `user_login_failed` (`reason: "no_account_access"`) is recorded in `audit_logs` and `ErrNoAccountAccess` is returned (`403 Forbidden` in `HandleCallback`).
  - When creating or updating the user row on login, `allowed_accounts` (`TEXT[]`) is persisted on `users` (and any change is recorded in `audit_logs` as `user_updated`).
  - When `SeedBootstrapAdminAndSettings` runs at startup, the bootstrap admin is seeded with `allowed_accounts` set to all configured account IDs. Note that `account_settings` is **not** required to have a pre-populated default assignee for every account on startup: if an account has no row in `account_settings` (or `default_assignee_id IS NULL`), incoming threads for that account enter an **unassigned** state until a default assignee is configured or an operator takes/assigns the thread.
- Add helper methods on `sqlc.User`:
  - `HasAccountAccess(accountID string) bool`
  - `HasMultipleAccounts() bool` (`len(u.AllowedAccounts) > 1`)
- Extend `internal/auth/devidm/server.go` and `cmd/dev-oidc/main.go` with `Groups []string` (`-groups` / `DEV_OIDC_GROUPS`, comma-separated) included in the signed ID token under the configured groups claim so local dev and E2E tests exercise group-based account resolution.

### 3. Database Migration & Queries (`internal/db/migrations/002_multi_account.sql`, `internal/db/migrations/embed.go`, `internal/db/sqlc/`)
- **Safe Backfill of Existing Rows Before `SET NOT NULL`**:
  - Adding `account_id TEXT NOT NULL` directly would fail if `threads`, `emails`, `imap_sync_state`, or `imap_jobs` already contain rows from a previous single-account deployment.
  - To migrate existing data safely without hardcoding an arbitrary default on the column:
    1. Update `migrations.Up(ctx, db, primaryAccountID)` in `internal/db/migrations/embed.go` (called with `cfg.Accounts[0].ID` from `cmd/minicrm/main.go`) to set the PostgreSQL session setting `SELECT set_config('minicrm.legacy_account_id', $1, false)` on the migration connection before running Goose.
    2. In `002_multi_account.sql`, add `account_id TEXT` (initially nullable), backfill any existing rows with `COALESCE(NULLIF(current_setting('minicrm.legacy_account_id', true), ''), 'legacy')`, and then apply `ALTER COLUMN account_id SET NOT NULL` so no `DEFAULT` remains on the column.
- **Unassigned Thread Support (`threads.assignee_id` Nullable & `account_settings`)**:
  - Allow threads to exist without an assignee when no default assignee is configured for an account:
    - `ALTER TABLE threads ALTER COLUMN assignee_id DROP NOT NULL;` (`sqlc.Thread.AssigneeID` becomes `*uuid.UUID`, and `assignment_source` is `'unassigned'` when `assignee_id IS NULL`).
    - `ALTER TABLE system_settings DROP COLUMN default_assignee_id;` (replaced by per-account `account_settings`).
    - Create `account_settings` with a nullable `default_assignee_id`:
      ```sql
      CREATE TABLE account_settings (
          account_id          TEXT PRIMARY KEY,
          default_assignee_id UUID REFERENCES users(id) ON DELETE SET NULL
      );
      ```
      If `account_settings` has no row for an `account_id` or `default_assignee_id IS NULL`, that account currently has no default assignee.
- **Full `002_multi_account.sql` Steps** (preserving all invariants: no `updated_at` columns, UTC `TIMESTAMPTZ` only):
  1. `ALTER TABLE users ADD COLUMN allowed_accounts TEXT[] NOT NULL DEFAULT '{}';`
  2. Create `account_settings (account_id TEXT PRIMARY KEY, default_assignee_id UUID REFERENCES users(id) ON DELETE SET NULL);` and drop `system_settings.default_assignee_id`.
  3. `ALTER TABLE threads ALTER COLUMN assignee_id DROP NOT NULL;`
  4. On `threads`, `emails`, `imap_sync_state`, and `imap_jobs`:
     - `ADD COLUMN account_id TEXT;`
     - `UPDATE <table> SET account_id = COALESCE(NULLIF(current_setting('minicrm.legacy_account_id', true), ''), 'legacy') WHERE account_id IS NULL;`
     - `ALTER COLUMN account_id SET NOT NULL;`
  5. Update constraints and indexes:
     - On `threads`: add `idx_threads_account_status_last_msg ON threads (account_id, status, last_message_at DESC)` and `idx_threads_account_subject_customer ON threads (account_id, lower(subject_normalized), customer_email, last_message_at DESC)`.
     - On `emails`: drop `emails_message_id_key` (`UNIQUE (message_id)`), add `emails_account_message_id_key UNIQUE (account_id, message_id)`, and update `idx_emails_imap_uid` to `(account_id, imap_mailbox, imap_uidvalidity, imap_uid)`.
     - On `imap_sync_state`: replace primary key `(mailbox)` with `PRIMARY KEY (account_id, mailbox)`.
- Update `internal/db/queries/queries.sql`, `internal/db/sqlc/models.go`, and `internal/db/sqlc/queries.go`:
  - `Thread.AssigneeID` changed to `*uuid.UUID`.
  - `ListThreadsFilter` gains `AllowedAccounts []string` (`account_id = ANY($1)`) and optional `AccountID *string`.
  - `GetEmailByMessageID`, `FindEmailByMessageIDs`, and `FindThreadBySubjectAndParticipant` all require `accountID string` so thread matching and deduplication are strictly isolated per account.
  - Add `GetAccountSettings`, `ListAccountSettings`, `UpsertAccountSettings`, and `AssignUnassignedThreadsByAccount(ctx, accountID, assigneeID)` (which assigns all active threads in `accountID` where `assignee_id IS NULL` when a default assignee is configured).

### 4. Attachment Single-Copy Deduplication & Access Control (`internal/storage/attachments.go`, `internal/web/handlers_threads.go`)
- **Single-copy storage on disk**:
  - `storage.Store.Save` already stores files by SHA-256 at `<sha256[0:2]>/<sha256[2:4]>/<sha256>`.
  - Ensure `storage.Store.Save` is safe when the target SHA-256 blob already exists on disk (if `finalPath` already exists, remove the temp file rather than failing on Windows/POSIX edge cases, and log `attachment_blob` reuse/creation).
  - When the same email with an attachment arrives in both Account 1 and Account 2, two `emails` rows and two `attachments` metadata rows are created (one per account's email), both referencing the same `sha256_hex` and `storage_relpath` on disk.
- **Strict attachment & email body authorization**:
  - In `HandleDownloadAttachment` (`GET /attachments/{id}/download`):
    - Fetch `att` -> `email` (`GetEmailByID(ctx, att.EmailID)`) -> verify `user.HasAccountAccess(email.AccountID)`. If false, return `http.StatusNotFound` (`404`) before opening the file or logging data access.
  - In `HandleIsolatedEmailBody` (`GET /emails/{id}/body`):
    - Fetch `email` -> verify `user.HasAccountAccess(email.AccountID)`. If false, return `http.StatusNotFound` (`404`).

### 5. Per-Account Workers, Threading, and Default Assignment
- **IMAP Sync (`internal/mail/imap_sync.go`)**:
  - Add `accountID string` to `IMAPSyncWorker`.
  - In `SyncOnce` and `ingestOne`:
    - Track sync state by `(w.accountID, inbox)`.
    - Deduplicate by `(w.accountID, parsed.MessageID)`.
    - Pass `AccountID: w.accountID` to `w.matcher.ResolveThread` and `w.store.CreateEmail`.
  - Start one `IMAPSyncWorker` per configured account in `cmd/minicrm/main.go`.
- **Thread Matcher & Unassigned Fallback (`internal/threading/matcher.go`)**:
  - Add `AccountID string` to `InboundMessageMetadata`.
  - Scope `FindEmailByMessageIDs(ctx, msg.AccountID, candidateIDs)` and `FindThreadBySubjectAndParticipant(ctx, msg.AccountID, normSubject, msg.FromAddress, since)` to `msg.AccountID`.
  - Pass `AccountID: msg.AccountID` to `m.assigner.Resolve`.
  - **What happens when no default assignee is set (`account_settings` has no row or `default_assignee_id IS NULL`)**:
    - `m.assigner.Resolve` returns `assignment.ErrNoAssigneeAvailable`.
    - `Matcher.ResolveThread` handles `ErrNoAssigneeAvailable` gracefully by creating the new thread in an **unassigned** state (`AssigneeID: nil`, `AssignmentSource: "unassigned"`, `Status: "new"`) and logging only the `created` `thread_event`.
    - If a valid assignee *is* resolved, `Matcher.ResolveThread` sets `AssigneeID: &cand.AssigneeID`, `AssignmentSource: cand.Source`, and logs both the `created` and `assigned` `thread_events`.
  - Because both `emails` deduplication and `Matcher` lookups are scoped by `account_id`, an email sent to multiple accounts creates independent threads (one per account).
- **Per-Account Default Assignee & Processing Unassigned Threads (`internal/assignment/default_resolver.go`, `internal/workflow/service.go`)**:
  - `EmailInput` gains `AccountID string`.
  - `DefaultAssigneeResolver.Resolve` queries `GetAccountSettings(ctx, input.AccountID)`:
    - If `pgx.ErrNoRows` or `settings.DefaultAssigneeID == nil`, return `nil, nil` (so the engine returns `ErrNoAssigneeAvailable` and the thread is created unassigned).
    - If `settings.DefaultAssigneeID != nil`, load the user and verify `user.IsActive && user.HasAccountAccess(input.AccountID)`. If valid, return `&Candidate{AssigneeID: user.ID, Source: "default", Confidence: 1.0}`; otherwise return `nil, nil` (leaving the thread unassigned rather than guessing an arbitrary user).
  - **Processing unassigned threads once a default assignee is set or an operator takes the thread**:
    1. **When a default assignee is configured in `/settings`**: `workflow.Service.AssignUnassignedThreadsForAccount(ctx, accountID, assigneeID, actorID)` finds all threads in `accountID` with `assignee_id IS NULL AND status IN ('new', 'in_progress')`, sets `assignee_id = assigneeID` (`assignment_source = 'default'`), and records an `assigned` `thread_event` for each thread.
    2. **When an operator manually assigns or clicks "Take Thread"**: `workflow.Service.Reassign` accepts an unassigned thread (`thread.AssigneeID == nil`), verifies `targetUser.IsActive && targetUser.HasAccountAccess(thread.AccountID)`, sets `assignee_id = &newAssigneeID` (`assignment_source = 'manual'`), and records an `assigned` event (if previously nil) or `reassigned` event (if previously assigned).
    3. **When an operator replies to an unassigned thread (`ComposeAndQueueReply`)**: If `thread.AssigneeID == nil`, automatically assign the thread to `p.SenderUserID` (`assignment_source = 'reply'`) and record an `assigned` `thread_event`.
- **Workflow & IMAP Action Worker (`internal/workflow/service.go`, `internal/mail/imap_actions.go`)**:
  - `Reassign` enforces that `targetUser.IsActive` **and** `targetUser.HasAccountAccess(thread.AccountID)`; if the target user does not have access to `thread.AccountID`, return `ErrAssigneeNoAccountAccess` (`422 Unprocessable Entity` in `HandleAssignThread`).
  - `enqueueFolderMoveJobs` sets `AccountID: em.AccountID` on `sqlc.IMAPJob`.
  - `ReopenAndMerge` verifies `followUp.AccountID == target.AccountID`.
  - `IMAPActionWorker` takes a dialer lookup `func(ctx context.Context, accountID string) (IMAPClient, error)` and connects to the IMAP server for `job.AccountID`.
- **SMTP Outbox Worker (`internal/mail/smtp_sender.go`)**:
  - Introduce `AccountSenderConfig` (`AccountID`, `FromAddress`, `FromName`, `Transport SMTPTransport`).
  - `OutboxWorker` stores `map[string]AccountSenderConfig` keyed by `accountID`.
  - `ComposeAndQueueReply` selects the sender config for `thread.AccountID`, sets `AccountID: thread.AccountID`, `FromAddress`, and `FromName` on the outbound `sqlc.Email`.
  - `sendOne` delivers via the `SMTPTransport` for `emailRec.AccountID` and enqueues `append_to_sent` with `AccountID: emailRec.AccountID`.

### 6. Web Handlers & Conditional Account UI (`internal/web/`)
- **Thread Handlers (`internal/web/handlers_threads.go`)**:
  - Guard `HandleViewThread`, `HandleTransitionThread`, `HandleAssignThread`, `HandleReopenMerge`, and `HandleReplyThread` with a helper `s.loadAuthorizedThread(ctx, user, threadID)` that returns `ErrThreadNotFound` / `404` if `!user.HasAccountAccess(thread.AccountID)`.
  - In `HandleViewThread`, filter the `Users` slice passed to `ThreadDetailViewData` to only include active users where `u.HasAccountAccess(thread.AccountID)` is true so the UI assignee `<select>` only offers users with access to the thread's account.
  - In `HandleAssignThread`, if `s.workflow.Reassign` returns `workflow.ErrAssigneeNoAccountAccess`, reject with `422 Unprocessable Entity`.
  - In `HandleListThreads`:
    - If `len(user.AllowedAccounts) == 0`, return empty list.
    - Read optional `?account=` query parameter; if non-empty and `user.HasAccountAccess(accountFilter)`, pass `AccountID: &accountFilter` to `ListThreadsFilter` alongside `AllowedAccounts: user.AllowedAccounts`.
- **Settings Handlers (`internal/web/handlers_settings.go`)**:
  - Allow admins to view and update the per-account default assignee for each configured account in `/settings` (including an `-- Unassigned (None) --` option; if a user is selected, validate in both the UI dropdown and backend handler that the chosen user is active and `assigneeUser.HasAccountAccess(accountID)`).
  - When a non-empty default assignee is saved for an account, call `s.workflow.AssignUnassignedThreadsForAccount(ctx, accountID, assigneeID, &user.ID)` to automatically assign any waiting unassigned threads for that account.
  - Allow admins to select initial `allowed_accounts` checkboxes when pre-creating a user so pre-created users can immediately be configured as default assignees or assigned threads for their permitted accounts.
- **Templates (`internal/web/views/views.go`)**:
  - Render `Unassigned` whenever `Thread.AssigneeID == nil`, and on the thread detail view include a quick **"Take Thread"** button (posting `assignee_id = CurrentUser.ID` to `/threads/{id}/assign`) when `Thread.AssigneeID == nil`.
  - Add `ShowAccountUI bool` (`len(user.AllowedAccounts) > 1`), `AccountFilter string`, and `Accounts []AccountOption` (`ID`, `Name`) to `InboxViewData`.
  - Add `ShowAccountUI bool` and `AccountName string` to `ThreadSummaryItem` and `ThreadDetailViewData`.
  - When `ShowAccountUI` is `true`:
    - Render the `Account` filter `<select name="account">` in the inbox filter form.
    - Render the `Account` column header and `<span class="badge">{{.AccountName}}</span>` cell in `#threads-table`.
    - Render `<strong>Account:</strong> <span class="badge">{{.AccountName}}</span> |` in the thread detail header card.
  - When `ShowAccountUI` is `false` (single-account user):
    - Omit the account filter dropdown, table column, and thread detail account badge completely.

---

## Files to Modify

- `internal/config/config.go` & `internal/config/config_test.go` — N-account config parsing (`MINICRM_ACCOUNT_<N>_*` with mandatory `ID`, `NAME`, `OIDC_GROUP`) and `MINICRM_OIDC_GROUPS_CLAIM`.
- `internal/db/migrations/002_multi_account.sql` (new) — Schema migration adding `users.allowed_accounts`, `account_settings`, and `account_id` scoping on `threads`, `emails`, `imap_sync_state`, and `imap_jobs`.
- `internal/db/queries/queries.sql`, `internal/db/sqlc/models.go`, `internal/db/sqlc/queries.go` — Account-scoped queries, models, and `User.HasAccountAccess` helpers.
- `internal/auth/oidc.go`, `internal/auth/auth_test.go` — OIDC group claim parsing, mapping groups to `allowed_accounts`, `ErrNoAccountAccess` rejection, and seeding per-account `account_settings`.
- `internal/auth/devidm/server.go`, `internal/auth/devidm/server_test.go`, `cmd/dev-oidc/main.go` — Add `Groups` claim support to the dummy OIDC provider.
- `internal/assignment/engine.go`, `internal/assignment/default_resolver.go`, `internal/assignment/engine_test.go` — Per-account default assignee resolution.
- `internal/threading/matcher.go`, `internal/threading/matcher_test.go` — Scope reply matching (`FindEmailByMessageIDs`, `FindThreadBySubjectAndParticipant`) and thread creation by `AccountID`.
- `internal/workflow/service.go`, `internal/workflow/service_test.go` — Propagate `AccountID` to `imap_jobs` and validate same-account on `ReopenAndMerge`.
- `internal/storage/attachments.go`, `internal/storage/attachments_test.go` — Ensure idempotent content-addressed `Save` across accounts.
- `internal/mail/imap_sync.go`, `internal/mail/imap_actions.go`, `internal/mail/smtp_sender.go`, `internal/mail/imap_workers_test.go`, `internal/mail/smtp_sender_test.go` — Account-scoped sync worker, multi-account action dialer routing, and multi-account SMTP outbox routing.
- `internal/web/handlers_threads.go`, `internal/web/handlers_settings.go`, `internal/web/views/views.go`, `internal/web/web_test.go` — Account authorization on all thread/email/attachment endpoints and conditional multi-account UI rendering.
- `cmd/minicrm/main.go`, `e2e/auth_e2e_test.go`, `.github/workflows/ci.yml` — Wire both accounts, workers, and E2E tests.
- `README.md`, `docs/architecture.md`, `docs/authentication.md`, `docs/imap.md`, `docs/storage.md`, `docs/workflow-and-threading.md` — Update documentation to reflect multi-account support.

---

## Reuse

- `internal/storage/attachments.go` (`storage.Store.Save`, `storage.Store.Open`) — Content-addressed SHA-256 blob storage (`<sha256[0:2]>/<sha256[2:4]>/<sha256>`) already deduplicates identical attachment payloads on disk.
- `internal/auth/oidc.go` (`OIDCProvider`, `Service.ResolveAndLoginUser`) — Extend existing ID token claim extraction and audit logging (`audit.Logger`).
- `internal/mail/imap_sync.go` (`IMAPSyncWorker`) — Instantiate one worker per configured `MailAccountConfig`.
- `internal/web/views/views.go` (`badge` CSS class in `/static/app.css`) — Reuse existing badge styling for account labels.

---

## Steps

- [x] **Step 1: Configuration (`internal/config/`)**
  - Update `Config` to dynamically parse `Accounts []MailAccountConfig` from `MINICRM_ACCOUNT_<N>_*` (`N = 1, 2, ...`, requiring mandatory `ID`, `NAME`, and `OIDC_GROUP` per account) and `OIDCGroupsClaim` (`MINICRM_OIDC_GROUPS_CLAIM`, default `"groups"`).
  - Update `internal/config/config_test.go` to test multi-account configuration (1, 2, and N accounts) and validation (missing ID, duplicate IDs, missing OIDCGroup, invalid ports).
- [x] **Step 2: Database Migration & SQLC Layer (`internal/db/`)**
  - Update `migrations.Up` (`internal/db/migrations/embed.go`) to accept `legacyAccountID` (`cfg.Accounts[0].ID`) and set `minicrm.legacy_account_id` before running Goose migrations.
  - Add `internal/db/migrations/002_multi_account.sql` (`users.allowed_accounts`, `account_settings` with nullable `default_assignee_id`, nullable `threads.assignee_id`, two-step add+backfill+`SET NOT NULL` for `account_id` on `threads`, `emails`, `imap_sync_state`, `imap_jobs`, and `UNIQUE (account_id, message_id)` on `emails`).
  - Update `internal/db/queries/queries.sql`, `internal/db/sqlc/models.go`, and `internal/db/sqlc/queries.go` with nullable `Thread.AssigneeID`, account-scoped queries, `AssignUnassignedThreadsByAccount`, and `User.HasAccountAccess` / `User.HasMultipleAccounts`.
- [x] **Step 3: OIDC Group Claim Mapping & DevIDM (`internal/auth/`, `cmd/dev-oidc/`)**
  - Extract the configured groups claim in `OIDCProvider.ExchangeAndVerify` into `VerifiedIdentity.Groups`.
  - Map OIDC groups to allowed account IDs (`rule.OIDCGroup -> rule.AccountID`) in `auth.Service.ResolveAndLoginUser`, persist `users.allowed_accounts`, reject users with zero allowed accounts (`ErrNoAccountAccess`), and allow specifying `allowed_accounts` in `PrecreateUser`.
  - Add `Groups` support to `devidm.Server` and `cmd/dev-oidc`.
  - Add unit tests in `internal/auth/auth_test.go` and `internal/auth/devidm/server_test.go` covering single-account, multi-account, and zero-account OIDC logins.
- [x] **Step 4: Per-Account Assignment, Unassigned Threads, Threading & Workflow (`internal/assignment/`, `internal/threading/`, `internal/workflow/`)**
  - Update `DefaultAssigneeResolver` to resolve per-account `default_assignee_id` from `account_settings` (returning no candidate if `account_settings` has no row or `default_assignee_id IS NULL`, or if the user lacks access to `input.AccountID`).
  - Update `Matcher.ResolveThread` to create threads in the `unassigned` state (`AssigneeID: nil`, `AssignmentSource: "unassigned"`) when `ErrNoAssigneeAvailable` is returned, and scope matching by `AccountID` so an email sent to multiple accounts creates independent threads.
  - Update `workflow.Service` to reject reassigning a thread to a user without access to `thread.AccountID` (`ErrAssigneeNoAccountAccess`), add `AssignUnassignedThreadsForAccount` to process unassigned threads when a default assignee is set, populate `AccountID` on `imap_jobs`, and verify matching `AccountID` on `ReopenAndMerge`.
  - Update unit tests in `engine_test.go`, `matcher_test.go`, and `service_test.go`.
- [x] **Step 5: Attachment Deduplication & Multi-Account Mail Workers (`internal/storage/`, `internal/mail/`)**
  - Ensure `storage.Store.Save` is idempotent when saving identical attachment bytes across accounts and add a test in `attachments_test.go`.
  - Scope `IMAPSyncWorker` to a single `accountID`, route `IMAPActionWorker` jobs by `job.AccountID`, and route `OutboxWorker` replies/SMTP transports by `thread.AccountID` / `email.AccountID`.
  - Update `imap_workers_test.go` and `smtp_sender_test.go` (including a test verifying that the same RFC 5322 email with an attachment synced across two accounts produces two threads and a single attachment file on disk).
- [x] **Step 6: Web Authorization & Account UI (`internal/web/`, `cmd/minicrm/`, `e2e/`)**
  - Enforce account access checks in all thread, email body (`/emails/{id}/body`), and attachment download (`/attachments/{id}/download`) handlers.
  - Filter the thread detail assignee dropdown to only users with access to `thread.AccountID`, and enforce `ErrAssigneeNoAccountAccess` (`422`) on `POST /threads/{id}/assign`.
  - Update `views.go` to show the Account badge, column, and inbox filter for multi-account users and hide them for single-account users.
  - Update `/settings` view and handler to manage per-account default assignees (restricted to users with access to each account) and pre-created user account permissions.
  - Wire N accounts in `cmd/minicrm/main.go`, update `.github/workflows/ci.yml`, `e2e/auth_e2e_test.go`, `web_test.go`, and documentation (`README.md`, `docs/*.md`).

---

## Verification

1. **Automated Linting & Race-Enabled Tests**:
   ```bash
   golangci-lint run ./...
   go test -v -race ./...
   ```
2. **Key Test Scenarios Covered**:
   - **OIDC Group-to-Account Access**: User with group `"crm-support"` gets `allowed_accounts = ["support"]`; user with `["crm-support", "crm-sales"]` gets `["support", "sales"]`; user with neither group gets `403 Forbidden`.
   - **Thread, Assignee & Attachment Isolation**:
     - User with access only to `"support"` gets `404` when requesting `GET /threads/{sales_thread}`, `POST /threads/{sales_thread}/*`, `GET /emails/{sales_email}/body`, or `GET /attachments/{sales_attachment}/download` (even when `sales_attachment` shares the same SHA-256 disk blob as a `"support"` attachment).
     - Thread detail view for a `"sales"` thread only lists users with `"sales"` access in the Assignee dropdown, and `POST /threads/{sales_thread}/assign` with a user lacking `"sales"` access returns `422 Unprocessable Entity`.
   - **Unassigned Thread Lifecycle**:
     - When `account_settings` has no row (or `default_assignee_id IS NULL`) for `"sales"`, an incoming email creates a thread with `AssigneeID == nil` (`assignment_source = "unassigned"`).
     - Setting a default assignee for `"sales"` in `/settings` automatically assigns existing unassigned `"sales"` threads to that user and records `assigned` thread events; alternatively, an operator with `"sales"` access can take/assign or reply to an unassigned thread directly.
   - **Cross-Account Email & Deduplicated Attachment**: Ingesting an identical RFC 5322 message (same `Message-ID` and attachment) via `"support"` and `"sales"` sync workers creates two separate threads and two `attachments` DB rows pointing to a single SHA-256 file on disk.
   - **Conditional UI**: HTML response for `/threads` and `/threads/{id}` includes the Account filter, column, and badge for a multi-account user, and omits them for a single-account user.
