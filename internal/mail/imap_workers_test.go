package mail_test

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/assignment"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/mail"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/threading"
	"github.com/wichert/minicrm/internal/workflow"
)

type fakeIMAPServer struct {
	uidValidity int64
	mailboxes   map[string]map[int64][]byte
	appended    map[string][][]byte
	lastSince   time.Time
	batches     [][]int64
}

func newFakeIMAPServer() *fakeIMAPServer {
	return &fakeIMAPServer{
		uidValidity: 500,
		mailboxes: map[string]map[int64][]byte{
			"INBOX":   {},
			"Archive": {},
			"Junk":    {},
			"Sent":    {},
		},
		appended: make(map[string][][]byte),
	}
}

func (s *fakeIMAPServer) SelectMailbox(_ context.Context, mailbox string) (int64, error) {
	if _, ok := s.mailboxes[mailbox]; !ok {
		s.mailboxes[mailbox] = make(map[int64][]byte)
	}
	return s.uidValidity, nil
}

func (s *fakeIMAPServer) ListUIDsSince(_ context.Context, mailbox string, afterUID int64, since time.Time) ([]int64, error) {
	if mailbox == "INBOX" {
		s.lastSince = since
	}
	var out []int64
	for uid := range s.mailboxes[mailbox] {
		if uid > afterUID {
			out = append(out, uid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (s *fakeIMAPServer) FetchMessagesByUID(_ context.Context, mailbox string, uids []int64) ([]mail.RawIMAPMessage, error) {
	s.batches = append(s.batches, append([]int64(nil), uids...))
	var out []mail.RawIMAPMessage
	for _, uid := range uids {
		if raw, ok := s.mailboxes[mailbox][uid]; ok {
			out = append(out, mail.RawIMAPMessage{UID: uid, UIDValidity: s.uidValidity, RawRFC822: raw})
		}
	}
	return out, nil
}

func (s *fakeIMAPServer) ResolveMailbox(_ context.Context, name string) (string, bool, error) {
	_, ok := s.mailboxes[name]
	return name, ok, nil
}

func (s *fakeIMAPServer) SearchByMessageID(_ context.Context, mailbox, messageID string, limit int) ([]mail.RawIMAPMessage, error) {
	var out []mail.RawIMAPMessage
	for uid, raw := range s.mailboxes[mailbox] {
		if strings.Contains(string(raw), "Message-ID: <"+messageID+">") {
			out = append(out, mail.RawIMAPMessage{UID: uid, UIDValidity: s.uidValidity, RawRFC822: raw})
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeIMAPServer) FindExactMessageID(ctx context.Context, mailbox, messageID string) ([]mail.RawIMAPMessage, error) {
	matches, err := s.SearchByMessageID(ctx, mailbox, messageID, 2)
	return matches, err
}

func (s *fakeIMAPServer) MoveMessage(_ context.Context, sourceMailbox string, _, uid int64, targetMailbox string) error {
	raw, ok := s.mailboxes[sourceMailbox][uid]
	if !ok {
		return mail.ErrMessageNotFound
	}
	delete(s.mailboxes[sourceMailbox], uid)
	if _, exists := s.mailboxes[targetMailbox]; !exists {
		s.mailboxes[targetMailbox] = make(map[int64][]byte)
	}
	s.mailboxes[targetMailbox][uid] = raw
	return nil
}

func (s *fakeIMAPServer) AppendMessage(_ context.Context, targetMailbox string, rawRFC822 []byte) error {
	s.appended[targetMailbox] = append(s.appended[targetMailbox], rawRFC822)
	if _, ok := s.mailboxes[targetMailbox]; !ok {
		s.mailboxes[targetMailbox] = make(map[int64][]byte)
	}
	var next int64 = 1
	for uid := range s.mailboxes[targetMailbox] {
		if uid >= next {
			next = uid + 1
		}
	}
	s.mailboxes[targetMailbox][next] = append([]byte(nil), rawRFC822...)
	return nil
}

func (s *fakeIMAPServer) Close() error { return nil }

type memCRMStore struct {
	defaultUser     sqlc.User
	syncStates      map[string]sqlc.IMAPSyncState
	accountSettings map[string]sqlc.AccountSettings
	threads         map[uuid.UUID]sqlc.Thread
	emails          map[uuid.UUID]sqlc.Email
	attachments     []sqlc.Attachment
	jobs            []sqlc.IMAPJob
	threadEvents    []sqlc.ThreadEvent
	auditLogs       []sqlc.AuditLog
}

func newMemCRMStore() *memCRMStore {
	u := sqlc.User{ID: uuid.New(), Email: "agent@example.com", IsActive: true, AllowedAccounts: []string{"support", "sales"}}
	return &memCRMStore{
		defaultUser:     u,
		syncStates:      make(map[string]sqlc.IMAPSyncState),
		accountSettings: make(map[string]sqlc.AccountSettings),
		threads:         make(map[uuid.UUID]sqlc.Thread),
		emails:          make(map[uuid.UUID]sqlc.Email),
	}
}

func (m *memCRMStore) GetAccountSettings(_ context.Context, accountID string) (sqlc.AccountSettings, error) {
	s, ok := m.accountSettings[accountID]
	if !ok {
		return sqlc.AccountSettings{}, pgx.ErrNoRows
	}
	return s, nil
}

func (m *memCRMStore) UpsertAccountSettings(_ context.Context, s sqlc.AccountSettings) (sqlc.AccountSettings, error) {
	m.accountSettings[s.AccountID] = s
	return s, nil
}

func (m *memCRMStore) GetSystemSettings(_ context.Context) (sqlc.SystemSettings, error) {
	return sqlc.SystemSettings{
		DisplayTimezone:        "UTC",
		IMAPInboxMailbox:       "INBOX",
		IMAPArchiveMailbox:     "Archive",
		IMAPJunkMailbox:        "Junk",
		IMAPSentMailbox:        "Sent",
		SubjectMatchWindowDays: 30,
	}, nil
}

func (m *memCRMStore) GetUserByID(_ context.Context, id uuid.UUID) (sqlc.User, error) {
	if id == m.defaultUser.ID {
		return m.defaultUser, nil
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *memCRMStore) ListUsers(_ context.Context) ([]sqlc.User, error) {
	return []sqlc.User{m.defaultUser}, nil
}

func (m *memCRMStore) GetIMAPSyncState(_ context.Context, accountID, mailbox string) (sqlc.IMAPSyncState, error) {
	st, ok := m.syncStates[accountID+"/"+mailbox]
	if !ok {
		return sqlc.IMAPSyncState{}, pgx.ErrNoRows
	}
	return st, nil
}

func (m *memCRMStore) UpsertIMAPSyncState(_ context.Context, s sqlc.IMAPSyncState) error {
	m.syncStates[s.AccountID+"/"+s.Mailbox] = s
	return nil
}

func (m *memCRMStore) GetEmailByMessageID(_ context.Context, accountID, msgID string) (sqlc.Email, error) {
	for _, em := range m.emails {
		if em.AccountID == accountID && em.MessageID == msgID {
			return em, nil
		}
	}
	return sqlc.Email{}, pgx.ErrNoRows
}

func (m *memCRMStore) FindEmailByMessageIDs(_ context.Context, accountID string, msgIDs []string) (sqlc.Email, error) {
	for _, id := range msgIDs {
		for _, em := range m.emails {
			if em.AccountID == accountID && em.MessageID == id {
				return em, nil
			}
		}
	}
	return sqlc.Email{}, pgx.ErrNoRows
}

func (m *memCRMStore) CreateEmail(_ context.Context, e sqlc.Email) (sqlc.Email, error) {
	e.ID = uuid.New()
	m.emails[e.ID] = e
	return e, nil
}

func (m *memCRMStore) CreateAttachment(_ context.Context, a sqlc.Attachment) (sqlc.Attachment, error) {
	a.ID = uuid.New()
	m.attachments = append(m.attachments, a)
	return a, nil
}

func (m *memCRMStore) GetThreadByID(_ context.Context, id uuid.UUID) (sqlc.Thread, error) {
	t, ok := m.threads[id]
	if !ok {
		return sqlc.Thread{}, pgx.ErrNoRows
	}
	return t, nil
}

func (m *memCRMStore) FindThreadBySubjectAndParticipant(_ context.Context, accountID, normSubject, customerEmail string, since time.Time) (sqlc.Thread, error) {
	for _, t := range m.threads {
		if t.AccountID == accountID &&
			strings.EqualFold(t.SubjectNormalized, normSubject) &&
			strings.EqualFold(t.CustomerEmail, customerEmail) &&
			!t.LastMessageAt.Before(since) &&
			t.Status != sqlc.ThreadStatusSpam {
			return t, nil
		}
	}
	return sqlc.Thread{}, pgx.ErrNoRows
}

func (m *memCRMStore) CreateThread(_ context.Context, t sqlc.Thread) (sqlc.Thread, error) {
	t.ID = uuid.New()
	m.threads[t.ID] = t
	return t, nil
}

func (m *memCRMStore) TouchThreadLastMessageAt(_ context.Context, id uuid.UUID, ts time.Time) error {
	t := m.threads[id]
	t.LastMessageAt = ts
	m.threads[id] = t
	return nil
}

func (m *memCRMStore) SetEmailIMAPLocation(_ context.Context, id uuid.UUID, mailbox string, uidValidity, uid int64) (bool, error) {
	e, ok := m.emails[id]
	if !ok || e.IMAPUID != nil {
		return false, nil
	}
	e.IMAPMailbox, e.IMAPUIDValidity, e.IMAPUID = &mailbox, &uidValidity, &uid
	m.emails[id] = e
	return true, nil
}

func (m *memCRMStore) UpdateThreadStatus(_ context.Context, id uuid.UUID, status sqlc.ThreadStatus) error {
	t := m.threads[id]
	t.Status = status
	m.threads[id] = t
	return nil
}

func (m *memCRMStore) UpdateThreadAssignee(_ context.Context, id, assigneeID uuid.UUID, source string) error {
	t := m.threads[id]
	assignee := assigneeID
	t.AssigneeID = &assignee
	t.AssignmentSource = source
	m.threads[id] = t
	return nil
}

func (m *memCRMStore) AssignUnassignedThreadsByAccount(_ context.Context, accountID string, assigneeID uuid.UUID, source string) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for id, t := range m.threads {
		if t.AccountID != accountID || t.AssigneeID != nil {
			continue
		}
		if t.Status != sqlc.ThreadStatusNew && t.Status != sqlc.ThreadStatusInProgress {
			continue
		}
		assignee := assigneeID
		t.AssigneeID = &assignee
		t.AssignmentSource = source
		m.threads[id] = t
		ids = append(ids, id)
	}
	return ids, nil
}

func (m *memCRMStore) ListEmailsByThread(_ context.Context, threadID uuid.UUID) ([]sqlc.Email, error) {
	var out []sqlc.Email
	for _, em := range m.emails {
		if em.ThreadID == threadID {
			out = append(out, em)
		}
	}
	return out, nil
}

func (m *memCRMStore) EnqueueIMAPJob(_ context.Context, job sqlc.IMAPJob) (sqlc.IMAPJob, error) {
	job.ID = uuid.New()
	job.Status = "pending"
	m.jobs = append(m.jobs, job)
	return job, nil
}

func (m *memCRMStore) ClaimNextIMAPJob(_ context.Context) (sqlc.IMAPJob, error) {
	for i := range m.jobs {
		if m.jobs[i].Status == "pending" {
			m.jobs[i].Status = "processing"
			m.jobs[i].Attempts++
			return m.jobs[i], nil
		}
	}
	return sqlc.IMAPJob{}, pgx.ErrNoRows
}

func (m *memCRMStore) RestoreIMAPEmail(_ context.Context, accountID string, id uuid.UUID) (sqlc.Email, error) {
	em, ok := m.emails[id]
	if !ok || em.AccountID != accountID {
		return sqlc.Email{}, pgx.ErrNoRows
	}
	return em, nil
}

func (m *memCRMStore) CompleteIMAPJob(_ context.Context, jobID, emailID uuid.UUID, newMailbox string) error {
	for i := range m.jobs {
		if m.jobs[i].ID == jobID {
			m.jobs[i].Status = "completed"
		}
	}
	if em, ok := m.emails[emailID]; ok {
		em.IMAPMailbox = &newMailbox
		em.IMAPUID = nil
		m.emails[emailID] = em
	}
	return nil
}

func (m *memCRMStore) FailIMAPJob(_ context.Context, jobID uuid.UUID, _, _ int, errMsg string, _ time.Time) error {
	for i := range m.jobs {
		if m.jobs[i].ID == jobID {
			m.jobs[i].Status = "failed"
			m.jobs[i].LastError = &errMsg
		}
	}
	return nil
}

func (m *memCRMStore) ReparentThreadEmailsAndEvents(_ context.Context, sourceThreadID, targetThreadID uuid.UUID) error {
	for id, em := range m.emails {
		if em.ThreadID == sourceThreadID {
			em.ThreadID = targetThreadID
			m.emails[id] = em
		}
	}
	return nil
}

func (m *memCRMStore) DeleteThread(_ context.Context, id uuid.UUID) error {
	delete(m.threads, id)
	return nil
}

func (m *memCRMStore) InsertAuditLog(_ context.Context, entry sqlc.AuditLog) error {
	m.auditLogs = append(m.auditLogs, entry)
	return nil
}

func (m *memCRMStore) InsertThreadEvent(_ context.Context, ev sqlc.ThreadEvent) error {
	m.threadEvents = append(m.threadEvents, ev)
	return nil
}

type wfStoreAdapter struct {
	*memCRMStore
}

func (a wfStoreAdapter) TouchThreadLastMessageAt(ctx context.Context, id uuid.UUID, src sqlc.Thread) error {
	return a.memCRMStore.TouchThreadLastMessageAt(ctx, id, src.LastMessageAt)
}

func TestRestoreFromJunkUsesUniqueMessageID(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		srv := newFakeIMAPServer()
		store := newMemCRMStore()
		id := uuid.New()
		junk := "Junk"
		store.emails[id] = sqlc.Email{ID: id, AccountID: "sales", MessageID: "recover@example.com", IMAPMailbox: &junk}
		raw := []byte("Message-ID: <recover@example.com>\r\nFrom: a@example.com\r\n\r\nbody")
		srv.mailboxes["Junk"][91] = raw
		if ambiguous {
			srv.mailboxes["Junk"][92] = raw
		}
		_, _ = store.EnqueueIMAPJob(context.Background(), sqlc.IMAPJob{AccountID: "sales", JobType: "restore_from_junk", EmailID: id, SourceMailbox: &junk, TargetMailbox: "INBOX", MaxAttempts: 1})
		worker := mail.NewIMAPActionWorker(store, nil, func(_ context.Context, account string) (mail.IMAPClient, error) {
			if account != "sales" {
				t.Fatalf("wrong account %s", account)
			}
			return srv, nil
		}, nil, nil)
		processed, err := worker.ProcessPending(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ambiguous && (processed != 0 || store.jobs[0].Status != "failed" || len(srv.mailboxes["Junk"]) != 2) {
			t.Fatalf("ambiguous recovery moved message: %+v", store.jobs)
		}
		if !ambiguous && (processed != 1 || store.jobs[0].Status != "completed" || len(srv.mailboxes["INBOX"]) != 1) {
			t.Fatalf("recovery failed: %+v", store.jobs)
		}
	}
}

func TestRecoveryUsesCurrentFolderAndReconcilesMovedMessages(t *testing.T) {
	for _, tc := range []struct {
		name, job, source, stored string
		destination, ambiguous    bool
	}{
		{"changed junk folder", "restore_from_junk", "OldJunk", "OldJunk", false, false},
		{"recovery lost response", "restore_from_junk", "OldJunk", "OldJunk", true, false},
		{"uidless move lost response", "move_to_junk", "INBOX", "INBOX", true, false},
		{"ambiguous destination", "restore_from_junk", "OldJunk", "OldJunk", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, store := newFakeIMAPServer(), newMemCRMStore()
			srv.mailboxes[tc.source] = make(map[int64][]byte)
			id, configured, target := uuid.New(), "NewJunk", "INBOX"
			if tc.job == "move_to_junk" {
				target = "Junk"
			}
			store.emails[id] = sqlc.Email{ID: id, AccountID: "support", MessageID: "retry@example.com", IMAPMailbox: &tc.stored}
			raw := []byte("Message-ID: <retry@example.com>\r\n\r\nbody")
			location := tc.source
			if tc.destination {
				location = target
			}
			srv.mailboxes[location][1] = raw
			if tc.ambiguous {
				srv.mailboxes[location][2] = raw
			}
			_, _ = store.EnqueueIMAPJob(context.Background(), sqlc.IMAPJob{AccountID: "support", JobType: tc.job, EmailID: id, SourceMailbox: &configured, TargetMailbox: target, MaxAttempts: 1})
			worker := mail.NewIMAPActionWorker(store, nil, func(context.Context, string) (mail.IMAPClient, error) { return srv, nil }, nil, nil)
			processed, err := worker.ProcessPending(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tc.ambiguous {
				if processed != 0 || store.jobs[0].Status != "failed" {
					t.Fatalf("ambiguous match accepted: %+v", store.jobs)
				}
				return
			}
			if processed != 1 || store.jobs[0].Status != "completed" || len(srv.mailboxes[target]) != 1 {
				t.Fatalf("not reconciled: %+v", store.jobs)
			}
		})
	}
}

func TestRecoveryFollowedBySpamMovesUsingCurrentUID(t *testing.T) {
	srv, store := newFakeIMAPServer(), newMemCRMStore()
	id, junk := uuid.New(), "Junk"
	store.emails[id] = sqlc.Email{ID: id, AccountID: "support", MessageID: "sequence@example.com", IMAPMailbox: &junk}
	srv.mailboxes["Junk"][91] = []byte("Message-ID: <sequence@example.com>\r\n\r\nbody")
	_, _ = store.EnqueueIMAPJob(context.Background(), sqlc.IMAPJob{AccountID: "support", JobType: "restore_from_junk", EmailID: id, SourceMailbox: &junk, TargetMailbox: "INBOX"})
	_, _ = store.EnqueueIMAPJob(context.Background(), sqlc.IMAPJob{AccountID: "support", JobType: "move_to_junk", EmailID: id, SourceMailbox: &junk, TargetMailbox: "Junk"})
	worker := mail.NewIMAPActionWorker(store, nil, func(context.Context, string) (mail.IMAPClient, error) { return srv, nil }, nil, nil)
	processed, err := worker.ProcessPending(context.Background())
	if err != nil || processed != 2 || len(srv.mailboxes["Junk"]) != 1 || len(srv.mailboxes["INBOX"]) != 0 {
		t.Fatalf("ordered moves failed: %d %v %+v", processed, err, store.jobs)
	}
}

func TestIMAPSyncArchiveDeleteSafetyAndReopen(t *testing.T) {
	imapSrv := newFakeIMAPServer()
	imapSrv.mailboxes["INBOX"][10] = []byte(
		"From: Customer <cust@client.com>\r\n" +
			"To: Support <support@company.com>\r\n" +
			"Subject: Contract Question\r\n" +
			"Message-ID: <m10@client.com>\r\n" +
			"Date: Sun, 12 Apr 2026 10:00:00 +0000\r\n\r\n" +
			"Hello Support, can we renew?\r\n",
	)

	store := newMemCRMStore()
	attStore, err := storage.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	auditLogger := audit.NewLogger(store, nil)
	assigner := assignment.NewEngine(0.5, assignment.NewDefaultAssigneeResolver(store))
	matcher := threading.NewMatcher(store, assigner, auditLogger)

	syncDialer := func(_ context.Context) (mail.IMAPClient, error) { return imapSrv, nil }
	accountDialer := func(_ context.Context, _ string) (mail.IMAPClient, error) { return imapSrv, nil }
	syncWorker := mail.NewIMAPSyncWorker("support", store, matcher, attStore, syncDialer, time.Minute, nil)
	actionWorker := mail.NewIMAPActionWorker(store, attStore, accountDialer, nil, nil)
	wfService := workflow.NewService(wfStoreAdapter{store}, auditLogger, nil)

	ctx := context.Background()

	// 1. Sync INBOX -> ingests UID 10
	n, err := syncWorker.SyncOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("SyncOnce: n=%d err=%v", n, err)
	}
	var threadID uuid.UUID
	for id := range store.threads {
		threadID = id
	}

	// 2. Transition new -> in_progress -> completed
	if err := wfService.Transition(ctx, threadID, sqlc.ThreadStatusInProgress, nil); err != nil {
		t.Fatalf("new -> in_progress: %v", err)
	}
	if err := wfService.Transition(ctx, threadID, sqlc.ThreadStatusCompleted, nil); err != nil {
		t.Fatalf("in_progress -> completed: %v", err)
	}

	// 3. Run IMAPActionWorker -> moves UID 10 from INBOX to Archive
	movedCount, err := actionWorker.ProcessPending(ctx)
	if err != nil || movedCount != 1 {
		t.Fatalf("ProcessPending: moved=%d err=%v", movedCount, err)
	}
	if _, inInbox := imapSrv.mailboxes["INBOX"][10]; inInbox {
		t.Fatal("expected UID 10 to be moved out of INBOX")
	}
	if _, inArchive := imapSrv.mailboxes["Archive"][10]; !inArchive {
		t.Fatal("expected UID 10 to be present in Archive")
	}

	// 4. Delete the email from the IMAP Archive folder externally -> must be safe and not break MiniCRM
	delete(imapSrv.mailboxes["Archive"], 10)

	// 5. Customer replies to the completed thread -> creates a follow-up thread linked via reopens_thread_id,
	// and ReopenAndMerge still works seamlessly even though the original email was deleted from IMAP Archive!
	imapSrv.mailboxes["INBOX"][11] = []byte(
		"From: Customer <cust@client.com>\r\n" +
			"To: Support <support@company.com>\r\n" +
			"Subject: Re: Contract Question\r\n" +
			"Message-ID: <m11@client.com>\r\n" +
			"In-Reply-To: <m10@client.com>\r\n" +
			"Date: Sun, 12 Apr 2026 11:00:00 +0000\r\n\r\n" +
			"Follow-up after completion.\r\n",
	)
	n2, err := syncWorker.SyncOnce(ctx)
	if err != nil || n2 != 1 {
		t.Fatalf("SyncOnce 2: n=%d err=%v", n2, err)
	}

	var followUpID uuid.UUID
	for id, th := range store.threads {
		if id != threadID && th.ReopensThreadID != nil && *th.ReopensThreadID == threadID {
			followUpID = id
		}
	}
	if followUpID == uuid.Nil {
		t.Fatalf("expected follow-up thread with ReopensThreadID=%s", threadID)
	}

	reopenedID, err := wfService.ReopenAndMerge(ctx, followUpID, nil)
	if err != nil || reopenedID != threadID {
		t.Fatalf("ReopenAndMerge after Archive deletion failed: id=%s err=%v", reopenedID, err)
	}
	if store.threads[threadID].Status != sqlc.ThreadStatusInProgress {
		t.Fatalf("expected reopened thread to be in_progress, got %s", store.threads[threadID].Status)
	}
	emails, _ := store.ListEmailsByThread(ctx, threadID)
	if len(emails) != 2 {
		t.Fatalf("expected both original and follow-up emails preserved in DB, got %d", len(emails))
	}
}

func TestIMAPSyncSameEmailTwoAccountsCreatesTwoThreadsAndOneBlob(t *testing.T) {
	imapSrv := newFakeIMAPServer()
	// A simple multipart message with an attachment delivered to both accounts.
	const msgText = "From: Customer <cust@client.com>\r\n" +
		"To: Support <support@company.com>\r\n" +
		"Subject: Shared Attachment\r\n" +
		"Message-ID: <shared-1@client.com>\r\n" +
		"Date: Sun, 12 Apr 2026 10:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"See attached.\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; name=notes.txt\r\n" +
		"Content-Disposition: attachment; filename=notes.txt\r\n\r\n" +
		"shared attachment payload\r\n" +
		"--BOUND--\r\n"
	imapSrv.mailboxes["INBOX"][20] = []byte(msgText)

	store := newMemCRMStore()
	baseDir := t.TempDir()
	attStore, err := storage.NewStore(baseDir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	auditLogger := audit.NewLogger(store, nil)
	matcher := threading.NewMatcher(store, assignment.NewEngine(0.5, assignment.NewDefaultAssigneeResolver(store)), auditLogger)
	dialer := func(_ context.Context) (mail.IMAPClient, error) { return imapSrv, nil }

	supportWorker := mail.NewIMAPSyncWorker("support", store, matcher, attStore, dialer, time.Minute, nil)
	salesWorker := mail.NewIMAPSyncWorker("sales", store, matcher, attStore, dialer, time.Minute, nil)

	ctx := context.Background()
	if n, err := supportWorker.SyncOnce(ctx); err != nil || n != 1 {
		t.Fatalf("support SyncOnce: n=%d err=%v", n, err)
	}
	if n, err := salesWorker.SyncOnce(ctx); err != nil || n != 1 {
		t.Fatalf("sales SyncOnce: n=%d err=%v", n, err)
	}

	// Two independent threads, one per account.
	if len(store.threads) != 2 {
		t.Fatalf("expected 2 threads (one per account), got %d", len(store.threads))
	}
	accounts := map[string]bool{}
	for _, th := range store.threads {
		accounts[th.AccountID] = true
	}
	if !accounts["support"] || !accounts["sales"] {
		t.Fatalf("expected one thread per account, got %v", accounts)
	}

	// Each account's email has its own attachment row, but only one blob exists on disk.
	if len(store.attachments) != 2 {
		t.Fatalf("expected 2 attachment rows, got %d", len(store.attachments))
	}
	paths := map[string]bool{}
	for _, a := range store.attachments {
		paths[a.StorageRelPath] = true
	}
	if len(paths) != 1 {
		t.Fatalf("expected a single content-addressed blob path, got %v", paths)
	}

	fileCount := 0
	err = filepath.WalkDir(baseDir, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			fileCount++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	if fileCount != 1 {
		t.Fatalf("expected exactly 1 attachment blob on disk, found %d", fileCount)
	}
}

func TestDialIMAPErrorIdentifiesServerAndUser(t *testing.T) {
	_, err := mail.DialIMAP("127.0.0.1", 1, "agent@example.com", "secret", true)
	if err == nil {
		t.Fatal("expected dial error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "127.0.0.1:1") || !strings.Contains(msg, "agent@example.com") {
		t.Fatalf("error must name server and username: %q", msg)
	}
	if strings.Contains(msg, "secret") {
		t.Fatalf("error must not leak the password: %q", msg)
	}
}

func syncTestRig(t *testing.T, imapSrv *fakeIMAPServer) (*mail.IMAPSyncWorker, *memCRMStore) {
	t.Helper()
	store := newMemCRMStore()
	attStore, err := storage.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	auditLogger := audit.NewLogger(store, nil)
	matcher := threading.NewMatcher(store, assignment.NewEngine(0.5, assignment.NewDefaultAssigneeResolver(store)), auditLogger)
	dialer := func(_ context.Context) (mail.IMAPClient, error) { return imapSrv, nil }
	worker := mail.NewIMAPSyncWorker("support", store, matcher, attStore, dialer, time.Minute, nil).
		WithTransitioner(workflow.NewService(wfStoreAdapter{store}, auditLogger, nil)).
		WithOwnAddresses("support@company.com")
	return worker, store
}

func plainMail(n int) []byte {
	return []byte(fmt.Sprintf("From: C <c%d@client.com>\r\nTo: s@company.com\r\nSubject: Subject %d\r\n"+
		"Message-ID: <m%d@client.com>\r\nDate: Sun, 12 Apr 2026 10:00:00 +0000\r\n\r\nBody %d\r\n", n, n, n, n))
}

func TestIMAPSyncInitialCutoffBatchesAndBackfill(t *testing.T) {
	imapSrv := newFakeIMAPServer()
	for i := 1; i <= 45; i++ {
		imapSrv.mailboxes["INBOX"][int64(i)] = plainMail(i)
	}
	worker, store := syncTestRig(t, imapSrv)
	ctx := context.Background()

	n, err := worker.SyncOnce(ctx)
	if err != nil || n != 45 {
		t.Fatalf("SyncOnce: n=%d err=%v", n, err)
	}
	// New account: only the last 60 days are requested by default.
	if imapSrv.lastSince.IsZero() || time.Since(imapSrv.lastSince) < 59*24*time.Hour || time.Since(imapSrv.lastSince) > 61*24*time.Hour {
		t.Fatalf("expected a ~60 day cutoff on the first sync, got %v", imapSrv.lastSince)
	}
	// Messages are downloaded in bounded batches rather than all at once.
	if len(imapSrv.batches) < 3 {
		t.Fatalf("expected the download to be batched, got %d batch(es)", len(imapSrv.batches))
	}
	for _, b := range imapSrv.batches {
		if len(b) > 20 {
			t.Fatalf("batch too large: %d", len(b))
		}
	}
	if st := store.syncStates["support/INBOX"]; st.LastSyncedUID != 45 {
		t.Fatalf("expected watermark 45, got %+v (%v)", st, store.syncStates)
	}

	// Subsequent syncs have a watermark and no date cutoff.
	imapSrv.mailboxes["INBOX"][46] = plainMail(46)
	if n, err := worker.SyncOnce(ctx); err != nil || n != 1 {
		t.Fatalf("SyncOnce 2: n=%d err=%v", n, err)
	}
	if !imapSrv.lastSince.IsZero() {
		t.Fatalf("expected no cutoff once a watermark exists, got %v", imapSrv.lastSince)
	}

	// initialSyncDays=0 backfills the complete history (no cutoff).
	full := newFakeIMAPServer()
	full.mailboxes["INBOX"][1] = plainMail(1)
	w2, _ := syncTestRig(t, full)
	if _, err := w2.WithInitialSyncDays(0).SyncOnce(ctx); err != nil {
		t.Fatalf("backfill SyncOnce: %v", err)
	}
	if !full.lastSince.IsZero() {
		t.Fatalf("backfill must not apply a cutoff, got %v", full.lastSince)
	}
}

func TestIMAPSyncSkipsUnparseableMessageAndContinues(t *testing.T) {
	imapSrv := newFakeIMAPServer()
	imapSrv.mailboxes["INBOX"][1] = plainMail(1)
	imapSrv.mailboxes["INBOX"][2] = []byte("From: a@b.c\r\nSubject: broken\r\nMessage-ID: <broken@x>\r\n" +
		"Content-Type: multipart/mixed; boundary=zz\r\n\r\n--zz\r\nContent-Type: text/plain\r\n\r\ntruncated")
	imapSrv.mailboxes["INBOX"][3] = plainMail(3)
	worker, store := syncTestRig(t, imapSrv)

	n, err := worker.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("a bad message must not fail the sync: %v", err)
	}
	if n != 2 && n != 3 {
		t.Fatalf("unexpected ingested count %d", n)
	}
	if got := len(store.emails); got != n {
		t.Fatalf("emails stored = %d, want %d", got, n)
	}
	if st := store.syncStates["support/INBOX"]; st.LastSyncedUID != 3 {
		t.Fatalf("watermark must advance past the bad message, got %+v", st)
	}
	// The next poll has nothing left to do.
	if n, err := worker.SyncOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("second sync: n=%d err=%v", n, err)
	}
}

func customerMail(id, subject, from, to, inReplyTo string) []byte {
	reply := ""
	if inReplyTo != "" {
		reply = "In-Reply-To: <" + inReplyTo + ">\r\n"
	}
	return []byte("From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject + "\r\nMessage-ID: <" + id + ">\r\n" +
		reply + "Date: Sun, 12 Apr 2026 10:00:00 +0000\r\n\r\nBody of " + id + "\r\n")
}

func emailsOf(store *memCRMStore, threadID uuid.UUID) (inbound, outbound int) {
	for _, e := range store.emails {
		if e.ThreadID != threadID {
			continue
		}
		if e.Direction == "outbound" {
			outbound++
		} else {
			inbound++
		}
	}
	return inbound, outbound
}

func onlyThread(t *testing.T, store *memCRMStore) sqlc.Thread {
	t.Helper()
	if len(store.threads) != 1 {
		t.Fatalf("expected exactly 1 thread, got %d", len(store.threads))
	}
	for _, th := range store.threads {
		return th
	}
	return sqlc.Thread{}
}

func hasThreadEvent(store *memCRMStore, threadID uuid.UUID, eventType string) bool {
	for _, ev := range store.threadEvents {
		if ev.ThreadID == threadID && ev.EventType == eventType {
			return true
		}
	}
	return false
}

func TestSentSyncExternalReplyAttachesAndStartsWork(t *testing.T) {
	imapSrv := newFakeIMAPServer()
	imapSrv.mailboxes["INBOX"][1] = customerMail("c1@client.com", "Help me", "Cust <cust@client.com>", "support@company.com", "")
	imapSrv.mailboxes["Sent"][1] = customerMail("out1@company.com", "Re: Help me", "Support <support@company.com>", "cust@client.com", "c1@client.com")
	worker, store := syncTestRig(t, imapSrv)
	ctx := context.Background()

	if _, err := worker.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	th := onlyThread(t, store)
	if in, out := emailsOf(store, th.ID); in != 1 || out != 1 {
		t.Fatalf("expected 1 inbound + 1 outbound email, got %d/%d", in, out)
	}
	if th.Status != sqlc.ThreadStatusInProgress {
		t.Fatalf("external reply must move a new thread to in_progress, got %s", th.Status)
	}
	if th.AssigneeID != nil {
		t.Fatalf("external reply must not assign the thread, got %v", th.AssigneeID)
	}
	if !hasThreadEvent(store, th.ID, "reply_sent_external") || !hasThreadEvent(store, th.ID, "status_changed") {
		t.Fatalf("expected reply_sent_external and status_changed events, got %+v", store.threadEvents)
	}
	if !th.LastMessageAt.After(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("last_message_at must reflect the reply, got %v", th.LastMessageAt)
	}
	for _, e := range store.emails {
		if e.Direction == "outbound" && (e.IMAPMailbox == nil || *e.IMAPMailbox != "Sent") {
			t.Fatalf("outbound email must record its Sent location: %+v", e)
		}
	}

	// Syncing again neither duplicates the reply nor re-sends events.
	if _, err := worker.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce 2: %v", err)
	}
	if in, out := emailsOf(store, th.ID); in != 1 || out != 1 {
		t.Fatalf("second sync duplicated mail: %d/%d", in, out)
	}
}

func TestSentSyncDoesNotDuplicateMiniCRMReplyAndBackfillsLocation(t *testing.T) {
	imapSrv := newFakeIMAPServer()
	imapSrv.mailboxes["INBOX"][1] = customerMail("c1@client.com", "Help me", "Cust <cust@client.com>", "support@company.com", "")
	worker, store := syncTestRig(t, imapSrv)
	ctx := context.Background()
	if _, err := worker.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	th := onlyThread(t, store)

	// A reply MiniCRM sent itself: stored at send time without an IMAP location.
	ownID := uuid.New()
	store.emails[ownID] = sqlc.Email{
		ID: ownID, ThreadID: th.ID, AccountID: "support", Direction: "outbound",
		MessageID: "own@minicrm.local", FromAddress: "support@company.com", SentAt: time.Now().UTC(),
	}
	imapSrv.mailboxes["Sent"][7] = customerMail("own@minicrm.local", "Re: Help me", "Support <support@company.com>", "cust@client.com", "c1@client.com")

	if _, err := worker.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce 2: %v", err)
	}
	if _, out := emailsOf(store, th.ID); out != 1 {
		t.Fatalf("MiniCRM's own reply was imported again: %d outbound emails", out)
	}
	got := store.emails[ownID]
	if got.IMAPUID == nil || *got.IMAPUID != 7 || got.IMAPMailbox == nil || *got.IMAPMailbox != "Sent" {
		t.Fatalf("expected IMAP location backfilled, got %+v", got)
	}
	if hasThreadEvent(store, th.ID, "reply_sent_external") {
		t.Fatal("MiniCRM's own reply must not be recorded as an external reply")
	}
}

func TestSentSyncStatusRules(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     sqlc.ThreadStatus
		wantStatus sqlc.ThreadStatus
		wantOut    int
	}{
		{"completed stays completed, reply attached silently", sqlc.ThreadStatusCompleted, sqlc.ThreadStatusCompleted, 1},
		{"in_progress stays in_progress", sqlc.ThreadStatusInProgress, sqlc.ThreadStatusInProgress, 1},
		{"spam is ignored", sqlc.ThreadStatusSpam, sqlc.ThreadStatusSpam, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			imapSrv := newFakeIMAPServer()
			imapSrv.mailboxes["INBOX"][1] = customerMail("c1@client.com", "Help me", "Cust <cust@client.com>", "support@company.com", "")
			worker, store := syncTestRig(t, imapSrv)
			ctx := context.Background()
			if _, err := worker.SyncOnce(ctx); err != nil {
				t.Fatalf("SyncOnce: %v", err)
			}
			th := onlyThread(t, store)
			th.Status = tc.status
			store.threads[th.ID] = th

			imapSrv.mailboxes["Sent"][1] = customerMail("out1@company.com", "Re: Help me", "Support <support@company.com>", "cust@client.com", "c1@client.com")
			if _, err := worker.SyncOnce(ctx); err != nil {
				t.Fatalf("SyncOnce 2: %v", err)
			}
			if got := store.threads[th.ID]; got.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s", got.Status, tc.wantStatus)
			}
			if _, out := emailsOf(store, th.ID); out != tc.wantOut {
				t.Fatalf("outbound emails = %d, want %d", out, tc.wantOut)
			}
			if got := len(store.threads); got != 1 {
				t.Fatalf("Sent must never create threads, got %d", got)
			}
		})
	}
}

func TestSentSyncIgnoresUnrelatedOutboundMail(t *testing.T) {
	imapSrv := newFakeIMAPServer()
	imapSrv.mailboxes["Sent"][1] = customerMail("fresh@company.com", "Hello there", "Support <support@company.com>", "new@client.com", "")
	worker, store := syncTestRig(t, imapSrv)
	if _, err := worker.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if len(store.threads) != 0 || len(store.emails) != 0 {
		t.Fatalf("unrelated outbound mail must be ignored: %d threads, %d emails", len(store.threads), len(store.emails))
	}
}

func TestSentSyncFailureOrMissingFolderDoesNotBlockInbox(t *testing.T) {
	imapSrv := newFakeIMAPServer()
	delete(imapSrv.mailboxes, "Sent")
	imapSrv.mailboxes["INBOX"][1] = customerMail("c1@client.com", "Help me", "Cust <cust@client.com>", "support@company.com", "")
	worker, store := syncTestRig(t, imapSrv)
	if n, err := worker.SyncOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("INBOX must sync without a Sent folder: n=%d err=%v", n, err)
	}
	if len(store.threads) != 1 {
		t.Fatalf("expected the inbox thread, got %d", len(store.threads))
	}
}

func TestCustomerReplyToExternallySentMailImportsOriginal(t *testing.T) {
	imapSrv := newFakeIMAPServer()
	// Sent mail nobody has replied to yet is ignored ...
	imapSrv.mailboxes["Sent"][1] = customerMail("out1@company.com", "Offer", "Support <support@company.com>", "Gina <gina@client.com>", "")
	worker, store := syncTestRig(t, imapSrv)
	ctx := context.Background()
	if _, err := worker.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if len(store.threads) != 0 {
		t.Fatalf("outbound-only mail must not create a thread, got %d", len(store.threads))
	}

	// ... until the customer replies: then the original outbound mail is pulled in too.
	imapSrv.mailboxes["INBOX"][1] = customerMail("g1@client.com", "Re: Offer", "Gina <gina@client.com>", "support@company.com", "out1@company.com")
	if _, err := worker.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce 2: %v", err)
	}
	th := onlyThread(t, store)
	if in, out := emailsOf(store, th.ID); in != 1 || out != 1 {
		t.Fatalf("expected the outbound original and the customer reply, got in=%d out=%d", in, out)
	}
	if th.Status != sqlc.ThreadStatusNew || th.CustomerEmail != "gina@client.com" {
		t.Fatalf("unexpected thread: %+v", th)
	}
	var parentOfReply *uuid.UUID
	for _, e := range store.emails {
		if e.Direction == "inbound" {
			parentOfReply = e.ParentEmailID
		}
	}
	if parentOfReply == nil {
		t.Fatal("the customer's reply must reference the imported outbound email as its parent")
	}

	// A later Sent pass must not import the original a second time.
	if _, err := worker.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce 3: %v", err)
	}
	if _, out := emailsOf(store, th.ID); out != 1 {
		t.Fatalf("outbound original duplicated: %d", out)
	}
}
