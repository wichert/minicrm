package auth_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"golang.org/x/oauth2"
)

type memUserStore struct {
	mu              sync.Mutex
	users           map[uuid.UUID]sqlc.User
	settings        *sqlc.SystemSettings
	accountSettings map[string]sqlc.AccountSettings
	auditLogs       []sqlc.AuditLog
	threadLogs      []sqlc.ThreadEvent
	failNextUpdate  error
}

func newMemUserStore() *memUserStore {
	return &memUserStore{users: make(map[uuid.UUID]sqlc.User)}
}

func (m *memUserStore) GetUserByID(_ context.Context, id uuid.UUID) (sqlc.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return sqlc.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (m *memUserStore) GetUserByOIDCSub(_ context.Context, sub string) (sqlc.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.OIDCSub != nil && *u.OIDCSub == sub {
			return u, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *memUserStore) GetUserByEmail(_ context.Context, email string) (sqlc.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *memUserStore) CreateUser(_ context.Context, arg sqlc.CreateUserParams) (sqlc.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNextUpdate != nil {
		err := m.failNextUpdate
		m.failNextUpdate = nil
		return sqlc.User{}, err
	}
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settings == nil {
		return sqlc.SystemSettings{}, pgx.ErrNoRows
	}
	return *m.settings, nil
}

func (m *memUserStore) UpsertSystemSettings(_ context.Context, s sqlc.SystemSettings) (sqlc.SystemSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.ID = 1
	m.settings = &s
	return s, nil
}

func (m *memUserStore) UpsertAccountSettings(_ context.Context, s sqlc.AccountSettings) (sqlc.AccountSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accountSettings == nil {
		m.accountSettings = make(map[string]sqlc.AccountSettings)
	}
	m.accountSettings[s.AccountID] = s
	return s, nil
}

func (m *memUserStore) InsertAuditLog(_ context.Context, entry sqlc.AuditLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditLogs = append(m.auditLogs, entry)
	return nil
}

func (m *memUserStore) InsertThreadEvent(_ context.Context, ev sqlc.ThreadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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

func TestTokenCipherEncryptDecryptAndUserBinding(t *testing.T) {
	cipher, err := auth.NewTokenCipher("test-master-secret-key-for-sessions")
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}
	userA := uuid.New()
	userB := uuid.New()
	rawRefresh := "rt_super_sensitive_refresh_token_value_12345"

	enc, err := cipher.Encrypt(rawRefresh, userA)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(enc, []byte(rawRefresh)) || bytes.Contains(enc, []byte("super_sensitive")) {
		t.Fatal("ciphertext must not contain plaintext refresh token")
	}

	got, err := cipher.Decrypt(enc, userA)
	if err != nil {
		t.Fatalf("Decrypt with matching user: %v", err)
	}
	if got != rawRefresh {
		t.Fatalf("got %q, want %q", got, rawRefresh)
	}

	// Wrong user UUID (AAD mismatch) must fail authentication.
	if _, err := cipher.Decrypt(enc, userB); err == nil {
		t.Fatal("expected Decrypt with wrong userID AAD to fail")
	}

	// Tampered byte must fail authentication.
	tampered := append([]byte(nil), enc...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := cipher.Decrypt(tampered, userA); err == nil {
		t.Fatal("expected Decrypt of tampered ciphertext to fail")
	}
}

func TestExtractRefreshTokenExpiry(t *testing.T) {
	now := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)

	// 1. From refresh_token_expires_in extra field
	tokWithExtra := (&oauth2.Token{RefreshToken: "opaque"}).WithExtra(map[string]any{
		"refresh_token_expires_in": float64(86400 * 14),
	})
	exp, ok := auth.ExtractRefreshTokenExpiry(tokWithExtra, "opaque", now)
	if !ok || !exp.Equal(now.Add(14*24*time.Hour)) {
		t.Fatalf("expected 14d expiry from extra field, got ok=%v exp=%v", ok, exp)
	}

	// 2. From JWT refresh token exp claim
	wantUnix := now.Add(30 * 24 * time.Hour).Unix()
	jwtHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	jwtPayload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sub":"u1","exp":%d}`, wantUnix)))
	jwtRefresh := jwtHeader + "." + jwtPayload + ".sig"
	exp, ok = auth.ExtractRefreshTokenExpiry(nil, jwtRefresh, now)
	if !ok || exp.Unix() != wantUnix {
		t.Fatalf("expected JWT exp %d, got ok=%v exp=%v", wantUnix, ok, exp)
	}

	// 3. Opaque token without extra field
	if _, ok := auth.ExtractRefreshTokenExpiry(nil, "opaque-random-token", now); ok {
		t.Fatal("did not expect expiry for opaque token without metadata")
	}
}

func TestDeepLinkReturnToAndExpiredEmailIframe(t *testing.T) {
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
	authedMux := http.NewServeMux()
	authedMux.HandleFunc("/threads/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	authedMux.HandleFunc("/emails/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/threads/", svc.RequireAuth(authedMux))
	mux.Handle("/emails/", svc.RequireAuth(authedMux))
	handler := sm.LoadAndSave(mux)

	// 1. Unauthenticated iframe request to /emails/{id}/body returns 401 with data-session-expired="true"
	iframeReq := httptest.NewRequest(http.MethodGet, "/emails/123/body", nil)
	iframeReq.Header.Set("Sec-Fetch-Dest", "iframe")
	iframeRec := httptest.NewRecorder()
	handler.ServeHTTP(iframeRec, iframeReq)
	if iframeRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated email iframe, got %d", iframeRec.Code)
	}
	if !strings.Contains(iframeRec.Body.String(), `data-session-expired="true"`) {
		t.Fatalf("expected data-session-expired marker in 401 body, got %s", iframeRec.Body.String())
	}

	// 2. Unauthenticated deep link GET /threads/abc-123?status=open preserves return_to across OIDC login
	deepLink := "/threads/abc-123?status=open"
	req1 := httptest.NewRequest(http.MethodGet, deepLink, nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusSeeOther || rec1.Header().Get("Location") != "/auth/login" {
		t.Fatalf("expected redirect to /auth/login, got %d %s", rec1.Code, rec1.Header().Get("Location"))
	}

	reqLogin := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	for _, c := range rec1.Result().Cookies() {
		reqLogin.AddCookie(c)
	}
	recLogin := httptest.NewRecorder()
	handler.ServeHTTP(recLogin, reqLogin)
	loc := recLogin.Header().Get("Location")
	stateVal := loc[strings.Index(loc, "state=")+len("state="):]

	reqCb := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state="+stateVal, nil)
	for _, c := range recLogin.Result().Cookies() {
		reqCb.AddCookie(c)
	}
	recCb := httptest.NewRecorder()
	handler.ServeHTTP(recCb, reqCb)
	if recCb.Code != http.StatusSeeOther || recCb.Header().Get("Location") != deepLink {
		t.Fatalf("expected callback redirect to %q, got %d %q", deepLink, recCb.Code, recCb.Header().Get("Location"))
	}
}

type rotatingFakeVerifier struct {
	mu           sync.Mutex
	identity     *auth.VerifiedIdentity
	validRefresh string
	nextRefresh  string
	refreshedID  *auth.VerifiedIdentity
	refreshCalls atomic.Int32
	failRefresh  bool
}

func (r *rotatingFakeVerifier) AuthCodeURL(state, _, _ string) string {
	return "https://idp.example.com/authorize?state=" + state
}

func (r *rotatingFakeVerifier) ExchangeAndVerify(_ context.Context, _, _, _ string) (*auth.VerifiedIdentity, error) {
	return r.identity, nil
}

func (r *rotatingFakeVerifier) RefreshAndVerify(_ context.Context, refreshToken string) (*auth.RefreshResult, error) {
	r.refreshCalls.Add(1)
	time.Sleep(25 * time.Millisecond) // widen window to exercise concurrent singleflight callers

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failRefresh || refreshToken != r.validRefresh {
		return nil, errors.New("invalid_grant: refresh token expired or already rotated")
	}
	// Strict rotation: invalidate old refresh token immediately on first use.
	r.validRefresh = r.nextRefresh
	return &auth.RefreshResult{
		Identity:           r.refreshedID,
		RefreshToken:       r.nextRefresh,
		IDTokenExpiry:      time.Now().UTC().Add(30 * time.Minute),
		RefreshTokenExpiry: time.Now().UTC().Add(14 * 24 * time.Hour),
	}, nil
}

func TestRefreshTokenSessionAlignmentEncryptionAndSingleflight(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	sm := auth.NewSessionManagerWithTimeouts(false, 24*time.Hour, 0)
	cipher, err := auth.NewTokenCipher("unit-test-encryption-secret")
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}

	initialRefresh := "rt_initial_secret_value_should_never_be_plaintext"
	rotatedRefresh := "rt_rotated_secret_value_after_singleflight"
	wantRefreshDeadline := time.Now().UTC().Add(14 * 24 * time.Hour).Truncate(time.Second)

	verifier := &rotatingFakeVerifier{
		identity: &auth.VerifiedIdentity{
			Subject:            "sub-dana",
			Email:              "dana@example.com",
			Name:               "Dana",
			Groups:             []string{"crm-support"},
			RefreshToken:       initialRefresh,
			IDTokenExpiry:      time.Now().UTC().Add(-1 * time.Minute), // already due for background refresh
			RefreshTokenExpiry: wantRefreshDeadline,
		},
		validRefresh: initialRefresh,
		nextRefresh:  rotatedRefresh,
		refreshedID: &auth.VerifiedIdentity{
			Subject: "sub-dana",
			Email:   "dana@example.com",
			Name:    "Dana Updated",
			Groups:  []string{"crm-support", "crm-sales"},
		},
	}

	svc := auth.NewService(store, auditLogger, sm, verifier, "admin@example.com", testAccountRules()...).WithTokenCipher(cipher)

	var (
		deadlineMu       sync.Mutex
		observedDeadline time.Time
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", svc.HandleLogin)
	mux.HandleFunc("/auth/callback", svc.HandleCallback)
	mux.Handle("/threads", svc.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadlineMu.Lock()
		observedDeadline = sm.Deadline(r.Context())
		deadlineMu.Unlock()
		w.WriteHeader(http.StatusOK)
	})))
	handler := sm.LoadAndSave(mux)

	// Perform login + callback
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	loc := rec1.Header().Get("Location")
	stateVal := loc[strings.Index(loc, "state=")+len("state="):]

	req2 := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state="+stateVal, nil)
	for _, c := range rec1.Result().Cookies() {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d", rec2.Code)
	}
	sessionCookies := rec2.Result().Cookies()
	if len(sessionCookies) == 0 {
		t.Fatal("expected session cookie on callback")
	}
	// Cookie MaxAge should be aligned to the 14-day refresh token (~1,209,600s), not the 24h default.
	if sessionCookies[0].MaxAge < int((13 * 24 * time.Hour).Seconds()) {
		t.Fatalf("expected cookie MaxAge aligned to 14d refresh token, got %ds", sessionCookies[0].MaxAge)
	}

	// Verify raw bytes in session store do NOT contain the plaintext refresh token.
	if iterable, ok := sm.Store.(scsIterableStore); ok {
		all, err := iterable.All()
		if err != nil {
			t.Fatalf("Store.All: %v", err)
		}
		for _, rawBytes := range all {
			if bytes.Contains(rawBytes, []byte(initialRefresh)) {
				t.Fatal("session store contains plaintext refresh token!")
			}
		}
	}

	// Fire 10 concurrent requests while IDTokenExpiry is expired; singleflight must deduplicate to 1 refresh call.
	const workers = 10
	var wg sync.WaitGroup
	codes := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/threads", nil)
			for _, c := range sessionCookies {
				req.AddCookie(c)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("worker %d got status %d, want 200", i, code)
		}
	}
	if got := verifier.refreshCalls.Load(); got != 1 {
		t.Fatalf("expected singleflight to execute exactly 1 refresh call, got %d", got)
	}
	if observedDeadline.Before(wantRefreshDeadline) {
		t.Fatalf("expected session deadline >= %v, got %v", wantRefreshDeadline, observedDeadline)
	}

	// Verify user's allowed_accounts and display_name were updated from the refreshed ID token.
	dana, err := store.GetUserByOIDCSub(context.Background(), "sub-dana")
	if err != nil {
		t.Fatalf("GetUserByOIDCSub: %v", err)
	}
	if !dana.HasAccountAccess("support") || !dana.HasAccountAccess("sales") || dana.DisplayName != "Dana Updated" {
		t.Fatalf("expected refreshed identity to update user, got %+v", dana)
	}

	// Verify that when a subsequent refresh fails (e.g. revoked at IdP), RequireAuth destroys the session.
	verifier2 := &rotatingFakeVerifier{
		identity: &auth.VerifiedIdentity{
			Subject:       "sub-revoked",
			Email:         "revoked@example.com",
			Name:          "Revoked",
			Groups:        []string{"crm-support"},
			RefreshToken:  "rt_will_be_revoked",
			IDTokenExpiry: time.Now().UTC().Add(-1 * time.Minute),
		},
		failRefresh: true,
	}
	sm2 := auth.NewSessionManager(false)
	svc2 := auth.NewService(store, auditLogger, sm2, verifier2, "admin@example.com", testAccountRules()...).WithTokenCipher(cipher)
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/auth/login", svc2.HandleLogin)
	mux2.HandleFunc("/auth/callback", svc2.HandleCallback)
	mux2.Handle("/threads", svc2.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	handler2 := sm2.LoadAndSave(mux2)

	rLogin := httptest.NewRecorder()
	handler2.ServeHTTP(rLogin, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	s2 := rLogin.Header().Get("Location")[strings.Index(rLogin.Header().Get("Location"), "state=")+len("state="):]
	rCbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state="+s2, nil)
	for _, c := range rLogin.Result().Cookies() {
		rCbReq.AddCookie(c)
	}
	rCb := httptest.NewRecorder()
	handler2.ServeHTTP(rCb, rCbReq)

	rAuthedReq := httptest.NewRequest(http.MethodGet, "/threads", nil)
	for _, c := range rCb.Result().Cookies() {
		rAuthedReq.AddCookie(c)
	}
	rAuthed := httptest.NewRecorder()
	handler2.ServeHTTP(rAuthed, rAuthedReq)
	if rAuthed.Code != http.StatusSeeOther || rAuthed.Header().Get("Location") != "/auth/login" {
		t.Fatalf("expected failed refresh to destroy session and redirect to /auth/login, got %d %s", rAuthed.Code, rAuthed.Header().Get("Location"))
	}
}

type scsIterableStore interface {
	All() (map[string][]byte, error)
}

func doLoginAndGetCookies(t *testing.T, handler http.Handler) []*http.Cookie {
	t.Helper()
	recLogin := httptest.NewRecorder()
	handler.ServeHTTP(recLogin, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	loc := recLogin.Header().Get("Location")
	stateVal := loc[strings.Index(loc, "state=")+len("state="):]
	reqCb := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state="+stateVal, nil)
	for _, c := range recLogin.Result().Cookies() {
		reqCb.AddCookie(c)
	}
	recCb := httptest.NewRecorder()
	handler.ServeHTTP(recCb, reqCb)
	if recCb.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want 303", recCb.Code)
	}
	return recCb.Result().Cookies()
}

type chainedRotatingVerifier struct {
	mu          sync.Mutex
	identity    *auth.VerifiedIdentity
	validToken  string
	rotations   map[string]string
	idExp       time.Time
	refreshedID *auth.VerifiedIdentity
	customErr   error
	calls       int
}

func (c *chainedRotatingVerifier) AuthCodeURL(state, _, _ string) string {
	return "https://idp.example.com/authorize?state=" + state
}

func (c *chainedRotatingVerifier) ExchangeAndVerify(_ context.Context, _, _, _ string) (*auth.VerifiedIdentity, error) {
	return c.identity, nil
}

func (c *chainedRotatingVerifier) RefreshAndVerify(_ context.Context, refreshToken string) (*auth.RefreshResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.customErr != nil {
		return nil, c.customErr
	}
	if refreshToken != c.validToken {
		return nil, auth.NewPermanentAuthError(errors.New("invalid_grant: consumed or unknown refresh token " + refreshToken))
	}
	next := c.rotations[refreshToken]
	if next == "" {
		next = refreshToken
	}
	c.validToken = next
	exp := c.idExp
	if exp.IsZero() {
		exp = time.Now().UTC().Add(30 * time.Minute)
	}
	return &auth.RefreshResult{
		Identity:           c.refreshedID,
		RefreshToken:       next,
		IDTokenExpiry:      exp,
		RefreshTokenExpiry: time.Now().UTC().Add(7 * 24 * time.Hour),
	}, nil
}

// Regression test for PR #1 Comment 1: a request that starts before IDTokenExpiry (skipping
// refresh) and modifies a session key must not overwrite a rotated refresh token committed by
// a concurrent request that ran after IDTokenExpiry.
func TestStaleSessionSaveDoesNotOverwriteRotatedRefreshToken(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	sm := auth.NewSessionManager(false)
	cipher, err := auth.NewTokenCipher("shared-session-encryption-secret")
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}

	verifier := &chainedRotatingVerifier{
		identity: &auth.VerifiedIdentity{
			Subject:            "sub-race",
			Email:              "race@example.com",
			Name:               "Race User",
			Groups:             []string{"crm-support"},
			GroupsPresent:      true,
			RefreshToken:       "rt_gen_1",
			IDTokenExpiry:      time.Now().UTC().Add(1 * time.Second), // initially valid for 1s
			RefreshTokenExpiry: time.Now().UTC().Add(7 * 24 * time.Hour),
		},
		validToken: "rt_gen_1",
		rotations: map[string]string{
			"rt_gen_1": "rt_gen_2",
			"rt_gen_2": "rt_gen_3",
		},
		idExp: time.Now().UTC().Add(2 * time.Second),
	}

	svc := auth.NewService(store, auditLogger, sm, verifier, "admin@example.com", testAccountRules()...).WithTokenCipher(cipher)

	reqAEnteredHandler := make(chan struct{})
	allowReqAFinish := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", svc.HandleLogin)
	mux.HandleFunc("/auth/callback", svc.HandleCallback)
	mux.Handle("/slow-inbox", svc.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sm.Put(r.Context(), "minicrm_last_inbox_url", "/threads?status=new")
		close(reqAEnteredHandler)
		<-allowReqAFinish
		w.WriteHeader(http.StatusOK)
	})))
	mux.Handle("/fast", svc.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sm.GetString(r.Context(), "minicrm_last_inbox_url")))
	})))
	handler := sm.LoadAndSave(mux)

	cookies := doLoginAndGetCookies(t, handler)

	// Start Request A before IDTokenExpiry: it skips refreshSessionIfNeeded and blocks in /slow-inbox.
	var wg sync.WaitGroup
	wg.Add(1)
	var recA *httptest.ResponseRecorder
	go func() {
		defer wg.Done()
		reqA := httptest.NewRequest(http.MethodGet, "/slow-inbox", nil)
		for _, c := range cookies {
			reqA.AddCookie(c)
		}
		recA = httptest.NewRecorder()
		handler.ServeHTTP(recA, reqA)
	}()

	<-reqAEnteredHandler

	// Wait for initial 1s IDTokenExpiry to pass, then run Request B which rotates rt_gen_1 -> rt_gen_2.
	time.Sleep(1100 * time.Millisecond)
	reqB := httptest.NewRequest(http.MethodGet, "/fast", nil)
	for _, c := range cookies {
		reqB.AddCookie(c)
	}
	recB := httptest.NewRecorder()
	handler.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("Request B status = %d, want 200", recB.Code)
	}

	// Now let Request A finish and commit its session (which originally loaded rt_gen_1).
	close(allowReqAFinish)
	wg.Wait()
	if recA.Code != http.StatusOK {
		t.Fatalf("Request A status = %d, want 200", recA.Code)
	}

	// Wait for rt_gen_2's 2s IDTokenExpiry to pass and construct a second Service/SessionManager
	// backed by the exact same session store (simulating after all in-memory caches expire).
	time.Sleep(1100 * time.Millisecond)
	verifier.mu.Lock()
	verifier.idExp = time.Now().UTC().Add(30 * time.Minute)
	verifier.mu.Unlock()

	smAfter := auth.NewSessionManager(false)
	smAfter.Store = sm.Store
	svcAfter := auth.NewService(store, auditLogger, smAfter, verifier, "admin@example.com", testAccountRules()...).WithTokenCipher(cipher)
	muxAfter := http.NewServeMux()
	muxAfter.Handle("/fast", svcAfter.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(smAfter.GetString(r.Context(), "minicrm_last_inbox_url")))
	})))
	handlerAfter := smAfter.LoadAndSave(muxAfter)

	reqC := httptest.NewRequest(http.MethodGet, "/fast", nil)
	for _, c := range cookies {
		reqC.AddCookie(c)
	}
	recC := httptest.NewRecorder()
	handlerAfter.ServeHTTP(recC, reqC)
	if recC.Code != http.StatusOK {
		t.Fatalf("Request C status = %d, want 200 (stale session overwrite detected)", recC.Code)
	}
	if got := recC.Body.String(); got != "/threads?status=new" {
		t.Fatalf("expected Request A's session key to be preserved, got %q", got)
	}
	verifier.mu.Lock()
	validNow := verifier.validToken
	callsNow := verifier.calls
	verifier.mu.Unlock()
	if validNow != "rt_gen_3" || callsNow != 2 {
		t.Fatalf("expected rotation to reach rt_gen_3 in 2 calls, got validToken=%q calls=%d", validNow, callsNow)
	}
}

// Regression test for PR #1 Comment 2: refreshed identity with empty or mismatched subject is rejected.
func TestRefreshRejectsMismatchedOrEmptySubject(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	sm := auth.NewSessionManager(false)

	verifier := &chainedRotatingVerifier{
		identity: &auth.VerifiedIdentity{
			Subject:       "sub-legit",
			Email:         "legit@example.com",
			Name:          "Legit User",
			Groups:        []string{"crm-support"},
			GroupsPresent: true,
			RefreshToken:  "rt_1",
			IDTokenExpiry: time.Now().UTC().Add(-1 * time.Minute),
		},
		validToken: "rt_1",
		refreshedID: &auth.VerifiedIdentity{
			Subject:       "sub-attacker",
			Email:         "attacker@example.com",
			Name:          "Attacker",
			Groups:        []string{"crm-support", "crm-sales"},
			GroupsPresent: true,
		},
	}

	svc := auth.NewService(store, auditLogger, sm, verifier, "admin@example.com", testAccountRules()...)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", svc.HandleLogin)
	mux.HandleFunc("/auth/callback", svc.HandleCallback)
	mux.Handle("/threads", svc.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	handler := sm.LoadAndSave(mux)

	cookies := doLoginAndGetCookies(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/threads", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/auth/login" {
		t.Fatalf("expected subject mismatch to destroy session and redirect to /auth/login, got %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// Verify user record in store was not modified with attacker's claims.
	u, err := store.GetUserByOIDCSub(context.Background(), "sub-legit")
	if err != nil {
		t.Fatalf("GetUserByOIDCSub: %v", err)
	}
	if u.DisplayName != "Legit User" || u.HasAccountAccess("sales") {
		t.Fatalf("user record must not be modified on subject mismatch: %+v", u)
	}
}

// Regression test for PR #1 Comment 3: when refreshed claims remove the user's final group,
// empty allowed_accounts is persisted in the store and audited before rejecting the session.
func TestRefreshPersistsEmptyAllowedAccountsOnFinalGroupRevocation(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	sm := auth.NewSessionManager(false)

	verifier := &chainedRotatingVerifier{
		identity: &auth.VerifiedIdentity{
			Subject:       "sub-revoked-groups",
			Email:         "user@example.com",
			Name:          "User",
			Groups:        []string{"crm-support", "crm-sales"},
			GroupsPresent: true,
			RefreshToken:  "rt_revoke_1",
			IDTokenExpiry: time.Now().UTC().Add(-1 * time.Minute),
		},
		validToken: "rt_revoke_1",
		refreshedID: &auth.VerifiedIdentity{
			Subject:       "sub-revoked-groups",
			Name:          "User",
			Groups:        []string{},
			GroupsPresent: true, // authoritative empty group list
		},
	}

	svc := auth.NewService(store, auditLogger, sm, verifier, "admin@example.com", testAccountRules()...)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", svc.HandleLogin)
	mux.HandleFunc("/auth/callback", svc.HandleCallback)
	mux.Handle("/threads", svc.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	handler := sm.LoadAndSave(mux)

	cookies := doLoginAndGetCookies(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/threads", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/auth/login" {
		t.Fatalf("expected revoked groups to redirect to /auth/login, got %d", rec.Code)
	}

	u, err := store.GetUserByOIDCSub(context.Background(), "sub-revoked-groups")
	if err != nil {
		t.Fatalf("GetUserByOIDCSub: %v", err)
	}
	if len(u.AllowedAccounts) != 0 {
		t.Fatalf("expected AllowedAccounts to be persisted as empty, got %v", u.AllowedAccounts)
	}
}

// Regression test for PR #1 Comment 4: transient IdP or DB failures return 503 without
// destroying the session, and a rotated token is retained across a transient DB failure.
func TestRefreshPreservesSessionOnTransientIdPAndDBFailures(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	sm := auth.NewSessionManager(false)

	verifier := &chainedRotatingVerifier{
		identity: &auth.VerifiedIdentity{
			Subject:       "sub-transient",
			Email:         "transient@example.com",
			Name:          "Transient User",
			Groups:        []string{"crm-support"},
			GroupsPresent: true,
			RefreshToken:  "rt_t1",
			IDTokenExpiry: time.Now().UTC().Add(-1 * time.Minute),
		},
		validToken: "rt_t1",
		rotations: map[string]string{
			"rt_t1": "rt_t2",
		},
		refreshedID: &auth.VerifiedIdentity{
			Subject:       "sub-transient",
			Name:          "Transient User",
			Groups:        []string{"crm-support", "crm-sales"},
			GroupsPresent: true,
		},
		customErr: errors.New("idp 503 service temporarily unavailable"),
	}

	svc := auth.NewService(store, auditLogger, sm, verifier, "admin@example.com", testAccountRules()...)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", svc.HandleLogin)
	mux.HandleFunc("/auth/callback", svc.HandleCallback)
	mux.Handle("/threads", svc.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	handler := sm.LoadAndSave(mux)

	cookies := doLoginAndGetCookies(t, handler)

	// 1. Transient IdP outage returns 503 and preserves session.
	req1 := httptest.NewRequest(http.MethodGet, "/threads", nil)
	for _, c := range cookies {
		req1.AddCookie(c)
	}
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on transient IdP error, got %d", rec1.Code)
	}

	// 2. IdP recovers and rotates rt_t1 -> rt_t2, but local DB UpdateUserOIDC fails transiently.
	verifier.mu.Lock()
	verifier.customErr = nil
	verifier.mu.Unlock()
	store.mu.Lock()
	store.failNextUpdate = errors.New("temporary database connection error")
	store.mu.Unlock()

	req2 := httptest.NewRequest(http.MethodGet, "/threads", nil)
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on transient DB sync error, got %d", rec2.Code)
	}
	if freshCookies := rec2.Result().Cookies(); len(freshCookies) > 0 {
		cookies = freshCookies
	}

	// 3. Next request retries synchronization using the retained rotated token rt_t2 and succeeds!
	req3 := httptest.NewRequest(http.MethodGet, "/threads", nil)
	for _, c := range cookies {
		req3.AddCookie(c)
	}
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200 after transient DB recovery, got %d", rec3.Code)
	}
	u, err := store.GetUserByOIDCSub(context.Background(), "sub-transient")
	if err != nil || !u.HasAccountAccess("sales") {
		t.Fatalf("expected user permissions to be synced on retry, got %+v (err=%v)", u, err)
	}
}

// Regression test for PR #1 Comment 5: refreshed ID token omitting email, name, and groups
// succeeds and retains the user's existing display name and account permissions.
func TestRefreshHandlesOmittedOptionalClaims(t *testing.T) {
	store := newMemUserStore()
	auditLogger := audit.NewLogger(store, nil)
	sm := auth.NewSessionManager(false)

	verifier := &chainedRotatingVerifier{
		identity: &auth.VerifiedIdentity{
			Subject:       "sub-omitted",
			Email:         "omitted@example.com",
			Name:          "Original Name",
			Groups:        []string{"crm-support"},
			GroupsPresent: true,
			RefreshToken:  "rt_o1",
			IDTokenExpiry: time.Now().UTC().Add(-1 * time.Minute),
		},
		validToken: "rt_o1",
		refreshedID: &auth.VerifiedIdentity{
			Subject:       "sub-omitted",
			Email:         "",    // omitted in refresh token
			Name:          "",    // omitted in refresh token
			Groups:        nil,   // omitted in refresh token
			GroupsPresent: false, // groups key absent from refreshed ID token
		},
	}

	svc := auth.NewService(store, auditLogger, sm, verifier, "admin@example.com", testAccountRules()...)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", svc.HandleLogin)
	mux.HandleFunc("/auth/callback", svc.HandleCallback)
	mux.Handle("/threads", svc.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	handler := sm.LoadAndSave(mux)

	cookies := doLoginAndGetCookies(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/threads", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when optional claims are omitted on refresh, got %d", rec.Code)
	}

	u, err := store.GetUserByOIDCSub(context.Background(), "sub-omitted")
	if err != nil {
		t.Fatalf("GetUserByOIDCSub: %v", err)
	}
	if u.DisplayName != "Original Name" || !u.HasAccountAccess("support") {
		t.Fatalf("expected user to retain Original Name and support access, got %+v", u)
	}
}
