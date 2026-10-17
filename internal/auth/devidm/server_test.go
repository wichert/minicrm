package devidm_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/auth/devidm"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

type memStore struct {
	users           map[uuid.UUID]sqlc.User
	settings        *sqlc.SystemSettings
	accountSettings map[string]sqlc.AccountSettings
	auditLogs       []sqlc.AuditLog
	threadLogs      []sqlc.ThreadEvent
}

func newMemStore() *memStore {
	return &memStore{users: make(map[uuid.UUID]sqlc.User)}
}

func (m *memStore) GetUserByID(_ context.Context, id uuid.UUID) (sqlc.User, error) {
	u, ok := m.users[id]
	if !ok {
		return sqlc.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (m *memStore) GetUserByOIDCSub(_ context.Context, sub string) (sqlc.User, error) {
	for _, u := range m.users {
		if u.OIDCSub != nil && *u.OIDCSub == sub {
			return u, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *memStore) GetUserByEmail(_ context.Context, email string) (sqlc.User, error) {
	for _, u := range m.users {
		if strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *memStore) CreateUser(_ context.Context, arg sqlc.CreateUserParams) (sqlc.User, error) {
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

func (m *memStore) UpdateUserOIDC(_ context.Context, id uuid.UUID, oidcSub, displayName string, allowedAccounts []string) (sqlc.User, error) {
	u := m.users[id]
	if oidcSub != "" {
		u.OIDCSub = &oidcSub
	}
	u.DisplayName = displayName
	u.AllowedAccounts = allowedAccounts
	m.users[id] = u
	return u, nil
}

func (m *memStore) UpsertAccountSettings(_ context.Context, s sqlc.AccountSettings) (sqlc.AccountSettings, error) {
	if m.accountSettings == nil {
		m.accountSettings = make(map[string]sqlc.AccountSettings)
	}
	m.accountSettings[s.AccountID] = s
	return s, nil
}

func (m *memStore) GetSystemSettings(_ context.Context) (sqlc.SystemSettings, error) {
	if m.settings == nil {
		return sqlc.SystemSettings{}, pgx.ErrNoRows
	}
	return *m.settings, nil
}

func (m *memStore) UpsertSystemSettings(_ context.Context, s sqlc.SystemSettings) (sqlc.SystemSettings, error) {
	s.ID = 1
	m.settings = &s
	return s, nil
}

func (m *memStore) InsertAuditLog(_ context.Context, entry sqlc.AuditLog) error {
	m.auditLogs = append(m.auditLogs, entry)
	return nil
}

func (m *memStore) InsertThreadEvent(_ context.Context, ev sqlc.ThreadEvent) error {
	m.threadLogs = append(m.threadLogs, ev)
	return nil
}

func TestDevIDMWithRealOIDCProviderAndPKCE(t *testing.T) {
	idm, err := devidm.NewServer(devidm.Config{
		ClientID:     "minicrm-client",
		ClientSecret: "minicrm-secret",
		Subject:      "dev-admin-sub",
		Email:        "admin@example.com",
		Name:         "Dev Admin",
		Groups:       []string{"crm-support"},
	})
	if err != nil {
		t.Fatalf("devidm.NewServer: %v", err)
	}
	idmSrv := httptest.NewServer(idm.Handler())
	defer idmSrv.Close()
	idm.SetIssuerURL(idmSrv.URL)

	store := newMemStore()
	auditLogger := audit.NewLogger(store, nil)
	sessions := auth.NewSessionManager(false)

	var appSrv *httptest.Server
	mux := http.NewServeMux()
	appSrv = httptest.NewServer(sessions.LoadAndSave(mux))
	defer appSrv.Close()

	ctx := context.Background()
	oidcProvider, err := auth.NewOIDCProvider(
		ctx,
		idmSrv.URL,
		"minicrm-client",
		"minicrm-secret",
		appSrv.URL+"/auth/callback",
		"groups",
		[]string{"openid", "profile", "email", "groups"},
	)
	if err != nil {
		t.Fatalf("auth.NewOIDCProvider against devidm: %v", err)
	}

	authSvc := auth.NewService(store, auditLogger, sessions, oidcProvider, "admin@example.com",
		auth.AccountAccessRule{AccountID: "support", AccountName: "Support", OIDCGroup: "crm-support"})
	if _, _, err := authSvc.SeedBootstrapAdminAndSettings(ctx, "admin@example.com", "Admin", "UTC"); err != nil {
		t.Fatalf("SeedBootstrapAdminAndSettings: %v", err)
	}

	// The provider must request the configured scopes, including the groups scope,
	// otherwise providers like Kanidm never emit group membership.
	authURL := oidcProvider.AuthCodeURL("state-1", "nonce-1", "verifier-1")
	for _, want := range []string{"scope=openid+profile+email+groups", "code_challenge_method=S256"} {
		if !strings.Contains(authURL, want) {
			t.Fatalf("expected %q in authorization URL, got %s", want, authURL)
		}
	}

	mux.HandleFunc("GET /auth/login", authSvc.HandleLogin)
	mux.HandleFunc("GET /auth/callback", authSvc.HandleCallback)
	mux.Handle("GET /threads", authSvc.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFromContext(r.Context())
		_, _ = io.WriteString(w, "AUTHENTICATED:"+u.Email+":"+u.DisplayName)
	})))

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// Request /threads -> redirects to /auth/login -> devidm /authorize -> /auth/callback -> /threads
	resp, err := client.Get(appSrv.URL + "/threads")
	if err != nil {
		t.Fatalf("GET /threads redirect chain failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "AUTHENTICATED:admin@example.com:Dev Admin" {
		t.Fatalf("unexpected response: status=%d body=%q", resp.StatusCode, string(body))
	}
}
