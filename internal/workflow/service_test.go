package workflow_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/workflow"
)

type fakeWorkflowStore struct {
	threads      map[uuid.UUID]sqlc.Thread
	emails       map[uuid.UUID][]sqlc.Email
	users        map[uuid.UUID]sqlc.User
	jobs         []sqlc.IMAPJob
	threadEvents []sqlc.ThreadEvent
	auditLogs    []sqlc.AuditLog
}

func newFakeWorkflowStore() *fakeWorkflowStore {
	return &fakeWorkflowStore{
		threads: make(map[uuid.UUID]sqlc.Thread),
		emails:  make(map[uuid.UUID][]sqlc.Email),
		users:   make(map[uuid.UUID]sqlc.User),
	}
}

func (f *fakeWorkflowStore) GetThreadByID(_ context.Context, id uuid.UUID) (sqlc.Thread, error) {
	t, ok := f.threads[id]
	if !ok {
		return sqlc.Thread{}, pgx.ErrNoRows
	}
	return t, nil
}

func (f *fakeWorkflowStore) UpdateThreadStatus(_ context.Context, id uuid.UUID, status sqlc.ThreadStatus) error {
	t := f.threads[id]
	t.Status = status
	f.threads[id] = t
	return nil
}

func (f *fakeWorkflowStore) UpdateThreadAssignee(_ context.Context, id, assigneeID uuid.UUID, source string) error {
	t := f.threads[id]
	t.AssigneeID = assigneeID
	t.AssignmentSource = source
	f.threads[id] = t
	return nil
}

func (f *fakeWorkflowStore) TouchThreadLastMessageAt(_ context.Context, id uuid.UUID, src sqlc.Thread) error {
	t := f.threads[id]
	t.LastMessageAt = src.LastMessageAt
	f.threads[id] = t
	return nil
}

func (f *fakeWorkflowStore) ListEmailsByThread(_ context.Context, threadID uuid.UUID) ([]sqlc.Email, error) {
	return f.emails[threadID], nil
}

func (f *fakeWorkflowStore) GetSystemSettings(_ context.Context) (sqlc.SystemSettings, error) {
	return sqlc.SystemSettings{
		IMAPInboxMailbox:   "INBOX",
		IMAPArchiveMailbox: "Archive",
		IMAPJunkMailbox:    "Junk",
		IMAPSentMailbox:    "Sent",
	}, nil
}

func (f *fakeWorkflowStore) EnqueueIMAPJob(_ context.Context, job sqlc.IMAPJob) (sqlc.IMAPJob, error) {
	job.ID = uuid.New()
	f.jobs = append(f.jobs, job)
	return job, nil
}

func (f *fakeWorkflowStore) GetUserByID(_ context.Context, id uuid.UUID) (sqlc.User, error) {
	u, ok := f.users[id]
	if !ok {
		return sqlc.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeWorkflowStore) ReparentThreadEmailsAndEvents(_ context.Context, sourceThreadID, targetThreadID uuid.UUID) error {
	f.emails[targetThreadID] = append(f.emails[targetThreadID], f.emails[sourceThreadID]...)
	delete(f.emails, sourceThreadID)
	return nil
}

func (f *fakeWorkflowStore) DeleteThread(_ context.Context, id uuid.UUID) error {
	delete(f.threads, id)
	return nil
}

func (f *fakeWorkflowStore) InsertAuditLog(_ context.Context, entry sqlc.AuditLog) error {
	f.auditLogs = append(f.auditLogs, entry)
	return nil
}

func (f *fakeWorkflowStore) InsertThreadEvent(_ context.Context, ev sqlc.ThreadEvent) error {
	f.threadEvents = append(f.threadEvents, ev)
	return nil
}

func TestWorkflowTransitionsAndIMAPJobQueueing(t *testing.T) {
	store := newFakeWorkflowStore()
	auditLogger := audit.NewLogger(store, nil)
	wakeCh := make(chan struct{}, 4)
	svc := workflow.NewService(store, auditLogger, wakeCh)

	inbox := "INBOX"
	uid := int64(42)
	uidVal := int64(100)

	// Thread 1: new -> in_progress -> completed (enqueues move_to_archive)
	t1ID := uuid.New()
	store.threads[t1ID] = sqlc.Thread{ID: t1ID, Status: sqlc.ThreadStatusNew}
	store.emails[t1ID] = []sqlc.Email{{
		ID:              uuid.New(),
		ThreadID:        t1ID,
		Direction:       "inbound",
		IMAPMailbox:     &inbox,
		IMAPUID:         &uid,
		IMAPUIDValidity: &uidVal,
	}}

	ctx := context.Background()
	// Invalid jump new -> completed must fail
	if err := svc.Transition(ctx, t1ID, sqlc.ThreadStatusCompleted, nil); !errors.Is(err, workflow.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for new -> completed, got: %v", err)
	}

	if err := svc.Transition(ctx, t1ID, sqlc.ThreadStatusInProgress, nil); err != nil {
		t.Fatalf("new -> in_progress: %v", err)
	}
	if err := svc.Transition(ctx, t1ID, sqlc.ThreadStatusCompleted, nil); err != nil {
		t.Fatalf("in_progress -> completed: %v", err)
	}
	if len(store.jobs) != 1 || store.jobs[0].JobType != "move_to_archive" || store.jobs[0].TargetMailbox != "Archive" {
		t.Fatalf("expected move_to_archive job to Archive, got: %+v", store.jobs)
	}

	// Thread 2: new -> spam (enqueues move_to_junk)
	t2ID := uuid.New()
	store.threads[t2ID] = sqlc.Thread{ID: t2ID, Status: sqlc.ThreadStatusNew}
	store.emails[t2ID] = []sqlc.Email{{
		ID:              uuid.New(),
		ThreadID:        t2ID,
		Direction:       "inbound",
		IMAPMailbox:     &inbox,
		IMAPUID:         &uid,
		IMAPUIDValidity: &uidVal,
	}}
	if err := svc.Transition(ctx, t2ID, sqlc.ThreadStatusSpam, nil); err != nil {
		t.Fatalf("new -> spam: %v", err)
	}
	if len(store.jobs) != 2 || store.jobs[1].JobType != "move_to_junk" || store.jobs[1].TargetMailbox != "Junk" {
		t.Fatalf("expected move_to_junk job to Junk, got: %+v", store.jobs)
	}
}

func TestWorkflowReopenAndMerge(t *testing.T) {
	store := newFakeWorkflowStore()
	auditLogger := audit.NewLogger(store, nil)
	svc := workflow.NewService(store, auditLogger, nil)

	completedID := uuid.New()
	followUpID := uuid.New()
	store.threads[completedID] = sqlc.Thread{
		ID:     completedID,
		Status: sqlc.ThreadStatusCompleted,
	}
	store.threads[followUpID] = sqlc.Thread{
		ID:              followUpID,
		Status:          sqlc.ThreadStatusNew,
		ReopensThreadID: &completedID,
	}
	store.emails[completedID] = []sqlc.Email{{ID: uuid.New(), ThreadID: completedID}}
	store.emails[followUpID] = []sqlc.Email{{ID: uuid.New(), ThreadID: followUpID}}

	mergedTargetID, err := svc.ReopenAndMerge(context.Background(), followUpID, nil)
	if err != nil {
		t.Fatalf("ReopenAndMerge: %v", err)
	}
	if mergedTargetID != completedID {
		t.Fatalf("got mergedTargetID %s, want %s", mergedTargetID, completedID)
	}
	if store.threads[completedID].Status != sqlc.ThreadStatusInProgress {
		t.Fatalf("expected target thread status in_progress, got %s", store.threads[completedID].Status)
	}
	if _, exists := store.threads[followUpID]; exists {
		t.Fatal("expected follow-up thread to be deleted after merge")
	}
	if len(store.emails[completedID]) != 2 {
		t.Fatalf("expected 2 emails in reopened thread, got %d", len(store.emails[completedID]))
	}
}
