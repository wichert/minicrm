package sqlc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is implemented by *pgxpool.Pool, *pgx.Conn, and pgx.Tx.
type DBTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Queries executes typed SQL queries against DBTX.
type Queries struct {
	db DBTX
}

// New creates a new Queries instance bound to db.
func New(db DBTX) *Queries {
	return &Queries{db: db}
}

// WithTx returns a Queries bound to the given transaction.
func (q *Queries) WithTx(tx pgx.Tx) *Queries {
	return &Queries{db: tx}
}

// GetUserByID fetches a user by UUID.
func (q *Queries) GetUserByID(ctx context.Context, id uuid.UUID) (User, error) {
	const query = `SELECT id, oidc_sub, email, display_name, role, is_active FROM users WHERE id = $1`
	var u User
	err := q.db.QueryRow(ctx, query, id).Scan(&u.ID, &u.OIDCSub, &u.Email, &u.DisplayName, &u.Role, &u.IsActive)
	return u, err
}

// GetUserByOIDCSub fetches a user by OIDC subject identifier.
func (q *Queries) GetUserByOIDCSub(ctx context.Context, sub string) (User, error) {
	const query = `SELECT id, oidc_sub, email, display_name, role, is_active FROM users WHERE oidc_sub = $1`
	var u User
	err := q.db.QueryRow(ctx, query, sub).Scan(&u.ID, &u.OIDCSub, &u.Email, &u.DisplayName, &u.Role, &u.IsActive)
	return u, err
}

// GetUserByEmail fetches a user by email (case-insensitive via CITEXT).
func (q *Queries) GetUserByEmail(ctx context.Context, email string) (User, error) {
	const query = `SELECT id, oidc_sub, email, display_name, role, is_active FROM users WHERE email = $1`
	var u User
	err := q.db.QueryRow(ctx, query, email).Scan(&u.ID, &u.OIDCSub, &u.Email, &u.DisplayName, &u.Role, &u.IsActive)
	return u, err
}

// ListUsers returns all users ordered by display_name.
func (q *Queries) ListUsers(ctx context.Context) ([]User, error) {
	const query = `SELECT id, oidc_sub, email, display_name, role, is_active FROM users ORDER BY display_name ASC`
	rows, err := q.db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.OIDCSub, &u.Email, &u.DisplayName, &u.Role, &u.IsActive); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// CreateUserParams holds parameters for creating a user.
type CreateUserParams struct {
	OIDCSub     *string
	Email       string
	DisplayName string
	Role        string
	IsActive    bool
}

// CreateUser inserts a new user row.
func (q *Queries) CreateUser(ctx context.Context, arg CreateUserParams) (User, error) {
	const query = `
		INSERT INTO users (oidc_sub, email, display_name, role, is_active)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, oidc_sub, email, display_name, role, is_active`
	var u User
	err := q.db.QueryRow(ctx, query, arg.OIDCSub, arg.Email, arg.DisplayName, arg.Role, arg.IsActive).
		Scan(&u.ID, &u.OIDCSub, &u.Email, &u.DisplayName, &u.Role, &u.IsActive)
	return u, err
}

// UpdateUserOIDC binds an OIDC sub and updated display_name to an existing pre-created user.
func (q *Queries) UpdateUserOIDC(ctx context.Context, id uuid.UUID, oidcSub, displayName string) (User, error) {
	const query = `
		UPDATE users
		SET oidc_sub = $2, display_name = $3
		WHERE id = $1
		RETURNING id, oidc_sub, email, display_name, role, is_active`
	var u User
	err := q.db.QueryRow(ctx, query, id, oidcSub, displayName).
		Scan(&u.ID, &u.OIDCSub, &u.Email, &u.DisplayName, &u.Role, &u.IsActive)
	return u, err
}

// GetSystemSettings returns the singleton system_settings row.
func (q *Queries) GetSystemSettings(ctx context.Context) (SystemSettings, error) {
	const query = `
		SELECT id, default_assignee_id, display_timezone, imap_inbox_mailbox,
		       imap_archive_mailbox, imap_junk_mailbox, imap_sent_mailbox, subject_match_window_days
		FROM system_settings WHERE id = 1`
	var s SystemSettings
	err := q.db.QueryRow(ctx, query).Scan(
		&s.ID, &s.DefaultAssigneeID, &s.DisplayTimezone,
		&s.IMAPInboxMailbox, &s.IMAPArchiveMailbox, &s.IMAPJunkMailbox, &s.IMAPSentMailbox,
		&s.SubjectMatchWindowDays,
	)
	return s, err
}

// UpsertSystemSettings inserts or updates the singleton system_settings row.
func (q *Queries) UpsertSystemSettings(ctx context.Context, s SystemSettings) (SystemSettings, error) {
	const query = `
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
		          imap_archive_mailbox, imap_junk_mailbox, imap_sent_mailbox, subject_match_window_days`
	var out SystemSettings
	err := q.db.QueryRow(ctx, query,
		s.DefaultAssigneeID, s.DisplayTimezone, s.IMAPInboxMailbox,
		s.IMAPArchiveMailbox, s.IMAPJunkMailbox, s.IMAPSentMailbox, s.SubjectMatchWindowDays,
	).Scan(
		&out.ID, &out.DefaultAssigneeID, &out.DisplayTimezone,
		&out.IMAPInboxMailbox, &out.IMAPArchiveMailbox, &out.IMAPJunkMailbox, &out.IMAPSentMailbox,
		&out.SubjectMatchWindowDays,
	)
	return out, err
}

// InsertAuditLog records an immutable audit event in audit_logs.
func (q *Queries) InsertAuditLog(ctx context.Context, entry AuditLog) error {
	const query = `
		INSERT INTO audit_logs (actor_user_id, target_user_id, event_type, ip_address, user_agent, old_value, new_value)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := q.db.Exec(ctx, query,
		entry.ActorUserID, entry.TargetUserID, entry.EventType,
		entry.IPAddress, entry.UserAgent, nullableJSON(entry.OldValue), nullableJSON(entry.NewValue),
	)
	return err
}

// ListAuditLogs returns the most recent audit log entries.
func (q *Queries) ListAuditLogs(ctx context.Context, limit int) ([]AuditLog, error) {
	const query = `
		SELECT id, actor_user_id, target_user_id, event_type, COALESCE(ip_address, ''), COALESCE(user_agent, ''),
		       COALESCE(old_value, 'null'::jsonb), COALESCE(new_value, 'null'::jsonb), occurred_at
		FROM audit_logs
		ORDER BY occurred_at DESC
		LIMIT $1`
	rows, err := q.db.Query(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logs []AuditLog
	for rows.Next() {
		var l AuditLog
		if err := rows.Scan(&l.ID, &l.ActorUserID, &l.TargetUserID, &l.EventType, &l.IPAddress, &l.UserAgent, &l.OldValue, &l.NewValue, &l.OccurredAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// InsertThreadEvent records an immutable thread event in thread_events.
func (q *Queries) InsertThreadEvent(ctx context.Context, ev ThreadEvent) error {
	const query = `
		INSERT INTO thread_events (thread_id, actor_user_id, event_type, old_value, new_value)
		VALUES ($1, $2, $3, $4, $5)`
	_, err := q.db.Exec(ctx, query,
		ev.ThreadID, ev.ActorUserID, ev.EventType, nullableJSON(ev.OldValue), nullableJSON(ev.NewValue),
	)
	return err
}

// ListThreadEvents returns all audit events for a thread in chronological order.
func (q *Queries) ListThreadEvents(ctx context.Context, threadID uuid.UUID) ([]ThreadEvent, error) {
	const query = `
		SELECT id, thread_id, actor_user_id, event_type,
		       COALESCE(old_value, 'null'::jsonb), COALESCE(new_value, 'null'::jsonb), occurred_at
		FROM thread_events
		WHERE thread_id = $1
		ORDER BY occurred_at ASC`
	rows, err := q.db.Query(ctx, query, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []ThreadEvent
	for rows.Next() {
		var ev ThreadEvent
		if err := rows.Scan(&ev.ID, &ev.ThreadID, &ev.ActorUserID, &ev.EventType, &ev.OldValue, &ev.NewValue, &ev.OccurredAt); err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// CreateThread inserts a new conversation thread.
func (q *Queries) CreateThread(ctx context.Context, t Thread) (Thread, error) {
	const query = `
		INSERT INTO threads (
			subject_normalized, subject_display, status, assignee_id,
			assignment_source, customer_email, customer_name, reopens_thread_id, last_message_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, subject_normalized, subject_display, status, assignee_id,
		          assignment_source, customer_email, COALESCE(customer_name, ''), reopens_thread_id, last_message_at`
	if t.LastMessageAt.IsZero() {
		t.LastMessageAt = time.Now().UTC()
	}
	var out Thread
	err := q.db.QueryRow(ctx, query,
		t.SubjectNormalized, t.SubjectDisplay, t.Status, t.AssigneeID,
		t.AssignmentSource, t.CustomerEmail, t.CustomerName, t.ReopensThreadID, t.LastMessageAt,
	).Scan(
		&out.ID, &out.SubjectNormalized, &out.SubjectDisplay, &out.Status, &out.AssigneeID,
		&out.AssignmentSource, &out.CustomerEmail, &out.CustomerName, &out.ReopensThreadID, &out.LastMessageAt,
	)
	return out, err
}

// GetThreadByID fetches a thread by UUID.
func (q *Queries) GetThreadByID(ctx context.Context, id uuid.UUID) (Thread, error) {
	const query = `
		SELECT id, subject_normalized, subject_display, status, assignee_id,
		       assignment_source, customer_email, COALESCE(customer_name, ''), reopens_thread_id, last_message_at
		FROM threads WHERE id = $1`
	var out Thread
	err := q.db.QueryRow(ctx, query, id).Scan(
		&out.ID, &out.SubjectNormalized, &out.SubjectDisplay, &out.Status, &out.AssigneeID,
		&out.AssignmentSource, &out.CustomerEmail, &out.CustomerName, &out.ReopensThreadID, &out.LastMessageAt,
	)
	return out, err
}

// ListThreadsFilter specifies optional filters for listing threads.
type ListThreadsFilter struct {
	Status     *ThreadStatus
	AssigneeID *uuid.UUID
	Limit      int
}

// ListThreads returns threads matching the given filter ordered by last_message_at DESC.
func (q *Queries) ListThreads(ctx context.Context, f ListThreadsFilter) ([]Thread, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	const query = `
		SELECT id, subject_normalized, subject_display, status, assignee_id,
		       assignment_source, customer_email, COALESCE(customer_name, ''), reopens_thread_id, last_message_at
		FROM threads
		WHERE ($1::thread_status IS NULL OR status = $1)
		  AND ($2::uuid IS NULL OR assignee_id = $2)
		ORDER BY last_message_at DESC
		LIMIT $3`
	rows, err := q.db.Query(ctx, query, f.Status, f.AssigneeID, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Thread
	for rows.Next() {
		var t Thread
		if err := rows.Scan(
			&t.ID, &t.SubjectNormalized, &t.SubjectDisplay, &t.Status, &t.AssigneeID,
			&t.AssignmentSource, &t.CustomerEmail, &t.CustomerName, &t.ReopensThreadID, &t.LastMessageAt,
		); err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

// UpdateThreadStatus updates the status of a thread.
func (q *Queries) UpdateThreadStatus(ctx context.Context, id uuid.UUID, status ThreadStatus) error {
	const query = `UPDATE threads SET status = $2 WHERE id = $1`
	_, err := q.db.Exec(ctx, query, id, status)
	return err
}

// UpdateThreadAssignee updates the assignee_id and assignment_source of a thread.
func (q *Queries) UpdateThreadAssignee(ctx context.Context, id, assigneeID uuid.UUID, source string) error {
	const query = `UPDATE threads SET assignee_id = $2, assignment_source = $3 WHERE id = $1`
	_, err := q.db.Exec(ctx, query, id, assigneeID, source)
	return err
}

// TouchThreadLastMessageAt updates last_message_at on a thread.
func (q *Queries) TouchThreadLastMessageAt(ctx context.Context, id uuid.UUID, ts time.Time) error {
	const query = `UPDATE threads SET last_message_at = GREATEST(last_message_at, $2) WHERE id = $1`
	_, err := q.db.Exec(ctx, query, id, ts)
	return err
}

// DeleteThread deletes a thread by ID.
func (q *Queries) DeleteThread(ctx context.Context, id uuid.UUID) error {
	const query = `DELETE FROM threads WHERE id = $1`
	_, err := q.db.Exec(ctx, query, id)
	return err
}

// CreateEmail inserts an email record.
func (q *Queries) CreateEmail(ctx context.Context, e Email) (Email, error) {
	toJSON, err := json.Marshal(e.ToAddresses)
	if err != nil {
		return Email{}, fmt.Errorf("marshal to_addresses: %w", err)
	}
	ccJSON, err := json.Marshal(e.CCAddresses)
	if err != nil {
		return Email{}, fmt.Errorf("marshal cc_addresses: %w", err)
	}
	hdrJSON, err := json.Marshal(e.RawHeaders)
	if err != nil {
		return Email{}, fmt.Errorf("marshal raw_headers: %w", err)
	}
	if e.ReferencesIDs == nil {
		e.ReferencesIDs = []string{}
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now().UTC()
	}
	const query = `
		INSERT INTO emails (
			thread_id, parent_email_id, direction, message_id, in_reply_to, references_ids,
			imap_mailbox, imap_uidvalidity, imap_uid, from_address, from_name,
			to_addresses, cc_addresses, subject, subject_normalized,
			body_text, body_html_sanitized, raw_headers, sent_at, received_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11,
			$12, $13, $14, $15,
			$16, $17, $18, $19, $20
		) RETURNING id`
	err = q.db.QueryRow(ctx, query,
		e.ThreadID, e.ParentEmailID, e.Direction, e.MessageID, e.InReplyTo, e.ReferencesIDs,
		e.IMAPMailbox, e.IMAPUIDValidity, e.IMAPUID, e.FromAddress, e.FromName,
		toJSON, ccJSON, e.Subject, e.SubjectNormalized,
		e.BodyText, e.BodyHTMLSanitized, hdrJSON, e.SentAt, e.ReceivedAt,
	).Scan(&e.ID)
	return e, err
}

// GetEmailByID fetches an email by UUID.
func (q *Queries) GetEmailByID(ctx context.Context, id uuid.UUID) (Email, error) {
	const query = `
		SELECT id, thread_id, parent_email_id, direction, message_id, in_reply_to, references_ids,
		       imap_mailbox, imap_uidvalidity, imap_uid, from_address, COALESCE(from_name, ''),
		       to_addresses, cc_addresses, subject, subject_normalized,
		       COALESCE(body_text, ''), COALESCE(body_html_sanitized, ''), raw_headers, sent_at, received_at
		FROM emails WHERE id = $1`
	return scanEmail(q.db.QueryRow(ctx, query, id))
}

// GetEmailByMessageID fetches an email by its normalized RFC 5322 Message-ID.
func (q *Queries) GetEmailByMessageID(ctx context.Context, msgID string) (Email, error) {
	const query = `
		SELECT id, thread_id, parent_email_id, direction, message_id, in_reply_to, references_ids,
		       imap_mailbox, imap_uidvalidity, imap_uid, from_address, COALESCE(from_name, ''),
		       to_addresses, cc_addresses, subject, subject_normalized,
		       COALESCE(body_text, ''), COALESCE(body_html_sanitized, ''), raw_headers, sent_at, received_at
		FROM emails WHERE message_id = $1`
	return scanEmail(q.db.QueryRow(ctx, query, msgID))
}

// FindEmailByMessageIDs returns the most recent email matching any of the given Message-IDs.
func (q *Queries) FindEmailByMessageIDs(ctx context.Context, msgIDs []string) (Email, error) {
	const query = `
		SELECT id, thread_id, parent_email_id, direction, message_id, in_reply_to, references_ids,
		       imap_mailbox, imap_uidvalidity, imap_uid, from_address, COALESCE(from_name, ''),
		       to_addresses, cc_addresses, subject, subject_normalized,
		       COALESCE(body_text, ''), COALESCE(body_html_sanitized, ''), raw_headers, sent_at, received_at
		FROM emails
		WHERE message_id = ANY($1)
		ORDER BY sent_at DESC
		LIMIT 1`
	return scanEmail(q.db.QueryRow(ctx, query, msgIDs))
}

// FindThreadBySubjectAndParticipant finds the most recent non-spam thread matching normalized subject and customer email within a time window.
func (q *Queries) FindThreadBySubjectAndParticipant(ctx context.Context, normSubject, customerEmail string, since time.Time) (Thread, error) {
	const query = `
		SELECT id, subject_normalized, subject_display, status, assignee_id,
		       assignment_source, customer_email, COALESCE(customer_name, ''), reopens_thread_id, last_message_at
		FROM threads
		WHERE lower(subject_normalized) = lower($1)
		  AND subject_normalized <> ''
		  AND customer_email = $2
		  AND last_message_at >= $3
		  AND status <> 'spam'
		ORDER BY last_message_at DESC
		LIMIT 1`
	var out Thread
	err := q.db.QueryRow(ctx, query, normSubject, customerEmail, since).Scan(
		&out.ID, &out.SubjectNormalized, &out.SubjectDisplay, &out.Status, &out.AssigneeID,
		&out.AssignmentSource, &out.CustomerEmail, &out.CustomerName, &out.ReopensThreadID, &out.LastMessageAt,
	)
	return out, err
}

// ListEmailsByThread returns all emails in a thread ordered by sent_at ASC.
func (q *Queries) ListEmailsByThread(ctx context.Context, threadID uuid.UUID) ([]Email, error) {
	const query = `
		SELECT id, thread_id, parent_email_id, direction, message_id, in_reply_to, references_ids,
		       imap_mailbox, imap_uidvalidity, imap_uid, from_address, COALESCE(from_name, ''),
		       to_addresses, cc_addresses, subject, subject_normalized,
		       COALESCE(body_text, ''), COALESCE(body_html_sanitized, ''), raw_headers, sent_at, received_at
		FROM emails
		WHERE thread_id = $1
		ORDER BY sent_at ASC, received_at ASC`
	rows, err := q.db.Query(ctx, query, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var emails []Email
	for rows.Next() {
		e, err := scanEmail(rows)
		if err != nil {
			return nil, err
		}
		emails = append(emails, e)
	}
	return emails, rows.Err()
}

// ReparentThreadEmailsAndEvents moves all emails and thread_events from sourceThreadID to targetThreadID.
func (q *Queries) ReparentThreadEmailsAndEvents(ctx context.Context, sourceThreadID, targetThreadID uuid.UUID) error {
	if _, err := q.db.Exec(ctx, `UPDATE emails SET thread_id = $2 WHERE thread_id = $1`, sourceThreadID, targetThreadID); err != nil {
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE thread_events SET thread_id = $2 WHERE thread_id = $1`, sourceThreadID, targetThreadID)
	return err
}

// CreateAttachment inserts an attachment metadata record.
func (q *Queries) CreateAttachment(ctx context.Context, a Attachment) (Attachment, error) {
	const query = `
		INSERT INTO attachments (email_id, filename, content_type, size_bytes, sha256_hex, storage_relpath, content_id, is_inline)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`
	err := q.db.QueryRow(ctx, query,
		a.EmailID, a.Filename, a.ContentType, a.SizeBytes, a.SHA256Hex, a.StorageRelPath, a.ContentID, a.IsInline,
	).Scan(&a.ID)
	return a, err
}

// GetAttachmentByID fetches attachment metadata by ID.
func (q *Queries) GetAttachmentByID(ctx context.Context, id uuid.UUID) (Attachment, error) {
	const query = `
		SELECT id, email_id, filename, content_type, size_bytes, sha256_hex, storage_relpath, COALESCE(content_id, ''), is_inline
		FROM attachments WHERE id = $1`
	var a Attachment
	err := q.db.QueryRow(ctx, query, id).Scan(
		&a.ID, &a.EmailID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.SHA256Hex, &a.StorageRelPath, &a.ContentID, &a.IsInline,
	)
	return a, err
}

// ListAttachmentsByEmail returns all attachments for an email.
func (q *Queries) ListAttachmentsByEmail(ctx context.Context, emailID uuid.UUID) ([]Attachment, error) {
	const query = `
		SELECT id, email_id, filename, content_type, size_bytes, sha256_hex, storage_relpath, COALESCE(content_id, ''), is_inline
		FROM attachments WHERE email_id = $1 ORDER BY filename ASC`
	rows, err := q.db.Query(ctx, query, emailID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Attachment
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.EmailID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.SHA256Hex, &a.StorageRelPath, &a.ContentID, &a.IsInline); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// GetIMAPSyncState fetches the UID watermark for a mailbox.
func (q *Queries) GetIMAPSyncState(ctx context.Context, mailbox string) (IMAPSyncState, error) {
	const query = `SELECT mailbox, uid_validity, last_synced_uid FROM imap_sync_state WHERE mailbox = $1`
	var s IMAPSyncState
	err := q.db.QueryRow(ctx, query, mailbox).Scan(&s.Mailbox, &s.UIDValidity, &s.LastSyncedUID)
	return s, err
}

// UpsertIMAPSyncState updates the UID watermark for a mailbox.
func (q *Queries) UpsertIMAPSyncState(ctx context.Context, s IMAPSyncState) error {
	const query = `
		INSERT INTO imap_sync_state (mailbox, uid_validity, last_synced_uid)
		VALUES ($1, $2, $3)
		ON CONFLICT (mailbox) DO UPDATE SET
			uid_validity = EXCLUDED.uid_validity,
			last_synced_uid = EXCLUDED.last_synced_uid`
	_, err := q.db.Exec(ctx, query, s.Mailbox, s.UIDValidity, s.LastSyncedUID)
	return err
}

// EnqueueIMAPJob inserts a pending job into imap_jobs.
func (q *Queries) EnqueueIMAPJob(ctx context.Context, job IMAPJob) (IMAPJob, error) {
	if job.MaxAttempts <= 0 {
		job.MaxAttempts = 10
	}
	const query = `
		INSERT INTO imap_jobs (
			job_type, email_id, source_mailbox, target_mailbox,
			imap_uidvalidity, imap_uid, raw_rfc822_path, max_attempts, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending')
		RETURNING id, enqueued_at`
	err := q.db.QueryRow(ctx, query,
		job.JobType, job.EmailID, job.SourceMailbox, job.TargetMailbox,
		job.IMAPUIDValidity, job.IMAPUID, job.RawRFC822Path, job.MaxAttempts,
	).Scan(&job.ID, &job.EnqueuedAt)
	job.Status = "pending"
	return job, err
}

// ClaimNextIMAPJob claims the next runnable pending job using FOR UPDATE SKIP LOCKED.
func (q *Queries) ClaimNextIMAPJob(ctx context.Context) (IMAPJob, error) {
	const query = `
		UPDATE imap_jobs
		SET status = 'processing', attempts = attempts + 1
		WHERE id = (
			SELECT id FROM imap_jobs
			WHERE status = 'pending' AND next_run_at <= now()
			ORDER BY enqueued_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, job_type, email_id, source_mailbox, target_mailbox,
		          imap_uidvalidity, imap_uid, raw_rfc822_path, attempts, max_attempts,
		          next_run_at, last_error, status, enqueued_at`
	var j IMAPJob
	err := q.db.QueryRow(ctx, query).Scan(
		&j.ID, &j.JobType, &j.EmailID, &j.SourceMailbox, &j.TargetMailbox,
		&j.IMAPUIDValidity, &j.IMAPUID, &j.RawRFC822Path, &j.Attempts, &j.MaxAttempts,
		&j.NextRunAt, &j.LastError, &j.Status, &j.EnqueuedAt,
	)
	return j, err
}

// CompleteIMAPJob marks an IMAP job as completed and updates the email's imap_mailbox.
func (q *Queries) CompleteIMAPJob(ctx context.Context, jobID, emailID uuid.UUID, newMailbox string) error {
	if _, err := q.db.Exec(ctx, `UPDATE imap_jobs SET status = 'completed', last_error = NULL WHERE id = $1`, jobID); err != nil {
		return err
	}
	_, err := q.db.Exec(ctx, `UPDATE emails SET imap_mailbox = $2, imap_uid = NULL WHERE id = $1`, emailID, newMailbox)
	return err
}

// FailIMAPJob reschedules a failed IMAP job with backoff or marks it failed if max_attempts reached.
func (q *Queries) FailIMAPJob(ctx context.Context, jobID uuid.UUID, attempts, maxAttempts int, errMsg string, nextRun time.Time) error {
	status := "pending"
	if attempts >= maxAttempts {
		status = "failed"
	}
	const query = `UPDATE imap_jobs SET status = $2, last_error = $3, next_run_at = $4 WHERE id = $1`
	_, err := q.db.Exec(ctx, query, jobID, status, errMsg, nextRun)
	return err
}

// EnqueueOutboxEmail queues an outbound email for SMTP delivery.
func (q *Queries) EnqueueOutboxEmail(ctx context.Context, emailID, senderUserID uuid.UUID) (OutboxEmail, error) {
	const query = `
		INSERT INTO outbox_emails (email_id, sender_user_id, status)
		VALUES ($1, $2, 'pending')
		RETURNING id, email_id, sender_user_id, status, attempts, next_run_at, last_error, enqueued_at`
	var o OutboxEmail
	err := q.db.QueryRow(ctx, query, emailID, senderUserID).Scan(
		&o.ID, &o.EmailID, &o.SenderUserID, &o.Status, &o.Attempts, &o.NextRunAt, &o.LastError, &o.EnqueuedAt,
	)
	return o, err
}

// ClaimNextOutboxEmail claims the next pending outbox email using FOR UPDATE SKIP LOCKED.
func (q *Queries) ClaimNextOutboxEmail(ctx context.Context) (OutboxEmail, error) {
	const query = `
		UPDATE outbox_emails
		SET status = 'sending', attempts = attempts + 1
		WHERE id = (
			SELECT id FROM outbox_emails
			WHERE status = 'pending' AND next_run_at <= now()
			ORDER BY enqueued_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, email_id, sender_user_id, status, attempts, next_run_at, last_error, enqueued_at`
	var o OutboxEmail
	err := q.db.QueryRow(ctx, query).Scan(
		&o.ID, &o.EmailID, &o.SenderUserID, &o.Status, &o.Attempts, &o.NextRunAt, &o.LastError, &o.EnqueuedAt,
	)
	return o, err
}

// CompleteOutboxEmail marks an outbox email as sent.
func (q *Queries) CompleteOutboxEmail(ctx context.Context, id uuid.UUID) error {
	_, err := q.db.Exec(ctx, `UPDATE outbox_emails SET status = 'sent', last_error = NULL WHERE id = $1`, id)
	return err
}

// FailOutboxEmail reschedules or fails an outbox email.
func (q *Queries) FailOutboxEmail(ctx context.Context, id uuid.UUID, attempts int, errMsg string, nextRun time.Time) error {
	status := "pending"
	if attempts >= 10 {
		status = "failed"
	}
	_, err := q.db.Exec(ctx, `UPDATE outbox_emails SET status = $2, last_error = $3, next_run_at = $4 WHERE id = $1`, id, status, errMsg, nextRun)
	return err
}

func scanEmail(row pgx.Row) (Email, error) {
	var e Email
	var toRaw, ccRaw, hdrRaw []byte
	err := row.Scan(
		&e.ID, &e.ThreadID, &e.ParentEmailID, &e.Direction, &e.MessageID, &e.InReplyTo, &e.ReferencesIDs,
		&e.IMAPMailbox, &e.IMAPUIDValidity, &e.IMAPUID, &e.FromAddress, &e.FromName,
		&toRaw, &ccRaw, &e.Subject, &e.SubjectNormalized,
		&e.BodyText, &e.BodyHTMLSanitized, &hdrRaw, &e.SentAt, &e.ReceivedAt,
	)
	if err != nil {
		return Email{}, err
	}
	if len(toRaw) > 0 {
		_ = json.Unmarshal(toRaw, &e.ToAddresses)
	}
	if len(ccRaw) > 0 {
		_ = json.Unmarshal(ccRaw, &e.CCAddresses)
	}
	if len(hdrRaw) > 0 {
		_ = json.Unmarshal(hdrRaw, &e.RawHeaders)
	}
	return e, nil
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}
