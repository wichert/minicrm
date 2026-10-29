// Package auth implements OAuth2/OIDC authentication, user provisioning, and session middleware.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

// ErrUserInactive is returned when a deactivated user attempts to log in.
var ErrUserInactive = errors.New("user account is inactive")

// ErrNoAccountAccess is returned when a user's OIDC groups grant access to no mail account.
var ErrNoAccountAccess = errors.New("user has no access to any mail account")

// ErrOIDCSubjectMismatch is returned when a refreshed ID token's sub claim is empty or
// does not match the signed-in user's bound oidc_sub.
var ErrOIDCSubjectMismatch = errors.New("oidc id_token subject mismatch")

// PermanentAuthError marks an authentication or authorization failure that definitively
// invalidates the current session, as opposed to a transient network or database error.
type PermanentAuthError struct {
	Err error
}

func (e *PermanentAuthError) Error() string {
	if e == nil || e.Err == nil {
		return "permanent authentication failure"
	}
	return e.Err.Error()
}

func (e *PermanentAuthError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewPermanentAuthError wraps err in a PermanentAuthError.
func NewPermanentAuthError(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentAuthError{Err: err}
}

// AccountAccessRule links an OIDC group/role claim value to a mail account ID.
type AccountAccessRule struct {
	AccountID   string
	AccountName string
	OIDCGroup   string
}

// Session keys used by SCS.
const (
	SessionKeyUserID       = "auth_user_id"
	SessionKeySessionID    = "auth_session_id"
	SessionKeyOIDCState    = "oidc_state"
	SessionKeyOIDCNonce    = "oidc_nonce"
	SessionKeyPKCEVerifier = "oidc_pkce_verifier"
	SessionKeyReturnTo     = "auth_return_to"
	// #nosec G101 -- SCS session map key names, not credentials
	SessionKeyRefreshToken = "auth_refresh_token_enc"
	// #nosec G101 -- SCS session map key names, not credentials
	SessionKeyIDTokenExpiry     = "auth_id_token_expiry"
	SessionKeyRefreshExpiry     = "auth_refresh_expiry"
	SessionKeyRefreshGen        = "auth_refresh_gen"
	defaultIDTokenRefreshWindow = 15 * time.Minute
	recentRefreshCacheTTL       = 30 * time.Second
)

// VerifiedIdentity represents the claims verified from the OIDC ID Token,
// along with optional OAuth2 token lifecycle metadata for session alignment.
type VerifiedIdentity struct {
	Subject string
	Email   string
	Name    string
	Groups  []string
	// GroupsPresent reports whether the groups claim was explicitly present in the
	// verified ID token or UserInfo response. On login this is always true; on token
	// refresh, false indicates the provider omitted the groups claim.
	GroupsPresent      bool
	RefreshToken       string
	IDTokenExpiry      time.Time
	RefreshTokenExpiry time.Time
}

// RefreshResult holds the outcome of a server-side OAuth2 refresh_token exchange.
type RefreshResult struct {
	Identity           *VerifiedIdentity
	RefreshToken       string
	IDTokenExpiry      time.Time
	RefreshTokenExpiry time.Time
}

// IdentityVerifier exchanges an OAuth2 authorization code + PKCE verifier for a VerifiedIdentity.
type IdentityVerifier interface {
	AuthCodeURL(state, nonce, codeVerifier string) string
	ExchangeAndVerify(ctx context.Context, code, codeVerifier, expectedNonce string) (*VerifiedIdentity, error)
}

// TokenRefresher is an optional extension of IdentityVerifier that refreshes an OAuth2
// refresh_token server-to-server and verifies any updated OIDC ID token.
type TokenRefresher interface {
	RefreshAndVerify(ctx context.Context, refreshToken string) (*RefreshResult, error)
}

// OIDCProvider implements IdentityVerifier using github.com/coreos/go-oidc/v3 and golang.org/x/oauth2.
type OIDCProvider struct {
	provider     *oidc.Provider
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	groupsClaim  string
}

// NewOIDCProvider discovers the OIDC issuer and constructs an OIDCProvider.
//
// groupsClaim names the ID token claim holding the user's group/role memberships.
// scopes are the scopes requested from the provider; when empty they default to
// "openid profile email". "openid" is always requested.
func NewOIDCProvider(ctx context.Context, issuerURL, clientID, clientSecret, redirectURL, groupsClaim string, scopes []string) (*OIDCProvider, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("discover oidc provider %q: %w", issuerURL, err)
	}
	oauthCfg := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       normalizeScopes(scopes),
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: clientID})
	if strings.TrimSpace(groupsClaim) == "" {
		groupsClaim = "groups"
	}
	return &OIDCProvider{
		provider:     provider,
		oauth2Config: oauthCfg,
		verifier:     verifier,
		groupsClaim:  groupsClaim,
	}, nil
}

// normalizeScopes returns the requested OAuth2 scopes, defaulting to
// "openid profile email" and always including "openid".
func normalizeScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return []string{oidc.ScopeOpenID, "profile", "email"}
	}
	out := make([]string, 0, len(scopes)+1)
	hasOpenID := false
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if _, dup := seen[scope]; dup {
			continue
		}
		seen[scope] = struct{}{}
		if scope == oidc.ScopeOpenID {
			hasOpenID = true
		}
		out = append(out, scope)
	}
	if !hasOpenID {
		out = append(out, oidc.ScopeOpenID)
	}
	return out
}

// AuthCodeURL returns the OAuth2 authorization URL with PKCE S256 challenge and OIDC nonce.
func (p *OIDCProvider) AuthCodeURL(state, nonce, codeVerifier string) string {
	return p.oauth2Config.AuthCodeURL(
		state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(codeVerifier),
	)
}

// ExchangeAndVerify exchanges the authorization code using PKCE and verifies the resulting OIDC ID Token.
func (p *OIDCProvider) ExchangeAndVerify(ctx context.Context, code, codeVerifier, expectedNonce string) (*VerifiedIdentity, error) {
	token, err := p.oauth2Config.Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return nil, fmt.Errorf("oauth2 code exchange: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, errors.New("missing id_token in oauth2 token response")
	}
	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("verify oidc id_token: %w", err)
	}
	if idToken.Nonce != expectedNonce {
		return nil, errors.New("oidc id_token nonce mismatch")
	}
	identity, err := p.parseVerifiedIdentity(idToken)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	identity.RefreshToken = token.RefreshToken
	identity.IDTokenExpiry = idToken.Expiry.UTC()
	if identity.IDTokenExpiry.IsZero() && !token.Expiry.IsZero() {
		identity.IDTokenExpiry = token.Expiry.UTC()
	}
	if token.RefreshToken != "" {
		if refreshExp, ok := ExtractRefreshTokenExpiry(token, token.RefreshToken, now); ok {
			identity.RefreshTokenExpiry = refreshExp
		}
	}
	return identity, nil
}

// RefreshAndVerify exchanges a refresh_token at the OIDC provider's token endpoint and
// verifies the refreshed ID token when present.
func (p *OIDCProvider) RefreshAndVerify(ctx context.Context, refreshToken string) (*RefreshResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, NewPermanentAuthError(errors.New("empty refresh token"))
	}
	ts := p.oauth2Config.TokenSource(ctx, &oauth2.Token{
		RefreshToken: refreshToken,
		Expiry:       time.Unix(1, 0),
	})
	token, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("oauth2 token refresh: %w", err)
	}
	now := time.Now().UTC()
	newRefresh := token.RefreshToken
	if newRefresh == "" {
		newRefresh = refreshToken
	}
	res := &RefreshResult{
		RefreshToken:  newRefresh,
		IDTokenExpiry: token.Expiry.UTC(),
	}
	if refreshExp, ok := ExtractRefreshTokenExpiry(token, newRefresh, now); ok {
		res.RefreshTokenExpiry = refreshExp
	}
	if rawIDToken, ok := token.Extra("id_token").(string); ok && rawIDToken != "" {
		idToken, err := p.verifier.Verify(ctx, rawIDToken)
		if err != nil {
			return nil, NewPermanentAuthError(fmt.Errorf("verify refreshed oidc id_token: %w", err))
		}
		identity, err := p.parseRefreshedIdentity(ctx, idToken, token)
		if err != nil {
			return nil, err
		}
		res.Identity = identity
		if !idToken.Expiry.IsZero() {
			res.IDTokenExpiry = idToken.Expiry.UTC()
		}
	}
	return res, nil
}

func (p *OIDCProvider) parseVerifiedIdentity(idToken *oidc.IDToken) (*VerifiedIdentity, error) {
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("parse oidc claims: %w", err)
	}
	email, _ := claims["email"].(string)
	name, _ := claims["name"].(string)
	if strings.TrimSpace(email) == "" {
		return nil, errors.New("oidc id_token missing email claim")
	}
	displayName := strings.TrimSpace(name)
	if displayName == "" {
		displayName = email
	}
	return &VerifiedIdentity{
		Subject:       idToken.Subject,
		Email:         strings.TrimSpace(email),
		Name:          displayName,
		Groups:        extractGroups(claims[p.groupsClaim]),
		GroupsPresent: true,
	}, nil
}

// parseRefreshedIdentity extracts claims from a refreshed ID token (where optional claims
// such as email, name, or groups may be omitted per OIDC Core §12.2) and consults UserInfo
// when the groups claim is omitted and the provider exposes a UserInfo endpoint.
func (p *OIDCProvider) parseRefreshedIdentity(ctx context.Context, idToken *oidc.IDToken, token *oauth2.Token) (*VerifiedIdentity, error) {
	sub := strings.TrimSpace(idToken.Subject)
	if sub == "" {
		return nil, NewPermanentAuthError(errors.New("refreshed oidc id_token missing sub claim"))
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, NewPermanentAuthError(fmt.Errorf("parse refreshed oidc claims: %w", err))
	}
	email, _ := claims["email"].(string)
	name, _ := claims["name"].(string)
	rawGroups, groupsPresent := claims[p.groupsClaim]
	groups := extractGroups(rawGroups)

	if !groupsPresent && p.provider != nil && token != nil && token.AccessToken != "" {
		ui, err := p.provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
		if err != nil {
			var retrieveErr *oauth2.RetrieveError
			if errors.As(err, &retrieveErr) && retrieveErr.Response != nil &&
				(retrieveErr.Response.StatusCode == http.StatusBadRequest || retrieveErr.Response.StatusCode == http.StatusUnauthorized) {
				return nil, NewPermanentAuthError(fmt.Errorf("oidc userinfo rejected token: %w", err))
			}
			return nil, fmt.Errorf("fetch oidc userinfo on refresh: %w", err)
		}
		if ui.Subject != "" && ui.Subject != sub {
			return nil, NewPermanentAuthError(ErrOIDCSubjectMismatch)
		}
		var uiClaims map[string]any
		if err := ui.Claims(&uiClaims); err != nil {
			return nil, NewPermanentAuthError(fmt.Errorf("parse oidc userinfo claims: %w", err))
		}
		if rawUIGroups, ok := uiClaims[p.groupsClaim]; ok {
			groups = extractGroups(rawUIGroups)
			groupsPresent = true
		}
		if strings.TrimSpace(name) == "" {
			name, _ = uiClaims["name"].(string)
		}
		if strings.TrimSpace(email) == "" {
			email, _ = uiClaims["email"].(string)
		}
	}

	return &VerifiedIdentity{
		Subject:       sub,
		Email:         strings.TrimSpace(email),
		Name:          strings.TrimSpace(name),
		Groups:        groups,
		GroupsPresent: groupsPresent,
	}, nil
}

// extractGroups normalizes a groups/roles claim value into a string slice.
// OIDC providers may emit either a JSON array of strings or a single string.
func extractGroups(raw any) []string {
	switch v := raw.(type) {
	case []any:
		groups := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				groups = append(groups, strings.TrimSpace(s))
			}
		}
		return groups
	case []string:
		return v
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{strings.TrimSpace(v)}
	default:
		return nil
	}
}

// UserStore defines the persistence operations required for user authentication and provisioning.
type UserStore interface {
	GetUserByID(ctx context.Context, id uuid.UUID) (sqlc.User, error)
	GetUserByOIDCSub(ctx context.Context, sub string) (sqlc.User, error)
	GetUserByEmail(ctx context.Context, email string) (sqlc.User, error)
	CreateUser(ctx context.Context, arg sqlc.CreateUserParams) (sqlc.User, error)
	UpdateUserOIDC(ctx context.Context, id uuid.UUID, oidcSub, displayName string, allowedAccounts []string) (sqlc.User, error)
	GetSystemSettings(ctx context.Context) (sqlc.SystemSettings, error)
	UpsertSystemSettings(ctx context.Context, s sqlc.SystemSettings) (sqlc.SystemSettings, error)
	UpsertAccountSettings(ctx context.Context, s sqlc.AccountSettings) (sqlc.AccountSettings, error)
}

type cachedRefreshOutcome struct {
	result          *RefreshResult
	updatedUser     *sqlc.User
	idTokenExpiry   time.Time
	refreshDeadline time.Time
	cachedAt        time.Time
}

type versionedSessionCreds struct {
	userID          uuid.UUID
	gen             int64
	encRefreshToken []byte
	idTokenExpiry   int64
	refreshExpiry   int64
	refreshDeadline time.Time
	updatedAt       time.Time
}

type pendingSyncState struct {
	result          *RefreshResult
	refreshDeadline time.Time
	cachedAt        time.Time
}

// Service manages OIDC login, pre-created user resolution, bootstrap seeding, and audit logging.
type Service struct {
	store           UserStore
	audit           *audit.Logger
	sessions        *scs.SessionManager
	verifier        IdentityVerifier
	bootstrapEmail  string
	accountRules    []AccountAccessRule
	tokenCipher     *TokenCipher
	refreshGroup    singleflight.Group
	recentRefreshMu sync.Mutex
	recentRefreshes map[string]cachedRefreshOutcome
	pendingSyncs    map[string]pendingSyncState
	sessionCredsMu  sync.Mutex
	sessionCreds    map[string]versionedSessionCreds
}

// NewService constructs an authentication Service.
//
// accountRules map OIDC group/role claim values to the mail account IDs they grant access to.
func NewService(store UserStore, auditLogger *audit.Logger, sessions *scs.SessionManager, verifier IdentityVerifier, bootstrapEmail string, accountRules ...AccountAccessRule) *Service {
	cipher, _ := NewTokenCipher()
	svc := &Service{
		store:           store,
		audit:           auditLogger,
		sessions:        sessions,
		verifier:        verifier,
		bootstrapEmail:  strings.TrimSpace(bootstrapEmail),
		accountRules:    accountRules,
		tokenCipher:     cipher,
		recentRefreshes: make(map[string]cachedRefreshOutcome),
		pendingSyncs:    make(map[string]pendingSyncState),
		sessionCreds:    make(map[string]versionedSessionCreds),
	}
	if sessions != nil {
		if _, alreadyWrapped := sessions.Codec.(*sessionCredsCodec); !alreadyWrapped {
			baseCodec := sessions.Codec
			if baseCodec == nil {
				baseCodec = scs.GobCodec{}
			}
			sessions.Codec = &sessionCredsCodec{base: baseCodec, svc: svc}
		}
	}
	return svc
}

// WithTokenCipher configures the AES-256-GCM TokenCipher used to encrypt refresh tokens at rest.
func (s *Service) WithTokenCipher(cipher *TokenCipher) *Service {
	if cipher != nil {
		s.tokenCipher = cipher
	}
	return s
}

// AccountRules returns the configured OIDC-group-to-account mappings.
func (s *Service) AccountRules() []AccountAccessRule {
	out := make([]AccountAccessRule, len(s.accountRules))
	copy(out, s.accountRules)
	return out
}

// ResolveAccounts maps a set of OIDC groups onto the configured mail account IDs.
func (s *Service) ResolveAccounts(groups []string) []string {
	allowed := make([]string, 0, len(s.accountRules))
	for _, rule := range s.accountRules {
		for _, g := range groups {
			if g == rule.OIDCGroup {
				allowed = append(allowed, rule.AccountID)
				break
			}
		}
	}
	return allowed
}

// SeedBootstrapAdminAndSettings ensures the bootstrap admin user and singleton system_settings exist.
func (s *Service) SeedBootstrapAdminAndSettings(ctx context.Context, email, name, displayTimezone string) (sqlc.User, sqlc.SystemSettings, error) {
	u, err := s.store.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		u, err = s.store.CreateUser(ctx, sqlc.CreateUserParams{
			Email:       email,
			DisplayName: name,
			Role:        "admin",
			IsActive:    true,
		})
		if err != nil {
			return sqlc.User{}, sqlc.SystemSettings{}, fmt.Errorf("create bootstrap admin: %w", err)
		}
		if err := s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
			TargetUserID: &u.ID,
			EventType:    "user_created",
			NewValue:     u,
		}); err != nil {
			return sqlc.User{}, sqlc.SystemSettings{}, err
		}
	} else if err != nil {
		return sqlc.User{}, sqlc.SystemSettings{}, fmt.Errorf("lookup bootstrap admin: %w", err)
	}

	settings, err := s.store.GetSystemSettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		settings, err = s.store.UpsertSystemSettings(ctx, sqlc.SystemSettings{
			ID:                     1,
			DisplayTimezone:        displayTimezone,
			IMAPInboxMailbox:       "INBOX",
			IMAPArchiveMailbox:     "Archive",
			IMAPJunkMailbox:        "Junk",
			IMAPSentMailbox:        "Sent",
			SubjectMatchWindowDays: 30,
			AttentionAgeDays:       2,
		})
		if err != nil {
			return sqlc.User{}, sqlc.SystemSettings{}, fmt.Errorf("seed system_settings: %w", err)
		}
		if err := s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
			ActorUserID: &u.ID,
			EventType:   "settings_updated",
			NewValue:    settings,
		}); err != nil {
			return sqlc.User{}, sqlc.SystemSettings{}, err
		}
	} else if err != nil {
		return sqlc.User{}, sqlc.SystemSettings{}, fmt.Errorf("get system_settings: %w", err)
	}

	// Seed the bootstrap admin's account access so it can be used as a default assignee.
	allAccounts := make([]string, 0, len(s.accountRules))
	for _, rule := range s.accountRules {
		allAccounts = append(allAccounts, rule.AccountID)
	}
	if len(allAccounts) > 0 {
		if _, err := s.store.UpdateUserOIDC(ctx, u.ID, derefOr(u.OIDCSub, ""), u.DisplayName, allAccounts); err != nil {
			return sqlc.User{}, sqlc.SystemSettings{}, fmt.Errorf("seed bootstrap admin account access: %w", err)
		}
	}
	return u, settings, nil
}

func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// PrecreateUser allows an admin to pre-create an assignee user before their first OIDC login.
func (s *Service) PrecreateUser(ctx context.Context, actorID uuid.UUID, email, displayName, role string, allowedAccounts ...string) (sqlc.User, error) {
	email = strings.TrimSpace(email)
	displayName = strings.TrimSpace(displayName)
	if email == "" || displayName == "" {
		return sqlc.User{}, errors.New("email and display_name are required")
	}
	if role != "agent" && role != "admin" {
		role = "agent"
	}
	accounts := make([]string, 0, len(allowedAccounts))
	seen := make(map[string]struct{}, len(allowedAccounts))
	for _, id := range allowedAccounts {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		accounts = append(accounts, id)
	}
	u, err := s.store.CreateUser(ctx, sqlc.CreateUserParams{
		Email:           email,
		DisplayName:     displayName,
		Role:            role,
		IsActive:        true,
		AllowedAccounts: accounts,
	})
	if err != nil {
		return sqlc.User{}, fmt.Errorf("precreate user: %w", err)
	}
	if err := s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
		ActorUserID:  &actorID,
		TargetUserID: &u.ID,
		EventType:    "user_precreated",
		NewValue:     u,
	}); err != nil {
		return sqlc.User{}, err
	}
	return u, nil
}

// ResolveAndLoginUser resolves a VerifiedIdentity against existing or pre-created users and logs audit events.
//
// The identity's OIDC groups are mapped onto configured mail accounts; users whose groups
// grant access to no account are rejected with ErrNoAccountAccess.
func (s *Service) ResolveAndLoginUser(ctx context.Context, id *VerifiedIdentity, ipAddr, userAgent string) (sqlc.User, error) {
	allowedAccounts := s.ResolveAccounts(id.Groups)
	if len(allowedAccounts) == 0 {
		s.revokeExistingUserAccountsOnLogin(ctx, id, ipAddr, userAgent)
		_ = s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
			EventType: "user_login_failed",
			IPAddress: ipAddr,
			UserAgent: userAgent,
			NewValue: map[string]any{
				"reason": "no_account_access",
				"email":  id.Email,
				"groups": id.Groups,
			},
		})
		return sqlc.User{}, ErrNoAccountAccess
	}

	// 1. Lookup by oidc_sub
	var (
		u   sqlc.User
		err error
	)
	u, err = s.store.GetUserByOIDCSub(ctx, id.Subject)
	switch {
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return sqlc.User{}, fmt.Errorf("lookup user by oidc_sub: %w", err)
	case err == nil:
		// 4. Existing user logging in again: refresh the OIDC-derived account access.
		if !slices.Equal(u.AllowedAccounts, allowedAccounts) {
			oldSnap := u
			sub := derefOr(u.OIDCSub, id.Subject)
			u, err = s.store.UpdateUserOIDC(ctx, u.ID, sub, u.DisplayName, allowedAccounts)
			if err != nil {
				return sqlc.User{}, fmt.Errorf("refresh user account access: %w", err)
			}
			if err := s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
				ActorUserID:  &u.ID,
				TargetUserID: &u.ID,
				EventType:    "user_updated",
				IPAddress:    ipAddr,
				UserAgent:    userAgent,
				OldValue:     oldSnap,
				NewValue:     u,
			}); err != nil {
				return sqlc.User{}, err
			}
		}
	default:
		// 2. Lookup by email (matches pre-created users)
		existing, emailErr := s.store.GetUserByEmail(ctx, id.Email)
		switch {
		case emailErr == nil:
			oldSnap := existing
			u, err = s.store.UpdateUserOIDC(ctx, existing.ID, id.Subject, id.Name, allowedAccounts)
			if err != nil {
				return sqlc.User{}, fmt.Errorf("bind oidc_sub to pre-created user: %w", err)
			}
			if err := s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
				ActorUserID:  &u.ID,
				TargetUserID: &u.ID,
				EventType:    "user_updated",
				IPAddress:    ipAddr,
				UserAgent:    userAgent,
				OldValue:     oldSnap,
				NewValue:     u,
			}); err != nil {
				return sqlc.User{}, err
			}
		case errors.Is(emailErr, pgx.ErrNoRows):
			// 3. Auto-provision new user on first login
			role := "agent"
			if strings.EqualFold(id.Email, s.bootstrapEmail) {
				role = "admin"
			}
			sub := id.Subject
			u, err = s.store.CreateUser(ctx, sqlc.CreateUserParams{
				OIDCSub:         &sub,
				Email:           id.Email,
				DisplayName:     id.Name,
				Role:            role,
				IsActive:        true,
				AllowedAccounts: allowedAccounts,
			})
			if err != nil {
				return sqlc.User{}, fmt.Errorf("create user on oidc login: %w", err)
			}
			if err := s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
				ActorUserID:  &u.ID,
				TargetUserID: &u.ID,
				EventType:    "user_created",
				IPAddress:    ipAddr,
				UserAgent:    userAgent,
				NewValue:     u,
			}); err != nil {
				return sqlc.User{}, err
			}
		default:
			return sqlc.User{}, fmt.Errorf("lookup user by email: %w", emailErr)
		}
	}

	if !u.IsActive {
		_ = s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
			ActorUserID:  &u.ID,
			TargetUserID: &u.ID,
			EventType:    "user_login_failed",
			IPAddress:    ipAddr,
			UserAgent:    userAgent,
			NewValue:     map[string]string{"reason": "inactive_user"},
		})
		return sqlc.User{}, ErrUserInactive
	}

	if err := s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
		ActorUserID:  &u.ID,
		TargetUserID: &u.ID,
		EventType:    "user_login",
		IPAddress:    ipAddr,
		UserAgent:    userAgent,
	}); err != nil {
		return sqlc.User{}, err
	}
	return u, nil
}

// HandleLogin initiates the OAuth2 Authorization Code + PKCE redirect.
func (s *Service) HandleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomToken(24)
	if err != nil {
		http.Error(w, "failed to generate state", http.StatusInternalServerError)
		return
	}
	nonce, err := randomToken(24)
	if err != nil {
		http.Error(w, "failed to generate nonce", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()

	s.sessions.Put(r.Context(), SessionKeyOIDCState, state)
	s.sessions.Put(r.Context(), SessionKeyOIDCNonce, nonce)
	s.sessions.Put(r.Context(), SessionKeyPKCEVerifier, verifier)

	http.Redirect(w, r, s.verifier.AuthCodeURL(state, nonce, verifier), http.StatusFound)
}

// HandleCallback processes the OIDC callback, logs the user in, and starts a session.
func (s *Service) HandleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	expectedState := s.sessions.PopString(ctx, SessionKeyOIDCState)
	expectedNonce := s.sessions.PopString(ctx, SessionKeyOIDCNonce)
	codeVerifier := s.sessions.PopString(ctx, SessionKeyPKCEVerifier)

	if expectedState == "" || r.URL.Query().Get("state") != expectedState {
		http.Error(w, "invalid oauth2 state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	identity, err := s.verifier.ExchangeAndVerify(ctx, code, codeVerifier, expectedNonce)
	if err != nil {
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}

	user, err := s.ResolveAndLoginUser(ctx, identity, r.RemoteAddr, r.UserAgent())
	if errors.Is(err, ErrUserInactive) {
		http.Error(w, "account is inactive", http.StatusForbidden)
		return
	}
	if errors.Is(err, ErrNoAccountAccess) {
		http.Error(w, "no access to any mail account", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "user provisioning failed", http.StatusInternalServerError)
		return
	}

	returnTo := s.sessions.PopString(ctx, SessionKeyReturnTo)
	if err := s.sessions.RenewToken(ctx); err != nil {
		http.Error(w, "session renewal failed", http.StatusInternalServerError)
		return
	}
	sessionID := uuid.NewString()
	s.sessions.Put(ctx, SessionKeyUserID, user.ID.String())
	s.sessions.Put(ctx, SessionKeySessionID, sessionID)

	now := time.Now().UTC()
	if identity.RefreshToken != "" {
		enc, err := s.tokenCipher.Encrypt(identity.RefreshToken, user.ID)
		if err != nil {
			http.Error(w, "session token encryption failed", http.StatusInternalServerError)
			return
		}
		refreshDeadline := identity.RefreshTokenExpiry
		if refreshDeadline.IsZero() || !refreshDeadline.After(now) {
			refreshDeadline = now.Add(s.sessions.Lifetime)
		}
		idExpiry := identity.IDTokenExpiry
		if idExpiry.IsZero() {
			idExpiry = now.Add(defaultIDTokenRefreshWindow)
		}
		s.recordAndApplySessionCreds(ctx, sessionID, user.ID, enc, idExpiry.Unix(), refreshDeadline.Unix(), refreshDeadline)
	} else {
		s.clearSessionCreds(ctx, sessionID)
		if !identity.IDTokenExpiry.IsZero() && identity.IDTokenExpiry.After(now.Add(s.sessions.Lifetime)) {
			s.sessions.SetDeadline(ctx, identity.IDTokenExpiry)
		}
	}

	target := sanitizeReturnTo(returnTo)
	if target == "" {
		target = "/threads"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Service) revokeExistingUserAccountsOnLogin(ctx context.Context, id *VerifiedIdentity, ipAddr, userAgent string) {
	if id == nil {
		return
	}
	var existing sqlc.User
	var err error
	if strings.TrimSpace(id.Subject) != "" {
		existing, err = s.store.GetUserByOIDCSub(ctx, id.Subject)
	}
	if (err != nil || existing.ID == uuid.Nil) && strings.TrimSpace(id.Email) != "" {
		existing, err = s.store.GetUserByEmail(ctx, id.Email)
	}
	if err != nil || len(existing.AllowedAccounts) == 0 {
		return
	}
	oldSnap := existing
	sub := derefOr(existing.OIDCSub, id.Subject)
	displayName := existing.DisplayName
	if strings.TrimSpace(id.Name) != "" {
		displayName = strings.TrimSpace(id.Name)
	}
	updated, err := s.store.UpdateUserOIDC(ctx, existing.ID, sub, displayName, []string{})
	if err != nil {
		return
	}
	_ = s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
		ActorUserID:  &updated.ID,
		TargetUserID: &updated.ID,
		EventType:    "user_updated",
		IPAddress:    ipAddr,
		UserAgent:    userAgent,
		OldValue:     oldSnap,
		NewValue:     updated,
	})
}

func tokenSingleflightKey(userID uuid.UUID, refreshToken string) string {
	sum := sha256.Sum256([]byte(userID.String() + ":" + refreshToken))
	return hex.EncodeToString(sum[:])
}

// HandleLogout destroys the current session and logs a user_logout audit event.
func (s *Service) HandleLogout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if user, ok := UserFromContext(ctx); ok {
		_ = s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
			ActorUserID:  &user.ID,
			TargetUserID: &user.ID,
			EventType:    "user_logout",
			IPAddress:    r.RemoteAddr,
			UserAgent:    r.UserAgent(),
		})
	}
	_ = s.sessions.Destroy(ctx)
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
}

func randomToken(numBytes int) (string, error) {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
