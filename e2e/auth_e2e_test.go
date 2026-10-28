// Package e2e_test contains end-to-end tests executed against MiniCRM and the dev-oidc identity server.
package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/auth/devidm"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/web"
	"github.com/wichert/minicrm/internal/workflow"
)

func TestEndToEndOIDCAuthenticationWithDevIDM(t *testing.T) {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("MINICRM_E2E_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = startSelfContainedE2EServers(t)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}

	var redirects []string
	client := &http.Client{
		Jar:     jar,
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			redirects = append(redirects, req.URL.String())
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	// 1. Request /threads unauthenticated -> follows OIDC redirect chain through dev-oidc and lands on /threads
	resp, err := client.Get(baseURL + "/threads")
	if err != nil {
		t.Fatalf("GET %s/threads failed: %v", baseURL, err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /threads body: %v", err)
	}
	body := string(bodyBytes)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /threads after OIDC login, got %d (redirects=%v, body=%s)", resp.StatusCode, redirects, body)
	}
	if len(redirects) < 3 {
		t.Fatalf("expected at least 3 redirects (/auth/login -> dev-oidc /authorize -> /auth/callback -> /threads), got %v", redirects)
	}
	if !strings.Contains(body, "Dev Admin") || !strings.Contains(body, "admin@example.com") {
		t.Fatalf("expected authenticated user 'Dev Admin (admin@example.com)' in /threads HTML, got:\n%s", body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("expected strict Content-Security-Policy header on /threads, got %q", csp)
	}

	// 2. Verify authenticated admin session can access /settings
	settingsResp, err := client.Get(baseURL + "/settings")
	if err != nil {
		t.Fatalf("GET %s/settings failed: %v", baseURL, err)
	}
	defer settingsResp.Body.Close()
	if settingsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /settings for admin user, got %d", settingsResp.StatusCode)
	}

	// 3. Verify user_login event was persisted in PostgreSQL audit_logs when running against live Postgres in CI
	dbURL := strings.TrimSpace(os.Getenv("MINICRM_DATABASE_URL"))
	if dbURL == "" {
		dbURL = strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	}
	if dbURL != "" && os.Getenv("MINICRM_E2E_BASE_URL") != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		pool, err := pgxpool.New(ctx, dbURL)
		if err != nil {
			t.Fatalf("pgxpool.New: %v", err)
		}
		defer pool.Close()

		queries := sqlc.New(pool)
		logs, err := queries.ListAuditLogs(ctx, 20)
		if err != nil {
			t.Fatalf("ListAuditLogs: %v", err)
		}
		foundLogin := false
		for _, l := range logs {
			if l.EventType == "user_login" {
				foundLogin = true
				break
			}
		}
		if !foundLogin {
			t.Fatalf("expected user_login event in audit_logs, got: %+v", logs)
		}
	}
}

func TestEndToEndBulkSpam(t *testing.T) {
	baseURL := startSelfContainedE2EServers(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	resp, err := client.Get(baseURL + "/threads")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), e2eBulkThreadID.String()) {
		t.Fatal("missing inbox selection fixture")
	}
	form := url.Values{"thread_id": {e2eBulkThreadID.String()}, "return_to": {"/threads?status=open"}}
	resp, err = client.PostForm(baseURL+"/threads/bulk-spam", form)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "1 marked as spam and queued for Junk; 0 failed.") {
		t.Fatalf("bulk failed: %d %s", resp.StatusCode, body)
	}
}

func TestEndToEndSharedDraftConflict(t *testing.T) {
	baseURL := startSelfContainedE2EServers(t)
	clients := make([]*http.Client, 2)
	for i := range clients {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		clients[i] = &http.Client{Jar: jar, Timeout: 15 * time.Second}
		resp, err := clients[i].Get(baseURL + "/threads")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	endpoint := baseURL + "/threads/" + e2eBulkThreadID.String() + "/draft"
	save := func(client *http.Client, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, endpoint, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := save(clients[0], `{"revision":"00000000-0000-0000-0000-000000000000","body":"first operator"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp, err := clients[1].Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var draft sqlc.ReplyDraft
	if err := json.NewDecoder(resp.Body).Decode(&draft); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if draft.Body != "first operator" {
		t.Fatalf("draft not restored: %+v", draft)
	}
	resp = save(clients[1], `{"revision":"00000000-0000-0000-0000-000000000000","body":"stale overwrite"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected conflict, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func startSelfContainedE2EServers(t *testing.T) string {
	t.Helper()

	idm, err := devidm.NewServer(devidm.Config{
		ClientID:     "minicrm-e2e",
		ClientSecret: "minicrm-e2e-secret",
		Subject:      "dev-admin-sub",
		Email:        "admin@example.com",
		Name:         "Dev Admin",
		Groups:       []string{"crm-support", "crm-sales"},
	})
	if err != nil {
		t.Fatalf("devidm.NewServer: %v", err)
	}
	idmSrv := httptest.NewServer(idm.Handler())
	t.Cleanup(idmSrv.Close)
	idm.SetIssuerURL(idmSrv.URL)

	store := newE2EMemStore()
	store.threads[e2eBulkThreadID] = sqlc.Thread{ID: e2eBulkThreadID, AccountID: "support", Status: sqlc.ThreadStatusNew, SubjectDisplay: "Bulk e2e fixture", LastMessageAt: time.Now()}
	auditLogger := audit.NewLogger(store, nil)
	attStore, err := storage.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.NewStore: %v", err)
	}
	sessions := auth.NewSessionManager(false)

	var handler http.Handler
	crmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(crmSrv.Close)

	ctx := context.Background()
	oidcProvider, err := auth.NewOIDCProvider(
		ctx,
		idmSrv.URL,
		"minicrm-e2e",
		"minicrm-e2e-secret",
		crmSrv.URL+"/auth/callback",
		"groups",
		[]string{"openid", "profile", "email", "groups"},
	)
	if err != nil {
		t.Fatalf("auth.NewOIDCProvider: %v", err)
	}

	authSvc := auth.NewService(store, auditLogger, sessions, oidcProvider, "admin@example.com",
		auth.AccountAccessRule{AccountID: "support", AccountName: "Support", OIDCGroup: "crm-support"},
		auth.AccountAccessRule{AccountID: "sales", AccountName: "Sales", OIDCGroup: "crm-sales"},
	)
	if _, _, err := authSvc.SeedBootstrapAdminAndSettings(ctx, "admin@example.com", "Dev Admin", "UTC"); err != nil {
		t.Fatalf("SeedBootstrapAdminAndSettings: %v", err)
	}
	wfSvc := workflow.NewService(store, auditLogger, nil)
	webSrv := web.NewServer(store, authSvc, wfSvc, nil, attStore, auditLogger, sessions, []web.AccountConfig{
		{ID: "support", Name: "Support", FromAddress: "support@example.com"},
		{ID: "sales", Name: "Sales", FromAddress: "sales@example.com"},
	})
	handler = web.NewRouter(webSrv, authSvc, sessions)

	return crmSrv.URL
}

var e2eBulkThreadID = uuid.MustParse("d9000000-0000-4000-8000-000000000001")

type e2eMemStore struct {
	threads         map[uuid.UUID]sqlc.Thread
	drafts          map[uuid.UUID]sqlc.ReplyDraft
	users           map[uuid.UUID]sqlc.User
	settings        *sqlc.SystemSettings
	accountSettings map[string]sqlc.AccountSettings
	auditLogs       []sqlc.AuditLog
}

func newE2EMemStore() *e2eMemStore {
	return &e2eMemStore{users: make(map[uuid.UUID]sqlc.User), threads: make(map[uuid.UUID]sqlc.Thread), drafts: make(map[uuid.UUID]sqlc.ReplyDraft)}
}

func (m *e2eMemStore) GetUserByID(_ context.Context, id uuid.UUID) (sqlc.User, error) {
	u, ok := m.users[id]
	if !ok {
		return sqlc.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (m *e2eMemStore) GetUserByOIDCSub(_ context.Context, sub string) (sqlc.User, error) {
	for _, u := range m.users {
		if u.OIDCSub != nil && *u.OIDCSub == sub {
			return u, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *e2eMemStore) GetUserByEmail(_ context.Context, email string) (sqlc.User, error) {
	for _, u := range m.users {
		if strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *e2eMemStore) CreateUser(_ context.Context, arg sqlc.CreateUserParams) (sqlc.User, error) {
	u := sqlc.User{
		ID:              uuid.New(),
		OIDCSub:         arg.OIDCSub,
		Email:           arg.Email,
		DisplayName:     arg.DisplayName,
		Role:            arg.Role,
		IsActive:        arg.IsActive,
		AllowedAccounts: arg.AllowedAccounts,
	}
	m.users[u.ID] = u
	return u, nil
}

func (m *e2eMemStore) UpdateUserOIDC(_ context.Context, id uuid.UUID, oidcSub, displayName string, allowedAccounts []string) (sqlc.User, error) {
	u := m.users[id]
	if oidcSub != "" {
		u.OIDCSub = &oidcSub
	}
	u.DisplayName = displayName
	u.AllowedAccounts = allowedAccounts
	m.users[id] = u
	return u, nil
}

func (m *e2eMemStore) GetAccountSettings(_ context.Context, accountID string) (sqlc.AccountSettings, error) {
	if a, ok := m.accountSettings[accountID]; ok {
		return a, nil
	}
	return sqlc.AccountSettings{}, pgx.ErrNoRows
}

func (m *e2eMemStore) ListAccountSettings(_ context.Context) ([]sqlc.AccountSettings, error) {
	var out []sqlc.AccountSettings
	for _, a := range m.accountSettings {
		out = append(out, a)
	}
	return out, nil
}

func (m *e2eMemStore) UpsertAccountSettings(_ context.Context, a sqlc.AccountSettings) (sqlc.AccountSettings, error) {
	if m.accountSettings == nil {
		m.accountSettings = make(map[string]sqlc.AccountSettings)
	}
	m.accountSettings[a.AccountID] = a
	return a, nil
}

func (m *e2eMemStore) ListUsers(_ context.Context) ([]sqlc.User, error) {
	var out []sqlc.User
	for _, u := range m.users {
		out = append(out, u)
	}
	return out, nil
}

func (m *e2eMemStore) GetSystemSettings(_ context.Context) (sqlc.SystemSettings, error) {
	if m.settings == nil {
		return sqlc.SystemSettings{}, pgx.ErrNoRows
	}
	return *m.settings, nil
}

func (m *e2eMemStore) UpsertSystemSettings(_ context.Context, s sqlc.SystemSettings) (sqlc.SystemSettings, error) {
	s.ID = 1
	m.settings = &s
	return s, nil
}

func (m *e2eMemStore) ListThreads(_ context.Context, _ sqlc.ListThreadsFilter) ([]sqlc.Thread, error) {
	var out []sqlc.Thread
	for _, thread := range m.threads {
		out = append(out, thread)
	}
	return out, nil
}

func (m *e2eMemStore) ListCustomerHistory(_ context.Context, _ []string, _ string, _ uuid.UUID, _ int) ([]sqlc.Thread, error) {
	return nil, nil
}

func (m *e2eMemStore) GetThreadByID(_ context.Context, id uuid.UUID) (sqlc.Thread, error) {
	thread, ok := m.threads[id]
	if !ok {
		return sqlc.Thread{}, pgx.ErrNoRows
	}
	return thread, nil
}

func (m *e2eMemStore) GetReplyDraft(_ context.Context, id uuid.UUID) (sqlc.ReplyDraft, error) {
	draft, ok := m.drafts[id]
	if !ok {
		return sqlc.ReplyDraft{}, pgx.ErrNoRows
	}
	return draft, nil
}

func (m *e2eMemStore) SaveReplyDraft(_ context.Context, id, revision uuid.UUID, body, formatting, session string, discard bool, _ *uuid.UUID) (sqlc.ReplyDraft, error) {
	if m.drafts[id].Revision != revision {
		return sqlc.ReplyDraft{}, sqlc.ErrDraftConflict
	}
	draft := sqlc.ReplyDraft{ThreadID: id, AccountID: m.threads[id].AccountID, Revision: uuid.New(), Body: body, Formatting: formatting, SessionID: session, Discarded: discard, OccurredAt: time.Now()}
	m.drafts[id] = draft
	return draft, nil
}

func (m *e2eMemStore) UpdateThreadStatus(_ context.Context, id uuid.UUID, status sqlc.ThreadStatus) error {
	thread := m.threads[id]
	thread.Status = status
	m.threads[id] = thread
	return nil
}

func (m *e2eMemStore) UpdateThreadAssignee(_ context.Context, _, _ uuid.UUID, _ string) error {
	return nil
}

func (m *e2eMemStore) TouchThreadLastMessageAt(_ context.Context, _ uuid.UUID, _ sqlc.Thread) error {
	return nil
}

func (m *e2eMemStore) ListEmailsByThread(_ context.Context, _ uuid.UUID) ([]sqlc.Email, error) {
	return nil, nil
}

func (m *e2eMemStore) GetEmailByID(_ context.Context, _ uuid.UUID) (sqlc.Email, error) {
	return sqlc.Email{}, pgx.ErrNoRows
}

func (m *e2eMemStore) ListAttachmentsByEmail(_ context.Context, _ uuid.UUID) ([]sqlc.Attachment, error) {
	return nil, nil
}

func (m *e2eMemStore) GetAttachmentByID(_ context.Context, _ uuid.UUID) (sqlc.Attachment, error) {
	return sqlc.Attachment{}, pgx.ErrNoRows
}

func (m *e2eMemStore) ListThreadEvents(_ context.Context, _ uuid.UUID) ([]sqlc.ThreadEvent, error) {
	return nil, nil
}

func (m *e2eMemStore) ListAuditLogs(_ context.Context, _ int) ([]sqlc.AuditLog, error) {
	return m.auditLogs, nil
}

func (m *e2eMemStore) EnqueueIMAPJob(_ context.Context, job sqlc.IMAPJob) (sqlc.IMAPJob, error) {
	return job, nil
}

func (m *e2eMemStore) ReparentThreadEmailsAndEvents(_ context.Context, _, _ uuid.UUID) error {
	return nil
}

func (m *e2eMemStore) DeleteThread(_ context.Context, _ uuid.UUID) error {
	return nil
}

func (m *e2eMemStore) AssignUnassignedThreadsByAccount(_ context.Context, _ string, _ uuid.UUID, _ string) ([]uuid.UUID, error) {
	return nil, nil
}

func (m *e2eMemStore) InsertAuditLog(_ context.Context, entry sqlc.AuditLog) error {
	m.auditLogs = append(m.auditLogs, entry)
	return nil
}

func (m *e2eMemStore) InsertThreadEvent(_ context.Context, _ sqlc.ThreadEvent) error {
	return nil
}
