-- name: GetUserByID :one
SELECT id, oidc_sub, email, display_name, role, is_active, allowed_accounts
FROM users
WHERE id = $1;

-- name: GetUserByOIDCSub :one
SELECT id, oidc_sub, email, display_name, role, is_active, allowed_accounts
FROM users
WHERE oidc_sub = $1;

-- name: GetUserByEmail :one
SELECT id, oidc_sub, email, display_name, role, is_active, allowed_accounts
FROM users
WHERE email = $1;

-- name: ListUsers :many
SELECT id, oidc_sub, email, display_name, role, is_active, allowed_accounts
FROM users
ORDER BY display_name ASC;

-- name: CreateUser :one
INSERT INTO users (oidc_sub, email, display_name, role, is_active, allowed_accounts)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, oidc_sub, email, display_name, role, is_active, allowed_accounts;

-- name: UpdateUserOIDC :one
UPDATE users
SET oidc_sub = $2, display_name = $3, allowed_accounts = $4
WHERE id = $1
RETURNING id, oidc_sub, email, display_name, role, is_active, allowed_accounts;

-- name: GetSystemSettings :one
SELECT id, display_timezone, imap_inbox_mailbox,
       imap_archive_mailbox, imap_junk_mailbox, imap_sent_mailbox, subject_match_window_days
FROM system_settings
WHERE id = 1;

-- name: UpsertSystemSettings :one
INSERT INTO system_settings (
    id, display_timezone, imap_inbox_mailbox,
    imap_archive_mailbox, imap_junk_mailbox, imap_sent_mailbox, subject_match_window_days
) VALUES (1, $1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO UPDATE SET
    display_timezone = EXCLUDED.display_timezone,
    imap_inbox_mailbox = EXCLUDED.imap_inbox_mailbox,
    imap_archive_mailbox = EXCLUDED.imap_archive_mailbox,
    imap_junk_mailbox = EXCLUDED.imap_junk_mailbox,
    imap_sent_mailbox = EXCLUDED.imap_sent_mailbox,
    subject_match_window_days = EXCLUDED.subject_match_window_days
RETURNING id, display_timezone, imap_inbox_mailbox,
          imap_archive_mailbox, imap_junk_mailbox, imap_sent_mailbox, subject_match_window_days;

-- name: GetAccountSettings :one
SELECT account_id, default_assignee_id
FROM account_settings
WHERE account_id = $1;

-- name: ListAccountSettings :many
SELECT account_id, default_assignee_id
FROM account_settings
ORDER BY account_id ASC;

-- name: UpsertAccountSettings :one
INSERT INTO account_settings (account_id, default_assignee_id)
VALUES ($1, $2)
ON CONFLICT (account_id) DO UPDATE SET default_assignee_id = EXCLUDED.default_assignee_id
RETURNING account_id, default_assignee_id;

-- name: AssignUnassignedThreadsByAccount :many
UPDATE threads
   SET assignee_id = $2, assignment_source = $3
 WHERE account_id = $1
   AND assignee_id IS NULL
   AND status IN ('new', 'in_progress')
RETURNING id;

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (actor_user_id, target_user_id, event_type, ip_address, user_agent, old_value, new_value)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertThreadEvent :exec
INSERT INTO thread_events (thread_id, actor_user_id, event_type, old_value, new_value)
VALUES ($1, $2, $3, $4, $5);

-- name: FindEmailByMessageIDs :one
SELECT id, thread_id, account_id, parent_email_id, direction, message_id, in_reply_to, references_ids,
       imap_mailbox, imap_uidvalidity, imap_uid, from_address, COALESCE(from_name, ''),
       to_addresses, cc_addresses, subject, subject_normalized,
       COALESCE(body_text, ''), COALESCE(body_html_sanitized, ''), raw_headers, sent_at, received_at
FROM emails
WHERE account_id = $1
  AND message_id = ANY($2)
ORDER BY sent_at DESC
LIMIT 1;

-- name: FindThreadBySubjectAndParticipant :one
SELECT id, account_id, subject_normalized, subject_display, status, assignee_id,
       assignment_source, customer_email, COALESCE(customer_name, ''), reopens_thread_id, last_message_at
FROM threads
WHERE account_id = $1
  AND lower(subject_normalized) = lower($2)
  AND subject_normalized <> ''
  AND customer_email = $3
  AND last_message_at >= $4
  AND status <> 'spam'
ORDER BY last_message_at DESC
LIMIT 1;

-- name: SetEmailIMAPLocation :execrows
UPDATE emails
   SET imap_mailbox = $2, imap_uidvalidity = $3, imap_uid = $4
 WHERE id = $1 AND imap_uid IS NULL;
