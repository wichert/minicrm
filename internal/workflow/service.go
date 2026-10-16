// Package workflow implements the thread state machine, IMAP folder move queueing, and Reopen & Merge.
package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// Domain errors returned by workflow operations.
var (
	ErrInvalidTransition = errors.New("invalid workflow state transition")
	ErrCannotReopenMerge = errors.New("thread cannot be reopened and merged")
)

// Store defines the persistence operations required by the workflow Service.
type Store interface {
	GetThreadByID(ctx context.Context, id uuid.UUID) (sqlc.Thread, error)
	UpdateThreadStatus(ctx context.Context, id uuid.UUID, status sqlc.ThreadStatus) error
	UpdateThreadAssignee(ctx context.Context, id, assigneeID uuid.UUID, source string) error
	TouchThreadLastMessageAt(ctx context.Context, id uuid.UUID, ts sqlc.Thread) error
	ListEmailsByThread(ctx context.Context, threadID uuid.UUID) ([]sqlc.Email, error)
	GetSystemSettings(ctx context.Context) (sqlc.SystemSettings, error)
	EnqueueIMAPJob(ctx context.Context, job sqlc.IMAPJob) (sqlc.IMAPJob, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (sqlc.User, error)
	ReparentThreadEmailsAndEvents(ctx context.Context, sourceThreadID, targetThreadID uuid.UUID) error
	DeleteThread(ctx context.Context, id uuid.UUID) error
}

// QueriesAdapter wraps *sqlc.Queries to satisfy Store.
type QueriesAdapter struct {
	*sqlc.Queries
}

// TouchThreadLastMessageAt updates last_message_at on target thread using source thread's timestamp.
func (a QueriesAdapter) TouchThreadLastMessageAt(ctx context.Context, id uuid.UUID, src sqlc.Thread) error {
	return a.Queries.TouchThreadLastMessageAt(ctx, id, src.LastMessageAt)
}

// Service manages thread state transitions, reassignments, and Reopen & Merge operations.
type Service struct {
	store     Store
	audit     *audit.Logger
	jobWakeCh chan<- struct{}
}

// NewService constructs a workflow Service.
func NewService(store Store, auditLogger *audit.Logger, jobWakeCh chan<- struct{}) *Service {
	return &Service{
		store:     store,
		audit:     auditLogger,
		jobWakeCh: jobWakeCh,
	}
}

// IsValidTransition returns true if transitioning from -> to is permitted in standard workflow.
func IsValidTransition(from, to sqlc.ThreadStatus) bool {
	switch from {
	case sqlc.ThreadStatusNew:
		return to == sqlc.ThreadStatusInProgress || to == sqlc.ThreadStatusSpam
	case sqlc.ThreadStatusInProgress:
		return to == sqlc.ThreadStatusCompleted
	default:
		return false
	}
}

// Transition validates and executes a state transition on threadID, enqueueing IMAP moves when needed.
func (s *Service) Transition(ctx context.Context, threadID uuid.UUID, targetStatus sqlc.ThreadStatus, actorID *uuid.UUID) error {
	thread, err := s.store.GetThreadByID(ctx, threadID)
	if err != nil {
		return fmt.Errorf("load thread %s: %w", threadID, err)
	}
	if !IsValidTransition(thread.Status, targetStatus) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, thread.Status, targetStatus)
	}

	if err := s.store.UpdateThreadStatus(ctx, threadID, targetStatus); err != nil {
		return fmt.Errorf("update thread status: %w", err)
	}

	if err := s.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
		ThreadID:    threadID,
		ActorUserID: actorID,
		EventType:   "status_changed",
		OldValue:    map[string]string{"status": string(thread.Status)},
		NewValue:    map[string]string{"status": string(targetStatus)},
	}); err != nil {
		return err
	}

	// Enqueue IMAP folder moves on transition to spam (Junk) or completed (Archive).
	if targetStatus == sqlc.ThreadStatusSpam || targetStatus == sqlc.ThreadStatusCompleted {
		if err := s.enqueueFolderMoveJobs(ctx, threadID, targetStatus); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) enqueueFolderMoveJobs(ctx context.Context, threadID uuid.UUID, targetStatus sqlc.ThreadStatus) error {
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return fmt.Errorf("load system_settings for imap move: %w", err)
	}
	emails, err := s.store.ListEmailsByThread(ctx, threadID)
	if err != nil {
		return fmt.Errorf("list thread emails for imap move: %w", err)
	}

	jobType := "move_to_archive"
	targetMailbox := settings.IMAPArchiveMailbox
	if targetStatus == sqlc.ThreadStatusSpam {
		jobType = "move_to_junk"
		targetMailbox = settings.IMAPJunkMailbox
	}

	enqueued := 0
	for _, em := range emails {
		if em.Direction != "inbound" || em.IMAPUID == nil || em.IMAPMailbox == nil {
			continue
		}
		if *em.IMAPMailbox == targetMailbox {
			continue
		}
		_, err := s.store.EnqueueIMAPJob(ctx, sqlc.IMAPJob{
			JobType:         jobType,
			EmailID:         em.ID,
			SourceMailbox:   em.IMAPMailbox,
			TargetMailbox:   targetMailbox,
			IMAPUIDValidity: em.IMAPUIDValidity,
			IMAPUID:         em.IMAPUID,
			MaxAttempts:     10,
		})
		if err != nil {
			return fmt.Errorf("enqueue %s job for email %s: %w", jobType, em.ID, err)
		}
		enqueued++
	}
	if enqueued > 0 && s.jobWakeCh != nil {
		select {
		case s.jobWakeCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// Reassign assigns a thread to a different active user and logs a reassigned event.
func (s *Service) Reassign(ctx context.Context, threadID, newAssigneeID uuid.UUID, actorID *uuid.UUID) error {
	thread, err := s.store.GetThreadByID(ctx, threadID)
	if err != nil {
		return fmt.Errorf("load thread: %w", err)
	}
	targetUser, err := s.store.GetUserByID(ctx, newAssigneeID)
	if err != nil {
		return fmt.Errorf("load assignee user: %w", err)
	}
	if !targetUser.IsActive {
		return errors.New("cannot assign thread to inactive user")
	}
	if err := s.store.UpdateThreadAssignee(ctx, threadID, newAssigneeID, "manual"); err != nil {
		return fmt.Errorf("update thread assignee: %w", err)
	}
	return s.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
		ThreadID:    threadID,
		ActorUserID: actorID,
		EventType:   "reassigned",
		OldValue:    map[string]string{"assignee_id": thread.AssigneeID.String()},
		NewValue:    map[string]string{"assignee_id": newAssigneeID.String(), "source": "manual"},
	})
}

// ReopenAndMerge merges a follow-up 'new' thread into its referenced 'completed' thread
// and transitions that completed thread back to 'in_progress'.
func (s *Service) ReopenAndMerge(ctx context.Context, followUpThreadID uuid.UUID, actorID *uuid.UUID) (uuid.UUID, error) {
	followUp, err := s.store.GetThreadByID(ctx, followUpThreadID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load follow-up thread: %w", err)
	}
	if followUp.ReopensThreadID == nil {
		return uuid.Nil, fmt.Errorf("%w: thread does not reference a completed thread", ErrCannotReopenMerge)
	}
	targetID := *followUp.ReopensThreadID
	target, err := s.store.GetThreadByID(ctx, targetID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load target completed thread: %w", err)
	}
	if target.Status != sqlc.ThreadStatusCompleted {
		return uuid.Nil, fmt.Errorf("%w: target thread status is %s (want completed)", ErrCannotReopenMerge, target.Status)
	}

	if err := s.store.ReparentThreadEmailsAndEvents(ctx, followUp.ID, target.ID); err != nil {
		return uuid.Nil, fmt.Errorf("reparent emails and events: %w", err)
	}
	if err := s.store.UpdateThreadStatus(ctx, target.ID, sqlc.ThreadStatusInProgress); err != nil {
		return uuid.Nil, fmt.Errorf("reopen target thread to in_progress: %w", err)
	}
	if err := s.store.TouchThreadLastMessageAt(ctx, target.ID, followUp); err != nil {
		return uuid.Nil, fmt.Errorf("touch target thread last_message_at: %w", err)
	}
	if err := s.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
		ThreadID:    target.ID,
		ActorUserID: actorID,
		EventType:   "reopen_merged",
		OldValue:    map[string]string{"status": string(sqlc.ThreadStatusCompleted), "merged_from_thread_id": followUp.ID.String()},
		NewValue:    map[string]string{"status": string(sqlc.ThreadStatusInProgress)},
	}); err != nil {
		return uuid.Nil, err
	}
	if err := s.store.DeleteThread(ctx, followUp.ID); err != nil {
		return uuid.Nil, fmt.Errorf("delete merged follow-up thread: %w", err)
	}
	return target.ID, nil
}
