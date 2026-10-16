-- name: GetUserByID :one
SELECT id, oidc_sub, email, display_name, role, is_active
FROM users
WHERE id = $1;

-- name: GetUserByOIDCSub :one
SELECT id, oidc_sub, email, display_name, role, is_active
FROM users
WHERE oidc_sub = $1;

-- name: GetUserByEmail :one
SELECT id, oidc_sub, email, display_name, role, is_active
FROM users
WHERE email = $1;

-- name: ListUsers :many
SELECT id, oidc_sub, email, display_name, role, is_active
FROM users
ORDER BY display_name ASC;

-- name: CreateUser :one
INSERT INTO users (oidc_sub, email, display_name, role, is_active)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, oidc_sub, email, display_name, role, is_active;

-- name: UpdateUserOIDC :one
UPDATE users
SET oidc_sub = $2, display_name = $3
WHERE id = $1
RETURNING id, oidc_sub, email, display_name, role, is_active;

-- name: GetSystemSettings :one
SELECT id, default_assignee_id, display_timezone, imap_inbox_mailbox,
       imap_archive_mailbox, imap_junk_mailbox, imap_sent_mailbox, subject_match_window_days
FROM system_settings
WHERE id = 1;

-- name: UpsertSystemSettings :one
INSERT INTO system_settings (
    id, default_assignee_id, display_timezone, imap_inbox_mailbox,
    imap_archive_mailbox, imap_junk_mailbox, imap_sent_mailbox, subject_match_window_days
) VALUES (1, $1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE SET
    default_assignee_id = EXCLUDED.default_assignee_id,
    display_timezone = EXCLUDED.display_timezone,
    imap_inbox_mailbox = EXCLUDED.imap_inbox_mailbox,
    imap_archive_mailbox = EXCLUDED.imap_archive_mailbox,
    imap_junk_mailbox = EXCLUDED.imap_junk_mailbox,
    imap_sent_mailbox = EXCLUDED.imap_sent_mailbox,
    subject_match_window_days = EXCLUDED.subject_match_window_days
RETURNING id, default_assignee_id, display_timezone, imap_inbox_mailbox,
          imap_archive_mailbox, imap_junk_mailbox, imap_sent_mailbox, subject_match_window_days;

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (actor_user_id, target_user_id, event_type, ip_address, user_agent, old_value, new_value)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertThreadEvent :exec
INSERT INTO thread_events (thread_id, actor_user_id, event_type, old_value, new_value)
VALUES ($1, $2, $3, $4, $5);
