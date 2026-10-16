-- +goose Up
-- +goose StatementBegin

CREATE EXTENSION IF NOT EXISTS citext;

-- 1. Users (Assignees & Operators)
-- Note: User creation, updates, and logins are recorded in audit_logs.
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    oidc_sub        TEXT UNIQUE,                -- Nullable until first OIDC login if pre-created by admin
    email           CITEXT NOT NULL UNIQUE,     -- Used to match pre-created users on first OIDC login
    display_name    TEXT NOT NULL,
    role            TEXT NOT NULL DEFAULT 'agent' CHECK (role IN ('agent', 'admin')),
    is_active       BOOLEAN NOT NULL DEFAULT TRUE
);

-- 2. System Settings (Singleton Configuration)
-- Note: Updates to system_settings are recorded in audit_logs.
CREATE TABLE system_settings (
    id                        INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    default_assignee_id       UUID NOT NULL REFERENCES users(id),
    display_timezone          TEXT NOT NULL DEFAULT 'UTC',
    imap_inbox_mailbox        TEXT NOT NULL DEFAULT 'INBOX',
    imap_archive_mailbox      TEXT NOT NULL DEFAULT 'Archive',
    imap_junk_mailbox         TEXT NOT NULL DEFAULT 'Junk',
    imap_sent_mailbox         TEXT NOT NULL DEFAULT 'Sent',
    subject_match_window_days INT NOT NULL DEFAULT 30
);

-- 3. Workflow State Enum
CREATE TYPE thread_status AS ENUM ('new', 'in_progress', 'completed', 'spam');

-- 4. Conversation Threads
-- Note: Thread creation and all state/assignment updates are recorded in thread_events.
CREATE TABLE threads (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_normalized      TEXT NOT NULL,
    subject_display         TEXT NOT NULL,
    status                  thread_status NOT NULL DEFAULT 'new',
    assignee_id             UUID NOT NULL REFERENCES users(id),
    assignment_source       TEXT NOT NULL DEFAULT 'default',
    customer_email          CITEXT NOT NULL,
    customer_name           TEXT,
    reopens_thread_id       UUID REFERENCES threads(id) ON DELETE SET NULL,
    last_message_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_threads_status_last_msg ON threads (status, last_message_at DESC);
CREATE INDEX idx_threads_assignee_status ON threads (assignee_id, status, last_message_at DESC);
CREATE INDEX idx_threads_subject_customer ON threads (lower(subject_normalized), customer_email, last_message_at DESC);

-- 5. Emails (Both Incoming and Outgoing)
CREATE TABLE emails (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id           UUID NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    parent_email_id     UUID REFERENCES emails(id) ON DELETE SET NULL,
    direction           TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    message_id          TEXT NOT NULL UNIQUE,
    in_reply_to         TEXT,
    references_ids      TEXT[] NOT NULL DEFAULT '{}',
    imap_mailbox        TEXT,
    imap_uidvalidity    BIGINT,
    imap_uid            BIGINT,
    from_address        CITEXT NOT NULL,
    from_name           TEXT,
    to_addresses        JSONB NOT NULL DEFAULT '[]',
    cc_addresses        JSONB NOT NULL DEFAULT '[]',
    subject             TEXT NOT NULL,
    subject_normalized  TEXT NOT NULL,
    body_text           TEXT,
    body_html_sanitized TEXT,
    raw_headers         JSONB NOT NULL DEFAULT '{}',
    sent_at             TIMESTAMPTZ NOT NULL,
    received_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_emails_thread_sent ON emails (thread_id, sent_at ASC);
CREATE INDEX idx_emails_message_id ON emails (message_id);
CREATE INDEX idx_emails_imap_uid ON emails (imap_mailbox, imap_uidvalidity, imap_uid);

-- 6. Attachments (Metadata in Postgres, binary payload on local disk)
CREATE TABLE attachments (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email_id        UUID NOT NULL REFERENCES emails(id) ON DELETE CASCADE,
    filename        TEXT NOT NULL,
    content_type    TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    sha256_hex      CHAR(64) NOT NULL,
    storage_relpath TEXT NOT NULL,
    content_id      TEXT,
    is_inline       BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX idx_attachments_email_id ON attachments (email_id);

-- 7. IMAP Sync State (Tracks UID watermark for INBOX)
CREATE TABLE imap_sync_state (
    mailbox         TEXT PRIMARY KEY,
    uid_validity    BIGINT NOT NULL,
    last_synced_uid BIGINT NOT NULL DEFAULT 0
);

-- 8. Asynchronous IMAP Jobs (Moving completed emails to Archive, spam to Junk, appending Sent)
CREATE TABLE imap_jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_type        TEXT NOT NULL CHECK (job_type IN ('move_to_archive', 'move_to_junk', 'append_to_sent')),
    email_id        UUID NOT NULL REFERENCES emails(id) ON DELETE CASCADE,
    source_mailbox  TEXT,
    target_mailbox  TEXT NOT NULL,
    imap_uidvalidity BIGINT,
    imap_uid        BIGINT,
    raw_rfc822_path TEXT,
    attempts        INT NOT NULL DEFAULT 0,
    max_attempts    INT NOT NULL DEFAULT 10,
    next_run_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    enqueued_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_imap_jobs_runnable ON imap_jobs (status, next_run_at) WHERE status = 'pending';

-- 9. Outgoing Email Queue (Transactional SMTP Outbox)
CREATE TABLE outbox_emails (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email_id        UUID NOT NULL UNIQUE REFERENCES emails(id) ON DELETE CASCADE,
    sender_user_id  UUID NOT NULL REFERENCES users(id),
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sending', 'sent', 'failed')),
    attempts        INT NOT NULL DEFAULT 0,
    next_run_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    enqueued_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 10. Thread & Assignment Audit Events
CREATE TABLE thread_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id       UUID NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    actor_user_id   UUID REFERENCES users(id),
    event_type      TEXT NOT NULL CHECK (event_type IN (
                        'created', 'status_changed', 'assigned', 'reassigned',
                        'reply_received', 'reply_sent', 'reopen_merged'
                    )),
    old_value       JSONB,
    new_value       JSONB,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_thread_events_thread ON thread_events (thread_id, occurred_at ASC);

-- 11. System & User Audit Trail
CREATE TABLE audit_logs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_user_id   UUID REFERENCES users(id),
    target_user_id  UUID REFERENCES users(id),
    event_type      TEXT NOT NULL CHECK (event_type IN (
                        'user_created', 'user_precreated', 'user_updated',
                        'user_login', 'user_login_failed', 'user_logout',
                        'settings_updated'
                    )),
    ip_address      TEXT,
    user_agent      TEXT,
    old_value       JSONB,
    new_value       JSONB,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_logs_occurred_at ON audit_logs (occurred_at DESC);
CREATE INDEX idx_audit_logs_target_user ON audit_logs (target_user_id, occurred_at DESC);

-- 12. HTTP Sessions (for alexedwards/scs pgxstore)
CREATE TABLE sessions (
    token   TEXT PRIMARY KEY,
    data    BYTEA NOT NULL,
    expiry  TIMESTAMPTZ NOT NULL
);

CREATE INDEX sessions_expiry_idx ON sessions (expiry);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS thread_events;
DROP TABLE IF EXISTS outbox_emails;
DROP TABLE IF EXISTS imap_jobs;
DROP TABLE IF EXISTS imap_sync_state;
DROP TABLE IF EXISTS attachments;
DROP TABLE IF EXISTS emails;
DROP TABLE IF EXISTS threads;
DROP TYPE IF EXISTS thread_status;
DROP TABLE IF EXISTS system_settings;
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
