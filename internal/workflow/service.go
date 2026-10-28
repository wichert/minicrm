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
	// ErrAssigneeNoAccountAccess is returned when a thread would be assigned to a user
	// who has no access to the thread's mail account.
	ErrAssigneeNoAccountAccess = errors.New("assignee has no access to the thread's mail account")
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
	AssignUnassignedThreadsByAccount(ctx context.Context, accountID string, assigneeID uuid.UUID, source string) ([]uuid.UUID, error)
}

// QueriesAdapter wraps *sqlc.Queries to satisfy Store.
type QueriesAdapter struct {
	*sqlc.Queries
}

// TouchThreadLastMessageAt updates last_message_at on target thread using source thread's timestamp.
func (a QueriesAdapter) TouchThreadLastMessageAt(ctx context.Context, id uuid.UUID, src sqlc.Thread) error {
	return a.Queries.TouchThreadLastMessageAt(ctx, id, src.LastMessageAt)
}

// AtomicStore runs a workflow operation and its audit writes in one transaction.
type AtomicStore interface {
	InTransaction(context.Context, func(Store, audit.Store) error) error
}

// InTransaction binds workflow and audit operations to a single database transaction.
func (a QueriesAdapter) InTransaction(ctx context.Context, fn func(Store, audit.Store) error) error {
	tx, err := a.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin workflow transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := a.WithTx(tx)
	if err := fn(lockedQueriesAdapter{QueriesAdapter{Queries: q}}, q); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type lockedQueriesAdapter struct{ QueriesAdapter }

func (a lockedQueriesAdapter) GetThreadByID(ctx context.Context, id uuid.UUID) (sqlc.Thread, error) {
	return a.LockThreadByID(ctx, id)
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
		return to == sqlc.ThreadStatusCompleted || to == sqlc.ThreadStatusSpam
	default:
		return false
	}
}

// Transition validates and executes a state transition on threadID, enqueueing IMAP moves when needed.
func (s *Service) Transition(ctx context.Context, threadID uuid.UUID, targetStatus sqlc.ThreadStatus, actorID *uuid.UUID) error {
	if atomicStore, ok := s.store.(AtomicStore); ok {
		err := atomicStore.InTransaction(ctx, func(store Store, auditStore audit.Store) error {
			txService := NewService(store, s.audit.WithStore(auditStore), nil)
			return txService.transition(ctx, threadID, targetStatus, actorID)
		})
		if err == nil && (targetStatus == sqlc.ThreadStatusSpam || targetStatus == sqlc.ThreadStatusCompleted) {
			s.wakeJobs()
		}
		return err
	}
	return s.transition(ctx, threadID, targetStatus, actorID)
}

func (s *Service) transition(ctx context.Context, threadID uuid.UUID, targetStatus sqlc.ThreadStatus, actorID *uuid.UUID) error {
	thread, err := s.store.GetThreadByID(ctx, threadID)
	if err != nil {
		return fmt.Errorf("load thread %s: %w", threadID, err)
	}
	if !IsValidTransition(thread.Status, targetStatus) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, thread.Status, targetStatus)
	}

	if targetStatus == sqlc.ThreadStatusSpam || targetStatus == sqlc.ThreadStatusCompleted {
		if draftStore, ok := s.store.(interface {
			ClearReplyDraft(context.Context, sqlc.Thread, *uuid.UUID, string) error
		}); ok {
			if err := draftStore.ClearReplyDraft(ctx, thread, actorID, "terminal_transition"); err != nil {
				return err
			}
		}
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

// CompleteFromNew atomically records both legal transitions and queues the archive move.
func (s *Service) CompleteFromNew(ctx context.Context, threadID uuid.UUID, actorID *uuid.UUID) error {
	atomicStore, ok := s.store.(AtomicStore)
	if !ok {
		return errors.New("workflow store does not support transactions")
	}
	err := atomicStore.InTransaction(ctx, func(store Store, auditStore audit.Store) error {
		txService := NewService(store, s.audit.WithStore(auditStore), nil)
		if err := txService.transition(ctx, threadID, sqlc.ThreadStatusInProgress, actorID); err != nil {
			return err
		}
		return txService.transition(ctx, threadID, sqlc.ThreadStatusCompleted, actorID)
	})
	if err == nil {
		s.wakeJobs()
	}
	return err
}

// RecoverSpam atomically returns a spam thread to in-progress and queues restoration.
// Jobs are ordered per email so an already-running junk move finishes first.
func (s *Service) RecoverSpam(ctx context.Context, threadID uuid.UUID, actorID *uuid.UUID) error {
	atomicStore, ok := s.store.(AtomicStore)
	if !ok {
		return errors.New("workflow store does not support transactions")
	}
	err := atomicStore.InTransaction(ctx, func(store Store, auditStore audit.Store) error {
		thread, err := store.GetThreadByID(ctx, threadID)
		if err != nil {
			return err
		}
		if thread.Status != sqlc.ThreadStatusSpam {
			return fmt.Errorf("%w: %s -> in_progress", ErrInvalidTransition, thread.Status)
		}
		settings, err := store.GetSystemSettings(ctx)
		if err != nil {
			return err
		}
		emails, err := store.ListEmailsByThread(ctx, threadID)
		if err != nil {
			return err
		}
		if err := store.UpdateThreadStatus(ctx, threadID, sqlc.ThreadStatusInProgress); err != nil {
			return err
		}
		logger := s.audit.WithStore(auditStore)
		if err := logger.LogThreadEvent(ctx, audit.ThreadEventParams{ThreadID: threadID, ActorUserID: actorID, EventType: "status_changed", OldValue: map[string]string{"status": "spam"}, NewValue: map[string]string{"status": "in_progress", "reason": "not_spam"}}); err != nil {
			return err
		}
		for _, em := range emails {
			if em.Direction != "inbound" {
				continue
			}
			source := settings.IMAPJunkMailbox
			if _, err := store.EnqueueIMAPJob(ctx, sqlc.IMAPJob{AccountID: em.AccountID, JobType: "restore_from_junk", EmailID: em.ID, SourceMailbox: &source, TargetMailbox: settings.IMAPInboxMailbox, MaxAttempts: 10}); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.wakeJobs()
	}
	return err
}

func (s *Service) wakeJobs() {
	if s.jobWakeCh != nil {
		select {
		case s.jobWakeCh <- struct{}{}:
		default:
		}
	}
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
		if em.Direction != "inbound" || em.IMAPMailbox == nil || (em.IMAPUID == nil && em.MessageID == "") {
			continue
		}
		_, err := s.store.EnqueueIMAPJob(ctx, sqlc.IMAPJob{
			AccountID:       em.AccountID,
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

// Reassign assigns a thread to a user and logs an assigned or reassigned event.
//
// The target user must be active and have access to the thread's mail account; otherwise
// ErrAssigneeNoAccountAccess is returned.
func (s *Service) Reassign(ctx context.Context, threadID, newAssigneeID uuid.UUID, actorID *uuid.UUID) error {
	if atomicStore, ok := s.store.(AtomicStore); ok {
		return atomicStore.InTransaction(ctx, func(store Store, auditStore audit.Store) error {
			return NewService(store, s.audit.WithStore(auditStore), nil).reassign(ctx, threadID, newAssigneeID, actorID)
		})
	}
	return s.reassign(ctx, threadID, newAssigneeID, actorID)
}

func (s *Service) reassign(ctx context.Context, threadID, newAssigneeID uuid.UUID, actorID *uuid.UUID) error {
	thread, err := s.store.GetThreadByID(ctx, threadID)
	if err != nil {
		return fmt.Errorf("load thread: %w", err)
	}
	if thread.Status != sqlc.ThreadStatusNew && thread.Status != sqlc.ThreadStatusInProgress {
		return fmt.Errorf("%w: cannot assign a %s thread", ErrInvalidTransition, thread.Status)
	}
	targetUser, err := s.store.GetUserByID(ctx, newAssigneeID)
	if err != nil {
		return fmt.Errorf("load assignee user: %w", err)
	}
	if !targetUser.IsActive {
		return errors.New("cannot assign thread to inactive user")
	}
	if !targetUser.HasAccountAccess(thread.AccountID) {
		return fmt.Errorf("%w: user %s has no access to account %q", ErrAssigneeNoAccountAccess, targetUser.Email, thread.AccountID)
	}
	if err := s.store.UpdateThreadAssignee(ctx, threadID, newAssigneeID, "manual"); err != nil {
		return fmt.Errorf("update thread assignee: %w", err)
	}

	eventType := "reassigned"
	oldValue := map[string]any{}
	if thread.AssigneeID == nil {
		eventType = "assigned"
		oldValue["assignee_id"] = nil
	} else {
		oldValue["assignee_id"] = thread.AssigneeID.String()
	}
	return s.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
		ThreadID:    threadID,
		ActorUserID: actorID,
		EventType:   eventType,
		OldValue:    oldValue,
		NewValue:    map[string]string{"assignee_id": newAssigneeID.String(), "source": "manual"},
	})
}

// AssignUnassignedThreadsForAccount assigns all still-unassigned active threads of an
// account to assigneeID, logging an assigned event for each affected thread.
//
// It is used when an account's default assignee is configured, so waiting threads are
// picked up automatically.
func (s *Service) AssignUnassignedThreadsForAccount(ctx context.Context, accountID string, assigneeID uuid.UUID, actorID *uuid.UUID) (int, error) {
	assignee, err := s.store.GetUserByID(ctx, assigneeID)
	if err != nil {
		return 0, fmt.Errorf("load assignee user: %w", err)
	}
	if !assignee.IsActive {
		return 0, errors.New("cannot assign threads to inactive user")
	}
	if !assignee.HasAccountAccess(accountID) {
		return 0, fmt.Errorf("%w: user %s has no access to account %q", ErrAssigneeNoAccountAccess, assignee.Email, accountID)
	}
	threadIDs, err := s.store.AssignUnassignedThreadsByAccount(ctx, accountID, assigneeID, "default")
	if err != nil {
		return 0, fmt.Errorf("assign unassigned threads for account %q: %w", accountID, err)
	}
	for _, id := range threadIDs {
		if err := s.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
			ThreadID:    id,
			ActorUserID: actorID,
			EventType:   "assigned",
			OldValue:    map[string]any{"assignee_id": nil},
			NewValue:    map[string]string{"assignee_id": assigneeID.String(), "source": "default"},
		}); err != nil {
			return 0, err
		}
	}
	return len(threadIDs), nil
}

// ReopenAndMerge merges a follow-up 'new' thread into its referenced 'completed' thread
// and transitions that completed thread back to 'in_progress'.
func (s *Service) ReopenAndMerge(ctx context.Context, followUpThreadID uuid.UUID, actorID *uuid.UUID) (uuid.UUID, error) {
	if atomicStore, ok := s.store.(AtomicStore); ok {
		var targetID uuid.UUID
		err := atomicStore.InTransaction(ctx, func(store Store, auditStore audit.Store) error {
			var err error
			targetID, err = NewService(store, s.audit.WithStore(auditStore), nil).reopenAndMerge(ctx, followUpThreadID, actorID)
			return err
		})
		if err != nil {
			return uuid.Nil, err
		}
		return targetID, nil
	}
	return s.reopenAndMerge(ctx, followUpThreadID, actorID)
}

func (s *Service) reopenAndMerge(ctx context.Context, followUpThreadID uuid.UUID, actorID *uuid.UUID) (uuid.UUID, error) {
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
	if followUp.AccountID != target.AccountID {
		return uuid.Nil, fmt.Errorf("%w: threads belong to different mail accounts", ErrCannotReopenMerge)
	}

	if drafts, ok := s.store.(interface {
		MergeReplyDrafts(context.Context, sqlc.Thread, sqlc.Thread) error
	}); ok {
		if err := drafts.MergeReplyDrafts(ctx, followUp, target); err != nil {
			return uuid.Nil, fmt.Errorf("merge reply drafts: %w", err)
		}
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
