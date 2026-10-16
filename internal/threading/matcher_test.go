package threading_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/assignment"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/threading"
)

func TestNormalizeSubject(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		wantPref bool
	}{
		{"Re: Fwd: [2]:  Invoice #42 ", "[2]: Invoice #42", true},
		{"Re[2]: AW: WG: Order Question", "Order Question", true},
		{"Plain Subject", "Plain Subject", false},
	}
	for _, tc := range cases {
		got, pref := threading.NormalizeSubject(tc.in)
		if got != tc.want || pref != tc.wantPref {
			t.Fatalf("NormalizeSubject(%q) = (%q, %v), want (%q, %v)", tc.in, got, pref, tc.want, tc.wantPref)
		}
	}
}

type fakeThreadingStore struct {
	threads      map[uuid.UUID]sqlc.Thread
	emailsByMsg  map[string]sqlc.Email
	defaultUser  sqlc.User
	threadEvents []sqlc.ThreadEvent
	auditLogs    []sqlc.AuditLog
}

func newFakeThreadingStore() *fakeThreadingStore {
	u := sqlc.User{ID: uuid.New(), Email: "agent@example.com", IsActive: true}
	return &fakeThreadingStore{
		threads:     make(map[uuid.UUID]sqlc.Thread),
		emailsByMsg: make(map[string]sqlc.Email),
		defaultUser: u,
	}
}

func (f *fakeThreadingStore) FindEmailByMessageIDs(_ context.Context, msgIDs []string) (sqlc.Email, error) {
	for _, id := range msgIDs {
		if em, ok := f.emailsByMsg[id]; ok {
			return em, nil
		}
	}
	return sqlc.Email{}, pgx.ErrNoRows
}

func (f *fakeThreadingStore) GetThreadByID(_ context.Context, id uuid.UUID) (sqlc.Thread, error) {
	t, ok := f.threads[id]
	if !ok {
		return sqlc.Thread{}, pgx.ErrNoRows
	}
	return t, nil
}

func (f *fakeThreadingStore) FindThreadBySubjectAndParticipant(_ context.Context, normSubject, customerEmail string, since time.Time) (sqlc.Thread, error) {
	for _, t := range f.threads {
		if strings.EqualFold(t.SubjectNormalized, normSubject) &&
			strings.EqualFold(t.CustomerEmail, customerEmail) &&
			!t.LastMessageAt.Before(since) &&
			t.Status != sqlc.ThreadStatusSpam {
			return t, nil
		}
	}
	return sqlc.Thread{}, pgx.ErrNoRows
}

func (f *fakeThreadingStore) CreateThread(_ context.Context, t sqlc.Thread) (sqlc.Thread, error) {
	t.ID = uuid.New()
	f.threads[t.ID] = t
	return t, nil
}

func (f *fakeThreadingStore) TouchThreadLastMessageAt(_ context.Context, id uuid.UUID, ts time.Time) error {
	t := f.threads[id]
	t.LastMessageAt = ts
	f.threads[id] = t
	return nil
}

func (f *fakeThreadingStore) GetSystemSettings(_ context.Context) (sqlc.SystemSettings, error) {
	return sqlc.SystemSettings{
		DefaultAssigneeID:      f.defaultUser.ID,
		SubjectMatchWindowDays: 30,
	}, nil
}

func (f *fakeThreadingStore) GetUserByID(_ context.Context, id uuid.UUID) (sqlc.User, error) {
	if id == f.defaultUser.ID {
		return f.defaultUser, nil
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (f *fakeThreadingStore) ListUsers(_ context.Context) ([]sqlc.User, error) {
	return []sqlc.User{f.defaultUser}, nil
}

func (f *fakeThreadingStore) InsertAuditLog(_ context.Context, entry sqlc.AuditLog) error {
	f.auditLogs = append(f.auditLogs, entry)
	return nil
}

func (f *fakeThreadingStore) InsertThreadEvent(_ context.Context, ev sqlc.ThreadEvent) error {
	f.threadEvents = append(f.threadEvents, ev)
	return nil
}

func TestMatcherHeaderFallbackAndCompletedThreadLink(t *testing.T) {
	store := newFakeThreadingStore()
	assigner := assignment.NewEngine(0.5, assignment.NewDefaultAssigneeResolver(store))
	auditLogger := audit.NewLogger(store, nil)
	matcher := threading.NewMatcher(store, assigner, auditLogger)

	ctx := context.Background()
	now := time.Now().UTC()

	// 1. First email creates a new thread
	res1, err := matcher.ResolveThread(ctx, threading.InboundMessageMetadata{
		MessageID:   "msg-1@client.com",
		FromAddress: "alice@client.com",
		FromName:    "Alice",
		Subject:     "Pricing Inquiry",
		SentAt:      now,
	})
	if err != nil {
		t.Fatalf("ResolveThread 1: %v", err)
	}
	if !res1.CreatedNewThread || res1.Thread.Status != sqlc.ThreadStatusNew {
		t.Fatalf("expected new thread, got %+v", res1)
	}
	store.emailsByMsg["msg-1@client.com"] = sqlc.Email{
		ID:        uuid.New(),
		ThreadID:  res1.Thread.ID,
		MessageID: "msg-1@client.com",
	}

	// 2. Reply via In-Reply-To attaches to the existing thread
	res2, err := matcher.ResolveThread(ctx, threading.InboundMessageMetadata{
		MessageID:   "msg-2@client.com",
		InReplyTo:   "<msg-1@client.com>",
		FromAddress: "alice@client.com",
		Subject:     "Re: Pricing Inquiry",
		SentAt:      now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ResolveThread 2: %v", err)
	}
	if res2.CreatedNewThread || res2.Thread.ID != res1.Thread.ID {
		t.Fatalf("expected reply to attach to thread %s, got %+v", res1.Thread.ID, res2)
	}

	// 3. Reply without headers matches via Subject + Sender fallback
	res3, err := matcher.ResolveThread(ctx, threading.InboundMessageMetadata{
		MessageID:   "msg-3@client.com",
		FromAddress: "alice@client.com",
		Subject:     "AW: Re: Pricing Inquiry",
		SentAt:      now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ResolveThread 3: %v", err)
	}
	if res3.CreatedNewThread || res3.Thread.ID != res1.Thread.ID {
		t.Fatalf("expected fallback match to thread %s, got %+v", res1.Thread.ID, res3)
	}

	// 4. Mark thread completed; next reply must create a NEW thread with reopens_thread_id set
	completedThread := store.threads[res1.Thread.ID]
	completedThread.Status = sqlc.ThreadStatusCompleted
	store.threads[res1.Thread.ID] = completedThread

	res4, err := matcher.ResolveThread(ctx, threading.InboundMessageMetadata{
		MessageID:   "msg-4@client.com",
		InReplyTo:   "<msg-1@client.com>",
		FromAddress: "alice@client.com",
		Subject:     "Re: Pricing Inquiry",
		SentAt:      now.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ResolveThread 4: %v", err)
	}
	if !res4.CreatedNewThread || res4.Thread.ID == res1.Thread.ID {
		t.Fatalf("expected brand new follow-up thread, got %+v", res4)
	}
	if res4.Thread.ReopensThreadID == nil || *res4.Thread.ReopensThreadID != res1.Thread.ID {
		t.Fatalf("expected ReopensThreadID=%s, got %v", res1.Thread.ID, res4.Thread.ReopensThreadID)
	}
}
