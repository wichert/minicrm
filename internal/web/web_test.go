package web_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/mail"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/web"
	"github.com/wichert/minicrm/internal/web/views"
	"github.com/wichert/minicrm/internal/workflow"
	gomail "github.com/wneessen/go-mail"
)

type noopSMTP struct{}

func (n *noopSMTP) Send(_ context.Context, _ *gomail.Msg) error { return nil }

type testWebStore struct {
	adminUser    sqlc.User
	users        map[uuid.UUID]sqlc.User
	settings     sqlc.SystemSettings
	threads      map[uuid.UUID]sqlc.Thread
	emails       map[uuid.UUID]sqlc.Email
	attachments  map[uuid.UUID]sqlc.Attachment
	threadEvents []sqlc.ThreadEvent
	auditLogs    []sqlc.AuditLog
	jobs         []sqlc.IMAPJob
}

func newTestWebStore() *testWebStore {
	admin := sqlc.User{
		ID:          uuid.New(),
		Email:       "admin@example.com",
		DisplayName: "Admin User",
		Role:        "admin",
		IsActive:    true,
	}
	return &testWebStore{
		adminUser: admin,
		users:     map[uuid.UUID]sqlc.User{admin.ID: admin},
		settings: sqlc.SystemSettings{
			ID:                     1,
			DefaultAssigneeID:      admin.ID,
			DisplayTimezone:        "Europe/Amsterdam",
			IMAPInboxMailbox:       "INBOX",
			IMAPArchiveMailbox:     "Archive",
			IMAPJunkMailbox:        "Junk",
			IMAPSentMailbox:        "Sent",
			SubjectMatchWindowDays: 30,
		},
		threads:     make(map[uuid.UUID]sqlc.Thread),
		emails:      make(map[uuid.UUID]sqlc.Email),
		attachments: make(map[uuid.UUID]sqlc.Attachment),
	}
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
		ID:          uuid.New(),
		OIDCSub:     arg.OIDCSub,
		Email:       arg.Email,
		DisplayName: arg.DisplayName,
		Role:        arg.Role,
		IsActive:    arg.IsActive,
	}
	s.users[u.ID] = u
	return u, nil
}

func (s *testWebStore) UpdateUserOIDC(_ context.Context, id uuid.UUID, oidcSub, displayName string) (sqlc.User, error) {
	u := s.users[id]
	u.OIDCSub = &oidcSub
	u.DisplayName = displayName
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

func (s *testWebStore) ListThreads(_ context.Context, _ sqlc.ListThreadsFilter) ([]sqlc.Thread, error) {
	var out []sqlc.Thread
	for _, t := range s.threads {
		out = append(out, t)
	}
	return out, nil
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
	t.AssigneeID = assigneeID
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
	// Amsterdam in January is CET (UTC+1) -> 13:00:00
	if !strings.Contains(got, "2026-01-15 13:00:00") {
		t.Fatalf("got %q, expected 2026-01-15 13:00:00 CET", got)
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
	outbox := mail.NewOutboxWorker(nil, attStore, &noopSMTP{}, auditLogger, "support@example.com", "Support", "example.com", nil, slogLogger)
	sm := auth.NewSessionManager(false)
	authSvc := auth.NewService(store, auditLogger, sm, nil, "admin@example.com")
	srv := web.NewServer(store, authSvc, wfSvc, outbox, attStore, auditLogger)

	completedID := uuid.New()
	followUpID := uuid.New()
	emailID := uuid.New()
	store.threads[completedID] = sqlc.Thread{
		ID:             completedID,
		SubjectDisplay: "Previous Ticket #1",
		Status:         sqlc.ThreadStatusCompleted,
		AssigneeID:     store.adminUser.ID,
		CustomerEmail:  "client@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}
	store.threads[followUpID] = sqlc.Thread{
		ID:              followUpID,
		SubjectDisplay:  "Re: Previous Ticket #1",
		Status:          sqlc.ThreadStatusNew,
		AssigneeID:      store.adminUser.ID,
		CustomerEmail:   "client@example.com",
		ReopensThreadID: &completedID,
		LastMessageAt:   time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
	}
	store.emails[emailID] = sqlc.Email{
		ID:                emailID,
		ThreadID:          followUpID,
		Direction:         "inbound",
		FromAddress:       "client@example.com",
		Subject:           "Re: Previous Ticket #1",
		BodyHTMLSanitized: "<p>Safe sanitized paragraph</p>",
		SentAt:            time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
	}

	// Build router where user is authenticated in context
	router := web.SecurityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))

	// 1. Check main thread detail page headers, sandboxed iframe, and Reopen & Merge banner
	req1 := httptest.NewRequest(http.MethodGet, "/threads/"+followUpID.String(), nil)
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec1.Code)
	}
	if gotCSP := rec1.Header().Get("Content-Security-Policy"); gotCSP != web.MainCSP {
		t.Fatalf("unexpected main CSP: %s", gotCSP)
	}
	if rec1.Header().Get("X-Frame-Options") != "DENY" || rec1.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing security headers: %+v", rec1.Header())
	}
	body1 := rec1.Body.String()
	if !strings.Contains(body1, `sandbox="allow-popups allow-popups-to-escape-sandbox"`) || !strings.Contains(body1, "credentialless") {
		t.Fatalf("expected sandboxed credentialless iframe in thread detail: %s", body1)
	}
	if !strings.Contains(body1, "Reopen completed thread") {
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

	// 3. Test updating settings (including display_timezone and Archive folder)
	form := url.Values{
		"default_assignee_id":       {store.adminUser.ID.String()},
		"display_timezone":          {"America/New_York"},
		"imap_inbox_mailbox":        {"INBOX"},
		"imap_archive_mailbox":      {"Archive"},
		"imap_junk_mailbox":         {"Junk"},
		"imap_sent_mailbox":         {"Sent"},
		"subject_match_window_days": {"45"},
	}
	req3 := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusSeeOther || store.settings.DisplayTimezone != "America/New_York" {
		t.Fatalf("settings update failed: code=%d tz=%s", rec3.Code, store.settings.DisplayTimezone)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, `"event_category":"data_access"`) || !strings.Contains(logs, `"event_category":"data_change"`) {
		t.Fatalf("expected structured data_access and data_change logs, got: %s", logs)
	}
}
