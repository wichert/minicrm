// Package sqlc provides type-safe PostgreSQL models, queries, and structured query tracing.
package sqlc

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ThreadStatus represents the workflow state of a thread.
type ThreadStatus string

// Valid workflow states for Thread.Status.
const (
	ThreadStatusNew        ThreadStatus = "new"
	ThreadStatusInProgress ThreadStatus = "in_progress"
	ThreadStatusCompleted  ThreadStatus = "completed"
	ThreadStatusSpam       ThreadStatus = "spam"
)

// User represents an operator or assignee in MiniCRM.
type User struct {
	ID              uuid.UUID `json:"id"`
	OIDCSub         *string   `json:"oidc_sub,omitempty"`
	Email           string    `json:"email"`
	DisplayName     string    `json:"display_name"`
	Role            string    `json:"role"`
	IsActive        bool      `json:"is_active"`
	AllowedAccounts []string  `json:"allowed_accounts,omitempty"`
}

// HasAccountAccess reports whether the user may access the given mail account.
func (u User) HasAccountAccess(accountID string) bool {
	if accountID == "" {
		return false
	}
	for _, id := range u.AllowedAccounts {
		if id == accountID {
			return true
		}
	}
	return false
}

// HasMultipleAccounts reports whether the user may access more than one mail account.
func (u User) HasMultipleAccounts() bool {
	return len(u.AllowedAccounts) > 1
}

// SystemSettings holds singleton CRM settings.
type SystemSettings struct {
	ID                     int    `json:"id"`
	DisplayTimezone        string `json:"display_timezone"`
	IMAPInboxMailbox       string `json:"imap_inbox_mailbox"`
	IMAPArchiveMailbox     string `json:"imap_archive_mailbox"`
	IMAPJunkMailbox        string `json:"imap_junk_mailbox"`
	IMAPSentMailbox        string `json:"imap_sent_mailbox"`
	SubjectMatchWindowDays int    `json:"subject_match_window_days"`
}

// AccountSettings holds per-account settings, such as the default assignee.
// A nil DefaultAssigneeID means the account currently has no default assignee and
// incoming threads for it stay unassigned until one is configured.
type AccountSettings struct {
	AccountID         string     `json:"account_id"`
	DefaultAssigneeID *uuid.UUID `json:"default_assignee_id,omitempty"`
}

// Thread represents a conversation thread of linked emails.
type Thread struct {
	ID                uuid.UUID    `json:"id"`
	AccountID         string       `json:"account_id"`
	SubjectNormalized string       `json:"subject_normalized"`
	SubjectDisplay    string       `json:"subject_display"`
	Status            ThreadStatus `json:"status"`
	AssigneeID        *uuid.UUID   `json:"assignee_id,omitempty"`
	AssignmentSource  string       `json:"assignment_source"`
	CustomerEmail     string       `json:"customer_email"`
	CustomerName      string       `json:"customer_name"`
	ReopensThreadID   *uuid.UUID   `json:"reopens_thread_id,omitempty"`
	LastMessageAt     time.Time    `json:"last_message_at"`
	// Summary fields populated by ListThreads for inbox triage.
	HasDraft        bool      `json:"has_draft,omitempty"`
	MessageCount    int       `json:"message_count,omitempty"`
	LatestBodyText  string    `json:"latest_body_text,omitempty"`
	LatestBodyHTML  string    `json:"latest_body_html,omitempty"`
	LatestDirection string    `json:"latest_direction,omitempty"`
	OpenedAt        time.Time `json:"opened_at,omitempty"`
}

// Address represents an RFC 5322 email participant.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// Email represents an inbound or outbound email message within a thread.
type Email struct {
	ID                uuid.UUID         `json:"id"`
	ThreadID          uuid.UUID         `json:"thread_id"`
	AccountID         string            `json:"account_id"`
	ParentEmailID     *uuid.UUID        `json:"parent_email_id,omitempty"`
	Direction         string            `json:"direction"` // "inbound" or "outbound"
	MessageID         string            `json:"message_id"`
	InReplyTo         *string           `json:"in_reply_to,omitempty"`
	ReferencesIDs     []string          `json:"references_ids"`
	IMAPMailbox       *string           `json:"imap_mailbox,omitempty"`
	IMAPUIDValidity   *int64            `json:"imap_uidvalidity,omitempty"`
	IMAPUID           *int64            `json:"imap_uid,omitempty"`
	FromAddress       string            `json:"from_address"`
	FromName          string            `json:"from_name"`
	ToAddresses       []Address         `json:"to_addresses"`
	CCAddresses       []Address         `json:"cc_addresses"`
	Subject           string            `json:"subject"`
	SubjectNormalized string            `json:"subject_normalized"`
	BodyText          string            `json:"body_text"`
	BodyHTMLSanitized string            `json:"body_html_sanitized"`
	RawHeaders        map[string]string `json:"raw_headers"`
	SentAt            time.Time         `json:"sent_at"`
	ReceivedAt        time.Time         `json:"received_at"`
}

// Attachment represents metadata for an email attachment stored on local disk.
type Attachment struct {
	ID             uuid.UUID `json:"id"`
	EmailID        uuid.UUID `json:"email_id"`
	Filename       string    `json:"filename"`
	ContentType    string    `json:"content_type"`
	SizeBytes      int64     `json:"size_bytes"`
	SHA256Hex      string    `json:"sha256_hex"`
	StorageRelPath string    `json:"storage_relpath"`
	ContentID      string    `json:"content_id,omitempty"`
	IsInline       bool      `json:"is_inline"`
}

// IMAPSyncState tracks the UID watermark for an IMAP mailbox.
type IMAPSyncState struct {
	AccountID     string `json:"account_id"`
	Mailbox       string `json:"mailbox"`
	UIDValidity   int64  `json:"uid_validity"`
	LastSyncedUID int64  `json:"last_synced_uid"`
}

// IMAPJob represents an asynchronous IMAP folder action.
type IMAPJob struct {
	ID              uuid.UUID `json:"id"`
	AccountID       string    `json:"account_id"`
	JobType         string    `json:"job_type"` // "move_to_archive", "move_to_junk", "append_to_sent"
	EmailID         uuid.UUID `json:"email_id"`
	SourceMailbox   *string   `json:"source_mailbox,omitempty"`
	TargetMailbox   string    `json:"target_mailbox"`
	IMAPUIDValidity *int64    `json:"imap_uidvalidity,omitempty"`
	IMAPUID         *int64    `json:"imap_uid,omitempty"`
	RawRFC822Path   *string   `json:"raw_rfc822_path,omitempty"`
	Attempts        int       `json:"attempts"`
	MaxAttempts     int       `json:"max_attempts"`
	NextRunAt       time.Time `json:"next_run_at"`
	LastError       *string   `json:"last_error,omitempty"`
	Status          string    `json:"status"`
	EnqueuedAt      time.Time `json:"enqueued_at"`
}

// OutboxEmail represents an outgoing email queued for SMTP delivery.
type OutboxEmail struct {
	ID           uuid.UUID `json:"id"`
	EmailID      uuid.UUID `json:"email_id"`
	SenderUserID uuid.UUID `json:"sender_user_id"`
	Status       string    `json:"status"`
	Attempts     int       `json:"attempts"`
	NextRunAt    time.Time `json:"next_run_at"`
	LastError    *string   `json:"last_error,omitempty"`
	EnqueuedAt   time.Time `json:"enqueued_at"`
}

// ThreadEvent records an immutable audit event on a thread.
type ThreadEvent struct {
	ID          uuid.UUID       `json:"id"`
	ThreadID    uuid.UUID       `json:"thread_id"`
	ActorUserID *uuid.UUID      `json:"actor_user_id,omitempty"`
	EventType   string          `json:"event_type"`
	OldValue    json.RawMessage `json:"old_value,omitempty"`
	NewValue    json.RawMessage `json:"new_value,omitempty"`
	OccurredAt  time.Time       `json:"occurred_at"`
}

// AuditLog records an immutable system/user/auth audit event.
type AuditLog struct {
	ID           uuid.UUID       `json:"id"`
	ActorUserID  *uuid.UUID      `json:"actor_user_id,omitempty"`
	TargetUserID *uuid.UUID      `json:"target_user_id,omitempty"`
	EventType    string          `json:"event_type"`
	IPAddress    string          `json:"ip_address,omitempty"`
	UserAgent    string          `json:"user_agent,omitempty"`
	OldValue     json.RawMessage `json:"old_value,omitempty"`
	NewValue     json.RawMessage `json:"new_value,omitempty"`
	OccurredAt   time.Time       `json:"occurred_at"`
}
