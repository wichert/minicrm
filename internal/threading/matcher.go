package threading

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/assignment"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// Store defines the persistence methods required by Matcher.
type Store interface {
	FindEmailByMessageIDs(ctx context.Context, msgIDs []string) (sqlc.Email, error)
	GetThreadByID(ctx context.Context, id uuid.UUID) (sqlc.Thread, error)
	FindThreadBySubjectAndParticipant(ctx context.Context, normSubject, customerEmail string, since time.Time) (sqlc.Thread, error)
	CreateThread(ctx context.Context, t sqlc.Thread) (sqlc.Thread, error)
	TouchThreadLastMessageAt(ctx context.Context, id uuid.UUID, ts time.Time) error
	GetSystemSettings(ctx context.Context) (sqlc.SystemSettings, error)
}

// InboundMessageMetadata holds the extracted fields needed to link an incoming email to a thread.
type InboundMessageMetadata struct {
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

	var matchedThread *sqlc.Thread
	var parentEmailID *uuid.UUID

	// Step B.1: Header match via In-Reply-To and References
	candidateIDs := buildCandidateMessageIDs(msg.InReplyTo, msg.ReferencesIDs)
	if len(candidateIDs) > 0 {
		matchedEmail, err := m.store.FindEmailByMessageIDs(ctx, candidateIDs)
		if err == nil {
			parentEmailID = &matchedEmail.ID
			t, tErr := m.store.GetThreadByID(ctx, matchedEmail.ThreadID)
			if tErr == nil {
				matchedThread = &t
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return ResolutionResult{}, fmt.Errorf("lookup parent email by message-id: %w", err)
		}
	}

	// Step B.2: Subject + Sender fallback if no header match was found
	if matchedThread == nil && normSubject != "" && normSubject != "(No Subject)" {
		windowDays := 30
		if settings, err := m.store.GetSystemSettings(ctx); err == nil && settings.SubjectMatchWindowDays > 0 {
			windowDays = settings.SubjectMatchWindowDays
		}
		refTime := msg.SentAt
		if refTime.IsZero() {
			refTime = time.Now().UTC()
		}
		since := refTime.AddDate(0, 0, -windowDays)
		t, err := m.store.FindThreadBySubjectAndParticipant(ctx, normSubject, msg.FromAddress, since)
		if err == nil {
			matchedThread = &t
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return ResolutionResult{}, fmt.Errorf("fallback subject+participant lookup: %w", err)
		}
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

	cand, err := m.assigner.Resolve(ctx, assignment.EmailInput{
		FromAddress:       msg.FromAddress,
		FromName:          msg.FromName,
		ToAddresses:       msg.ToAddresses,
		Subject:           msg.Subject,
		SubjectNormalized: normSubject,
		BodyText:          msg.BodyText,
		PreviousThreadID:  reopensThreadID,
	})
	if err != nil {
		return ResolutionResult{}, fmt.Errorf("resolve assignee for new thread: %w", err)
	}

	newThread, err := m.store.CreateThread(ctx, sqlc.Thread{
		SubjectNormalized: normSubject,
		SubjectDisplay:    displaySubject,
		Status:            sqlc.ThreadStatusNew,
		AssigneeID:        cand.AssigneeID,
		AssignmentSource:  cand.Source,
		CustomerEmail:     msg.FromAddress,
		CustomerName:      msg.FromName,
		ReopensThreadID:   reopensThreadID,
		LastMessageAt:     msg.SentAt,
	})
	if err != nil {
		return ResolutionResult{}, fmt.Errorf("create thread: %w", err)
	}

	if err := m.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
		ThreadID:  newThread.ID,
		EventType: "created",
		NewValue: map[string]any{
			"status":            newThread.Status,
			"reopens_thread_id": reopensThreadID,
		},
	}); err != nil {
		return ResolutionResult{}, err
	}
	if err := m.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
		ThreadID:  newThread.ID,
		EventType: "assigned",
		NewValue: map[string]string{
			"assignee_id": cand.AssigneeID.String(),
			"source":      cand.Source,
		},
	}); err != nil {
		return ResolutionResult{}, err
	}

	return ResolutionResult{
		Thread:           newThread,
		ParentEmailID:    parentEmailID,
		CreatedNewThread: true,
		ReopensThread:    reopensThread,
	}, nil
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
