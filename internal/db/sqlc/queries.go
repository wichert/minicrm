package sqlc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// TxBeginner starts a database transaction.
type TxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// Begin starts a transaction if the underlying database supports it.
func (q *Queries) Begin(ctx context.Context) (pgx.Tx, error) {
	beginner, ok := q.db.(TxBeginner)
	if !ok {
		return nil, fmt.Errorf("database does not support transactions")
	}
	return beginner.Begin(ctx)
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

const userColumns = `id, oidc_sub, email, display_name, role, is_active, allowed_accounts`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.OIDCSub, &u.Email, &u.DisplayName, &u.Role, &u.IsActive, &u.AllowedAccounts)
	return u, err
}

func scanUsers(rows pgx.Rows) ([]User, error) {
	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// GetUserByID fetches a user by UUID.
func (q *Queries) GetUserByID(ctx context.Context, id uuid.UUID) (User, error) {
	return scanUser(q.db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

// GetUserByOIDCSub fetches a user by OIDC subject identifier.
func (q *Queries) GetUserByOIDCSub(ctx context.Context, sub string) (User, error) {
	return scanUser(q.db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE oidc_sub = $1`, sub))
}

// GetUserByEmail fetches a user by email (case-insensitive via CITEXT).
func (q *Queries) GetUserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(q.db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, email))
}

// ListUsers returns all users ordered by display_name.
func (q *Queries) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := q.db.Query(ctx, `SELECT `+userColumns+` FROM users ORDER BY display_name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsers(rows)
}

// CreateUserParams holds parameters for creating a user.
type CreateUserParams struct {
	OIDCSub         *string
	Email           string
	DisplayName     string
	Role            string
	IsActive        bool
	AllowedAccounts []string
}

// CreateUser inserts a new user row.
func (q *Queries) CreateUser(ctx context.Context, arg CreateUserParams) (User, error) {
	allowed := arg.AllowedAccounts
	if allowed == nil {
		allowed = []string{}
	}
	const query = `
		INSERT INTO users (oidc_sub, email, display_name, role, is_active, allowed_accounts)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING ` + userColumns
	return scanUser(q.db.QueryRow(ctx, query,
		arg.OIDCSub, arg.Email, arg.DisplayName, arg.Role, arg.IsActive, allowed))
}

// UpdateUserOIDC binds an OIDC sub, updated display_name, and resolved account access to an existing user.
func (q *Queries) UpdateUserOIDC(ctx context.Context, id uuid.UUID, oidcSub, displayName string, allowedAccounts []string) (User, error) {
	allowed := allowedAccounts
	if allowed == nil {
		allowed = []string{}
	}
	const query = `
		UPDATE users
		SET oidc_sub = $2, display_name = $3, allowed_accounts = $4
		WHERE id = $1
		RETURNING ` + userColumns
	return scanUser(q.db.QueryRow(ctx, query, id, oidcSub, displayName, allowed))
}

// GetSystemSettings returns the singleton system_settings row.
func (q *Queries) GetSystemSettings(ctx context.Context) (SystemSettings, error) {
	const query = `
		SELECT id, display_timezone, imap_inbox_mailbox,
		       imap_archive_mailbox, imap_junk_mailbox, imap_sent_mailbox,
		       subject_match_window_days, attention_age_days
		FROM system_settings WHERE id = 1`
	var s SystemSettings
	err := q.db.QueryRow(ctx, query).Scan(
		&s.ID, &s.DisplayTimezone,
		&s.IMAPInboxMailbox, &s.IMAPArchiveMailbox, &s.IMAPJunkMailbox, &s.IMAPSentMailbox,
		&s.SubjectMatchWindowDays, &s.AttentionAgeDays,
	)
	return s, err
}

// UpsertSystemSettings inserts or updates the singleton system_settings row.
func (q *Queries) UpsertSystemSettings(ctx context.Context, s SystemSettings) (SystemSettings, error) {
	// No clamping here: the settings handler validates 1..365 and the DB CHECK
	// constraint is the final guard against out-of-range values.
	const query = `
		INSERT INTO system_settings (
			id, display_timezone, imap_inbox_mailbox,
			imap_archive_mailbox, imap_junk_mailbox, imap_sent_mailbox,
			subject_match_window_days, attention_age_days
		) VALUES (1, $1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			display_timezone = EXCLUDED.display_timezone,
			imap_inbox_mailbox = EXCLUDED.imap_inbox_mailbox,
			imap_archive_mailbox = EXCLUDED.imap_archive_mailbox,
			imap_junk_mailbox = EXCLUDED.imap_junk_mailbox,
			imap_sent_mailbox = EXCLUDED.imap_sent_mailbox,
			subject_match_window_days = EXCLUDED.subject_match_window_days,
			attention_age_days = EXCLUDED.attention_age_days
		RETURNING id, display_timezone, imap_inbox_mailbox,
		          imap_archive_mailbox, imap_junk_mailbox, imap_sent_mailbox,
		          subject_match_window_days, attention_age_days`
	var out SystemSettings
	err := q.db.QueryRow(ctx, query,
		s.DisplayTimezone, s.IMAPInboxMailbox,
		s.IMAPArchiveMailbox, s.IMAPJunkMailbox, s.IMAPSentMailbox,
		s.SubjectMatchWindowDays, s.AttentionAgeDays,
	).Scan(
		&out.ID, &out.DisplayTimezone,
		&out.IMAPInboxMailbox, &out.IMAPArchiveMailbox, &out.IMAPJunkMailbox, &out.IMAPSentMailbox,
		&out.SubjectMatchWindowDays, &out.AttentionAgeDays,
	)
	return out, err
}

// GetAccountSettings fetches the settings row for a mail account.
// It returns pgx.ErrNoRows when the account has no settings row yet.
func (q *Queries) GetAccountSettings(ctx context.Context, accountID string) (AccountSettings, error) {
	const query = `SELECT account_id, default_assignee_id FROM account_settings WHERE account_id = $1`
	var s AccountSettings
	err := q.db.QueryRow(ctx, query, accountID).Scan(&s.AccountID, &s.DefaultAssigneeID)
	return s, err
}

// ListAccountSettings returns all account settings rows.
func (q *Queries) ListAccountSettings(ctx context.Context) ([]AccountSettings, error) {
	rows, err := q.db.Query(ctx, `SELECT account_id, default_assignee_id FROM account_settings ORDER BY account_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountSettings
	for rows.Next() {
		var s AccountSettings
		if err := rows.Scan(&s.AccountID, &s.DefaultAssigneeID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpsertAccountSettings inserts or updates the settings row for a mail account.
func (q *Queries) UpsertAccountSettings(ctx context.Context, s AccountSettings) (AccountSettings, error) {
	const query = `
		INSERT INTO account_settings (account_id, default_assignee_id)
		VALUES ($1, $2)
		ON CONFLICT (account_id) DO UPDATE SET default_assignee_id = EXCLUDED.default_assignee_id
		RETURNING account_id, default_assignee_id`
	var out AccountSettings
	err := q.db.QueryRow(ctx, query, s.AccountID, s.DefaultAssigneeID).
		Scan(&out.AccountID, &out.DefaultAssigneeID)
	return out, err
}

// AssignUnassignedThreadsByAccount assigns all active unassigned threads of an account
// to the given user and returns the affected thread IDs.
func (q *Queries) AssignUnassignedThreadsByAccount(ctx context.Context, accountID string, assigneeID uuid.UUID, source string) ([]uuid.UUID, error) {
	const query = `
		UPDATE threads
		   SET assignee_id = $2, assignment_source = $3
		 WHERE account_id = $1
		   AND assignee_id IS NULL
		   AND status IN ('new', 'in_progress')
		RETURNING id`
	rows, err := q.db.Query(ctx, query, accountID, assigneeID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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

// ListAuditLogsPage returns one page of audit log entries, newest first.
// offset is the number of newest entries to skip; limit caps the page size.
func (q *Queries) ListAuditLogsPage(ctx context.Context, limit, offset int) ([]AuditLog, error) {
	const query = `
		SELECT id, actor_user_id, target_user_id, event_type, COALESCE(ip_address, ''), COALESCE(user_agent, ''),
		       COALESCE(old_value, 'null'::jsonb), COALESCE(new_value, 'null'::jsonb), occurred_at
		FROM audit_logs
		ORDER BY occurred_at DESC, id DESC
		LIMIT $1 OFFSET $2`
	rows, err := q.db.Query(ctx, query, limit, offset)
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
		ORDER BY occurred_at ASC, event_order ASC`
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

const threadColumns = `id, account_id, subject_normalized, subject_display, status, assignee_id,
	       assignment_source, customer_email, COALESCE(customer_name, ''), reopens_thread_id, last_message_at`

// scanThread scans the shared threadColumns projection.
func scanThread(row pgx.Row) (Thread, error) {
	var t Thread
	err := row.Scan(
		&t.ID, &t.AccountID, &t.SubjectNormalized, &t.SubjectDisplay, &t.Status, &t.AssigneeID,
		&t.AssignmentSource, &t.CustomerEmail, &t.CustomerName, &t.ReopensThreadID, &t.LastMessageAt,
	)
	return t, err
}

// scanThreadWithOpenedAt scans threadColumns plus a trailing opened_at column. The
// column list is shared with scanThread, so any addition to threadColumns must be
// mirrored in both scanners — the trailing opened_at is scanned in addition to it.
func scanThreadWithOpenedAt(row pgx.Row) (Thread, error) {
	var t Thread
	err := row.Scan(
		&t.ID, &t.AccountID, &t.SubjectNormalized, &t.SubjectDisplay, &t.Status, &t.AssigneeID,
		&t.AssignmentSource, &t.CustomerEmail, &t.CustomerName, &t.ReopensThreadID, &t.LastMessageAt,
		&t.OpenedAt,
	)
	return t, err
}

// CreateThread inserts a new conversation thread.
func (q *Queries) CreateThread(ctx context.Context, t Thread) (Thread, error) {
	if t.LastMessageAt.IsZero() {
		t.LastMessageAt = time.Now().UTC()
	}
	if t.AssignmentSource == "" {
		if t.AssigneeID == nil {
			t.AssignmentSource = "unassigned"
		} else {
			t.AssignmentSource = "default"
		}
	}
	const query = `
		INSERT INTO threads (
			account_id, subject_normalized, subject_display, status, assignee_id,
			assignment_source, customer_email, customer_name, reopens_thread_id, last_message_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING ` + threadColumns
	return scanThread(q.db.QueryRow(ctx, query,
		t.AccountID, t.SubjectNormalized, t.SubjectDisplay, t.Status, t.AssigneeID,
		t.AssignmentSource, t.CustomerEmail, t.CustomerName, t.ReopensThreadID, t.LastMessageAt,
	))
}

// GetThreadByID fetches a thread by UUID. Unlike the shared threadColumns select, it also
// returns opened_at (earliest email sent_at) so the detail page can show the thread-age badge
// without relying on the first email's SentAt.
func (q *Queries) GetThreadByID(ctx context.Context, id uuid.UUID) (Thread, error) {
	return scanThreadWithOpenedAt(q.db.QueryRow(ctx, `SELECT `+threadColumns+`,
	       COALESCE((SELECT MIN(e.sent_at) FROM emails e WHERE e.thread_id = threads.id), threads.last_message_at) AS opened_at
	       FROM threads WHERE id = $1`, id))
}

// LockThreadByID fetches and locks a thread for a transactional workflow operation.
func (q *Queries) LockThreadByID(ctx context.Context, id uuid.UUID) (Thread, error) {
	return scanThread(q.db.QueryRow(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = $1 FOR UPDATE`, id))
}

// ThreadCounts holds live per-tab counts for the inbox navigation strip.
type ThreadCounts struct {
	NeedsAttention int
	New            int
	Mine           int
	InProgress     int
	Completed      int
	Spam           int
	All            int
}

// CountThreadsFilter specifies account and threshold parameters for CountThreadsByTab.
type CountThreadsFilter struct {
	AllowedAccounts []string
	AccountID       *string
	UserID          uuid.UUID
	AttentionCutoff time.Time
}

// ListThreadsFilter specifies optional filters for listing threads.
type ListThreadsFilter struct {
	Status          *ThreadStatus
	OpenOnly        bool
	NeedsAttention  bool
	AttentionCutoff time.Time
	AssigneeID      *uuid.UUID
	Unassigned      bool
	AccountID       *string
	AllowedAccounts []string
	Search          string
	CustomerEmail   string
	ExcludeThreadID *uuid.UUID
	// Sort is the allow-listed ordering: "newest" (default) orders by
	// last_message_at DESC; "oldest_open" orders by opened_at ASC. Only these two
	// constants are accepted — callers must never interpolate raw strings.
	Sort       string
	BeforeTime *time.Time
	BeforeID   *uuid.UUID
	Limit      int
}

// CountThreadsByTab computes all inbox tab counts in a single aggregate query scoped to
// the user's allowed accounts and optional account filter.
func (q *Queries) CountThreadsByTab(ctx context.Context, f CountThreadsFilter) (ThreadCounts, error) {
	if len(f.AllowedAccounts) == 0 {
		return ThreadCounts{}, nil
	}
	const query = `
		SELECT
			-- Inclusive <= cutoff: a thread opened exactly at the cutoff counts as
			-- needing attention (matches the needs_attention predicate below).
			COUNT(*) FILTER (WHERE t.status IN ('new', 'in_progress') AND COALESCE(stats.opened_at, t.last_message_at) <= $3::timestamptz)::int AS needs_attention,
			COUNT(*) FILTER (WHERE t.status = 'new')::int AS new_count,
			COUNT(*) FILTER (WHERE t.status IN ('new', 'in_progress') AND t.assignee_id = $4::uuid)::int AS mine_count,
			COUNT(*) FILTER (WHERE t.status = 'in_progress')::int AS in_progress_count,
			COUNT(*) FILTER (WHERE t.status = 'completed')::int AS completed_count,
			COUNT(*) FILTER (WHERE t.status = 'spam')::int AS spam_count,
			COUNT(*)::int AS all_count
		FROM threads t
		LEFT JOIN LATERAL (
			SELECT MIN(e.sent_at) AS opened_at
			FROM emails e
			WHERE e.thread_id = t.id
		) stats ON true
		WHERE ($1::text IS NULL OR t.account_id = $1)
		  AND ($2::text[] IS NULL OR t.account_id = ANY($2))`
	var out ThreadCounts
	err := q.db.QueryRow(ctx, query, f.AccountID, f.AllowedAccounts, f.AttentionCutoff, f.UserID).Scan(
		&out.NeedsAttention, &out.New, &out.Mine, &out.InProgress, &out.Completed, &out.Spam, &out.All,
	)
	return out, err
}

// ListThreads returns threads matching the given filter, ordered by the
// allow-listed Sort field ("newest" = last_message_at DESC, "oldest_open" =
// opened_at ASC), enriched with latest-message preview metadata and message count.
func (q *Queries) ListThreads(ctx context.Context, f ListThreadsFilter) ([]Thread, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	if (f.BeforeTime == nil) != (f.BeforeID == nil) {
		return nil, fmt.Errorf("inbox cursor must include both time and ID")
	}
	// Two fully static query variants instead of one ORDER BY CASE expression:
	// the parameterized CASE defeats index scans for the default "newest" sort,
	// while a literal ORDER BY t.last_message_at DESC, t.id DESC stays
	// index-orderable. Sort values never reach the SQL text; the branch is
	// selected in Go from the allow-listed f.Sort field only.
	const oldestOpenOrder = `
		ORDER BY
			ROW(COALESCE(stats.opened_at, t.last_message_at), t.id)`
	const newestOrder = `
		ORDER BY t.last_message_at DESC, t.id DESC`
	orderClause := newestOrder
	if f.Sort == "oldest_open" {
		orderClause = oldestOpenOrder
	}
	query := `
		SELECT t.id, t.account_id, t.subject_normalized, t.subject_display, t.status, t.assignee_id,
		       t.assignment_source, t.customer_email, COALESCE(t.customer_name, ''), t.reopens_thread_id, t.last_message_at,
		       COALESCE(stats.message_count, 0),
		       COALESCE(latest.body_text, ''),
		       COALESCE(latest.body_html_sanitized, ''),
		       COALESCE(latest.direction, ''),
		       COALESCE(stats.opened_at, t.last_message_at),
		       COALESCE((SELECT NOT d.discarded AND d.body <> '' FROM reply_drafts d WHERE d.thread_id=t.id ORDER BY d.id DESC LIMIT 1), false)
		FROM threads t
		LEFT JOIN LATERAL (
			SELECT COUNT(*)::int AS message_count,
			       MIN(e.sent_at) AS opened_at
			FROM emails e
			WHERE e.thread_id = t.id
		) stats ON true
		LEFT JOIN LATERAL (
			SELECT e.body_text, e.body_html_sanitized, e.direction
			FROM emails e
			WHERE e.thread_id = t.id
			ORDER BY e.sent_at DESC, e.received_at DESC
			LIMIT 1
		) latest ON true
		WHERE ($1::thread_status IS NULL OR t.status = $1)
		  AND ($2::uuid IS NULL OR t.assignee_id = $2)
		  AND (NOT $3::boolean OR t.assignee_id IS NULL)
		  AND ($4::text IS NULL OR t.account_id = $4)
		  AND ($5::text[] IS NULL OR t.account_id = ANY($5))
		  AND (NOT $6::boolean OR t.status IN ('new', 'in_progress'))
		  AND ($7::text = '' OR strpos(lower(t.subject_display), lower($7)) > 0
		       OR strpos(lower(t.customer_email::text), lower($7)) > 0
		       OR strpos(lower(COALESCE(t.customer_name, '')), lower($7)) > 0)
		  AND ($8::timestamptz IS NULL OR (t.last_message_at, t.id) < ($8, $9::uuid))
		  AND ($10::text = '' OR t.customer_email = $10::citext)
		  AND ($11::uuid IS NULL OR t.id <> $11)
		  -- Inclusive <= attention cutoff, matching the count query above.
		  AND (NOT $13::boolean OR (t.status IN ('new', 'in_progress') AND COALESCE(stats.opened_at, t.last_message_at) <= $14::timestamptz))
		` + orderClause + `
		LIMIT $12`
	// f.Sort is allow-listed upstream; it only selects between two static query
	// variants — no user input reaches the SQL text.
	rows, err := q.db.Query(ctx, query, f.Status, f.AssigneeID, f.Unassigned, f.AccountID, f.AllowedAccounts, f.OpenOnly, f.Search, f.BeforeTime, f.BeforeID, f.CustomerEmail, f.ExcludeThreadID, f.Limit, f.NeedsAttention, f.AttentionCutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Thread
	for rows.Next() {
		var t Thread
		if err := rows.Scan(
			&t.ID, &t.AccountID, &t.SubjectNormalized, &t.SubjectDisplay, &t.Status, &t.AssigneeID,
			&t.AssignmentSource, &t.CustomerEmail, &t.CustomerName, &t.ReopensThreadID, &t.LastMessageAt,
			&t.MessageCount, &t.LatestBodyText, &t.LatestBodyHTML, &t.LatestDirection, &t.OpenedAt, &t.HasDraft,
		); err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

// ListCustomerHistory returns only the metadata displayed in the customer history panel.
func (q *Queries) ListCustomerHistory(ctx context.Context, accounts []string, email string, exclude uuid.UUID, limit int) ([]Thread, error) {
	if len(accounts) == 0 || strings.TrimSpace(email) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.db.Query(ctx, `SELECT id, account_id, subject_display, status, customer_email, last_message_at
		FROM threads WHERE account_id = ANY($1::text[]) AND customer_email = $2::citext AND id <> $3
		ORDER BY last_message_at DESC, id DESC LIMIT $4`, accounts, email, exclude, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Thread
	for rows.Next() {
		var thread Thread
		if err := rows.Scan(&thread.ID, &thread.AccountID, &thread.SubjectDisplay, &thread.Status, &thread.CustomerEmail, &thread.LastMessageAt); err != nil {
			return nil, err
		}
		result = append(result, thread)
	}
	return result, rows.Err()
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
			thread_id, account_id, parent_email_id, direction, message_id, in_reply_to, references_ids,
			imap_mailbox, imap_uidvalidity, imap_uid, from_address, from_name,
			to_addresses, cc_addresses, subject, subject_normalized,
			body_text, body_html_sanitized, raw_headers, sent_at, received_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12,
			$13, $14, $15, $16,
			$17, $18, $19, $20, $21
		) RETURNING id`
	err = q.db.QueryRow(ctx, query,
		e.ThreadID, e.AccountID, e.ParentEmailID, e.Direction, e.MessageID, e.InReplyTo, e.ReferencesIDs,
		e.IMAPMailbox, e.IMAPUIDValidity, e.IMAPUID, e.FromAddress, e.FromName,
		toJSON, ccJSON, e.Subject, e.SubjectNormalized,
		e.BodyText, e.BodyHTMLSanitized, hdrJSON, e.SentAt, e.ReceivedAt,
	).Scan(&e.ID)
	return e, err
}

const emailColumns = `id, thread_id, account_id, parent_email_id, direction, message_id, in_reply_to, references_ids,
	       imap_mailbox, imap_uidvalidity, imap_uid, from_address, COALESCE(from_name, ''),
	       to_addresses, cc_addresses, subject, subject_normalized,
	       COALESCE(body_text, ''), COALESCE(body_html_sanitized, ''), raw_headers, sent_at, received_at`

// GetEmailByID fetches an email by UUID.
func (q *Queries) GetEmailByID(ctx context.Context, id uuid.UUID) (Email, error) {
	return scanEmail(q.db.QueryRow(ctx, `SELECT `+emailColumns+` FROM emails WHERE id = $1`, id))
}

// GetEmailByMessageID fetches an email by its normalized RFC 5322 Message-ID within an account.
func (q *Queries) GetEmailByMessageID(ctx context.Context, accountID, msgID string) (Email, error) {
	const query = `SELECT ` + emailColumns + ` FROM emails WHERE account_id = $1 AND message_id = $2`
	return scanEmail(q.db.QueryRow(ctx, query, accountID, msgID))
}

// FindEmailByMessageIDs returns the most recent email of the given account matching any of the Message-IDs.
func (q *Queries) FindEmailByMessageIDs(ctx context.Context, accountID string, msgIDs []string) (Email, error) {
	const query = `
		SELECT ` + emailColumns + `
		FROM emails
		WHERE account_id = $1
		  AND message_id = ANY($2)
		ORDER BY sent_at DESC
		LIMIT 1`
	return scanEmail(q.db.QueryRow(ctx, query, accountID, msgIDs))
}

// FindThreadBySubjectAndParticipant finds the most recent non-spam thread of an account
// matching normalized subject and customer email within a time window.
func (q *Queries) FindThreadBySubjectAndParticipant(ctx context.Context, accountID, normSubject, customerEmail string, since time.Time) (Thread, error) {
	const query = `
		SELECT ` + threadColumns + `
		FROM threads
		WHERE account_id = $1
		  AND lower(subject_normalized) = lower($2)
		  AND subject_normalized <> ''
		  AND customer_email = $3
		  AND last_message_at >= $4
		  AND status <> 'spam'
		ORDER BY last_message_at DESC
		LIMIT 1`
	return scanThread(q.db.QueryRow(ctx, query, accountID, normSubject, customerEmail, since))
}

// SetEmailIMAPLocation records where an email lives on the IMAP server. It only fills in a missing
// location (imap_uid IS NULL) so existing locations are never overwritten; it reports whether a row changed.
func (q *Queries) SetEmailIMAPLocation(ctx context.Context, id uuid.UUID, mailbox string, uidValidity, uid int64) (bool, error) {
	const query = `
		UPDATE emails
		   SET imap_mailbox = $2, imap_uidvalidity = $3, imap_uid = $4
		 WHERE id = $1 AND imap_uid IS NULL`
	tag, err := q.db.Exec(ctx, query, id, mailbox, uidValidity, uid)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ListEmailsByThread returns all emails in a thread ordered by sent_at ASC.
func (q *Queries) ListEmailsByThread(ctx context.Context, threadID uuid.UUID) ([]Email, error) {
	const query = `
		SELECT ` + emailColumns + `
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

// GetIMAPSyncState fetches the UID watermark for an account mailbox.
func (q *Queries) GetIMAPSyncState(ctx context.Context, accountID, mailbox string) (IMAPSyncState, error) {
	const query = `SELECT account_id, mailbox, uid_validity, last_synced_uid FROM imap_sync_state WHERE account_id = $1 AND mailbox = $2`
	var s IMAPSyncState
	err := q.db.QueryRow(ctx, query, accountID, mailbox).Scan(&s.AccountID, &s.Mailbox, &s.UIDValidity, &s.LastSyncedUID)
	return s, err
}

// UpsertIMAPSyncState updates the UID watermark for an account mailbox.
func (q *Queries) UpsertIMAPSyncState(ctx context.Context, s IMAPSyncState) error {
	const query = `
		INSERT INTO imap_sync_state (account_id, mailbox, uid_validity, last_synced_uid)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (account_id, mailbox) DO UPDATE SET
			uid_validity = EXCLUDED.uid_validity,
			last_synced_uid = EXCLUDED.last_synced_uid`
	_, err := q.db.Exec(ctx, query, s.AccountID, s.Mailbox, s.UIDValidity, s.LastSyncedUID)
	return err
}

// EnqueueIMAPJob inserts a pending job into imap_jobs.
func (q *Queries) EnqueueIMAPJob(ctx context.Context, job IMAPJob) (IMAPJob, error) {
	if job.MaxAttempts <= 0 {
		job.MaxAttempts = 10
	}
	const query = `
		INSERT INTO imap_jobs (
			account_id, job_type, email_id, source_mailbox, target_mailbox,
			imap_uidvalidity, imap_uid, raw_rfc822_path, max_attempts, status, enqueued_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', clock_timestamp())
		RETURNING id, enqueued_at`
	err := q.db.QueryRow(ctx, query,
		job.AccountID, job.JobType, job.EmailID, job.SourceMailbox, job.TargetMailbox,
		job.IMAPUIDValidity, job.IMAPUID, job.RawRFC822Path, job.MaxAttempts,
	).Scan(&job.ID, &job.EnqueuedAt)
	job.Status = "pending"
	return job, err
}

// BeginIMAPJob holds the claim row lock until the action has finished. A dead
// worker's transaction is rolled back by PostgreSQL, releasing the claim.
func (q *Queries) BeginIMAPJob(ctx context.Context) (IMAPJob, *Queries, pgx.Tx, error) {
	tx, err := q.Begin(ctx)
	if err != nil {
		return IMAPJob{}, nil, nil, err
	}
	bound := q.WithTx(tx)
	job, err := bound.ClaimNextIMAPJob(ctx)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return IMAPJob{}, nil, nil, err
	}
	return job, bound, tx, nil
}

// ClaimNextIMAPJob claims the next runnable job using FOR UPDATE SKIP LOCKED.
// Call inside a transaction held for the entire external action.
func (q *Queries) ClaimNextIMAPJob(ctx context.Context) (IMAPJob, error) {
	const query = `
		UPDATE imap_jobs
		SET status = 'processing', attempts = attempts + 1
		WHERE id = (
			SELECT j.id FROM imap_jobs j
			WHERE (j.status = 'processing' OR (j.status = 'pending' AND j.next_run_at <= now()))
			  AND NOT EXISTS (
			    SELECT 1 FROM imap_jobs earlier
			    WHERE earlier.email_id = j.email_id AND earlier.account_id = j.account_id
			      AND earlier.status IN ('pending', 'processing')
			      AND (earlier.enqueued_at, earlier.id) < (j.enqueued_at, j.id)
			  )
			ORDER BY j.enqueued_at ASC, j.id ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, account_id, job_type, email_id, source_mailbox, target_mailbox,
		          imap_uidvalidity, imap_uid, raw_rfc822_path, attempts, max_attempts,
		          next_run_at, last_error, status, enqueued_at`
	var j IMAPJob
	err := q.db.QueryRow(ctx, query).Scan(
		&j.ID, &j.AccountID, &j.JobType, &j.EmailID, &j.SourceMailbox, &j.TargetMailbox,
		&j.IMAPUIDValidity, &j.IMAPUID, &j.RawRFC822Path, &j.Attempts, &j.MaxAttempts,
		&j.NextRunAt, &j.LastError, &j.Status, &j.EnqueuedAt,
	)
	return j, err
}

// RestoreIMAPEmail returns account-scoped message metadata for ordered folder moves.
func (q *Queries) RestoreIMAPEmail(ctx context.Context, accountID string, emailID uuid.UUID) (Email, error) {
	var e Email
	err := q.db.QueryRow(ctx, `SELECT message_id, imap_mailbox, imap_uidvalidity, imap_uid FROM emails WHERE id=$1 AND account_id=$2`, emailID, accountID).Scan(&e.MessageID, &e.IMAPMailbox, &e.IMAPUIDValidity, &e.IMAPUID)
	return e, err
}

// CompleteIMAPJob marks an IMAP job as completed and updates the email's imap_mailbox.
func (q *Queries) CompleteIMAPJob(ctx context.Context, jobID, emailID uuid.UUID, newMailbox string) error {
	_, err := q.db.Exec(ctx, `WITH finished AS (
		UPDATE imap_jobs SET status = 'completed', last_error = NULL WHERE id = $1 AND email_id = $2 RETURNING email_id
	) UPDATE emails SET imap_mailbox = $3, imap_uid = NULL WHERE id IN (SELECT email_id FROM finished)`, jobID, emailID, newMailbox)
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
		&e.ID, &e.ThreadID, &e.AccountID, &e.ParentEmailID, &e.Direction, &e.MessageID, &e.InReplyTo, &e.ReferencesIDs,
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
