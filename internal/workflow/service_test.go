package workflow_test

import (
	"context"
	"errors"
	"maps"
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
	failEnqueue  bool
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
	assignee := assigneeID
	t.AssigneeID = &assignee
	t.AssignmentSource = source
	f.threads[id] = t
	return nil
}

func (f *fakeWorkflowStore) AssignUnassignedThreadsByAccount(_ context.Context, accountID string, assigneeID uuid.UUID, source string) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for id, t := range f.threads {
		if t.AccountID != accountID || t.AssigneeID != nil {
			continue
		}
		if t.Status != sqlc.ThreadStatusNew && t.Status != sqlc.ThreadStatusInProgress {
			continue
		}
		assignee := assigneeID
		t.AssigneeID = &assignee
		t.AssignmentSource = source
		f.threads[id] = t
		ids = append(ids, id)
	}
	return ids, nil
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
	if f.failEnqueue {
		return sqlc.IMAPJob{}, errors.New("enqueue failed")
	}
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

func (f *fakeWorkflowStore) InTransaction(_ context.Context, fn func(workflow.Store, audit.Store) error) error {
	copyStore := *f
	copyStore.threads = maps.Clone(f.threads)
	copyStore.jobs = append([]sqlc.IMAPJob(nil), f.jobs...)
	copyStore.threadEvents = append([]sqlc.ThreadEvent(nil), f.threadEvents...)
	if err := fn(&copyStore, &copyStore); err != nil {
		return err
	}
	*f = copyStore
	return nil
}

func TestCompleteFromNewIsAtomic(t *testing.T) {
	store := newFakeWorkflowStore()
	id := uuid.New()
	store.threads[id] = sqlc.Thread{ID: id, AccountID: "support", Status: sqlc.ThreadStatusNew}
	mailbox, uid := "INBOX", int64(42)
	store.emails[id] = []sqlc.Email{{ID: uuid.New(), ThreadID: id, AccountID: "support", Direction: "inbound", IMAPMailbox: &mailbox, IMAPUID: &uid}}
	svc := workflow.NewService(store, audit.NewLogger(store, nil), nil)
	store.failEnqueue = true
	if err := svc.CompleteFromNew(context.Background(), id, nil); err == nil {
		t.Fatal("expected enqueue failure")
	}
	if store.threads[id].Status != sqlc.ThreadStatusNew || len(store.threadEvents) != 0 || len(store.jobs) != 0 {
		t.Fatal("failed operation did not roll back")
	}
	store.failEnqueue = false
	if err := svc.CompleteFromNew(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	if store.threads[id].Status != sqlc.ThreadStatusCompleted || len(store.threadEvents) != 2 || len(store.jobs) != 1 || store.jobs[0].JobType != "move_to_archive" || store.jobs[0].AccountID != "support" {
		t.Fatalf("unexpected result: thread=%+v events=%+v jobs=%+v", store.threads[id], store.threadEvents, store.jobs)
	}
	if err := svc.CompleteFromNew(context.Background(), id, nil); !errors.Is(err, workflow.ErrInvalidTransition) {
		t.Fatalf("expected invalid transition, got %v", err)
	}
	if len(store.threadEvents) != 2 || len(store.jobs) != 1 {
		t.Fatal("invalid transition changed state")
	}
}

func TestSpamRecoveryIsAtomicAndExplicit(t *testing.T) {
	store := newFakeWorkflowStore()
	id := uuid.New()
	store.threads[id] = sqlc.Thread{ID: id, AccountID: "sales", Status: sqlc.ThreadStatusInProgress}
	inbox, uid := "INBOX", int64(42)
	store.emails[id] = []sqlc.Email{{ID: uuid.New(), ThreadID: id, AccountID: "sales", Direction: "inbound", IMAPMailbox: &inbox, IMAPUID: &uid}}
	svc := workflow.NewService(store, audit.NewLogger(store, nil), nil)
	if err := svc.Transition(context.Background(), id, sqlc.ThreadStatusSpam, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.jobs) != 1 || store.jobs[0].AccountID != "sales" || store.jobs[0].JobType != "move_to_junk" {
		t.Fatalf("junk job: %+v", store.jobs)
	}
	if err := svc.Transition(context.Background(), id, sqlc.ThreadStatusInProgress, nil); !errors.Is(err, workflow.ErrInvalidTransition) {
		t.Fatalf("direct recovery accepted: %v", err)
	}
	store.failEnqueue = true
	if err := svc.RecoverSpam(context.Background(), id, nil); err == nil {
		t.Fatal("recovery should roll back")
	}
	if store.threads[id].Status != sqlc.ThreadStatusSpam || len(store.jobs) != 1 || len(store.threadEvents) != 1 {
		t.Fatal("failed recovery changed state")
	}
	store.failEnqueue = false
	if err := svc.RecoverSpam(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	if store.threads[id].Status != sqlc.ThreadStatusInProgress || len(store.threadEvents) != 2 || len(store.jobs) != 2 || store.jobs[1].JobType != "restore_from_junk" || store.jobs[1].AccountID != "sales" {
		t.Fatalf("recovery failed: %+v %+v", store.threadEvents, store.jobs)
	}
	if err := svc.RecoverSpam(context.Background(), id, nil); !errors.Is(err, workflow.ErrInvalidTransition) {
		t.Fatalf("duplicate recovery accepted: %v", err)
	}
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
	store.threads[t1ID] = sqlc.Thread{ID: t1ID, AccountID: "support", Status: sqlc.ThreadStatusNew}
	store.emails[t1ID] = []sqlc.Email{{
		ID:              uuid.New(),
		ThreadID:        t1ID,
		AccountID:       "support",
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
	if len(store.jobs) != 1 || store.jobs[0].JobType != "move_to_archive" || store.jobs[0].TargetMailbox != "Archive" || store.jobs[0].AccountID != "support" {
		t.Fatalf("expected move_to_archive job to Archive, got: %+v", store.jobs)
	}

	// Thread 2: new -> spam (enqueues move_to_junk)
	t2ID := uuid.New()
	store.threads[t2ID] = sqlc.Thread{ID: t2ID, AccountID: "support", Status: sqlc.ThreadStatusNew}
	store.emails[t2ID] = []sqlc.Email{{
		ID:              uuid.New(),
		ThreadID:        t2ID,
		AccountID:       "support",
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
		ID:        completedID,
		AccountID: "support",
		Status:    sqlc.ThreadStatusCompleted,
	}
	store.threads[followUpID] = sqlc.Thread{
		ID:              followUpID,
		AccountID:       "support",
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

func TestReassignEnforcesAccountAccess(t *testing.T) {
	store := newFakeWorkflowStore()
	svc := workflow.NewService(store, audit.NewLogger(store, nil), nil)

	supportUser := sqlc.User{ID: uuid.New(), Email: "support@example.com", IsActive: true, AllowedAccounts: []string{"support"}}
	salesUser := sqlc.User{ID: uuid.New(), Email: "sales@example.com", IsActive: true, AllowedAccounts: []string{"sales"}}
	store.users[supportUser.ID] = supportUser
	store.users[salesUser.ID] = salesUser

	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{ID: threadID, AccountID: "support", Status: sqlc.ThreadStatusNew}

	if err := svc.Reassign(context.Background(), threadID, salesUser.ID, nil); !errors.Is(err, workflow.ErrAssigneeNoAccountAccess) {
		t.Fatalf("expected ErrAssigneeNoAccountAccess, got %v", err)
	}

	if err := svc.Reassign(context.Background(), threadID, supportUser.ID, nil); err != nil {
		t.Fatalf("Reassign to allowed user: %v", err)
	}
	if got := store.threads[threadID].AssigneeID; got == nil || *got != supportUser.ID {
		t.Fatalf("expected assignee %s, got %v", supportUser.ID, got)
	}
}

func TestAssignUnassignedThreadsForAccount(t *testing.T) {
	store := newFakeWorkflowStore()
	svc := workflow.NewService(store, audit.NewLogger(store, nil), nil)

	user := sqlc.User{ID: uuid.New(), Email: "support@example.com", IsActive: true, AllowedAccounts: []string{"support", "sales"}}
	store.users[user.ID] = user

	unassigned := uuid.New()
	completedUnassigned := uuid.New()
	alreadyAssigned := uuid.New()
	otherAccount := uuid.New()
	someone := uuid.New()
	store.threads[unassigned] = sqlc.Thread{ID: unassigned, AccountID: "support", Status: sqlc.ThreadStatusNew}
	store.threads[completedUnassigned] = sqlc.Thread{ID: completedUnassigned, AccountID: "support", Status: sqlc.ThreadStatusCompleted}
	store.threads[alreadyAssigned] = sqlc.Thread{ID: alreadyAssigned, AccountID: "support", Status: sqlc.ThreadStatusNew, AssigneeID: &someone}
	store.threads[otherAccount] = sqlc.Thread{ID: otherAccount, AccountID: "sales", Status: sqlc.ThreadStatusNew}

	n, err := svc.AssignUnassignedThreadsForAccount(context.Background(), "support", user.ID, nil)
	if err != nil {
		t.Fatalf("AssignUnassignedThreadsForAccount: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 assigned thread, got %d", n)
	}
	if got := store.threads[unassigned].AssigneeID; got == nil || *got != user.ID {
		t.Fatalf("expected unassigned thread to be assigned, got %v", got)
	}
	if store.threads[completedUnassigned].AssigneeID != nil {
		t.Fatal("completed threads must not be reassigned")
	}
	if store.threads[otherAccount].AssigneeID != nil {
		t.Fatal("threads of other accounts must not be reassigned")
	}
}
