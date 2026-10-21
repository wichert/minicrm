package threading

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/assignment"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// Store defines the persistence methods required by Matcher.
type Store interface {
	FindEmailByMessageIDs(ctx context.Context, accountID string, msgIDs []string) (sqlc.Email, error)
	GetThreadByID(ctx context.Context, id uuid.UUID) (sqlc.Thread, error)
	FindThreadBySubjectAndParticipant(ctx context.Context, accountID, normSubject, customerEmail string, since time.Time) (sqlc.Thread, error)
	CreateThread(ctx context.Context, t sqlc.Thread) (sqlc.Thread, error)
	TouchThreadLastMessageAt(ctx context.Context, id uuid.UUID, ts time.Time) error
	GetSystemSettings(ctx context.Context) (sqlc.SystemSettings, error)
}

// InboundMessageMetadata holds the extracted fields needed to link an incoming email to a thread.
type InboundMessageMetadata struct {
	AccountID     string
	MessageID     string
	InReplyTo     string
	ReferencesIDs []string
	FromAddress   string
	FromName      string
	ToAddresses   []string
	Subject       string
	BodyText      string
	SentAt        time.Time
}

// ResolutionResult describes the thread and optional parent email resolved for an incoming message.
type ResolutionResult struct {
	Thread           sqlc.Thread
	ParentEmailID    *uuid.UUID
	CreatedNewThread bool
	ReopensThread    *sqlc.Thread
}

// Matcher resolves incoming emails into existing or newly created conversation threads.
type Matcher struct {
	store    Store
	assigner *assignment.Engine
	audit    *audit.Logger
}

// NewMatcher creates a thread Matcher.
func NewMatcher(store Store, assigner *assignment.Engine, auditLogger *audit.Logger) *Matcher {
	return &Matcher{
		store:    store,
		assigner: assigner,
		audit:    auditLogger,
	}
}

// ResolveThread links an incoming email to an existing thread or creates a new thread.
func (m *Matcher) ResolveThread(ctx context.Context, msg InboundMessageMetadata) (ResolutionResult, error) {
	normSubject, _ := NormalizeSubject(msg.Subject)
	displaySubject := msg.Subject
	if displaySubject == "" {
		displaySubject = "(No Subject)"
	}
	if normSubject == "" {
		normSubject = displaySubject
	}

	matchedThread, parentEmailID, err := m.findExisting(ctx, msg.AccountID, msg.InReplyTo, msg.ReferencesIDs, normSubject, msg.FromAddress, msg.SentAt)
	if err != nil {
		return ResolutionResult{}, err
	}

	// Step C: Thread Resolution & Completed-Thread Handling
	// Case 2: Matched thread is 'new' or 'in_progress' -> attach to existing thread
	if matchedThread != nil && (matchedThread.Status == sqlc.ThreadStatusNew || matchedThread.Status == sqlc.ThreadStatusInProgress) {
		if err := m.store.TouchThreadLastMessageAt(ctx, matchedThread.ID, msg.SentAt); err != nil {
			return ResolutionResult{}, fmt.Errorf("update thread last_message_at: %w", err)
		}
		if err := m.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
			ThreadID:  matchedThread.ID,
			EventType: "reply_received",
			NewValue:  map[string]string{"message_id": msg.MessageID, "from": msg.FromAddress},
		}); err != nil {
			return ResolutionResult{}, err
		}
		return ResolutionResult{
			Thread:           *matchedThread,
			ParentEmailID:    parentEmailID,
			CreatedNewThread: false,
		}, nil
	}

	// Case 1 (no match / spam) or Case 3 (matched thread is 'completed')
	var reopensThreadID *uuid.UUID
	var reopensThread *sqlc.Thread
	if matchedThread != nil && matchedThread.Status == sqlc.ThreadStatusCompleted {
		reopensThreadID = &matchedThread.ID
		reopensThread = matchedThread
	}

	newThread, err := m.createThread(ctx, createThreadParams{
		AccountID: msg.AccountID, NormSubject: normSubject, DisplaySubject: displaySubject,
		CustomerEmail: msg.FromAddress, CustomerName: msg.FromName,
		BodyText: msg.BodyText, ToAddresses: msg.ToAddresses, SentAt: msg.SentAt,
		ReopensThreadID: reopensThreadID,
	})
	if err != nil {
		return ResolutionResult{}, err
	}
	return ResolutionResult{
		Thread:           newThread,
		ParentEmailID:    parentEmailID,
		CreatedNewThread: true,
		ReopensThread:    reopensThread,
	}, nil
}

// findExisting looks for an existing thread an incoming message belongs to: first by
// In-Reply-To/References header match, then by normalized subject + customer address within
// the configured window. It returns the matched thread (or nil) and the parent email, if any.
func (m *Matcher) findExisting(ctx context.Context, accountID, inReplyTo string, references []string, normSubject, customerEmail string, sentAt time.Time) (*sqlc.Thread, *uuid.UUID, error) {
	var matchedThread *sqlc.Thread
	var parentEmailID *uuid.UUID

	// Step B.1: Header match via In-Reply-To and References
	candidateIDs := buildCandidateMessageIDs(inReplyTo, references)
	if len(candidateIDs) > 0 {
		matchedEmail, err := m.store.FindEmailByMessageIDs(ctx, accountID, candidateIDs)
		if err == nil {
			parentEmailID = &matchedEmail.ID
			t, tErr := m.store.GetThreadByID(ctx, matchedEmail.ThreadID)
			if tErr == nil {
				matchedThread = &t
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fmt.Errorf("lookup parent email by message-id: %w", err)
		}
	}

	// Step B.2: Subject + participant fallback if no header match was found
	if matchedThread == nil && normSubject != "" && normSubject != "(No Subject)" {
		windowDays := 30
		if settings, err := m.store.GetSystemSettings(ctx); err == nil && settings.SubjectMatchWindowDays > 0 {
			windowDays = settings.SubjectMatchWindowDays
		}
		refTime := sentAt
		if refTime.IsZero() {
			refTime = time.Now().UTC()
		}
		since := refTime.AddDate(0, 0, -windowDays)
		t, err := m.store.FindThreadBySubjectAndParticipant(ctx, accountID, normSubject, customerEmail, since)
		if err == nil {
			matchedThread = &t
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fmt.Errorf("fallback subject+participant lookup: %w", err)
		}
	}
	return matchedThread, parentEmailID, nil
}

// HasExistingThread reports whether an inbound message already belongs to a known thread,
// i.e. whether ResolveThread would attach it instead of creating a new thread.
func (m *Matcher) HasExistingThread(ctx context.Context, msg InboundMessageMetadata) (bool, error) {
	normSubject, _ := NormalizeSubject(msg.Subject)
	thread, _, err := m.findExisting(ctx, msg.AccountID, msg.InReplyTo, msg.ReferencesIDs, normSubject, msg.FromAddress, msg.SentAt)
	return thread != nil, err
}

// CandidateMessageIDs returns the normalized parent Message-IDs a message refers to, most recent first.
func CandidateMessageIDs(inReplyTo string, references []string) []string {
	return buildCandidateMessageIDs(inReplyTo, references)
}

// OutboundMessageMetadata holds the fields needed to link a message sent by the team (found in
// the Sent folder) to a thread.
type OutboundMessageMetadata struct {
	AccountID     string
	MessageID     string
	InReplyTo     string
	ReferencesIDs []string
	FromAddress   string
	Recipients    []sqlc.Address // To followed by Cc
	Subject       string
	BodyText      string
	SentAt        time.Time
	// OwnAddresses are addresses of the mail account itself; they are never treated as the customer.
	OwnAddresses []string
}

// OutboundResolution describes where an externally sent message belongs.
type OutboundResolution struct {
	Found         bool
	Thread        sqlc.Thread
	ParentEmailID *uuid.UUID
}

// ResolveOutboundThread links a message found in the Sent folder to an existing thread. It never
// creates a thread and never changes thread status; spam threads are ignored. When a thread is
// found it advances last_message_at (never beyond now) and records a reply_sent_external event.
func (m *Matcher) ResolveOutboundThread(ctx context.Context, msg OutboundMessageMetadata) (OutboundResolution, error) {
	normSubject, _ := NormalizeSubject(msg.Subject)
	var thread *sqlc.Thread
	var parent *uuid.UUID

	thread, parent, err := m.findExisting(ctx, msg.AccountID, msg.InReplyTo, msg.ReferencesIDs, "", "", msg.SentAt)
	if err != nil {
		return OutboundResolution{}, err
	}
	if thread == nil {
		// No headers matched: fall back to subject + the customer being one of the recipients.
		for _, r := range msg.Recipients {
			if isOwnAddress(r.Email, msg.FromAddress, msg.OwnAddresses) {
				continue
			}
			thread, parent, err = m.findExisting(ctx, msg.AccountID, "", nil, normSubject, r.Email, msg.SentAt)
			if err != nil {
				return OutboundResolution{}, err
			}
			if thread != nil {
				break
			}
		}
	}
	if thread == nil || thread.Status == sqlc.ThreadStatusSpam {
		return OutboundResolution{}, nil
	}

	sentAt := msg.SentAt
	if now := time.Now().UTC(); sentAt.IsZero() || sentAt.After(now) {
		sentAt = now
	}
	if err := m.store.TouchThreadLastMessageAt(ctx, thread.ID, sentAt); err != nil {
		return OutboundResolution{}, fmt.Errorf("update thread last_message_at: %w", err)
	}
	if err := m.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
		ThreadID:  thread.ID,
		EventType: "reply_sent_external",
		NewValue:  map[string]string{"message_id": msg.MessageID, "from": msg.FromAddress},
	}); err != nil {
		return OutboundResolution{}, err
	}
	return OutboundResolution{Found: true, Thread: *thread, ParentEmailID: parent}, nil
}

// CreateThreadFromOutbound creates a new thread for a message that was first sent from outside
// MiniCRM and has since received a customer reply. The customer is the first recipient that is not
// one of the account's own addresses; ok is false when there is none.
func (m *Matcher) CreateThreadFromOutbound(ctx context.Context, msg OutboundMessageMetadata) (thread sqlc.Thread, ok bool, err error) {
	var customer sqlc.Address
	for _, r := range msg.Recipients {
		if r.Email != "" && !isOwnAddress(r.Email, msg.FromAddress, msg.OwnAddresses) {
			customer = r
			break
		}
	}
	if customer.Email == "" {
		return sqlc.Thread{}, false, nil
	}
	normSubject, _ := NormalizeSubject(msg.Subject)
	displaySubject := msg.Subject
	if displaySubject == "" {
		displaySubject = "(No Subject)"
	}
	if normSubject == "" {
		normSubject = displaySubject
	}
	toList := make([]string, 0, len(msg.Recipients))
	for _, r := range msg.Recipients {
		toList = append(toList, r.Email)
	}
	// The customer's reply (which triggered this import) needs attention, so the thread starts as "new".
	created, err := m.createThread(ctx, createThreadParams{
		AccountID: msg.AccountID, NormSubject: normSubject, DisplaySubject: displaySubject,
		CustomerEmail: customer.Email, CustomerName: customer.Name,
		BodyText: msg.BodyText, ToAddresses: toList, SentAt: msg.SentAt, Origin: "outbound",
	})
	return created, err == nil, err
}

func isOwnAddress(addr, from string, own []string) bool {
	if strings.EqualFold(addr, from) {
		return true
	}
	for _, o := range own {
		if strings.EqualFold(addr, o) {
			return true
		}
	}
	return false
}

type createThreadParams struct {
	AccountID, NormSubject, DisplaySubject string
	CustomerEmail, CustomerName            string
	BodyText                               string
	ToAddresses                            []string
	SentAt                                 time.Time
	ReopensThreadID                        *uuid.UUID
	// Origin is recorded on the "created" event ("" for inbound customer mail).
	Origin string
}

// createThread creates a new "new" thread, resolving the default assignee and logging events.
func (m *Matcher) createThread(ctx context.Context, p createThreadParams) (sqlc.Thread, error) {
	cand, err := m.assigner.Resolve(ctx, assignment.EmailInput{
		AccountID:         p.AccountID,
		FromAddress:       p.CustomerEmail,
		FromName:          p.CustomerName,
		ToAddresses:       p.ToAddresses,
		Subject:           p.DisplaySubject,
		SubjectNormalized: p.NormSubject,
		BodyText:          p.BodyText,
		PreviousThreadID:  p.ReopensThreadID,
	})
	// No default assignee configured for this account: create the thread unassigned
	// so it can be picked up later, when a default assignee is configured or an
	// operator takes/assigns the thread.
	unassigned := errors.Is(err, assignment.ErrNoAssigneeAvailable)
	if err != nil && !unassigned {
		return sqlc.Thread{}, fmt.Errorf("resolve assignee for new thread: %w", err)
	}

	var assigneeID *uuid.UUID
	assignmentSource := "unassigned"
	if !unassigned {
		id := cand.AssigneeID
		assigneeID = &id
		assignmentSource = cand.Source
	}

	newThread, err := m.store.CreateThread(ctx, sqlc.Thread{
		AccountID:         p.AccountID,
		SubjectNormalized: p.NormSubject,
		SubjectDisplay:    p.DisplaySubject,
		Status:            sqlc.ThreadStatusNew,
		AssigneeID:        assigneeID,
		AssignmentSource:  assignmentSource,
		CustomerEmail:     p.CustomerEmail,
		CustomerName:      p.CustomerName,
		ReopensThreadID:   p.ReopensThreadID,
		LastMessageAt:     p.SentAt,
	})
	if err != nil {
		return sqlc.Thread{}, fmt.Errorf("create thread: %w", err)
	}

	created := map[string]any{
		"account_id":        newThread.AccountID,
		"status":            newThread.Status,
		"reopens_thread_id": p.ReopensThreadID,
	}
	if p.Origin != "" {
		created["origin"] = p.Origin
	}
	if err := m.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
		ThreadID:  newThread.ID,
		EventType: "created",
		NewValue:  created,
	}); err != nil {
		return sqlc.Thread{}, err
	}
	if assigneeID != nil {
		if err := m.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
			ThreadID:  newThread.ID,
			EventType: "assigned",
			NewValue: map[string]string{
				"assignee_id": assigneeID.String(),
				"source":      assignmentSource,
			},
		}); err != nil {
			return sqlc.Thread{}, err
		}
	}
	return newThread, nil
}

func buildCandidateMessageIDs(inReplyTo string, references []string) []string {
	seen := make(map[string]struct{})
	var out []string
	if id := NormalizeMessageID(inReplyTo); id != "" {
		seen[id] = struct{}{}
		out = append(out, id)
	}
	// Walk References from most recent (end) to oldest (start).
	for i := len(references) - 1; i >= 0; i-- {
		id := NormalizeMessageID(references[i])
		if id == "" {
			continue
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}
