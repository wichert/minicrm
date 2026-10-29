package web_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/mail"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/web"
	"github.com/wichert/minicrm/internal/web/static"
	"github.com/wichert/minicrm/internal/web/views"
	"github.com/wichert/minicrm/internal/workflow"
	gomail "github.com/wneessen/go-mail"
)

type noopSMTP struct{}

func (n *noopSMTP) Send(_ context.Context, _ *gomail.Msg) error { return nil }

type testWebStore struct {
	adminUser       sqlc.User
	users           map[uuid.UUID]sqlc.User
	settings        sqlc.SystemSettings
	threads         map[uuid.UUID]sqlc.Thread
	emails          map[uuid.UUID]sqlc.Email
	attachments     map[uuid.UUID]sqlc.Attachment
	threadEvents    []sqlc.ThreadEvent
	auditLogs       []sqlc.AuditLog
	jobs            []sqlc.IMAPJob
	accountSettings map[string]sqlc.AccountSettings
	drafts          map[uuid.UUID]sqlc.ReplyDraft
	// countThreadsErr, when set, makes CountThreadsByTab fail so tests can pin the
	// handler's degraded-but-working behaviour on tab-count failures.
	countThreadsErr error
	outbox          []sqlc.OutboxEmail
}

func newTestWebStore() *testWebStore {
	admin := sqlc.User{
		ID:              uuid.New(),
		Email:           "admin@example.com",
		DisplayName:     "Admin User",
		Role:            "admin",
		IsActive:        true,
		AllowedAccounts: []string{"support", "sales"},
	}
	return &testWebStore{
		adminUser: admin,
		users:     map[uuid.UUID]sqlc.User{admin.ID: admin},
		settings: sqlc.SystemSettings{
			ID:                     1,
			DisplayTimezone:        "Europe/Amsterdam",
			IMAPInboxMailbox:       "INBOX",
			IMAPArchiveMailbox:     "Archive",
			IMAPJunkMailbox:        "Junk",
			IMAPSentMailbox:        "Sent",
			SubjectMatchWindowDays: 30,
			AttentionAgeDays:       2,
		},
		threads:         make(map[uuid.UUID]sqlc.Thread),
		emails:          make(map[uuid.UUID]sqlc.Email),
		attachments:     make(map[uuid.UUID]sqlc.Attachment),
		accountSettings: make(map[string]sqlc.AccountSettings),
		drafts:          make(map[uuid.UUID]sqlc.ReplyDraft),
	}
}

func (s *testWebStore) InTransaction(_ context.Context, fn func(workflow.Store, audit.Store) error) error {
	clone := *s
	clone.threads = make(map[uuid.UUID]sqlc.Thread, len(s.threads))
	for id, thread := range s.threads {
		clone.threads[id] = thread
	}
	clone.threadEvents = append([]sqlc.ThreadEvent(nil), s.threadEvents...)
	clone.jobs = append([]sqlc.IMAPJob(nil), s.jobs...)
	if err := fn(&clone, &clone); err != nil {
		return err
	}
	s.threads, s.threadEvents, s.jobs = clone.threads, clone.threadEvents, clone.jobs
	return nil
}

func (s *testWebStore) GetReplyDraft(_ context.Context, id uuid.UUID) (sqlc.ReplyDraft, error) {
	d, ok := s.drafts[id]
	if !ok {
		return sqlc.ReplyDraft{}, pgx.ErrNoRows
	}
	return d, nil
}

func (s *testWebStore) SaveReplyDraft(_ context.Context, id, expected uuid.UUID, body, formatting, session string, discard bool, actor *uuid.UUID) (sqlc.ReplyDraft, error) {
	old := s.drafts[id]
	if old.Revision != expected {
		return sqlc.ReplyDraft{}, sqlc.ErrDraftConflict
	}
	thread := s.threads[id]
	if discard {
		body = ""
		formatting = ""
	}
	d := sqlc.ReplyDraft{ThreadID: id, AccountID: thread.AccountID, Revision: uuid.New(), Body: body, Formatting: formatting, SessionID: session, Discarded: discard, OccurredAt: time.Now().UTC()}
	s.drafts[id] = d
	if discard && old.Revision != uuid.Nil && !old.Discarded && old.Body != "" {
		s.threadEvents = append(s.threadEvents, sqlc.ThreadEvent{ThreadID: id, ActorUserID: actor, EventType: "draft_discarded"})
	}
	return d, nil
}

func (s *testWebStore) GetAccountSettings(_ context.Context, accountID string) (sqlc.AccountSettings, error) {
	a, ok := s.accountSettings[accountID]
	if !ok {
		return sqlc.AccountSettings{}, pgx.ErrNoRows
	}
	return a, nil
}

func (s *testWebStore) ListAccountSettings(_ context.Context) ([]sqlc.AccountSettings, error) {
	var out []sqlc.AccountSettings
	for _, a := range s.accountSettings {
		out = append(out, a)
	}
	return out, nil
}

func (s *testWebStore) UpsertAccountSettings(_ context.Context, a sqlc.AccountSettings) (sqlc.AccountSettings, error) {
	s.accountSettings[a.AccountID] = a
	return a, nil
}

func (s *testWebStore) AssignUnassignedThreadsByAccount(_ context.Context, accountID string, assigneeID uuid.UUID, source string) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for id, t := range s.threads {
		if t.AccountID != accountID || t.AssigneeID != nil {
			continue
		}
		if t.Status != sqlc.ThreadStatusNew && t.Status != sqlc.ThreadStatusInProgress {
			continue
		}
		assignee := assigneeID
		t.AssigneeID = &assignee
		t.AssignmentSource = source
		s.threads[id] = t
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *testWebStore) GetUserByID(_ context.Context, id uuid.UUID) (sqlc.User, error) {
	u, ok := s.users[id]
	if !ok {
		return sqlc.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (s *testWebStore) GetUserByOIDCSub(_ context.Context, _ string) (sqlc.User, error) {
	return sqlc.User{}, pgx.ErrNoRows
}

func (s *testWebStore) GetUserByEmail(_ context.Context, email string) (sqlc.User, error) {
	for _, u := range s.users {
		if strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (s *testWebStore) CreateUser(_ context.Context, arg sqlc.CreateUserParams) (sqlc.User, error) {
	u := sqlc.User{
		ID:              uuid.New(),
		OIDCSub:         arg.OIDCSub,
		Email:           arg.Email,
		DisplayName:     arg.DisplayName,
		Role:            arg.Role,
		IsActive:        arg.IsActive,
		AllowedAccounts: arg.AllowedAccounts,
	}
	s.users[u.ID] = u
	return u, nil
}

func (s *testWebStore) UpdateUserOIDC(_ context.Context, id uuid.UUID, oidcSub, displayName string, allowedAccounts []string) (sqlc.User, error) {
	u := s.users[id]
	if oidcSub != "" {
		u.OIDCSub = &oidcSub
	}
	u.DisplayName = displayName
	u.AllowedAccounts = allowedAccounts
	s.users[id] = u
	return u, nil
}

func (s *testWebStore) ListUsers(_ context.Context) ([]sqlc.User, error) {
	var out []sqlc.User
	for _, u := range s.users {
		out = append(out, u)
	}
	return out, nil
}

func (s *testWebStore) GetSystemSettings(_ context.Context) (sqlc.SystemSettings, error) {
	return s.settings, nil
}

func (s *testWebStore) UpsertSystemSettings(_ context.Context, cfg sqlc.SystemSettings) (sqlc.SystemSettings, error) {
	cfg.ID = 1
	s.settings = cfg
	return cfg, nil
}

func (s *testWebStore) threadOpenedAt(t sqlc.Thread) time.Time {
	if !t.OpenedAt.IsZero() {
		return t.OpenedAt
	}
	var earliest time.Time
	for _, em := range s.emails {
		if em.ThreadID == t.ID && !em.SentAt.IsZero() {
			if earliest.IsZero() || em.SentAt.Before(earliest) {
				earliest = em.SentAt
			}
		}
	}
	if !earliest.IsZero() {
		return earliest
	}
	return t.LastMessageAt
}

func (s *testWebStore) CountThreadsByTab(_ context.Context, f sqlc.CountThreadsFilter) (sqlc.ThreadCounts, error) {
	if s.countThreadsErr != nil {
		return sqlc.ThreadCounts{}, s.countThreadsErr
	}
	if len(f.AllowedAccounts) == 0 {
		return sqlc.ThreadCounts{}, nil
	}
	var out sqlc.ThreadCounts
	for _, t := range s.threads {
		if f.AccountID != nil && t.AccountID != *f.AccountID {
			continue
		}
		allowed := false
		for _, id := range f.AllowedAccounts {
			if id == t.AccountID {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		out.All++
		isOpen := t.Status == sqlc.ThreadStatusNew || t.Status == sqlc.ThreadStatusInProgress
		if isOpen && !s.threadOpenedAt(t).After(f.AttentionCutoff) {
			out.NeedsAttention++
		}
		if isOpen && t.AssigneeID != nil && *t.AssigneeID == f.UserID {
			out.Mine++
		}
		switch t.Status {
		case sqlc.ThreadStatusNew:
			out.New++
		case sqlc.ThreadStatusInProgress:
			out.InProgress++
		case sqlc.ThreadStatusCompleted:
			out.Completed++
		case sqlc.ThreadStatusSpam:
			out.Spam++
		}
	}
	return out, nil
}

func (s *testWebStore) ListThreads(_ context.Context, f sqlc.ListThreadsFilter) ([]sqlc.Thread, error) {
	var out []sqlc.Thread
	for _, t := range s.threads {
		if f.Status != nil && t.Status != *f.Status {
			continue
		}
		if f.OpenOnly && t.Status != sqlc.ThreadStatusNew && t.Status != sqlc.ThreadStatusInProgress {
			continue
		}
		if f.NeedsAttention {
			if t.Status != sqlc.ThreadStatusNew && t.Status != sqlc.ThreadStatusInProgress {
				continue
			}
			if s.threadOpenedAt(t).After(f.AttentionCutoff) {
				continue
			}
		}
		if f.AccountID != nil && t.AccountID != *f.AccountID {
			continue
		}
		if f.AssigneeID != nil && (t.AssigneeID == nil || *t.AssigneeID != *f.AssigneeID) {
			continue
		}
		if f.Unassigned && t.AssigneeID != nil {
			continue
		}
		allowed := false
		for _, id := range f.AllowedAccounts {
			if id == t.AccountID {
				allowed = true
				break
			}
		}
		if len(f.AllowedAccounts) > 0 && !allowed {
			continue
		}
		if f.CustomerEmail != "" && !strings.EqualFold(t.CustomerEmail, f.CustomerEmail) {
			continue
		}
		if f.ExcludeThreadID != nil && t.ID == *f.ExcludeThreadID {
			continue
		}
		if f.Search != "" && !strings.Contains(strings.ToLower(t.SubjectDisplay), strings.ToLower(f.Search)) &&
			!strings.Contains(strings.ToLower(t.CustomerEmail), strings.ToLower(f.Search)) &&
			!strings.Contains(strings.ToLower(t.CustomerName), strings.ToLower(f.Search)) {
			continue
		}
		if f.BeforeTime != nil && f.BeforeID != nil &&
			(t.LastMessageAt.After(*f.BeforeTime) || (t.LastMessageAt.Equal(*f.BeforeTime) && strings.Compare(t.ID.String(), f.BeforeID.String()) >= 0)) {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastMessageAt.Equal(out[j].LastMessageAt) {
			return strings.Compare(out[i].ID.String(), out[j].ID.String()) > 0
		}
		return out[i].LastMessageAt.After(out[j].LastMessageAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (s *testWebStore) ListCustomerHistory(ctx context.Context, accounts []string, email string, exclude uuid.UUID, limit int) ([]sqlc.Thread, error) {
	return s.ListThreads(ctx, sqlc.ListThreadsFilter{AllowedAccounts: accounts, CustomerEmail: email, ExcludeThreadID: &exclude, Limit: limit})
}

func (s *testWebStore) GetThreadByID(_ context.Context, id uuid.UUID) (sqlc.Thread, error) {
	t, ok := s.threads[id]
	if !ok {
		return sqlc.Thread{}, pgx.ErrNoRows
	}
	return t, nil
}

func (s *testWebStore) UpdateThreadStatus(_ context.Context, id uuid.UUID, status sqlc.ThreadStatus) error {
	t := s.threads[id]
	t.Status = status
	s.threads[id] = t
	return nil
}

func (s *testWebStore) UpdateThreadAssignee(_ context.Context, id, assigneeID uuid.UUID, source string) error {
	t := s.threads[id]
	assignee := assigneeID
	t.AssigneeID = &assignee
	t.AssignmentSource = source
	s.threads[id] = t
	return nil
}

func (s *testWebStore) TouchThreadLastMessageAt(_ context.Context, id uuid.UUID, src sqlc.Thread) error {
	t := s.threads[id]
	t.LastMessageAt = src.LastMessageAt
	s.threads[id] = t
	return nil
}

// outboxStoreAdapter adapts testWebStore to mail.OutboxStore, which expects a
// time.Time signature for TouchThreadLastMessageAt while workflow.Store takes
// a source thread.
type outboxStoreAdapter struct{ *testWebStore }

func (s outboxStoreAdapter) TouchThreadLastMessageAt(_ context.Context, id uuid.UUID, ts time.Time) error {
	t := s.threads[id]
	t.LastMessageAt = ts
	s.threads[id] = t
	return nil
}

func (s *testWebStore) ListEmailsByThread(_ context.Context, threadID uuid.UUID) ([]sqlc.Email, error) {
	var out []sqlc.Email
	for _, em := range s.emails {
		if em.ThreadID == threadID {
			out = append(out, em)
		}
	}
	return out, nil
}

func (s *testWebStore) GetEmailByID(_ context.Context, id uuid.UUID) (sqlc.Email, error) {
	em, ok := s.emails[id]
	if !ok {
		return sqlc.Email{}, pgx.ErrNoRows
	}
	return em, nil
}

func (s *testWebStore) CreateEmail(_ context.Context, e sqlc.Email) (sqlc.Email, error) {
	e.ID = uuid.New()
	s.emails[e.ID] = e
	return e, nil
}

func (s *testWebStore) ListAttachmentsByEmail(_ context.Context, emailID uuid.UUID) ([]sqlc.Attachment, error) {
	var out []sqlc.Attachment
	for _, a := range s.attachments {
		if a.EmailID == emailID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *testWebStore) GetAttachmentByID(_ context.Context, id uuid.UUID) (sqlc.Attachment, error) {
	a, ok := s.attachments[id]
	if !ok {
		return sqlc.Attachment{}, pgx.ErrNoRows
	}
	return a, nil
}

func (s *testWebStore) CreateAttachment(_ context.Context, a sqlc.Attachment) (sqlc.Attachment, error) {
	a.ID = uuid.New()
	s.attachments[a.ID] = a
	return a, nil
}

func (s *testWebStore) ListThreadEvents(_ context.Context, threadID uuid.UUID) ([]sqlc.ThreadEvent, error) {
	var out []sqlc.ThreadEvent
	for _, ev := range s.threadEvents {
		if ev.ThreadID == threadID {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (s *testWebStore) ListAuditLogs(_ context.Context, _ int) ([]sqlc.AuditLog, error) {
	return s.auditLogs, nil
}

func (s *testWebStore) EnqueueOutboxEmail(_ context.Context, emailID, senderUserID uuid.UUID) (sqlc.OutboxEmail, error) {
	item := sqlc.OutboxEmail{ID: uuid.New(), EmailID: emailID, SenderUserID: senderUserID, Status: "pending", EnqueuedAt: time.Now().UTC()}
	s.outbox = append(s.outbox, item)
	return item, nil
}

func (s *testWebStore) ClaimNextOutboxEmail(_ context.Context) (sqlc.OutboxEmail, error) {
	for i := range s.outbox {
		if s.outbox[i].Status == "pending" {
			s.outbox[i].Status = "sending"
			s.outbox[i].Attempts++
			return s.outbox[i], nil
		}
	}
	return sqlc.OutboxEmail{}, pgx.ErrNoRows
}

func (s *testWebStore) CompleteOutboxEmail(_ context.Context, id uuid.UUID) error {
	for i := range s.outbox {
		if s.outbox[i].ID == id {
			s.outbox[i].Status = "sent"
		}
	}
	return nil
}

func (s *testWebStore) FailOutboxEmail(_ context.Context, id uuid.UUID, attempts int, errMsg string, nextRun time.Time) error {
	for i := range s.outbox {
		if s.outbox[i].ID == id {
			s.outbox[i].Status = "failed"
			s.outbox[i].Attempts = attempts
			s.outbox[i].LastError = &errMsg
			s.outbox[i].NextRunAt = nextRun
		}
	}
	return nil
}

func (s *testWebStore) EnqueueIMAPJob(_ context.Context, job sqlc.IMAPJob) (sqlc.IMAPJob, error) {
	job.ID = uuid.New()
	s.jobs = append(s.jobs, job)
	return job, nil
}

func (s *testWebStore) ReparentThreadEmailsAndEvents(_ context.Context, sourceThreadID, targetThreadID uuid.UUID) error {
	for id, em := range s.emails {
		if em.ThreadID == sourceThreadID {
			em.ThreadID = targetThreadID
			s.emails[id] = em
		}
	}
	return nil
}

func (s *testWebStore) DeleteThread(_ context.Context, id uuid.UUID) error {
	delete(s.threads, id)
	return nil
}

func (s *testWebStore) InsertAuditLog(_ context.Context, entry sqlc.AuditLog) error {
	s.auditLogs = append(s.auditLogs, entry)
	return nil
}

func (s *testWebStore) InsertThreadEvent(_ context.Context, ev sqlc.ThreadEvent) error {
	s.threadEvents = append(s.threadEvents, ev)
	return nil
}

func TestFormatTimestampDisplayTimezone(t *testing.T) {
	utcTime := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	got := views.FormatTimestamp(utcTime, "Europe/Amsterdam")
	// Amsterdam in January is CET (UTC+1); visible timestamps omit seconds and timezone suffix.
	if got != "2026-01-15 13:00" {
		t.Fatalf("got %q, expected 2026-01-15 13:00", got)
	}
	if full := views.FormatTimestampFull(utcTime, "Europe/Amsterdam"); full != "2026-01-15 13:00:00 CET" {
		t.Fatalf("got %q, expected full timestamp with zone", full)
	}
}

func TestSecurityHeadersCSPUpgradeOnlyForHTTPS(t *testing.T) {
	handler := web.SecurityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name        string
		setup       func(*http.Request)
		wantCSP     string
		wantUpgrade bool
	}{
		{
			name:        "plain HTTP",
			wantCSP:     web.MainCSPInsecure,
			wantUpgrade: false,
		},
		{
			name: "direct TLS",
			setup: func(r *http.Request) {
				r.TLS = &tls.ConnectionState{}
			},
			wantCSP:     web.MainCSP,
			wantUpgrade: true,
		},
		{
			name: "TLS-terminating proxy",
			setup: func(r *http.Request) {
				r.Header.Set("X-Forwarded-Proto", "https")
			},
			wantCSP:     web.MainCSP,
			wantUpgrade: true,
		},
		{
			name: "proxy proto list",
			setup: func(r *http.Request) {
				r.Header.Set("X-Forwarded-Proto", "https, http")
			},
			wantCSP:     web.MainCSP,
			wantUpgrade: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/threads", nil)
			if tt.setup != nil {
				tt.setup(req)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			got := rec.Header().Get("Content-Security-Policy")
			if got != tt.wantCSP {
				t.Fatalf("unexpected CSP: %s", got)
			}
			if upgrade := strings.Contains(got, "upgrade-insecure-requests"); upgrade != tt.wantUpgrade {
				t.Fatalf("upgrade-insecure-requests = %v, want %v (CSP: %s)", upgrade, tt.wantUpgrade, got)
			}
		})
	}
}

func TestSecurityHeadersSandboxedIframeAndReopenBanner(t *testing.T) {
	var logBuf bytes.Buffer
	slogLogger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	store := newTestWebStore()
	auditLogger := audit.NewLogger(store, slogLogger)
	attStore, err := storage.NewStore(t.TempDir(), slogLogger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	wfSvc := workflow.NewService(store, auditLogger, nil)
	outbox := mail.NewOutboxWorker(nil, attStore, []mail.AccountSenderConfig{
		{AccountID: "support", FromAddress: "support@example.com", FromName: "Support", Transport: &noopSMTP{}},
	}, auditLogger, "example.com", nil, slogLogger)
	sm := auth.NewSessionManager(false)
	authSvc := auth.NewService(store, auditLogger, sm, nil, "admin@example.com")
	srv := web.NewServer(store, authSvc, wfSvc, outbox, attStore, auditLogger, sm, testAccounts())

	completedID := uuid.New()
	followUpID := uuid.New()
	emailID := uuid.New()
	adminID := store.adminUser.ID
	store.threads[completedID] = sqlc.Thread{
		ID:             completedID,
		AccountID:      "support",
		SubjectDisplay: "Previous Ticket #1",
		Status:         sqlc.ThreadStatusCompleted,
		AssigneeID:     &adminID,
		CustomerEmail:  "client@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}
	store.threads[followUpID] = sqlc.Thread{
		ID:              followUpID,
		AccountID:       "support",
		SubjectDisplay:  "Re: Previous Ticket #1",
		Status:          sqlc.ThreadStatusNew,
		AssigneeID:      &adminID,
		CustomerEmail:   "client@example.com",
		ReopensThreadID: &completedID,
		LastMessageAt:   time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
	}
	store.emails[emailID] = sqlc.Email{
		ID:                emailID,
		ThreadID:          followUpID,
		AccountID:         "support",
		Direction:         "inbound",
		FromAddress:       "client@example.com",
		Subject:           "Re: Previous Ticket #1",
		BodyHTMLSanitized: "<p>Safe sanitized paragraph</p>",
		SentAt:            time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
	}

	// Build router where user is authenticated in context
	router := web.SecurityHeadersMiddleware(sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithUser(r.Context(), store.adminUser))
		switch {
		case r.URL.Path == "/threads/"+followUpID.String():
			rctx := web.WithChiParam(r, "id", followUpID.String())
			srv.HandleViewThread(w, rctx)
		case r.URL.Path == "/emails/"+emailID.String()+"/body":
			rctx := web.WithChiParam(r, "id", emailID.String())
			srv.HandleIsolatedEmailBody(w, rctx)
		case r.URL.Path == "/settings" && r.Method == http.MethodPost:
			srv.HandleUpdateSettings(w, r)
		}
	})))

	// 1. Check main thread detail page headers, sandboxed iframe, and Reopen & Merge banner
	req1 := httptest.NewRequest(http.MethodGet, "/threads/"+followUpID.String(), nil)
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec1.Code)
	}
	if gotCSP := rec1.Header().Get("Content-Security-Policy"); gotCSP != web.MainCSPInsecure {
		t.Fatalf("unexpected main CSP for plain HTTP request: %s", gotCSP)
	}
	if rec1.Header().Get("X-Frame-Options") != "DENY" || rec1.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing security headers: %+v", rec1.Header())
	}
	body1 := rec1.Body.String()
	if !strings.Contains(body1, `sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"`) || strings.Contains(body1, "credentialless") {
		t.Fatalf("expected sandboxed iframe without credentialless in thread detail: %s", body1)
	}
	if !strings.Contains(body1, `title="Email message from client@example.com"`) {
		t.Fatalf("expected titled email iframe in thread detail: %s", body1)
	}
	if !strings.Contains(body1, "Reopen &amp; merge") {
		t.Fatalf("expected Reopen & Merge banner in thread detail: %s", body1)
	}

	// 2. Check isolated email body endpoint headers and content
	req2 := httptest.NewRequest(http.MethodGet, "/emails/"+emailID.String()+"/body", nil)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("email body status %d, want 200", rec2.Code)
	}
	if gotCSP := rec2.Header().Get("Content-Security-Policy"); gotCSP != web.EmailBodyCSP {
		t.Fatalf("unexpected email body CSP: %s", gotCSP)
	}
	if rec2.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("expected X-Frame-Options SAMEORIGIN on email body, got %s", rec2.Header().Get("X-Frame-Options"))
	}
	if !strings.Contains(rec2.Body.String(), `<meta name="responsive-embedded-sizing">`) {
		t.Fatalf("expected responsive-embedded-sizing meta in isolated email body: %s", rec2.Body.String())
	}

	// 3. Test updating settings (including display_timezone and Archive folder)
	form := url.Values{
		"account_default_assignee_support": {store.adminUser.ID.String()},
		"display_timezone":                 {"America/New_York"},
		"imap_inbox_mailbox":               {"INBOX"},
		"imap_archive_mailbox":             {"Archive"},
		"imap_junk_mailbox":                {"Junk"},
		"imap_sent_mailbox":                {"Sent"},
		"subject_match_window_days":        {"45"},
		"attention_age_days":               {"2"},
	}
	req3 := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusSeeOther || store.settings.DisplayTimezone != "America/New_York" {
		t.Fatalf("settings update failed: code=%d tz=%s", rec3.Code, store.settings.DisplayTimezone)
	}
	if got := store.accountSettings["support"].DefaultAssigneeID; got == nil || *got != store.adminUser.ID {
		t.Fatalf("expected support default assignee saved, got %v", got)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, `"event_category":"data_access"`) || !strings.Contains(logs, `"event_category":"data_change"`) {
		t.Fatalf("expected structured data_access and data_change logs, got: %s", logs)
	}
}

func testAccounts() []web.AccountConfig {
	return []web.AccountConfig{
		{ID: "support", Name: "Customer Support", FromAddress: "support@example.com"},
		{ID: "sales", Name: "Sales", FromAddress: "sales@example.com"},
	}
}

// buildAccountTestServer builds a web.Server with two configured accounts.
// testInboxPageSize, when > 0, overrides the inbox page size for servers built by
// buildAccountTestServer (needed for pagination tests; the returned *web.Server pointer
// cannot be reconfigured after the router closes over it).
var testInboxPageSize int

func buildAccountTestServer(t *testing.T) (*web.Server, *testWebStore, http.Handler, sqlc.User, sqlc.User) {
	t.Helper()
	store := newTestWebStore()
	auditLogger := audit.NewLogger(store, nil)
	attStore, err := storage.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	wfSvc := workflow.NewService(store, auditLogger, nil)
	outbox := mail.NewOutboxWorker(outboxStoreAdapter{store}, attStore, []mail.AccountSenderConfig{
		{AccountID: "support", FromAddress: "support@example.com", FromName: "Support", Transport: &noopSMTP{}},
	}, auditLogger, "example.com", nil, nil)
	sm := auth.NewSessionManager(false)
	authSvc := auth.NewService(store, auditLogger, sm, nil, "admin@example.com")
	srv := web.NewServer(store, authSvc, wfSvc, outbox, attStore, auditLogger, sm, testAccounts())
	if testInboxPageSize > 0 {
		srv = srv.WithInboxPageSize(testInboxPageSize)
	}

	multiUser := store.adminUser
	singleUser := sqlc.User{
		ID:              uuid.New(),
		Email:           "support-agent@example.com",
		DisplayName:     "Support Agent",
		Role:            "agent",
		IsActive:        true,
		AllowedAccounts: []string{"support"},
	}
	store.users[singleUser.ID] = singleUser

	router := web.SecurityHeadersMiddleware(sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := r.URL.Query().Get("as")
		user := multiUser
		if current == "single" {
			user = singleUser
		}
		r = r.WithContext(auth.WithUser(r.Context(), user))
		switch {
		case r.URL.Path == "/threads":
			srv.HandleListThreads(w, r)
		case r.URL.Path == "/threads/bulk-spam" && r.Method == http.MethodPost:
			srv.HandleBulkSpam(w, r)
		case r.URL.Path == "/threads/bulk-assign" && r.Method == http.MethodPost:
			srv.HandleBulkAssign(w, r)
		case r.URL.Path == "/threads/bulk-complete" && r.Method == http.MethodPost:
			srv.HandleBulkComplete(w, r)
		case r.URL.Path == "/settings":
			if r.Method == http.MethodPost {
				srv.HandleUpdateSettings(w, r)
				return
			}
			srv.HandleGetSettings(w, r)
		case r.URL.Path == "/settings/users" && r.Method == http.MethodPost:
			srv.HandlePrecreateUser(w, r)
		case strings.HasPrefix(r.URL.Path, "/threads/"):
			rest := strings.TrimPrefix(r.URL.Path, "/threads/")
			switch {
			case strings.HasSuffix(rest, "/draft"):
				srv.HandleDraft(w, web.WithChiParam(r, "id", strings.TrimSuffix(rest, "/draft")))
			case strings.HasSuffix(rest, "/assign"):
				srv.HandleAssignThread(w, web.WithChiParam(r, "id", strings.TrimSuffix(rest, "/assign")))
			case strings.HasSuffix(rest, "/reply"):
				srv.HandleReplyThread(w, web.WithChiParam(r, "id", strings.TrimSuffix(rest, "/reply")))
			case strings.HasSuffix(rest, "/transition"):
				srv.HandleTransitionThread(w, web.WithChiParam(r, "id", strings.TrimSuffix(rest, "/transition")))
			default:
				srv.HandleViewThread(w, web.WithChiParam(r, "id", rest))
			}
		case strings.HasPrefix(r.URL.Path, "/emails/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/emails/"), "/body")
			srv.HandleIsolatedEmailBody(w, web.WithChiParam(r, "id", id))
		case strings.HasPrefix(r.URL.Path, "/attachments/"):
			rest := strings.TrimPrefix(r.URL.Path, "/attachments/")
			switch {
			case strings.HasSuffix(rest, "/view"):
				srv.HandleViewAttachment(w, web.WithChiParam(r, "id", strings.TrimSuffix(rest, "/view")))
			default:
				srv.HandleDownloadAttachment(w, web.WithChiParam(r, "id", strings.TrimSuffix(rest, "/download")))
			}
		}
	})))
	return srv, store, router, multiUser, singleUser
}

func TestCompleteFromNewHTTP(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	id := uuid.New()
	store.threads[id] = sqlc.Thread{ID: id, AccountID: "support", Status: sqlc.ThreadStatusNew, CustomerEmail: "customer@example.com"}
	request := func(query string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/threads/"+id.String()+"/transition"+query, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := request("", url.Values{"status": {"spam"}, "complete_from_new": {"1"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid flag: %d", rec.Code)
	}
	thread := store.threads[id]
	thread.AccountID = "sales"
	store.threads[id] = thread
	if rec := request("?as=single", url.Values{"status": {"completed"}, "complete_from_new": {"1"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("account isolation: %d", rec.Code)
	}
	thread.AccountID = "support"
	store.threads[id] = thread
	if rec := request("", url.Values{"status": {"completed"}, "complete_from_new": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("completion: %d %s", rec.Code, rec.Body.String())
	}
	if store.threads[id].Status != sqlc.ThreadStatusCompleted || len(store.threadEvents) != 2 {
		t.Fatalf("unexpected result: %+v events=%d", store.threads[id], len(store.threadEvents))
	}
	if rec := request("", url.Values{"status": {"completed"}, "complete_from_new": {"1"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong source status: %d", rec.Code)
	}
}

func TestDraftSaveRestoreConflictAndIsolation(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	id := uuid.New()
	store.threads[id] = sqlc.Thread{ID: id, AccountID: "sales", Status: sqlc.ThreadStatusNew, LastMessageAt: time.Now()}
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/threads/"+id.String()+"/draft", strings.NewReader(body)))
		return rec
	}
	rec := put(`{"revision":"00000000-0000-0000-0000-000000000000","body":"shared draft","formatting":"bold-state"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	var draft sqlc.ReplyDraft
	if err := json.Unmarshal(rec.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	if rec = put(`{"revision":"00000000-0000-0000-0000-000000000000","body":"stale"}`); rec.Code != http.StatusConflict {
		t.Fatalf("stale save: %d", rec.Code)
	}
	page := httptest.NewRecorder()
	router.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/threads/"+id.String(), nil))
	if !strings.Contains(page.Body.String(), "shared draft") || !strings.Contains(page.Body.String(), "bold-state") || !strings.Contains(page.Body.String(), draft.Revision.String()) {
		t.Fatal("draft not restored")
	}
	private := httptest.NewRecorder()
	router.ServeHTTP(private, httptest.NewRequest(http.MethodGet, "/threads/"+id.String()+"/draft?as=single", nil))
	if private.Code != http.StatusNotFound {
		t.Fatalf("private draft accessible: %d", private.Code)
	}
	rec = put(fmt.Sprintf(`{"revision":%q,"discard":true}`, draft.Revision.String()))
	if rec.Code != http.StatusOK || len(store.threadEvents) != 1 || store.threadEvents[0].EventType != "draft_discarded" {
		t.Fatalf("discard: %d events=%v", rec.Code, store.threadEvents)
	}
}

func TestRemoteImageAccessAndNotice(t *testing.T) {
	srv, store, router, _, singleUser := buildAccountTestServer(t)
	threadID, emailID := uuid.New(), uuid.New()
	store.threads[threadID] = sqlc.Thread{ID: threadID, AccountID: "sales", Status: sqlc.ThreadStatusNew, LastMessageAt: time.Now()}
	store.emails[emailID] = sqlc.Email{ID: emailID, ThreadID: threadID, AccountID: "sales", BodyHTMLSanitized: `<img data-blocked-src="https://example.com/pixel">`}
	page := httptest.NewRecorder()
	router.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String(), nil))
	if !strings.Contains(page.Body.String(), "Remote images blocked for your privacy") || !strings.Contains(page.Body.String(), "Load images for this message") {
		t.Fatal("missing image consent notice")
	}
	req := httptest.NewRequest(http.MethodGet, "/emails/"+emailID.String()+"/images/0", nil)
	req = req.WithContext(auth.WithUser(req.Context(), singleUser))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", emailID.String())
	rctx.URLParams.Add("index", "0")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	srv.HandleRemoteImage(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("inaccessible image: %d", rec.Code)
	}
}

func TestAccountIsolationAndConditionalAccountsUI(t *testing.T) {
	srv, store, router, multiUser, _ := buildAccountTestServer(t)
	_ = srv

	adminID := multiUser.ID
	supportThread := uuid.New()
	salesThread := uuid.New()
	store.threads[supportThread] = sqlc.Thread{
		ID: supportThread, AccountID: "support", SubjectDisplay: "Support Ticket",
		Status: sqlc.ThreadStatusNew, AssigneeID: &adminID, CustomerEmail: "a@client.com",
		LastMessageAt: time.Now().UTC(),
	}
	store.threads[salesThread] = sqlc.Thread{
		ID: salesThread, AccountID: "sales", SubjectDisplay: "Sales Ticket",
		Status: sqlc.ThreadStatusNew, AssigneeID: &adminID, CustomerEmail: "b@client.com",
		LastMessageAt: time.Now().UTC(),
	}
	supportEmail := uuid.New()
	store.emails[supportEmail] = sqlc.Email{
		ID: supportEmail, ThreadID: supportThread, AccountID: "support", Direction: "inbound",
		FromAddress: "a@client.com", BodyText: "support body", SentAt: time.Now().UTC(),
	}
	salesEmail := uuid.New()
	store.emails[salesEmail] = sqlc.Email{
		ID: salesEmail, ThreadID: salesThread, AccountID: "sales", Direction: "inbound",
		FromAddress: "b@client.com", BodyText: "sales body", SentAt: time.Now().UTC(),
	}
	// Both attachments share the same content-addressed blob path on disk.
	sharedBlob := "ab/cd/abcdfeedface"
	supportAtt := uuid.New()
	store.attachments[supportAtt] = sqlc.Attachment{
		ID: supportAtt, EmailID: supportEmail, Filename: "shared.txt", ContentType: "text/plain",
		StorageRelPath: sharedBlob,
	}
	salesAtt := uuid.New()
	store.attachments[salesAtt] = sqlc.Attachment{
		ID: salesAtt, EmailID: salesEmail, Filename: "shared.txt", ContentType: "text/plain",
		StorageRelPath: sharedBlob,
	}

	// Multi-account user: Account column, filter and badge are rendered.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?as=multi", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("multi /threads status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="account"`) {
		t.Fatalf("expected account filter for multi-account user: %s", body)
	}
	if !strings.Contains(body, "Customer Support") || !strings.Contains(body, "Sales") {
		t.Fatalf("expected account names in inbox: %s", body)
	}
	if !strings.Contains(body, `class="thread-meta"`) || !strings.Contains(body, "Customer Support &middot; ") {
		t.Fatalf("expected account name in the merged thread metadata line: %s", body)
	}

	// Single-account user: account UI is hidden.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?as=single", nil))
	body = rec.Body.String()
	if strings.Contains(body, `name="account"`) {
		t.Fatalf("expected no account UI for single-account user: %s", body)
	}
	if strings.Contains(body, "Sales Ticket") {
		t.Fatalf("single-account user must not see threads of other accounts: %s", body)
	}
	if !strings.Contains(body, "Support Ticket") {
		t.Fatalf("single-account user should see own account threads: %s", body)
	}

	// Single-account user cannot fetch another account's thread, email body, or attachment.
	for _, target := range []string{
		"/threads/" + salesThread.String() + "?as=single",
		"/emails/" + salesEmail.String() + "/body?as=single",
		"/attachments/" + salesAtt.String() + "/view?as=single",
		"/attachments/" + salesAtt.String() + "/download?as=single",
	} {
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: expected 404, got %d", target, rec.Code)
		}
	}

	// ... while the same single-account user can access its own account's data.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+supportThread.String()+"?as=single", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for own-account thread, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/emails/"+supportEmail.String()+"/body?as=single", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for own-account email body, got %d", rec.Code)
	}

	// Thread detail for a multi-account user shows the account badge.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+salesThread.String()+"?as=multi", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Sales") {
		t.Fatalf("expected account badge in thread detail, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAssigningToUserWithoutAccountAccessIsRejected(t *testing.T) {
	_, store, router, multiUser, singleUser := buildAccountTestServer(t)
	_ = store

	salesThread := uuid.New()
	store.threads[salesThread] = sqlc.Thread{
		ID: salesThread, AccountID: "sales", SubjectDisplay: "Sales Ticket",
		Status: sqlc.ThreadStatusNew, AssigneeID: &multiUser.ID, CustomerEmail: "b@client.com",
		LastMessageAt: time.Now().UTC(),
	}

	form := url.Values{"assignee_id": {singleUser.ID.String()}}
	req := httptest.NewRequest(http.MethodPost, "/threads/"+salesThread.String()+"/assign?as=multi", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 assigning to user without access, got %d (%s)", rec.Code, rec.Body.String())
	}

	// The assignee dropdown must not offer users without access to the thread's account.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+salesThread.String()+"?as=multi", nil))
	body := rec.Body.String()
	idx := strings.Index(body, `for="assignee-`)
	if idx < 0 {
		t.Fatalf("expected assignee form in thread detail: %s", body)
	}
	if strings.Contains(body[idx:], singleUser.Email) {
		t.Fatalf("assignee dropdown must not offer users without account access: %s", body[idx:])
	}
}

func TestRenderSettingsWithAccountDefaultAssignees(t *testing.T) {
	admin := sqlc.User{
		ID: uuid.New(), Email: "admin@example.com", DisplayName: "Admin",
		Role: "admin", IsActive: true, AllowedAccounts: []string{"support", "sales"},
	}
	supportAgent := sqlc.User{
		ID: uuid.New(), Email: "support@example.com", DisplayName: "Support Agent",
		Role: "agent", IsActive: true, AllowedAccounts: []string{"support"},
	}
	inactive := sqlc.User{
		ID: uuid.New(), Email: "former@example.com", DisplayName: "Former Agent",
		Role: "agent", IsActive: false, AllowedAccounts: []string{"support"},
	}
	salesID := admin.ID
	var buf bytes.Buffer
	err := views.RenderSettings(&buf, views.SettingsViewData{
		CurrentUser: admin,
		Settings:    sqlc.SystemSettings{ID: 1, DisplayTimezone: "UTC", IMAPInboxMailbox: "INBOX"},
		Users:       []sqlc.User{admin, supportAgent, inactive},
		Accounts: []views.AccountOption{
			{ID: "support", Name: "Customer Support"},
			{ID: "sales", Name: "Sales"},
		},
		AccountSettings: []views.AccountSettingsView{
			{Account: views.AccountOption{ID: "support", Name: "Customer Support"}},
			{Account: views.AccountOption{ID: "sales", Name: "Sales"}, DefaultAssigneeValue: salesID.String()},
		},
	})
	if err != nil {
		t.Fatalf("RenderSettings: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		`name="account_default_assignee_support"`,
		`name="account_default_assignee_sales"`,
		"-- Unassigned --",
		"Customer Support",
		`name="allowed_accounts"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in settings page: %s", want, body)
		}
	}
	if !strings.Contains(body, `class="badge badge-active">Active<`) || !strings.Contains(body, `class="badge badge-inactive">Inactive<`) {
		t.Fatalf("expected dedicated active/inactive badge classes in settings: %s", body)
	}

	// The support-only agent must not be offered as the sales default assignee.
	salesIdx := strings.Index(body, `name="account_default_assignee_sales"`)
	if salesIdx < 0 {
		t.Fatal("expected sales default assignee field in settings page")
	}
	salesSection := body[salesIdx:]
	if end := strings.Index(salesSection, "</select>"); end >= 0 {
		salesSection = salesSection[:end]
	}
	if strings.Contains(salesSection, "support@example.com") {
		t.Fatalf("support-only agent must not be offered for the sales account: %s", salesSection)
	}
}

func TestFormatStatusLabels(t *testing.T) {
	cases := map[sqlc.ThreadStatus]string{
		sqlc.ThreadStatusNew:        "New",
		sqlc.ThreadStatusInProgress: "In progress",
		sqlc.ThreadStatusCompleted:  "Completed",
		sqlc.ThreadStatusSpam:       "Spam",
	}
	for status, want := range cases {
		if got := views.FormatStatus(status); got != want {
			t.Errorf("FormatStatus(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestThreadDetailSemanticsAndNoInlineStyles(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	emailID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Printer is on fire",
		Status: sqlc.ThreadStatusNew, CustomerName: "Bob Builder", CustomerEmail: "bob@customer.example",
		LastMessageAt: time.Now().UTC(),
	}
	store.emails[emailID] = sqlc.Email{
		ID: emailID, ThreadID: threadID, AccountID: "support", Direction: "inbound",
		FromName: "Bob Builder", FromAddress: "bob@customer.example", BodyText: "Help",
		SentAt: time.Now().UTC(),
	}
	store.threadEvents = append(store.threadEvents, sqlc.ThreadEvent{
		ID: uuid.New(), ThreadID: threadID, EventType: "created", OccurredAt: time.Now().UTC(),
	})
	_ = multiUser

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<nav class="navbar" aria-label="Main"`,
		`aria-current="page"`,
		`class="badge badge-new">New<`,
		`title="Email message from bob@customer.example"`,
		`<label for="reply-body">Message</label>`,
		`>Start working<`,
		`data-confirm=`,
		`>Unassigned<`,
		`data-auto-submit`,
		`>Assign to me<`,
		`Back to threads`,
		`<details class="card message message-inbound" open>`,
		`sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"`,
		`From</dt><dd>Customer Support &lt;support@example.com&gt;`,
		`reply-context-details`,
		`data-open-reply-sheet`,
		`data-close-reply-sheet`,
		`>Customer<`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in thread detail: %s", want, body)
		}
	}
	if strings.Contains(body, `style="`) {
		t.Fatalf("thread detail must not contain inline style attributes: %s", body)
	}
	if strings.Contains(body, "new -> in-progress") || strings.Contains(body, "in_progress -> completed") {
		t.Fatalf("thread detail must not leak internal transition syntax: %s", body)
	}
}

func TestInboxAssigneeFilterAndTableSemantics(t *testing.T) {
	_, store, router, multiUser, singleUser := buildAccountTestServer(t)
	mineID := uuid.New()
	unassignedID := uuid.New()
	otherID := uuid.New()
	store.threads[mineID] = sqlc.Thread{
		ID: mineID, AccountID: "support", SubjectDisplay: "Mine", Status: sqlc.ThreadStatusNew,
		AssigneeID: &multiUser.ID, CustomerEmail: "a@client.com", LastMessageAt: time.Now().UTC(),
	}
	store.threads[unassignedID] = sqlc.Thread{
		ID: unassignedID, AccountID: "support", SubjectDisplay: "Nobody", Status: sqlc.ThreadStatusNew,
		CustomerEmail: "b@client.com", LastMessageAt: time.Now().UTC(),
	}
	store.threads[otherID] = sqlc.Thread{
		ID: otherID, AccountID: "support", SubjectDisplay: "Someone else", Status: sqlc.ThreadStatusNew,
		AssigneeID: &singleUser.ID, CustomerEmail: "c@client.com", LastMessageAt: time.Now().UTC(),
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?assignee=me", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Mine") || strings.Contains(body, "Nobody") || strings.Contains(body, "Someone else") {
		t.Fatalf("assigned-to-me filter mismatch: %s", body)
	}
	if strings.Contains(body, `style="`) {
		t.Fatalf("inbox must not contain inline styles: %s", body)
	}
	for _, want := range []string{`name="assignee"`, `scope="col"`, `class="results-count"`, "1 thread", `aria-current="page"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in inbox: %s", want, body)
		}
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?assignee=unassigned", nil))
	body = rec.Body.String()
	if !strings.Contains(body, "Nobody") || strings.Contains(body, ">Mine<") || strings.Contains(body, "Someone else") {
		t.Fatalf("unassigned filter mismatch: %s", body)
	}
}

func TestInboxCompactChromeFiltersAndRefreshFragment(t *testing.T) {
	_, store, router, multiUser, singleUser := buildAccountTestServer(t)
	// A second active user removes the single-member assignee shortcut.
	store.users[singleUser.ID] = singleUser
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Compact chrome", Status: sqlc.ThreadStatusNew,
		AssigneeID: &multiUser.ID, CustomerEmail: "lena@client.example", LastMessageAt: time.Now().UTC(),
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?q=compact", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /threads status = %d", rec.Code)
	}
	body := rec.Body.String()

	// One filter row: search with icon + clear link, both selects, no Search button.
	for _, want := range []string{
		`class="filter-inline"`,
		`class="search-icon" aria-hidden="true"`,
		`name="q" value="compact"`,
		`class="search-clear" href="/threads?status=open"`,
		`name="assignee"`,
		`data-inbox-results`,
		`data-refresh-link`,
		`<noscript><button type="submit"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in compact inbox: %s", want, body)
		}
	}
	if strings.Contains(body, `>Search</button>`) {
		t.Fatalf("inbox must not render a dedicated Search button: %s", body)
	}
	// The results count moved next to the tab strip.
	if !strings.Contains(body, `class="inbox-toolbar" data-inbox-toolbar`) || !strings.Contains(body, `class="results-count"`) {
		t.Fatalf("expected the results area in the tab strip: %s", body)
	}
	// The customer email stays out of the list, in the title attribute only.
	if strings.Contains(body, ">lena@client.example<") {
		t.Fatalf("inbox rows must not print the customer email: %s", body)
	}
	if !strings.Contains(body, `title="lena@client.example"`) {
		t.Fatalf("expected the customer email in the row title: %s", body)
	}

	// Fragment refresh returns just the results region for the auto-refresh loop.
	fragRec := httptest.NewRecorder()
	fragReq := httptest.NewRequest(http.MethodGet, "/threads?q=compact", nil)
	fragReq.Header.Set("HX-Request", "true")
	router.ServeHTTP(fragRec, fragReq)
	frag := fragRec.Body.String()
	if !strings.Contains(frag, `class="thread-link"`) {
		t.Fatalf("expected thread rows in the refresh fragment: %s", frag)
	}
	if strings.Contains(frag, `data-inbox-toolbar`) {
		t.Fatalf("refresh fragment must not re-render the toolbar: %s", frag)
	}
}

func TestInboxHidesAssigneeFilterForSoloTeamMember(t *testing.T) {
	_, store, router, soloUser, _ := buildAccountTestServer(t)
	// Only one active user can access the account, so the assignee select is pointless.
	store.adminUser = soloUser
	store.users = map[uuid.UUID]sqlc.User{soloUser.ID: soloUser}
	store.threads[uuid.New()] = sqlc.Thread{
		ID: uuid.New(), AccountID: "support", SubjectDisplay: "Solo", Status: sqlc.ThreadStatusNew,
		LastMessageAt: time.Now().UTC(),
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /threads status = %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, `name="assignee"`) {
		t.Fatalf("expected no assignee filter for a solo team member: %s", body)
	}
}

func TestTerminalThreadReplyRejectedWithPreservedDraft(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  sqlc.ThreadStatus
		message string
	}{
		{name: "spam", status: sqlc.ThreadStatusSpam, message: "Replies are disabled on spam threads."},
		{name: "completed", status: sqlc.ThreadStatusCompleted, message: "Reopen and merge the customer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, router, multiUser, _ := buildAccountTestServer(t)
			threadID := uuid.New()
			store.threads[threadID] = sqlc.Thread{
				ID: threadID, AccountID: "support", SubjectDisplay: "Closed thread",
				Status: tc.status, AssigneeID: &multiUser.ID, CustomerEmail: "bob@customer.example",
				LastMessageAt: time.Now().UTC(),
			}
			form := url.Values{"body_text": {"hello draft that must survive"}}
			req := httptest.NewRequest(http.MethodPost, "/threads/"+threadID.String()+"/reply", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.message) || !strings.Contains(body, "hello draft that must survive") {
				t.Fatalf("expected inline error and preserved draft, got: %s", body)
			}
			if !strings.Contains(body, "<!DOCTYPE html>") {
				t.Fatalf("expected full styled page, got: %s", body)
			}
		})
	}
}

func TestStyledNotFoundPage(t *testing.T) {
	_, _, router, _, _ := buildAccountTestServer(t)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+uuid.NewString(), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<!DOCTYPE html>", "Thread not found", "Back to threads", "error-page"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in styled error page: %s", want, body)
		}
	}
}

func TestFlashAndNoInlineStylesAcrossViews(t *testing.T) {
	admin := sqlc.User{ID: uuid.New(), Email: "admin@example.com", DisplayName: "Admin", Role: "admin", IsActive: true, AllowedAccounts: []string{"support"}}

	var inbox bytes.Buffer
	if err := views.RenderInbox(&inbox, views.InboxViewData{
		Title: "Threads", ActiveNav: "threads", CurrentUser: admin, DisplayTZ: "UTC",
		Flash: views.Flash{Kind: "success", Message: "Reply queued for delivery."},
	}, false); err != nil {
		t.Fatalf("RenderInbox: %v", err)
	}
	for _, want := range []string{`role="status"`, "Reply queued for delivery.", "flash-success", "data-flash-dismiss"} {
		if !strings.Contains(inbox.String(), want) {
			t.Fatalf("expected %q in flash render: %s", want, inbox.String())
		}
	}
	if strings.Contains(inbox.String(), `style="`) {
		t.Fatalf("inbox must not contain inline styles: %s", inbox.String())
	}

	var settings bytes.Buffer
	if err := views.RenderSettings(&settings, views.SettingsViewData{
		Title: "Settings", ActiveNav: "settings", CurrentUser: admin,
		Settings: sqlc.SystemSettings{ID: 1, DisplayTimezone: "UTC", IMAPInboxMailbox: "INBOX"},
		Users:    []sqlc.User{admin},
		AuditLogs: []views.AuditEntryView{
			{FormattedTime: "2026-01-01 00:00:00 UTC", ActorName: "Admin", Label: "Settings updated", TargetName: "System settings"},
		},
		Accounts:     []views.AccountOption{{ID: "support", Name: "Customer Support"}},
		AccountNames: map[string]string{"support": "Customer Support"},
		NewUser:      views.NewUserForm{Role: "agent", AllowedAccounts: []string{"support"}},
	}); err != nil {
		t.Fatalf("RenderSettings: %v", err)
	}
	for _, want := range []string{"Team members", "admin@example.com", "Settings updated", "Customer Support", `checked`} {
		if !strings.Contains(settings.String(), want) {
			t.Fatalf("expected %q in settings page: %s", want, settings.String())
		}
	}
	if strings.Contains(settings.String(), `style="`) || strings.Contains(settings.String(), "(support)") {
		t.Fatalf("settings must not contain inline styles or raw account slugs: %s", settings.String())
	}

	var emailBody bytes.Buffer
	if err := views.RenderIsolatedEmailBody(&emailBody, sqlc.Email{ID: uuid.New(), BodyText: "plain text"}); err != nil {
		t.Fatalf("RenderIsolatedEmailBody: %v", err)
	}
	if strings.Contains(emailBody.String(), `style="`) {
		t.Fatalf("email body must not contain inline style attributes: %s", emailBody.String())
	}
	if !strings.Contains(emailBody.String(), "<style>") {
		t.Fatalf("expected a stylesheet inside the isolated email body: %s", emailBody.String())
	}
}

func TestFormatThreadEventSummary(t *testing.T) {
	admin := sqlc.User{ID: uuid.New(), DisplayName: "Dev Admin"}
	userNames := map[uuid.UUID]string{admin.ID: admin.DisplayName}
	statusEvent := sqlc.ThreadEvent{
		EventType: "status_changed",
		OldValue:  []byte(`{"status":"new"}`),
		NewValue:  []byte(`{"status":"in_progress"}`),
	}
	if got := views.FormatThreadEventSummary(statusEvent, admin.DisplayName, userNames); got != "Dev Admin changed status from New to In progress." {
		t.Fatalf("unexpected status summary: %q", got)
	}
	assignEvent := sqlc.ThreadEvent{
		EventType: "assigned",
		NewValue:  []byte(`{"assignee_id":"` + admin.ID.String() + `","source":"reply"}`),
	}
	if got := views.FormatThreadEventSummary(assignEvent, admin.DisplayName, userNames); got != "Dev Admin assigned this thread to Dev Admin when replying." {
		t.Fatalf("unexpected assignment summary: %q", got)
	}
	selfAssignEvent := sqlc.ThreadEvent{
		EventType:   "assigned",
		ActorUserID: &admin.ID,
		NewValue:    []byte(`{"assignee_id":"` + admin.ID.String() + `","source":"manual"}`),
	}
	if got := views.FormatThreadEventSummary(selfAssignEvent, admin.DisplayName, userNames); got != "Dev Admin assigned this thread to themselves." {
		t.Fatalf("unexpected self-assignment summary: %q", got)
	}
}

// newMultipartRequest builds a bounded multipart/form-data request with the given fields.
func newMultipartRequest(t *testing.T, method, target string, fields url.Values) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for key, values := range fields {
		for _, value := range values {
			if err := writer.WriteField(key, value); err != nil {
				t.Fatalf("WriteField(%s): %v", key, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(method, target, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestInboxStatusTabsKeepOverflowAndKeyboardAffordances(t *testing.T) {
	_, _, router, _, _ := buildAccountTestServer(t)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=spam", nil))
	body := rec.Body.String()
	for _, want := range []string{`data-tab-strip`, `aria-label="Thread status"`, `aria-current="true"><span>Spam</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox is missing %q", want)
		}
	}
	js, err := static.FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`addEventListener("focus"`, `scrollIntoView`, `overflowStart`, `overflowEnd`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("tab behavior is missing %q", want)
		}
	}
	css, err := static.FS.ReadFile("app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`@media (max-width: 768px)`, `.tab-strip[data-overflow-end="true"]`, `:focus-visible`} {
		if !strings.Contains(string(css), want) {
			t.Errorf("tab styles are missing %q", want)
		}
	}
}

func TestPreviousConversationsRespectEmailAndAccountAccess(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	now := time.Now().UTC()
	current := uuid.New()
	store.threads[current] = sqlc.Thread{ID: current, AccountID: "support", SubjectDisplay: "Current case", CustomerEmail: "customer@example.com", Status: sqlc.ThreadStatusNew, LastMessageAt: now}
	for _, item := range []struct{ account, email, subject string }{
		{"support", "CUSTOMER@example.com", "Visible prior"},
		{"sales", "customer@example.com", "Visible cross-account"},
		{"secret", "customer@example.com", "Hidden private"},
		{"support", "someone@example.com", "Hidden other customer"},
	} {
		id := uuid.New()
		store.threads[id] = sqlc.Thread{ID: id, AccountID: item.account, CustomerEmail: item.email, SubjectDisplay: item.subject, Status: sqlc.ThreadStatusCompleted, LastMessageAt: now}
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+current.String(), nil))
	body := rec.Body.String()
	for _, want := range []string{
		"<summary>2 earlier conversations</summary>",
		"Other conversations from this email address",
		"Visible prior",
		"Visible cross-account",
		`class="previous-conversation-row"`,
		`target="_blank" rel="noopener"`,
		`title="Opens in a new tab so your reply stays here."`,
		`class="badge badge-completed">Completed<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("history missing %q", want)
		}
	}
	for _, hidden := range []string{"Hidden private", "Hidden other customer", "No other conversations found.", "Links open in a new tab so your reply stays here."} {
		if strings.Contains(body, hidden) {
			t.Errorf("history exposed %q", hidden)
		}
	}

	// Single-account user sees 1 earlier conversation and no account badge in the history row.
	singleRec := httptest.NewRecorder()
	router.ServeHTTP(singleRec, httptest.NewRequest(http.MethodGet, "/threads/"+current.String()+"?as=single", nil))
	singleBody := singleRec.Body.String()
	for _, want := range []string{"<summary>1 earlier conversation</summary>", "Other conversations from this email address", "Visible prior"} {
		if !strings.Contains(singleBody, want) {
			t.Errorf("single-account history missing %q", want)
		}
	}
	if strings.Contains(singleBody, "Visible cross-account") || strings.Contains(singleBody, "badge-account") {
		t.Errorf("single-account history exposed cross-account or account badge: %s", singleBody)
	}

	// A thread with no other accessible conversations hides the history disclosure completely.
	solo := uuid.New()
	store.threads[solo] = sqlc.Thread{ID: solo, AccountID: "support", SubjectDisplay: "First time caller", CustomerEmail: "solo@example.com", Status: sqlc.ThreadStatusNew, LastMessageAt: now}
	secretPrior := uuid.New()
	store.threads[secretPrior] = sqlc.Thread{ID: secretPrior, AccountID: "secret", SubjectDisplay: "Secret prior", CustomerEmail: "solo@example.com", Status: sqlc.ThreadStatusCompleted, LastMessageAt: now}
	soloRec := httptest.NewRecorder()
	router.ServeHTTP(soloRec, httptest.NewRequest(http.MethodGet, "/threads/"+solo.String(), nil))
	soloBody := soloRec.Body.String()
	for _, forbidden := range []string{"previous-conversations", "Other conversations from this email address", "No other conversations found.", "earlier conversation", "Secret prior"} {
		if strings.Contains(soloBody, forbidden) {
			t.Errorf("empty history must not contain %q", forbidden)
		}
	}
}

func TestInboxKeysetPaginationAndSearch(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 105; i++ {
		id := uuid.New()
		store.threads[id] = sqlc.Thread{ID: id, AccountID: "support", SubjectDisplay: fmt.Sprintf("Match %03d", i),
			CustomerEmail: "client@example.com", Status: sqlc.ThreadStatusNew, LastMessageAt: now}
	}
	nonMatchID := uuid.New()
	store.threads[nonMatchID] = sqlc.Thread{ID: nonMatchID, AccountID: "support", SubjectDisplay: "Unrelated subject",
		CustomerEmail: "other@example.com", Status: sqlc.ThreadStatusNew, LastMessageAt: now}
	secretID := uuid.New()
	store.threads[secretID] = sqlc.Thread{ID: secretID, AccountID: "secret", SubjectDisplay: "Match private", Status: sqlc.ThreadStatusNew, LastMessageAt: now}
	path := "/threads?status=new&account=support&q=Match"
	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, path, nil))
	body := first.Body.String()
	if first.Code != http.StatusOK || strings.Contains(body, "Match private") || strings.Count(body, `class="thread-link"`) != 100 {
		t.Fatalf("unexpected first page: status %d, links %d", first.Code, strings.Count(body, `class="thread-link"`))
	}
	const marker = `href="`
	end := strings.Index(body, `">Older threads</a>`)
	if end < 0 {
		t.Fatal("missing next page")
	}
	start := strings.LastIndex(body[:end], marker)
	next := html.UnescapeString(body[start+len(marker) : end])
	parsed, err := url.Parse(next)
	if err != nil || parsed.Query().Get("q") != "Match" || parsed.Query().Get("account") != "support" {
		t.Fatalf("invalid next link %q: %v", next, err)
	}
	newID := uuid.New()
	store.threads[newID] = sqlc.Thread{ID: newID, AccountID: "support", SubjectDisplay: "Match inserted", Status: sqlc.ThreadStatusNew, LastMessageAt: now.Add(time.Second)}
	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, next, nil))
	secondBody := second.Body.String()
	if second.Code != http.StatusOK || strings.Count(secondBody, `class="thread-link"`) != 5 || strings.Contains(secondBody, "Match inserted") || strings.Contains(secondBody, `>Older threads</a>`) {
		t.Fatalf("unexpected second page: status %d, links %d", second.Code, strings.Count(secondBody, `class="thread-link"`))
	}
	for i := 0; i < 105; i++ {
		subject := fmt.Sprintf("Match %03d", i)
		if strings.Contains(body, subject) == strings.Contains(secondBody, subject) {
			t.Fatalf("%s should appear on exactly one page", subject)
		}
	}
	bad := httptest.NewRecorder()
	router.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/threads?before=invalid&id=invalid", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor returned %d", bad.Code)
	}
}

func TestInboxUnknownStatusFilterFallsBackToOpenQueue(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Visible thread",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "a@client.com", LastMessageAt: time.Now().UTC(),
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=bogus", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown status filter, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "Inbox unavailable") {
		t.Fatalf("unknown status must not render an error page: %s", body)
	}
	if !strings.Contains(body, "Visible thread") {
		t.Fatalf("unknown status should fall back to the open queue: %s", body)
	}
	if strings.Contains(body, `value="bogus"`) {
		t.Fatalf("unknown status must not be echoed into the filter form: %s", body)
	}
}

func TestInboxEmptyStateRendersOutsideScrollableTable(t *testing.T) {
	_, _, router, _, _ := buildAccountTestServer(t)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=spam", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty inbox, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="card empty-state"`) || !strings.Contains(body, "No threads match these filters.") {
		t.Fatalf("expected an empty-state card in the inbox: %s", body)
	}
	if strings.Contains(body, "threads-table") || strings.Contains(body, `class="table-wrap"`) {
		t.Fatalf("empty inbox must not render the horizontally scrolling table: %s", body)
	}
}

func TestReplyValidationErrorRenderedOnceNextToField(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Whitespace reply",
		Status: sqlc.ThreadStatusInProgress, AssigneeID: &multiUser.ID, CustomerEmail: "bob@customer.example",
		LastMessageAt: time.Now().UTC(),
	}
	form := url.Values{"body_text": {"   "}}
	req := httptest.NewRequest(http.MethodPost, "/threads/"+threadID.String()+"/reply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for whitespace reply, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	const message = "reply body must not be empty"
	if got := strings.Count(body, message); got != 1 {
		t.Fatalf("expected the reply error exactly once, got %d: %s", got, body)
	}
	replyCard := strings.Index(body, `class="card reply-card"`)
	errorIndex := strings.Index(body, message)
	if replyCard < 0 || errorIndex < replyCard {
		t.Fatalf("expected the reply error inside the reply card: %s", body)
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Fatalf("expected the reply error to keep role=\"alert\": %s", body)
	}

	// A terminal thread has no reply card; an empty draft reply still renders the error once.
	completedID := uuid.New()
	store.threads[completedID] = sqlc.Thread{
		ID: completedID, AccountID: "support", SubjectDisplay: "Completed reply",
		Status: sqlc.ThreadStatusCompleted, AssigneeID: &multiUser.ID, CustomerEmail: "bob@customer.example",
		LastMessageAt: time.Now().UTC(),
	}
	req = httptest.NewRequest(http.MethodPost, "/threads/"+completedID.String()+"/reply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for completed reply, got %d", rec.Code)
	}
	body = rec.Body.String()
	if got := strings.Count(body, "This thread is completed. Reopen and merge the customer"); got != 1 {
		t.Fatalf("expected the completed-thread reply error exactly once, got %d: %s", got, body)
	}
}

func TestSettingsEndpointsAcceptMultipartForm(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)

	fields := url.Values{
		"account_default_assignee_support": {multiUser.ID.String()},
		"display_timezone":                 {"Europe/Paris"},
		"imap_inbox_mailbox":               {"INBOX"},
		"imap_archive_mailbox":             {"Archive"},
		"imap_junk_mailbox":                {"Junk"},
		"imap_sent_mailbox":                {"Sent"},
		"subject_match_window_days":        {"45"},
		"attention_age_days":               {"2"},
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, newMultipartRequest(t, http.MethodPost, "/settings", fields))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 for multipart settings post, got %d: %s", rec.Code, rec.Body.String())
	}
	if store.settings.DisplayTimezone != "Europe/Paris" || store.settings.SubjectMatchWindowDays != 45 {
		t.Fatalf("multipart settings must save submitted values, got %+v", store.settings)
	}
	if got := store.accountSettings["support"].DefaultAssigneeID; got == nil || *got != multiUser.ID {
		t.Fatalf("expected multipart default assignee saved, got %v", got)
	}

	userFields := url.Values{
		"email":            {"newagent@example.com"},
		"display_name":     {"New Agent"},
		"role":             {"agent"},
		"allowed_accounts": {"support"},
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, newMultipartRequest(t, http.MethodPost, "/settings/users", userFields))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 for multipart precreate user, got %d: %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, u := range store.users {
		if u.Email == "newagent@example.com" && u.DisplayName == "New Agent" {
			found = true
		}
	}
	if !found {
		t.Fatalf("multipart precreate user must create the submitted user, got %+v", store.users)
	}
}

func TestBulkSpamRevalidatesAndPreservesFilter(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	newID, progressID, spamID, completedID, privateID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, entry := range []struct {
		id      uuid.UUID
		account string
		status  sqlc.ThreadStatus
	}{
		{newID, "support", sqlc.ThreadStatusNew}, {progressID, "support", sqlc.ThreadStatusInProgress}, {spamID, "support", sqlc.ThreadStatusSpam}, {completedID, "support", sqlc.ThreadStatusCompleted}, {privateID, "sales", sqlc.ThreadStatusNew},
	} {
		store.threads[entry.id] = sqlc.Thread{ID: entry.id, AccountID: entry.account, Status: entry.status, SubjectDisplay: "Example", LastMessageAt: time.Now()}
		uid, inbox := int64(42), "INBOX"
		em := sqlc.Email{ID: uuid.New(), ThreadID: entry.id, AccountID: entry.account, Direction: "inbound", IMAPUID: &uid, IMAPMailbox: &inbox}
		store.emails[em.ID] = em
	}
	page := "/threads?status=new&q=Example"
	form := url.Values{"thread_id": {newID.String(), progressID.String(), spamID.String(), completedID.String(), privateID.String()}, "return_to": {page}}
	req := httptest.NewRequest(http.MethodPost, "/threads/bulk-spam?as=single", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != page {
		t.Fatalf("redirect: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if store.threads[newID].Status != sqlc.ThreadStatusSpam || store.threads[progressID].Status != sqlc.ThreadStatusSpam || store.threads[privateID].Status != sqlc.ThreadStatusNew || len(store.jobs) != 2 || len(store.threadEvents) != 2 {
		t.Fatalf("unexpected transitions: jobs=%+v events=%+v", store.jobs, store.threadEvents)
	}
	for _, job := range store.jobs {
		if job.AccountID != "support" || job.JobType != "move_to_junk" {
			t.Fatalf("wrong job: %+v", job)
		}
	}
	follow := httptest.NewRequest(http.MethodGet, page+"&as=single", nil)
	for _, cookie := range rec.Result().Cookies() {
		follow.AddCookie(cookie)
	}
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, follow)
	if !strings.Contains(rec2.Body.String(), "2 marked as spam and queued for Junk; 3 failed.") || !strings.Contains(rec2.Body.String(), privateID.String()+": unavailable") {
		t.Fatalf("summary missing: %s", rec2.Body.String())
	}
}

func TestBulkAssignMixedAccountAccess(t *testing.T) {
	_, store, router, _, assignee := buildAccountTestServer(t)
	support, sales, completed := uuid.New(), uuid.New(), uuid.New()
	for _, item := range []struct {
		id      uuid.UUID
		account string
		status  sqlc.ThreadStatus
	}{
		{support, "support", sqlc.ThreadStatusNew}, {sales, "sales", sqlc.ThreadStatusInProgress}, {completed, "support", sqlc.ThreadStatusCompleted},
	} {
		store.threads[item.id] = sqlc.Thread{ID: item.id, AccountID: item.account, Status: item.status, SubjectDisplay: "Example", LastMessageAt: time.Now()}
	}
	page := "/threads?status=open&q=Example"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, page, nil))
	if !strings.Contains(rec.Body.String(), `formaction="/threads/bulk-assign"`) {
		t.Fatal("missing bulk assignment control")
	}
	form := url.Values{"thread_id": {support.String(), sales.String(), completed.String()}, "assignee_id": {assignee.ID.String()}, "return_to": {page}}
	req := httptest.NewRequest(http.MethodPost, "/threads/bulk-assign", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != page || store.threads[support].AssigneeID == nil || *store.threads[support].AssigneeID != assignee.ID || store.threads[sales].AssigneeID != nil || store.threads[completed].AssigneeID != nil {
		t.Fatalf("unexpected assignment: %d %+v", rec.Code, store.threads)
	}
	follow := httptest.NewRequest(http.MethodGet, page, nil)
	for _, cookie := range rec.Result().Cookies() {
		follow.AddCookie(cookie)
	}
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, follow)
	if !strings.Contains(rec2.Body.String(), "1 assigned; 2 failed.") || !strings.Contains(rec2.Body.String(), sales.String()+": assignee lacks account access") {
		t.Fatalf("summary missing: %s", rec2.Body.String())
	}

	// Empty bulk spam, bulk complete, and empty/unassigned bulk assign redirect with a flash rather than a 400 body.
	for _, tc := range []struct {
		endpoint string
		form     url.Values
		want     string
	}{
		{"/threads/bulk-spam", url.Values{"return_to": {page}}, "Select at least one conversation."},
		{"/threads/bulk-complete", url.Values{"return_to": {page}}, "Select at least one conversation."},
		{"/threads/bulk-assign", url.Values{"assignee_id": {assignee.ID.String()}, "return_to": {page}}, "Select at least one conversation."},
		{"/threads/bulk-assign", url.Values{"thread_id": {support.String()}, "return_to": {page}}, "Choose an assignee."},
	} {
		postReq := httptest.NewRequest(http.MethodPost, tc.endpoint, strings.NewReader(tc.form.Encode()))
		postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		postRec := httptest.NewRecorder()
		router.ServeHTTP(postRec, postReq)
		if postRec.Code != http.StatusSeeOther || postRec.Header().Get("Location") != page {
			t.Fatalf("%s redirect: got %d %q", tc.endpoint, postRec.Code, postRec.Header().Get("Location"))
		}
		followReq := httptest.NewRequest(http.MethodGet, page, nil)
		for _, cookie := range postRec.Result().Cookies() {
			followReq.AddCookie(cookie)
		}
		followRec := httptest.NewRecorder()
		router.ServeHTTP(followRec, followReq)
		if !strings.Contains(followRec.Body.String(), tc.want) {
			t.Fatalf("%s expected flash %q, got %s", tc.endpoint, tc.want, followRec.Body.String())
		}
	}

	// Completed and Spam tabs must not render bulk assign or spam actions.
	spamID := uuid.New()
	store.threads[spamID] = sqlc.Thread{ID: spamID, AccountID: "support", Status: sqlc.ThreadStatusSpam, SubjectDisplay: "Spam item", LastMessageAt: time.Now()}
	for _, status := range []string{"completed", "spam"} {
		tabRec := httptest.NewRecorder()
		router.ServeHTTP(tabRec, httptest.NewRequest(http.MethodGet, "/threads?status="+status, nil))
		tabBody := tabRec.Body.String()
		if strings.Contains(tabBody, `id="bulk-spam-form"`) || strings.Contains(tabBody, `formaction="/threads/bulk-assign"`) || strings.Contains(tabBody, `formaction="/threads/bulk-complete"`) || strings.Contains(tabBody, `Mark selected as spam`) {
			t.Fatalf("%s tab must not render bulk actions: %s", status, tabBody)
		}
	}
}

func TestQueueCompleteRowAndBulkComplete(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	rowNewID, bulkNewID, bulkInProgressID, spamID, completedID, privateID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, entry := range []struct {
		id      uuid.UUID
		account string
		status  sqlc.ThreadStatus
		subject string
	}{
		{rowNewID, "support", sqlc.ThreadStatusNew, "Row complete item"},
		{bulkNewID, "support", sqlc.ThreadStatusNew, "Bulk new item"},
		{bulkInProgressID, "sales", sqlc.ThreadStatusInProgress, "Bulk progress item"},
		{spamID, "support", sqlc.ThreadStatusSpam, "Spam item"},
		{completedID, "support", sqlc.ThreadStatusCompleted, "Already done"},
		{privateID, "secret", sqlc.ThreadStatusNew, "Secret item"},
	} {
		store.threads[entry.id] = sqlc.Thread{ID: entry.id, AccountID: entry.account, Status: entry.status, SubjectDisplay: entry.subject, LastMessageAt: time.Now()}
		uid, inbox := int64(77), "INBOX"
		em := sqlc.Email{ID: uuid.New(), ThreadID: entry.id, AccountID: entry.account, Direction: "inbound", IMAPUID: &uid, IMAPMailbox: &inbox}
		store.emails[em.ID] = em
	}

	page := "/threads?status=open&q=item"
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, page, nil))
	listBody := listRec.Body.String()
	for _, want := range []string{
		`data-row-menu`,
		`aria-label="Actions for Row complete item"`,
		`aria-label="Complete Row complete item"`,
		`aria-label="Mark Row complete item as spam"`,
		`aria-label="Complete Bulk new item"`,
		`aria-label="Complete Bulk progress item"`,
		`formaction="/threads/bulk-complete"`,
		`>Complete selected<`,
	} {
		if !strings.Contains(listBody, want) {
			t.Fatalf("queue missing %q: %s", want, listBody)
		}
	}
	if !strings.Contains(listBody, `<th scope="col">Thread</th>`) {
		t.Fatalf("expected a Thread column in the inbox header: %s", listBody)
	}
	// Account and assignee live in the merged metadata line under the subject, not
	// in their own columns, so the row stays two lines on desktop.
	if !strings.Contains(listBody, `class="thread-meta"`) {
		t.Fatalf("expected the merged thread metadata line: %s", listBody)
	}

	// Row complete on a new thread uses CompleteFromNew, records 2 thread_events, queues 1 move_to_archive, and redirects to return_to.
	rowForm := url.Values{"status": {"completed"}, "complete_from_new": {"1"}, "return_to": {page}}
	rowReq := httptest.NewRequest(http.MethodPost, "/threads/"+rowNewID.String()+"/transition", strings.NewReader(rowForm.Encode()))
	rowReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rowRec := httptest.NewRecorder()
	router.ServeHTTP(rowRec, rowReq)
	if rowRec.Code != http.StatusSeeOther || rowRec.Header().Get("Location") != page {
		t.Fatalf("row complete redirect: got %d %q", rowRec.Code, rowRec.Header().Get("Location"))
	}
	if store.threads[rowNewID].Status != sqlc.ThreadStatusCompleted {
		t.Fatalf("expected rowNewID completed, got %s", store.threads[rowNewID].Status)
	}
	var rowEvents []sqlc.ThreadEvent
	for _, ev := range store.threadEvents {
		if ev.ThreadID == rowNewID && ev.EventType == "status_changed" {
			rowEvents = append(rowEvents, ev)
		}
	}
	if len(rowEvents) != 2 || !strings.Contains(string(rowEvents[0].OldValue), `"new"`) || !strings.Contains(string(rowEvents[0].NewValue), `"in_progress"`) ||
		!strings.Contains(string(rowEvents[1].OldValue), `"in_progress"`) || !strings.Contains(string(rowEvents[1].NewValue), `"completed"`) {
		t.Fatalf("expected 2 composite status_changed events for rowNewID, got %+v", rowEvents)
	}
	var rowArchiveJobs int
	for _, job := range store.jobs {
		if job.AccountID == "support" && job.JobType == "move_to_archive" {
			rowArchiveJobs++
		}
	}
	if rowArchiveJobs != 1 {
		t.Fatalf("expected 1 move_to_archive job for rowNewID, got %d", rowArchiveJobs)
	}

	// Bulk complete handles new (CompleteFromNew), in_progress (Transition), and rejects spam, completed, and inaccessible threads.
	bulkForm := url.Values{
		"thread_id": {bulkNewID.String(), bulkInProgressID.String(), spamID.String(), completedID.String(), privateID.String()},
		"return_to": {page},
	}
	bulkReq := httptest.NewRequest(http.MethodPost, "/threads/bulk-complete", strings.NewReader(bulkForm.Encode()))
	bulkReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bulkRec := httptest.NewRecorder()
	router.ServeHTTP(bulkRec, bulkReq)
	if bulkRec.Code != http.StatusSeeOther || bulkRec.Header().Get("Location") != page {
		t.Fatalf("bulk complete redirect: got %d %q", bulkRec.Code, bulkRec.Header().Get("Location"))
	}
	if store.threads[bulkNewID].Status != sqlc.ThreadStatusCompleted || store.threads[bulkInProgressID].Status != sqlc.ThreadStatusCompleted {
		t.Fatalf("expected bulk open threads completed, got new=%s in_progress=%s", store.threads[bulkNewID].Status, store.threads[bulkInProgressID].Status)
	}
	if store.threads[spamID].Status != sqlc.ThreadStatusSpam || store.threads[privateID].Status != sqlc.ThreadStatusNew {
		t.Fatalf("unexpected mutation on spam/private threads: spam=%s private=%s", store.threads[spamID].Status, store.threads[privateID].Status)
	}
	follow := httptest.NewRequest(http.MethodGet, page, nil)
	for _, cookie := range bulkRec.Result().Cookies() {
		follow.AddCookie(cookie)
	}
	followRec := httptest.NewRecorder()
	router.ServeHTTP(followRec, follow)
	followBody := followRec.Body.String()
	if !strings.Contains(followBody, "2 completed and queued for Archive; 3 failed.") || !strings.Contains(followBody, privateID.String()+": unavailable") {
		t.Fatalf("unexpected bulk complete summary: %s", followBody)
	}
}

func TestInboxAssignToMePreservesPageAndEnforcesAccess(t *testing.T) {
	_, store, router, user, _ := buildAccountTestServer(t)
	id := uuid.New()
	completedID := uuid.New()
	spamID := uuid.New()
	store.threads[id] = sqlc.Thread{ID: id, AccountID: "support", SubjectDisplay: "Needs attention", Status: sqlc.ThreadStatusNew, LastMessageAt: time.Now().UTC()}
	store.threads[completedID] = sqlc.Thread{ID: completedID, AccountID: "support", SubjectDisplay: "Done item", Status: sqlc.ThreadStatusCompleted, LastMessageAt: time.Now().UTC()}
	store.threads[spamID] = sqlc.Thread{ID: spamID, AccountID: "support", SubjectDisplay: "Junk item", Status: sqlc.ThreadStatusSpam, LastMessageAt: time.Now().UTC()}
	page := "/threads?status=new&q=attention"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, page, nil))
	if !strings.Contains(rec.Body.String(), `aria-label="Assign Needs attention to me"`) {
		t.Fatal("missing queue assignment control")
	}
	for _, status := range []string{"completed", "spam"} {
		statusRec := httptest.NewRecorder()
		router.ServeHTTP(statusRec, httptest.NewRequest(http.MethodGet, "/threads?status="+status, nil))
		if strings.Contains(statusRec.Body.String(), `>Assign to me<`) {
			t.Fatalf("%s list must not offer Assign to me: %s", status, statusRec.Body.String())
		}
	}
	form := url.Values{"assignee_id": {user.ID.String()}, "return_to": {page}}
	req := httptest.NewRequest(http.MethodPost, "/threads/"+id.String()+"/assign", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != page || store.threads[id].AssigneeID == nil || *store.threads[id].AssigneeID != user.ID {
		t.Fatalf("assignment failed: %d location=%s thread=%+v", rec.Code, rec.Header().Get("Location"), store.threads[id])
	}
	privateID := uuid.New()
	store.threads[privateID] = sqlc.Thread{ID: privateID, AccountID: "secret", Status: sqlc.ThreadStatusNew}
	req = httptest.NewRequest(http.MethodPost, "/threads/"+privateID.String()+"/assign", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || store.threads[privateID].AssigneeID != nil {
		t.Fatal("private assignment was not rejected")
	}
}

func TestCompletedThreadAssignmentDisabledAndRejected(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Completed thread",
		Status: sqlc.ThreadStatusCompleted, AssigneeID: &multiUser.ID, CustomerEmail: "a@client.com",
		LastMessageAt: time.Now().UTC(),
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `for="assignee-`) || strings.Contains(body, `>Assign to me<`) || strings.Contains(body, `>Assign<`) {
		t.Fatalf("completed thread must not render assignment controls: %s", body)
	}
	if !strings.Contains(body, "Replies and assignment are disabled") {
		t.Fatalf("expected completed helper to explain disabled assignment: %s", body)
	}

	form := url.Values{"assignee_id": {multiUser.ID.String()}}
	req := httptest.NewRequest(http.MethodPost, "/threads/"+threadID.String()+"/assign", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected forged assign on completed thread to be rejected with 422, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Completed threads cannot be reassigned") {
		t.Fatalf("expected completed-assignment error, got: %s", rec.Body.String())
	}
}

func TestMergedFollowUpTimelineSection(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Reopened thread",
		Status: sqlc.ThreadStatusInProgress, AssigneeID: &multiUser.ID, CustomerEmail: "a@client.com",
		LastMessageAt: time.Now().UTC(),
	}
	base := time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)
	store.threadEvents = append(store.threadEvents,
		sqlc.ThreadEvent{ID: uuid.New(), ThreadID: threadID, EventType: "created", OccurredAt: base},
		sqlc.ThreadEvent{ID: uuid.New(), ThreadID: threadID, EventType: "created", OccurredAt: base.Add(48 * time.Hour)},
		sqlc.ThreadEvent{
			ID: uuid.New(), ThreadID: threadID, EventType: "reopen_merged",
			ActorUserID: &multiUser.ID, OccurredAt: base.Add(72 * time.Hour),
			OldValue: []byte(`{"merged_from_thread_id":"` + uuid.NewString() + `"}`),
		},
	)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if got := strings.Count(body, "Follow-up thread"); got != 1 {
		t.Fatalf("expected exactly one follow-up timeline section, got %d: %s", got, body)
	}
	if !strings.Contains(body, "reopened and merged a follow-up reply") {
		t.Fatalf("expected merge event in timeline: %s", body)
	}
}

func TestLayoutAccessibilityAndStaticAssets(t *testing.T) {
	admin := sqlc.User{ID: uuid.New(), Email: "admin@example.com", DisplayName: "Admin", Role: "admin", IsActive: true}
	var buf bytes.Buffer
	if err := views.RenderInbox(&buf, views.InboxViewData{
		Title: "Threads", ActiveNav: "threads", CurrentUser: admin, DisplayTZ: "UTC",
	}, false); err != nil {
		t.Fatalf("RenderInbox: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		`<meta name="color-scheme" content="light">`,
		`<link rel="icon" type="image/svg+xml" href="/static/favicon.svg">`,
		`class="skip-link" href="#main-content"`,
		`id="main-content"`,
		`id="confirm-dialog"`,
		`data-confirm-accept`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in layout: %s", want, body)
		}
	}

	js, err := static.FS.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read embedded app.js: %v", err)
	}
	if strings.Contains(string(js), "window.confirm") {
		t.Fatalf("app.js must not use window.confirm")
	}
	if !strings.Contains(string(js), "showModal") || !strings.Contains(string(js), "requestSubmit") {
		t.Fatalf("app.js must confirm via <dialog> and keep the form submit path")
	}
	icon, err := static.FS.ReadFile("favicon.svg")
	if err != nil || !strings.Contains(string(icon), "<svg") {
		t.Fatalf("expected embedded favicon.svg, err=%v", err)
	}
}

func TestInboxDefaultsToOpenQueueAndShowsSnippets(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)
	for name, status := range map[string]sqlc.ThreadStatus{
		"Open one": sqlc.ThreadStatusNew, "Done one": sqlc.ThreadStatusCompleted, "Junk one": sqlc.ThreadStatusSpam,
	} {
		id := uuid.New()
		store.threads[id] = sqlc.Thread{
			ID: id, AccountID: "support", SubjectDisplay: name, Status: status, AssigneeID: &multiUser.ID,
			CustomerEmail: "a@client.com", LastMessageAt: time.Now().UTC(),
			MessageCount: 3, LatestBodyText: "Latest   words\nhere", LatestDirection: "outbound",
			OpenedAt: time.Now().UTC().Add(-50 * time.Hour),
		}
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Open one") || strings.Contains(body, "Done one") || strings.Contains(body, "Junk one") {
		t.Fatalf("default inbox must only list open threads: %s", body)
	}
	for _, want := range []string{"Latest words here", "You:", `class="count-pill"`, "Open 2d", "wait-warning"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in inbox: %s", want, body)
		}
	}
	if strings.Contains(body, "Europe/Amsterdam") || strings.Contains(body, "</time>CEST") || strings.Contains(body, "00 CEST</") {
		t.Fatalf("inbox must not repeat the timezone: %s", body)
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=all", nil))
	body = rec.Body.String()
	if !strings.Contains(body, "Done one") || !strings.Contains(body, "Junk one") {
		t.Fatalf("status=all must list every thread: %s", body)
	}
}

func TestExtractSnippetAndWaitDuration(t *testing.T) {
	if got := views.ExtractEmailSnippet("", "<p>Hello &amp; <b>welcome</b></p>", 50); got != "Hello & welcome" {
		t.Fatalf("html snippet = %q", got)
	}
	if got := views.ExtractEmailSnippet("abcdefghij", "", 5); got != "abcde…" {
		t.Fatalf("truncated snippet = %q", got)
	}
	now := time.Now()
	if label, tier := views.FormatWaitDuration(now.Add(-5*time.Hour), now); label != "Waiting 5h" || tier != "warning" {
		t.Fatalf("wait = %q %q", label, tier)
	}
}

func TestAttachmentPreviewAndViewEndpoints(t *testing.T) {
	store := newTestWebStore()
	auditLogger := audit.NewLogger(store, nil)
	attStore, err := storage.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wfSvc := workflow.NewService(store, auditLogger, nil)
	outbox := mail.NewOutboxWorker(nil, attStore, []mail.AccountSenderConfig{
		{AccountID: "support", FromAddress: "support@example.com", FromName: "Support", Transport: &noopSMTP{}},
	}, auditLogger, "example.com", nil, nil)
	sm := auth.NewSessionManager(false)
	authSvc := auth.NewService(store, auditLogger, sm, nil, "admin@example.com")
	srv := web.NewServer(store, authSvc, wfSvc, outbox, attStore, auditLogger, sm, testAccounts())
	multiUser := store.adminUser

	mux := web.SecurityHeadersMiddleware(sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithUser(r.Context(), multiUser))
		switch {
		case strings.HasPrefix(r.URL.Path, "/threads/"):
			srv.HandleViewThread(w, web.WithChiParam(r, "id", strings.TrimPrefix(r.URL.Path, "/threads/")))
		case strings.HasPrefix(r.URL.Path, "/emails/"):
			srv.HandleIsolatedEmailBody(w, web.WithChiParam(r, "id", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/emails/"), "/body")))
		case strings.HasSuffix(r.URL.Path, "/view"):
			srv.HandleViewAttachment(w, web.WithChiParam(r, "id", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/attachments/"), "/view")))
		case strings.HasSuffix(r.URL.Path, "/download"):
			srv.HandleDownloadAttachment(w, web.WithChiParam(r, "id", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/attachments/"), "/download")))
		}
	})))

	blob, err := attStore.Save(context.Background(), strings.NewReader("fake-blob"))
	if err != nil {
		t.Fatal(err)
	}

	threadID := uuid.New()
	emailID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Attachments thread",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "erin@customer.example", LastMessageAt: time.Now().UTC(),
	}
	store.emails[emailID] = sqlc.Email{
		ID: emailID, ThreadID: threadID, AccountID: "support", Direction: "inbound",
		FromAddress:       "erin@customer.example",
		BodyHTMLSanitized: `<p><img src="cid:logo@acme" alt="Logo"></p>`,
		SentAt:            time.Now().UTC(),
	}

	inlineLogoAtt := uuid.New()
	store.attachments[inlineLogoAtt] = sqlc.Attachment{
		ID: inlineLogoAtt, EmailID: emailID, Filename: "logo.png", ContentType: "image/png",
		SizeBytes: 512, ContentID: "logo@acme", IsInline: true, StorageRelPath: blob.StorageRelPath,
	}
	pngAtt := uuid.New()
	store.attachments[pngAtt] = sqlc.Attachment{
		ID: pngAtt, EmailID: emailID, Filename: "screenshot.png", ContentType: "image/png",
		SizeBytes: 1024, StorageRelPath: blob.StorageRelPath,
	}
	pdfAtt := uuid.New()
	store.attachments[pdfAtt] = sqlc.Attachment{
		ID: pdfAtt, EmailID: emailID, Filename: "receipt.pdf", ContentType: "application/pdf",
		SizeBytes: 2048, StorageRelPath: blob.StorageRelPath,
	}
	svgAtt := uuid.New()
	store.attachments[svgAtt] = sqlc.Attachment{
		ID: svgAtt, EmailID: emailID, Filename: "unsafe.svg", ContentType: "image/svg+xml",
		SizeBytes: 512, StorageRelPath: blob.StorageRelPath,
	}

	// 1. Thread detail lists non-inline attachments with thumbnails and icon actions, omitting the inline CID logo.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("thread detail status %d", rec.Code)
	}
	detailHTML := rec.Body.String()
	for _, want := range []string{
		`<img class="attachment-preview-img" src="/attachments/` + pngAtt.String() + `/view"`,
		`aria-label="View screenshot.png"`,
		`aria-label="Download screenshot.png"`,
		`Image &middot; 1.0 KB`,
		`PDF document &middot; 2.0 KB`,
		`href="/attachments/` + pdfAtt.String() + `/view"`,
		`href="/attachments/` + pdfAtt.String() + `/download"`,
		`href="/attachments/` + svgAtt.String() + `/download"`,
	} {
		if !strings.Contains(detailHTML, want) {
			t.Fatalf("expected %q in thread detail HTML: %s", want, detailHTML)
		}
	}
	if strings.Contains(detailHTML, inlineLogoAtt.String()) || strings.Contains(detailHTML, "logo.png") {
		t.Fatalf("inline CID image referenced by <img> must not be listed as an attachment: %s", detailHTML)
	}
	if strings.Contains(detailHTML, `/attachments/`+svgAtt.String()+`/view`) {
		t.Fatalf("unsafe SVG attachment must not have an inline view link: %s", detailHTML)
	}

	// 2. Isolated email body resolves cid:logo@acme to /attachments/<inlineLogoAtt>/view.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/emails/"+emailID.String()+"/body", nil))
	if !strings.Contains(rec.Body.String(), `src="/attachments/`+inlineLogoAtt.String()+`/view"`) {
		t.Fatalf("expected cid: image resolved to attachment view URL, got: %s", rec.Body.String())
	}

	// 3. /attachments/{id}/view serves PNG and PDF inline, but forces attachment download for SVG.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/attachments/"+pngAtt.String()+"/view", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "inline;") || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("unexpected PNG view headers: %+v", rec.Header())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/attachments/"+pdfAtt.String()+"/view", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "inline;") || rec.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("unexpected PDF view headers: %+v", rec.Header())
	}
	if rec.Header().Get("Cross-Origin-Embedder-Policy") != "" {
		t.Fatalf("PDF inline view must omit COEP so browser PDF viewers can load: %+v", rec.Header())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/attachments/"+svgAtt.String()+"/view", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("unsafe SVG on /view must fall back to Content-Disposition: attachment, got: %+v", rec.Header())
	}
}

func TestExternalReplyEventSummaries(t *testing.T) {
	if got := views.FormatThreadEventSummary(sqlc.ThreadEvent{EventType: "reply_sent_external"}, "", nil); got != "A reply was sent from another mail client." {
		t.Fatalf("reply_sent_external summary = %q", got)
	}
	created := sqlc.ThreadEvent{EventType: "created", NewValue: []byte(`{"origin":"outbound"}`)}
	if got := views.FormatThreadEventSummary(created, "", nil); got != "A message sent from another mail client opened this thread." {
		t.Fatalf("outbound-origin created summary = %q", got)
	}
	plain := sqlc.ThreadEvent{EventType: "created", NewValue: []byte(`{}`)}
	if got := views.FormatThreadEventSummary(plain, "", nil); got != "The customer opened this thread." {
		t.Fatalf("created summary = %q", got)
	}
}

// After a reply sent from another mail client the thread is in progress,
// shows the "You:" snippet, and displays its thread-age badge based on opened_at.
func TestInboxReflectsExternalReply(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)
	id := uuid.New()
	now := time.Now().UTC()
	store.threads[id] = sqlc.Thread{
		ID: id, AccountID: "support", SubjectDisplay: "Answered elsewhere", Status: sqlc.ThreadStatusInProgress,
		AssigneeID: &multiUser.ID, CustomerEmail: "a@client.com",
		LastMessageAt: now.Add(-5 * time.Minute), OpenedAt: now.Add(-72 * time.Hour),
		MessageCount: 2, LatestDirection: "outbound", LatestBodyText: "Sent from Thunderbird",
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads", nil))
	body := rec.Body.String()
	for _, want := range []string{"Answered elsewhere", "You:", "Sent from Thunderbird", "Open 3d", "wait-warning"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in inbox: %s", want, body)
		}
	}
	if strings.Contains(body, "Waiting") {
		t.Fatalf("an answered thread must not show a legacy waiting badge: %s", body)
	}
}

func TestPWAManifestServiceWorkerAndCacheHeaders(t *testing.T) {
	srv, store, _, _, _ := buildAccountTestServer(t)
	srv.WithBranding(web.AppBranding{
		Name:        "Acme Support CRM",
		ShortName:   "AcmeCRM",
		Description: "Acme customer support workspace",
	})
	auditLogger := audit.NewLogger(store, nil)
	sm := auth.NewSessionManager(false)
	authSvc := auth.NewService(store, auditLogger, sm, nil, "admin@example.com")
	router := web.NewRouter(srv, authSvc, sm)

	// 1. CSP includes manifest-src 'self' and worker-src 'self'
	if !strings.Contains(web.MainCSP, "manifest-src 'self'; worker-src 'self'") {
		t.Fatalf("MainCSP missing manifest-src/worker-src: %s", web.MainCSP)
	}
	if !strings.Contains(web.MainCSPInsecure, "manifest-src 'self'; worker-src 'self'") {
		t.Fatalf("MainCSPInsecure missing manifest-src/worker-src: %s", web.MainCSPInsecure)
	}

	// 2. Unauthenticated GET /manifest.webmanifest succeeds and reflects custom branding
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /manifest.webmanifest status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/manifest+json") {
		t.Fatalf("unexpected manifest Content-Type: %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("unexpected manifest Cache-Control: %q, want no-cache", got)
	}
	var manifest map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("unmarshal manifest JSON: %v", err)
	}
	if manifest["name"] != "Acme Support CRM" || manifest["short_name"] != "AcmeCRM" || manifest["description"] != "Acme customer support workspace" {
		t.Fatalf("unexpected manifest branding fields: %+v", manifest)
	}
	if manifest["display"] != "standalone" || manifest["start_url"] != "/threads" {
		t.Fatalf("unexpected manifest display/start_url: %+v", manifest)
	}

	// 3. Unauthenticated GET /sw.js succeeds with Service-Worker-Allowed: / and Cache-Control: no-cache
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sw.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sw.js status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/javascript") {
		t.Fatalf("unexpected sw.js Content-Type: %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("unexpected sw.js Cache-Control: %q, want no-cache", got)
	}
	if got := rec.Header().Get("Service-Worker-Allowed"); got != "/" {
		t.Fatalf("unexpected Service-Worker-Allowed: %q, want /", got)
	}

	// 4. Static assets and icons under /static/* receive public cache headers
	for _, asset := range []string{
		"/static/app.css",
		"/static/app.js",
		"/static/icon-192.png",
		"/static/icon-512.png",
		"/static/icon-maskable-512.png",
		"/static/apple-touch-icon.png",
		"/static/offline.html",
	} {
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, asset, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", asset, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600, must-revalidate" {
			t.Fatalf("GET %s Cache-Control = %q, want public, max-age=3600, must-revalidate", asset, got)
		}
	}
}

func TestThreadDetailBackURLAndRefreshControls(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	now := time.Now().UTC()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Filter preservation test",
		Status: sqlc.ThreadStatusNew, AssigneeID: &multiUser.ID, CustomerEmail: "c@example.com",
		LastMessageAt: now, OpenedAt: now,
	}

	// 1. Inbox renders the refresh affordance with active filter params and head PWA metadata
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=new&assignee=me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /threads status = %d", rec.Code)
	}
	inboxHTML := rec.Body.String()
	for _, want := range []string{
		`<link rel="manifest" href="/manifest.webmanifest">`,
		`<link rel="apple-touch-icon" href="/static/apple-touch-icon.png">`,
		`<meta name="theme-color" content="#14142b">`,
		`<h1 class="sr-only">Threads</h1>`,
		`href="/threads?status=new&amp;assignee=me" data-refresh-link`,
		`data-refresh-status`,
	} {
		if !strings.Contains(inboxHTML, want) {
			t.Fatalf("expected %q in inbox HTML", want)
		}
	}
	if strings.Contains(inboxHTML, `<h1>Threads</h1>`) {
		t.Fatalf("inbox should not render a visible Threads heading")
	}

	// 2. Thread detail preserves BackURL from same-origin /threads?... Referer and rejects cross-origin Referer
	req := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String(), nil)
	req.Host = "crm.example.com"
	req.Header.Set("Referer", "https://crm.example.com/threads?status=new&assignee=me")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /threads/{id} status = %d", rec.Code)
	}
	detailHTML := rec.Body.String()
	if !strings.Contains(detailHTML, `href="/threads?status=new&amp;assignee=me"`) {
		t.Fatalf("expected Back to threads link to preserve filter query params, got: %s", detailHTML)
	}
	if !strings.Contains(detailHTML, `href="/threads/`+threadID.String()+`">Refresh</a>`) {
		t.Fatalf("expected Refresh button on thread detail page")
	}

	// Cross-origin Referer must be ignored and fall back to /threads
	reqEvil := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String(), nil)
	reqEvil.Host = "crm.example.com"
	reqEvil.Header.Set("Referer", "https://evil.example.com/threads?status=new")
	recEvil := httptest.NewRecorder()
	router.ServeHTTP(recEvil, reqEvil)
	if !strings.Contains(recEvil.Body.String(), `class="back-link" href="/threads"`) {
		t.Fatalf("expected cross-origin Referer to fall back to /threads, got: %s", recEvil.Body.String())
	}
}

func TestThreadAgeBadge(t *testing.T) {
	// Reference time: 2026-10-29 01:30:00 CET (which is 2026-10-29 00:30:00 UTC)
	now := time.Date(2026, 10, 29, 0, 30, 0, 0, time.UTC)

	tests := []struct {
		name        string
		status      sqlc.ThreadStatus
		openedAt    time.Time
		now         time.Time // optional; zero means the shared "now" reference time
		displayTZ   string
		wantLabel   string
		wantUrgency string
	}{
		{
			name:        "new thread opened today in display timezone",
			status:      sqlc.ThreadStatusNew,
			openedAt:    time.Date(2026, 10, 28, 23, 15, 0, 0, time.UTC), // 2026-10-29 00:15 CET (same local calendar day as now)
			displayTZ:   "Europe/Amsterdam",
			wantLabel:   "Today",
			wantUrgency: "fresh",
		},
		{
			name:        "same timestamp in America/New_York is still Oct 28 for both opened and now",
			status:      sqlc.ThreadStatusNew,
			openedAt:    time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), // Oct 28 08:00 EDT; now is Oct 28 20:30 EDT (same local day)
			displayTZ:   "America/New_York",
			wantLabel:   "Today",
			wantUrgency: "fresh",
		},
		{
			name:        "timezone boundary: Oct 28 22:30 UTC is Oct 28 23:30 CET vs now Oct 29 01:30 CET -> Open 1d",
			status:      sqlc.ThreadStatusNew,
			openedAt:    time.Date(2026, 10, 28, 22, 30, 0, 0, time.UTC),
			displayTZ:   "Europe/Amsterdam",
			wantLabel:   "Open 1d",
			wantUrgency: "warning",
		},
		{
			name:        "in_progress thread opened 3 days ago gets warning badge",
			status:      sqlc.ThreadStatusInProgress,
			openedAt:    time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC),
			displayTZ:   "Europe/Amsterdam",
			wantLabel:   "Open 3d",
			wantUrgency: "warning",
		},
		{
			name:        "in_progress thread opened 7 days ago gets warning badge (boundary)",
			status:      sqlc.ThreadStatusInProgress,
			openedAt:    time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC),
			displayTZ:   "Europe/Amsterdam",
			wantLabel:   "Open 7d",
			wantUrgency: "warning",
		},
		{
			name:        "in_progress thread opened 8 days ago gets overdue urgent badge",
			status:      sqlc.ThreadStatusInProgress,
			openedAt:    time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
			displayTZ:   "Europe/Amsterdam",
			wantLabel:   "Overdue · 8d",
			wantUrgency: "urgent",
		},
		{
			name:        "completed thread gets no age badge",
			status:      sqlc.ThreadStatusCompleted,
			openedAt:    time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
			displayTZ:   "Europe/Amsterdam",
			wantLabel:   "",
			wantUrgency: "",
		},
		{
			// Spring-forward DST: on 2026-03-29 the Amsterdam clock jumps 02:00 CET -> 03:00 CEST,
			// so this local day is only 23 hours long. now = 2026-03-29 01:30 UTC = 03:30 CEST
			// (local Mar 29); openedAt = 2026-03-27 23:30 UTC = Mar 28 00:30 CET (local Mar 28).
			// Calendar-day bucketing counts Mar 29 - Mar 28 = 1 day: the skipped hour must not
			// inflate the label to "Open 2d".
			name:        "spring-forward DST transition does not off-by-one the day count",
			now:         time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC), // 03:30 CEST in Amsterdam
			status:      sqlc.ThreadStatusNew,
			openedAt:    time.Date(2026, 3, 27, 23, 30, 0, 0, time.UTC),
			displayTZ:   "Europe/Amsterdam",
			wantLabel:   "Open 1d",
			wantUrgency: "warning",
		},
		{
			// Fall-back DST: 2026-10-25 the Amsterdam clock repeats 02:00-03:00 CEST->CET, making
			// that local day 25 hours. now = 2026-10-26 00:30 UTC = 01:30 CET (Oct 26);
			// openedAt = 2026-10-24 23:30 UTC = Oct 25 01:30 CEST (Oct 25) -> "Open 1d".
			name:        "fall-back DST transition does not off-by-one the day count",
			now:         time.Date(2026, 10, 26, 0, 30, 0, 0, time.UTC), // 01:30 CET in Amsterdam
			status:      sqlc.ThreadStatusNew,
			openedAt:    time.Date(2026, 10, 24, 23, 30, 0, 0, time.UTC),
			displayTZ:   "Europe/Amsterdam",
			wantLabel:   "Open 1d",
			wantUrgency: "warning",
		},
		{
			name:        "invalid timezone string falls back to UTC",
			status:      sqlc.ThreadStatusNew,
			openedAt:    time.Date(2026, 10, 28, 23, 15, 0, 0, time.UTC), // UTC day Oct 28; now is Oct 29 00:30 UTC
			displayTZ:   "Not/AZone",
			wantLabel:   "Open 1d",
			wantUrgency: "warning",
		},
		{
			name:        "spam thread gets no age badge",
			status:      sqlc.ThreadStatusSpam,
			openedAt:    time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
			displayTZ:   "Europe/Amsterdam",
			wantLabel:   "",
			wantUrgency: "",
		},
	}

	for _, tc := range tests {
		tc := tc
		caseNow := now
		if !tc.now.IsZero() {
			caseNow = tc.now
		}
		t.Run(tc.name, func(t *testing.T) {
			gotLabel, gotUrgency := views.ThreadAgeBadge(tc.status, tc.openedAt, caseNow, tc.displayTZ)
			if gotLabel != tc.wantLabel || gotUrgency != tc.wantUrgency {
				t.Fatalf("ThreadAgeBadge(%q) = (%q, %q), want (%q, %q)",
					tc.status, gotLabel, gotUrgency, tc.wantLabel, tc.wantUrgency)
			}
		})
	}
}

func TestSettingsAttentionAgeDaysRange(t *testing.T) {
	_, store, router, user, _ := buildAccountTestServer(t)
	_ = user

	for name, value := range map[string]int{"zero": 0, "too large": 10000} {
		t.Run(name, func(t *testing.T) {
			form := url.Values{
				"display_timezone":          {"Europe/Amsterdam"},
				"subject_match_window_days": {"30"},
				"attention_age_days":        {fmt.Sprintf("%d", value)},
				"imap_inbox_mailbox":        {"INBOX"},
				"imap_archive_mailbox":      {"Archive"},
				"imap_junk_mailbox":         {"Junk"},
				"imap_sent_mailbox":         {"Sent"},
			}
			req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST /settings attention_age_days=%d status = %d, want 400", value, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "Needs attention threshold must be between 1 and 365 days.") {
				t.Fatalf("expected range validation message in response: %s", rec.Body.String())
			}
			if store.settings.AttentionAgeDays != 2 {
				t.Fatalf("settings must be unchanged on validation failure, got AttentionAgeDays=%d", store.settings.AttentionAgeDays)
			}
		})
	}

	// Boundary values are accepted.
	for _, value := range []int{1, 365} {
		form := url.Values{
			"display_timezone":          {"Europe/Amsterdam"},
			"subject_match_window_days": {"30"},
			"attention_age_days":        {fmt.Sprintf("%d", value)},
			"imap_inbox_mailbox":        {"INBOX"},
			"imap_archive_mailbox":      {"Archive"},
			"imap_junk_mailbox":         {"Junk"},
			"imap_sent_mailbox":         {"Sent"},
		}
		req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("POST /settings attention_age_days=%d status = %d, want 303", value, rec.Code)
		}
		if store.settings.AttentionAgeDays != value {
			t.Fatalf("store.settings.AttentionAgeDays = %d, want %d", store.settings.AttentionAgeDays, value)
		}
	}
}

// findAttentionBeforeLink extracts the next-page "Older threads" anchor. Only the anchor
// tag counts: hidden return_to inputs and the Refresh button embed the refresh URL, which
// carries the same before/id params.
func findAttentionBeforeLink(body string) []string {
	return regexp.MustCompile(`<a class="btn btn-secondary" href="(/threads\?[^"]*)">Older threads</a>`).FindStringSubmatch(body)
}

func TestNeedsAttentionPagination(t *testing.T) {
	// The router in buildAccountTestServer closes over its own server variable, so the
	// page size must be set via this hook before the server is built.
	testInboxPageSize = 2
	t.Cleanup(func() { testInboxPageSize = 0 })
	_, store, router, _, _ := buildAccountTestServer(t)

	now := time.Now().UTC()
	// Three stale new threads; attention filtering keys on OpenedAt while the pagination
	// cursor keys on (LastMessageAt, ID), so make the two orderings differ.
	type spec struct {
		name       string
		opened, ln time.Time
	}
	specs := []spec{
		{"Attention A", now.Add(-72 * time.Hour), now.Add(-1 * time.Hour)},
		{"Attention B", now.Add(-60 * time.Hour), now.Add(-2 * time.Hour)},
		{"Attention C", now.Add(-48 * time.Hour), now.Add(-3 * time.Hour)},
	}
	ids := map[string]uuid.UUID{}
	for _, sp := range specs {
		id := uuid.New()
		ids[sp.name] = id
		store.threads[id] = sqlc.Thread{
			ID: id, AccountID: "support", SubjectDisplay: sp.name,
			Status: sqlc.ThreadStatusNew, CustomerEmail: "p@example.com",
			LastMessageAt: sp.ln, OpenedAt: sp.opened,
		}
	}

	req := func(url string) string {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, url, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d: %s", url, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	page1 := req("/threads?status=attention")
	if !strings.Contains(page1, "Attention A") || !strings.Contains(page1, "Attention B") {
		t.Fatalf("page 1 must show the two most recent threads: %s", page1)
	}
	if strings.Contains(page1, "Attention C") {
		t.Fatalf("page 1 must not show the third thread: %s", page1)
	}
	m := findAttentionBeforeLink(page1)
	if m == nil {
		t.Fatalf("page 1 must contain a next-page link: %s", page1)
	}

	page2 := req(strings.ReplaceAll(m[1], "&amp;", "&"))
	if !strings.Contains(page2, "Attention C") {
		t.Fatalf("page 2 must continue with the remaining thread: %s", page2)
	}
	if strings.Contains(page2, "Attention A") || strings.Contains(page2, "Attention B") {
		t.Fatalf("page 2 must not repeat page-1 threads: %s", page2)
	}
	// Page 2 is the last page: it must not offer a next-page link, and the cursor from
	// page 2 renders an empty page without error.
	if m2 := findAttentionBeforeLink(page2); m2 != nil {
		t.Fatalf("page 2 must not have a next link: %s", page2)
	}
	page3 := req("/threads?status=attention&before=" + url.QueryEscape(now.Add(-48*time.Hour).Format(time.RFC3339Nano)) + "&id=" + ids["Attention C"].String())
	if strings.Contains(page3, "Attention A") || strings.Contains(page3, "Attention B") || strings.Contains(page3, "Attention C") {
		t.Fatalf("page 3 must be empty: %s", page3)
	}
}

func TestCountThreadsByTabErrorStillRendersInbox(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	store.threads[uuid.New()] = sqlc.Thread{
		ID: uuid.New(), AccountID: "support", SubjectDisplay: "Plain New",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "x@example.com",
		LastMessageAt: time.Now().UTC().Add(-time.Hour),
	}
	store.countThreadsErr = fmt.Errorf("db unavailable")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads", nil))
	// The inbox must still render the thread list (200) — tab counts degrade to 0
	// rather than failing the whole page — but the failure must not be silent.
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /threads status = %d, want 200 despite tab-count failure: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Plain New") {
		t.Fatalf("expected thread list to still render: %s", rec.Body.String())
	}
}

func TestNeedsAttentionFilterAndTabCounts(t *testing.T) {
	_, store, router, multiUser, supportOnlyUser := buildAccountTestServer(t)
	now := time.Now().UTC()

	// Configure attention threshold to 2 days (default) and verify settings update can change it.
	otherUserID := supportOnlyUser.ID

	threads := []sqlc.Thread{
		{
			ID: uuid.New(), AccountID: "support", SubjectDisplay: "Stale New Support",
			Status: sqlc.ThreadStatusNew, AssigneeID: &multiUser.ID, CustomerEmail: "a@example.com",
			LastMessageAt: now.Add(-1 * time.Hour), OpenedAt: now.Add(-72 * time.Hour), // 3d old -> Needs attention
		},
		{
			ID: uuid.New(), AccountID: "support", SubjectDisplay: "Stale InProgress Support",
			Status: sqlc.ThreadStatusInProgress, AssigneeID: &otherUserID, CustomerEmail: "b@example.com",
			LastMessageAt: now.Add(-2 * time.Hour), OpenedAt: now.Add(-60 * time.Hour), // 2.5d old -> Needs attention
		},
		{
			ID: uuid.New(), AccountID: "support", SubjectDisplay: "Fresh New Support",
			Status: sqlc.ThreadStatusNew, AssigneeID: &multiUser.ID, CustomerEmail: "c@example.com",
			LastMessageAt: now.Add(-30 * time.Minute), OpenedAt: now.Add(-4 * time.Hour), // 4h old -> NOT Needs attention
		},
		{
			ID: uuid.New(), AccountID: "support", SubjectDisplay: "Old Completed Support",
			Status: sqlc.ThreadStatusCompleted, AssigneeID: &multiUser.ID, CustomerEmail: "d@example.com",
			LastMessageAt: now.Add(-96 * time.Hour), OpenedAt: now.Add(-120 * time.Hour), // Closed -> NOT Needs attention
		},
		{
			ID: uuid.New(), AccountID: "sales", SubjectDisplay: "Stale New Sales",
			Status: sqlc.ThreadStatusNew, AssigneeID: &multiUser.ID, CustomerEmail: "e@example.com",
			LastMessageAt: now.Add(-3 * time.Hour), OpenedAt: now.Add(-96 * time.Hour), // 4d old in sales -> Needs attention for multiUser only
		},
	}
	for _, th := range threads {
		store.threads[th.ID] = th
	}

	// 1. GET /threads?status=attention returns 200 OK with only open threads older than threshold (2d)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=attention", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /threads?status=attention status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Stale New Support",
		"Stale InProgress Support",
		"Stale New Sales",
		`aria-current="true">Needs attention <span class="count-pill">3</span></a>`,
		`New <span class="count-pill">3</span></a>`,
		`Mine <span class="count-pill">3</span></a>`,
		`<span>In progress</span> <span class="count-pill">1</span>`,
		`<span>Completed</span> <span class="count-pill">1</span>`,
		`<span>Spam</span> <span class="count-pill">0</span>`,
		`<span>All</span> <span class="count-pill">5</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in Needs attention response: %s", want, body)
		}
	}
	for _, unwanted := range []string{"Fresh New Support", "Old Completed Support"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("did not expect %q in Needs attention response: %s", unwanted, body)
		}
	}

	// 2. Account filter scopes both the list and the tab count pills
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=attention&account=support", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /threads?status=attention&account=support status = %d", rec.Code)
	}
	supportBody := rec.Body.String()
	if strings.Contains(supportBody, "Stale New Sales") {
		t.Fatalf("account=support must exclude sales threads: %s", supportBody)
	}
	for _, want := range []string{
		`Needs attention <span class="count-pill">2</span></a>`,
		`New <span class="count-pill">2</span></a>`,
		`Mine <span class="count-pill">2</span></a>`,
		`<span>All</span> <span class="count-pill">4</span>`,
	} {
		if !strings.Contains(supportBody, want) {
			t.Fatalf("expected account-scoped count %q in response: %s", want, supportBody)
		}
	}

	// 3. Updating attention_age_days via POST /settings changes the Needs attention cutoff
	form := url.Values{
		"display_timezone":          {"Europe/Amsterdam"},
		"subject_match_window_days": {"30"},
		"attention_age_days":        {"4"},
		"imap_inbox_mailbox":        {"INBOX"},
		"imap_archive_mailbox":      {"Archive"},
		"imap_junk_mailbox":         {"Junk"},
		"imap_sent_mailbox":         {"Sent"},
	}
	reqSettings := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	reqSettings.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recSettings := httptest.NewRecorder()
	router.ServeHTTP(recSettings, reqSettings)
	if recSettings.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings status = %d: %s", recSettings.Code, recSettings.Body.String())
	}
	if store.settings.AttentionAgeDays != 4 {
		t.Fatalf("store.settings.AttentionAgeDays = %d, want 4", store.settings.AttentionAgeDays)
	}

	// With threshold=4d, only "Stale New Sales" (96h old) qualifies; 3d and 2.5d threads no longer qualify.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=attention", nil))
	body4d := rec.Body.String()
	if !strings.Contains(body4d, "Stale New Sales") || strings.Contains(body4d, "Stale New Support") || strings.Contains(body4d, "Stale InProgress Support") {
		t.Fatalf("with 4d threshold, only Stale New Sales should appear: %s", body4d)
	}
	if !strings.Contains(body4d, `Needs attention <span class="count-pill">1</span></a>`) {
		t.Fatalf("expected Needs attention count pill of 1 after threshold update: %s", body4d)
	}
}

// srvOutboxProcess drains the test server's pending outbox via a worker wired
// to the same test store, so SMTP/append_to_sent side effects are observable.
func srvOutboxProcess(store *testWebStore) (int, error) {
	attStore, err := storage.NewStore(os.TempDir()+"/minicrm-test-outbox", nil)
	if err != nil {
		return 0, err
	}
	worker := mail.NewOutboxWorker(outboxStoreAdapter{store}, attStore, []mail.AccountSenderConfig{
		{AccountID: "support", FromAddress: "support@example.com", FromName: "Support", Transport: &noopSMTP{}},
	}, audit.NewLogger(store, nil), "example.com", nil, nil)
	return worker.ProcessPending(context.Background())
}

func TestReplySendsExactlyOneAppendToSentJob(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", Status: sqlc.ThreadStatusNew, CustomerEmail: "bob@customer.example",
		LastMessageAt: time.Now().UTC(),
	}
	form := url.Values{"body_text": {"A single reply"}, "draft_revision": {uuid.Nil.String()}}
	req := httptest.NewRequest(http.MethodPost, "/threads/"+threadID.String()+"/reply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect after reply, got %d: %s", rec.Code, rec.Body.String())
	}
	sent, err := srvOutboxProcess(store)
	if err != nil {
		t.Fatalf("ProcessPending: %v", err)
	}
	if sent != 1 {
		t.Fatalf("expected exactly 1 sent outbox email, got %d", sent)
	}
	var emails int
	for _, em := range store.emails {
		if em.ThreadID == threadID && em.Direction == "outbound" {
			emails++
		}
	}
	if emails != 1 {
		t.Fatalf("expected exactly 1 outbound email row, got %d", emails)
	}
	emailIDs := map[uuid.UUID]bool{}
	for _, em := range store.emails {
		if em.ThreadID == threadID {
			emailIDs[em.ID] = true
		}
	}
	var sentJobs int
	for _, job := range store.jobs {
		if !emailIDs[job.EmailID] {
			continue
		}
		if job.JobType != "append_to_sent" {
			t.Fatalf("unexpected job type for reply: %+v", job)
		}
		if job.AccountID != "support" {
			t.Fatalf("job must carry the owning account: %+v", job)
		}
		sentJobs++
	}
	if sentJobs != 1 {
		t.Fatalf("expected exactly 1 append_to_sent imap_job, got %d", sentJobs)
	}
}

func TestReplyContextDetailsRenderOpenForDesktopParity(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", Status: sqlc.ThreadStatusNew, CustomerName: "Bob", CustomerEmail: "bob@customer.example",
		LastMessageAt: time.Now().UTC(),
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<details class="reply-context-details" open>`) {
		t.Fatalf("expected reply-context-details rendered open: %s", body)
	}
	// Draft textarea must survive a rejected send (sheet open/close keeps the card mounted).
	form := url.Values{"body_text": {"   "}}
	req := httptest.NewRequest(http.MethodPost, "/threads/"+threadID.String()+"/reply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), `class="card reply-card"`) {
		t.Fatalf("expected reply card still rendered after failed send: %s", rec2.Body.String())
	}
}
