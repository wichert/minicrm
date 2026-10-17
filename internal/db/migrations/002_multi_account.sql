-- +goose Up
-- +goose StatementBegin

-- 1. Users: the set of mail account IDs a user may access (resolved from OIDC groups on login).
ALTER TABLE users ADD COLUMN allowed_accounts TEXT[] NOT NULL DEFAULT '{}';

-- 2. Per-account settings: replaces the single global default assignee.
--    A missing row (or a NULL default_assignee_id) means the account has no default
--    assignee yet; incoming threads then remain unassigned until one is configured
--    or an operator takes the thread.
CREATE TABLE account_settings (
    account_id          TEXT PRIMARY KEY,
    default_assignee_id UUID REFERENCES users(id) ON DELETE SET NULL
);

ALTER TABLE system_settings DROP COLUMN default_assignee_id;

-- 3. Threads may be unassigned until a default assignee is configured or an operator takes them.
ALTER TABLE threads ALTER COLUMN assignee_id DROP NOT NULL;
ALTER TABLE threads ALTER COLUMN assignment_source SET DEFAULT 'unassigned';

-- 4. Scope data by mail account. account_id is added nullable, backfilled for any
--    pre-existing single-account rows using the legacy account id provided by the
--    migration runner, and only then made NOT NULL (so no DEFAULT is left behind).
ALTER TABLE threads ADD COLUMN account_id TEXT;
UPDATE threads
   SET account_id = COALESCE(NULLIF(current_setting('minicrm.legacy_account_id', true), ''), 'legacy')
 WHERE account_id IS NULL;
ALTER TABLE threads ALTER COLUMN account_id SET NOT NULL;

ALTER TABLE emails ADD COLUMN account_id TEXT;
UPDATE emails
   SET account_id = COALESCE(NULLIF(current_setting('minicrm.legacy_account_id', true), ''), 'legacy')
 WHERE account_id IS NULL;
ALTER TABLE emails ALTER COLUMN account_id SET NOT NULL;

ALTER TABLE imap_jobs ADD COLUMN account_id TEXT;
UPDATE imap_jobs
   SET account_id = COALESCE(NULLIF(current_setting('minicrm.legacy_account_id', true), ''), 'legacy')
 WHERE account_id IS NULL;
ALTER TABLE imap_jobs ALTER COLUMN account_id SET NOT NULL;

-- imap_sync_state gets account_id as part of a new primary key.
ALTER TABLE imap_sync_state ADD COLUMN account_id TEXT;
UPDATE imap_sync_state
   SET account_id = COALESCE(NULLIF(current_setting('minicrm.legacy_account_id', true), ''), 'legacy')
 WHERE account_id IS NULL;
ALTER TABLE imap_sync_state ALTER COLUMN account_id SET NOT NULL;

-- 5. Indexes & constraints scoped per account.

-- A single email delivered to multiple accounts becomes one row per account,
-- hence Message-ID uniqueness is per account rather than global.
ALTER TABLE emails DROP CONSTRAINT emails_message_id_key;
ALTER TABLE emails ADD CONSTRAINT emails_account_message_id_key UNIQUE (account_id, message_id);

DROP INDEX IF EXISTS idx_emails_imap_uid;
CREATE INDEX idx_emails_imap_uid ON emails (account_id, imap_mailbox, imap_uidvalidity, imap_uid);

DROP INDEX IF EXISTS idx_threads_subject_customer;
CREATE INDEX idx_threads_account_subject_customer
    ON threads (account_id, lower(subject_normalized), customer_email, last_message_at DESC);
CREATE INDEX idx_threads_account_status_last_msg
    ON threads (account_id, status, last_message_at DESC);

ALTER TABLE imap_sync_state DROP CONSTRAINT imap_sync_state_pkey;
ALTER TABLE imap_sync_state ADD CONSTRAINT imap_sync_state_pkey PRIMARY KEY (account_id, mailbox);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE imap_sync_state DROP CONSTRAINT imap_sync_state_pkey;
DELETE FROM imap_sync_state a USING imap_sync_state b
 WHERE a.account_id <> b.account_id AND a.mailbox = b.mailbox;
ALTER TABLE imap_sync_state ADD CONSTRAINT imap_sync_state_pkey PRIMARY KEY (mailbox);

DROP INDEX IF EXISTS idx_threads_account_status_last_msg;
DROP INDEX IF EXISTS idx_threads_account_subject_customer;
CREATE INDEX idx_threads_subject_customer
    ON threads (lower(subject_normalized), customer_email, last_message_at DESC);

DROP INDEX IF EXISTS idx_emails_imap_uid;
CREATE INDEX idx_emails_imap_uid ON emails (imap_mailbox, imap_uidvalidity, imap_uid);

ALTER TABLE emails DROP CONSTRAINT emails_account_message_id_key;
DELETE FROM emails a USING emails b WHERE a.account_id <> b.account_id AND a.message_id = b.message_id;
ALTER TABLE emails ADD CONSTRAINT emails_message_id_key UNIQUE (message_id);

ALTER TABLE imap_sync_state DROP COLUMN account_id;
ALTER TABLE imap_jobs DROP COLUMN account_id;
ALTER TABLE emails DROP COLUMN account_id;
ALTER TABLE threads DROP COLUMN account_id;

ALTER TABLE threads ALTER COLUMN assignment_source SET DEFAULT 'default';
UPDATE threads SET assignee_id = (SELECT id FROM users ORDER BY id LIMIT 1) WHERE assignee_id IS NULL;
ALTER TABLE threads ALTER COLUMN assignee_id SET NOT NULL;

ALTER TABLE system_settings ADD COLUMN default_assignee_id UUID NOT NULL REFERENCES users(id);

DROP TABLE IF EXISTS account_settings;
ALTER TABLE users DROP COLUMN allowed_accounts;
-- +goose StatementEnd
