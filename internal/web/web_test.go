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
		},
		threads:         make(map[uuid.UUID]sqlc.Thread),
		emails:          make(map[uuid.UUID]sqlc.Email),
		attachments:     make(map[uuid.UUID]sqlc.Attachment),
		accountSettings: make(map[string]sqlc.AccountSettings),
	}
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

func (s *testWebStore) ListThreads(_ context.Context, f sqlc.ListThreadsFilter) ([]sqlc.Thread, error) {
	var out []sqlc.Thread
	for _, t := range s.threads {
		if f.Status != nil && t.Status != *f.Status {
			continue
		}
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
		if len(f.AllowedAccounts) > 0 && !allowed {
			continue
		}
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
	outbox := mail.NewOutboxWorker(nil, attStore, []mail.AccountSenderConfig{
		{AccountID: "support", FromAddress: "support@example.com", FromName: "Support", Transport: &noopSMTP{}},
	}, auditLogger, "example.com", nil, slogLogger)
	sm := auth.NewSessionManager(false)
	authSvc := auth.NewService(store, auditLogger, sm, nil, "admin@example.com")
	srv := web.NewServer(store, authSvc, wfSvc, outbox, attStore, auditLogger, testAccounts())

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
		"account_default_assignee_support": {store.adminUser.ID.String()},
		"display_timezone":                 {"America/New_York"},
		"imap_inbox_mailbox":               {"INBOX"},
		"imap_archive_mailbox":             {"Archive"},
		"imap_junk_mailbox":                {"Junk"},
		"imap_sent_mailbox":                {"Sent"},
		"subject_match_window_days":        {"45"},
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
		{ID: "support", Name: "Customer Support"},
		{ID: "sales", Name: "Sales"},
	}
}

// buildAccountTestServer builds a web.Server with two configured accounts.
func buildAccountTestServer(t *testing.T) (*web.Server, *testWebStore, http.Handler, sqlc.User, sqlc.User) {
	t.Helper()
	store := newTestWebStore()
	auditLogger := audit.NewLogger(store, nil)
	attStore, err := storage.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	wfSvc := workflow.NewService(store, auditLogger, nil)
	outbox := mail.NewOutboxWorker(nil, attStore, []mail.AccountSenderConfig{
		{AccountID: "support", FromAddress: "support@example.com", FromName: "Support", Transport: &noopSMTP{}},
	}, auditLogger, "example.com", nil, nil)
	sm := auth.NewSessionManager(false)
	authSvc := auth.NewService(store, auditLogger, sm, nil, "admin@example.com")
	srv := web.NewServer(store, authSvc, wfSvc, outbox, attStore, auditLogger, testAccounts())

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

	router := web.SecurityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := r.URL.Query().Get("as")
		user := multiUser
		if current == "single" {
			user = singleUser
		}
		r = r.WithContext(auth.WithUser(r.Context(), user))
		switch {
		case r.URL.Path == "/threads":
			srv.HandleListThreads(w, r)
		case strings.HasPrefix(r.URL.Path, "/threads/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/threads/"), "/assign")
			rctx := web.WithChiParam(r, "id", id)
			if r.Method == http.MethodPost {
				srv.HandleAssignThread(w, rctx)
				return
			}
			srv.HandleViewThread(w, rctx)
		case strings.HasPrefix(r.URL.Path, "/emails/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/emails/"), "/body")
			srv.HandleIsolatedEmailBody(w, web.WithChiParam(r, "id", id))
		case strings.HasPrefix(r.URL.Path, "/attachments/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/attachments/"), "/download")
			srv.HandleDownloadAttachment(w, web.WithChiParam(r, "id", id))
		}
	}))
	return srv, store, router, multiUser, singleUser
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
	if !strings.Contains(body, `name="account"`) || !strings.Contains(body, "<th>Account</th>") {
		t.Fatalf("expected account filter and column for multi-account user: %s", body)
	}
	if !strings.Contains(body, "Customer Support") || !strings.Contains(body, "Sales") {
		t.Fatalf("expected account names in inbox: %s", body)
	}

	// Single-account user: account UI is hidden.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?as=single", nil))
	body = rec.Body.String()
	if strings.Contains(body, `name="account"`) || strings.Contains(body, "<th>Account</th>") {
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
	idx := strings.Index(body, "Assignee:")
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
	salesID := admin.ID
	var buf bytes.Buffer
	err := views.RenderSettings(&buf, views.SettingsViewData{
		CurrentUser: admin,
		Settings:    sqlc.SystemSettings{ID: 1, DisplayTimezone: "UTC", IMAPInboxMailbox: "INBOX"},
		Users:       []sqlc.User{admin, supportAgent},
		Accounts: []views.AccountOption{
			{ID: "support", Name: "Customer Support"},
			{ID: "sales", Name: "Sales"},
		},
		AccountSettings: []views.AccountSettingsView{
			{Account: views.AccountOption{ID: "support", Name: "Customer Support"}},
			{Account: views.AccountOption{ID: "sales", Name: "Sales"}, DefaultAssigneeID: &salesID},
		},
	})
	if err != nil {
		t.Fatalf("RenderSettings: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		`name="account_default_assignee_support"`,
		`name="account_default_assignee_sales"`,
		"-- Unassigned (None) --",
		"Customer Support",
		`name="allowed_accounts"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in settings page: %s", want, body)
		}
	}
	// The support-only agent must not be offered as the sales default assignee.
	salesIdx := strings.Index(body, `name="account_default_assignee_sales"`)
	if salesIdx < 0 {
		t.Fatal("expected sales default assignee field in settings page")
	}
	salesSection := body[salesIdx:]
	if strings.Contains(salesSection, "support@example.com") {
		t.Fatalf("support-only agent must not be offered for the sales account: %s", salesSection)
	}
}
