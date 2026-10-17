package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

type memUserStore struct {
	users           map[uuid.UUID]sqlc.User
	settings        *sqlc.SystemSettings
	accountSettings map[string]sqlc.AccountSettings
	auditLogs       []sqlc.AuditLog
	threadLogs      []sqlc.ThreadEvent
}

func newMemUserStore() *memUserStore {
	return &memUserStore{users: make(map[uuid.UUID]sqlc.User)}
}

func (m *memUserStore) GetUserByID(_ context.Context, id uuid.UUID) (sqlc.User, error) {
	u, ok := m.users[id]
	if !ok {
		return sqlc.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (m *memUserStore) GetUserByOIDCSub(_ context.Context, sub string) (sqlc.User, error) {
	for _, u := range m.users {
		if u.OIDCSub != nil && *u.OIDCSub == sub {
			return u, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *memUserStore) GetUserByEmail(_ context.Context, email string) (sqlc.User, error) {
	for _, u := range m.users {
		if strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *memUserStore) CreateUser(_ context.Context, arg sqlc.CreateUserParams) (sqlc.User, error) {
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

func (m *memUserStore) UpdateUserOIDC(_ context.Context, id uuid.UUID, oidcSub, displayName string, allowedAccounts []string) (sqlc.User, error) {
	u, ok := m.users[id]
	if !ok {
		return sqlc.User{}, pgx.ErrNoRows
	}
	if oidcSub != "" {
		u.OIDCSub = &oidcSub
	}
	u.DisplayName = displayName
	u.AllowedAccounts = allowedAccounts
	m.users[id] = u
	return u, nil
}

func (m *memUserStore) GetSystemSettings(_ context.Context) (sqlc.SystemSettings, error) {
	if m.settings == nil {
		return sqlc.SystemSettings{}, pgx.ErrNoRows
	}
	return *m.settings, nil
}

func (m *memUserStore) UpsertSystemSettings(_ context.Context, s sqlc.SystemSettings) (sqlc.SystemSettings, error) {
	s.ID = 1
	m.settings = &s
	return s, nil
}

func (m *memUserStore) UpsertAccountSettings(_ context.Context, s sqlc.AccountSettings) (sqlc.AccountSettings, error) {
	if m.accountSettings == nil {
		m.accountSettings = make(map[string]sqlc.AccountSettings)
	}
	m.accountSettings[s.AccountID] = s
	return s, nil
}

func (m *memUserStore) InsertAuditLog(_ context.Context, entry sqlc.AuditLog) error {
	m.auditLogs = append(m.auditLogs, entry)
	return nil
}

func (m *memUserStore) InsertThreadEvent(_ context.Context, ev sqlc.ThreadEvent) error {
	m.threadLogs = append(m.threadLogs, ev)
	return nil
}

type fakeVerifier struct {
	identity *auth.VerifiedIdentity
}

func (f *fakeVerifier) AuthCodeURL(state, _, _ string) string {
	return "https://idp.example.com/authorize?state=" + state
}

func (f *fakeVerifier) ExchangeAndVerify(_ context.Context, _, _, _ string) (*auth.VerifiedIdentity, error) {
	if f.identity == nil {
		return nil, errors.New("invalid code")
	}
	return f.identity, nil
}

func TestSeedPrecreateAndLoginAuditTrail(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	sm := auth.NewSessionManager(false)
	verifier := &fakeVerifier{}
	svc := auth.NewService(store, auditLogger, sm, verifier, "admin@example.com", testAccountRules()...)

	ctx := context.Background()
	admin, settings, err := svc.SeedBootstrapAdminAndSettings(ctx, "admin@example.com", "Admin", "Europe/Amsterdam")
	if err != nil {
		t.Fatalf("SeedBootstrapAdminAndSettings: %v", err)
	}
	if admin.Role != "admin" || settings.DisplayTimezone != "Europe/Amsterdam" {
		t.Fatalf("unexpected seed result: admin=%+v settings=%+v", admin, settings)
	}
	seeded, err := store.GetUserByID(ctx, admin.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if !seeded.HasAccountAccess("support") || !seeded.HasAccountAccess("sales") {
		t.Fatalf("expected bootstrap admin to have access to all accounts, got %v", seeded.AllowedAccounts)
	}

	// Precreate user bob@example.com before first OIDC login
	bob, err := svc.PrecreateUser(ctx, admin.ID, "bob@example.com", "Bob Agent", "agent")
	if err != nil {
		t.Fatalf("PrecreateUser: %v", err)
	}
	if bob.OIDCSub != nil {
		t.Fatalf("expected nil OIDCSub on pre-created user, got %v", bob.OIDCSub)
	}

	// Now Bob logs in via OIDC for the first time
	loggedInBob, err := svc.ResolveAndLoginUser(ctx, &auth.VerifiedIdentity{
		Subject: "oidc-sub-bob",
		Email:   "bob@example.com",
		Name:    "Bob OIDC",
		Groups:  []string{"crm-support", "other"},
	}, "192.0.2.5", "TestBrowser/1.0")
	if err != nil {
		t.Fatalf("ResolveAndLoginUser: %v", err)
	}
	if loggedInBob.ID != bob.ID || loggedInBob.OIDCSub == nil || *loggedInBob.OIDCSub != "oidc-sub-bob" {
		t.Fatalf("expected precreated user to be bound to oidc-sub-bob: %+v", loggedInBob)
	}

	// Verify audit trail has user_created, settings_updated, user_precreated, user_updated, user_login
	wantEvents := []string{"user_created", "settings_updated", "user_precreated", "user_updated", "user_login"}
	if len(store.auditLogs) != len(wantEvents) {
		t.Fatalf("got %d audit logs, want %d: %+v", len(store.auditLogs), len(wantEvents), store.auditLogs)
	}
	for i, want := range wantEvents {
		if store.auditLogs[i].EventType != want {
			t.Fatalf("auditLog[%d] = %q, want %q", i, store.auditLogs[i].EventType, want)
		}
	}
}

func TestHTTPLoginCallbackFlow(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	sm := auth.NewSessionManager(false)
	verifier := &fakeVerifier{
		identity: &auth.VerifiedIdentity{
			Subject: "sub-alice",
			Email:   "alice@example.com",
			Name:    "Alice",
			Groups:  []string{"crm-support"},
		},
	}
	svc := auth.NewService(store, auditLogger, sm, verifier, "admin@example.com", testAccountRules()...)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", svc.HandleLogin)
	mux.HandleFunc("/auth/callback", svc.HandleCallback)
	handler := sm.LoadAndSave(mux)

	req1 := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusFound {
		t.Fatalf("got status %d, want 302", rec1.Code)
	}
	loc := rec1.Header().Get("Location")
	stateIdx := strings.Index(loc, "state=")
	if stateIdx < 0 {
		t.Fatalf("missing state in redirect URL: %s", loc)
	}
	stateVal := loc[stateIdx+len("state="):]

	req2 := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state="+stateVal, nil)
	for _, c := range rec1.Result().Cookies() {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusSeeOther || rec2.Header().Get("Location") != "/threads" {
		t.Fatalf("callback failed: code=%d loc=%s body=%s", rec2.Code, rec2.Header().Get("Location"), rec2.Body.String())
	}
}

func testAccountRules() []auth.AccountAccessRule {
	return []auth.AccountAccessRule{
		{AccountID: "support", AccountName: "Support", OIDCGroup: "crm-support"},
		{AccountID: "sales", AccountName: "Sales", OIDCGroup: "crm-sales"},
	}
}

func TestResolveAccountsMapping(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	svc := auth.NewService(store, auditLogger, auth.NewSessionManager(false), &fakeVerifier{}, "admin@example.com", testAccountRules()...)

	cases := []struct {
		name   string
		groups []string
		want   []string
	}{
		{"single", []string{"crm-support"}, []string{"support"}},
		{"both", []string{"crm-support", "crm-sales"}, []string{"support", "sales"}},
		{"unrelated", []string{"staff"}, nil},
		{"none", nil, nil},
	}
	for _, tc := range cases {
		got := svc.ResolveAccounts(tc.groups)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}

func TestLoginSingleAndMultiAccountAccess(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	svc := auth.NewService(store, auditLogger, auth.NewSessionManager(false), &fakeVerifier{}, "admin@example.com", testAccountRules()...)
	ctx := context.Background()

	single, err := svc.ResolveAndLoginUser(ctx, &auth.VerifiedIdentity{
		Subject: "sub-single", Email: "single@example.com", Name: "Single", Groups: []string{"crm-support"},
	}, "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("single account login: %v", err)
	}
	if !single.HasAccountAccess("support") || single.HasAccountAccess("sales") || single.HasMultipleAccounts() {
		t.Fatalf("unexpected single account access: %v", single.AllowedAccounts)
	}

	multi, err := svc.ResolveAndLoginUser(ctx, &auth.VerifiedIdentity{
		Subject: "sub-multi", Email: "multi@example.com", Name: "Multi", Groups: []string{"crm-support", "crm-sales"},
	}, "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("multi account login: %v", err)
	}
	if !multi.HasAccountAccess("support") || !multi.HasAccountAccess("sales") || !multi.HasMultipleAccounts() {
		t.Fatalf("unexpected multi account access: %v", multi.AllowedAccounts)
	}
}

func TestLoginWithoutAccountAccessIsRejected(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	svc := auth.NewService(store, auditLogger, auth.NewSessionManager(false), &fakeVerifier{}, "admin@example.com", testAccountRules()...)

	_, err := svc.ResolveAndLoginUser(context.Background(), &auth.VerifiedIdentity{
		Subject: "sub-nobody", Email: "nobody@example.com", Name: "Nobody", Groups: []string{"staff"},
	}, "127.0.0.1", "test")
	if !errors.Is(err, auth.ErrNoAccountAccess) {
		t.Fatalf("got error %v, want ErrNoAccountAccess", err)
	}
	found := false
	for _, l := range store.auditLogs {
		if l.EventType == "user_login_failed" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected user_login_failed audit event")
	}
}

func TestLoginCallbackWithoutAccountAccessReturns403(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	sm := auth.NewSessionManager(false)
	verifier := &fakeVerifier{
		identity: &auth.VerifiedIdentity{
			Subject: "sub-carol", Email: "carol@example.com", Name: "Carol", Groups: []string{"staff"},
		},
	}
	svc := auth.NewService(store, auditLogger, sm, verifier, "admin@example.com", testAccountRules()...)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", svc.HandleLogin)
	mux.HandleFunc("/auth/callback", svc.HandleCallback)
	handler := sm.LoadAndSave(mux)

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	loc := rec1.Header().Get("Location")
	stateIdx := strings.Index(loc, "state=")
	if stateIdx < 0 {
		t.Fatalf("missing state in redirect URL: %s", loc)
	}
	stateVal := loc[stateIdx+len("state="):]

	req2 := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state="+stateVal, nil)
	for _, c := range rec1.Result().Cookies() {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("got status %d, want 403", rec2.Code)
	}
}
