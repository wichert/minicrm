// Package auth implements OAuth2/OIDC authentication, user provisioning, and session middleware.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/alexedwards/scs/v2"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"golang.org/x/oauth2"
)

// ErrUserInactive is returned when a deactivated user attempts to log in.
var ErrUserInactive = errors.New("user account is inactive")

// Session keys used by SCS.
const (
	SessionKeyUserID       = "auth_user_id"
	SessionKeyOIDCState    = "oidc_state"
	SessionKeyOIDCNonce    = "oidc_nonce"
	SessionKeyPKCEVerifier = "oidc_pkce_verifier"
)

// VerifiedIdentity represents the claims verified from the OIDC ID Token.
type VerifiedIdentity struct {
	Subject string
	Email   string
	Name    string
}

// IdentityVerifier exchanges an OAuth2 authorization code + PKCE verifier for a VerifiedIdentity.
type IdentityVerifier interface {
	AuthCodeURL(state, nonce, codeVerifier string) string
	ExchangeAndVerify(ctx context.Context, code, codeVerifier, expectedNonce string) (*VerifiedIdentity, error)
}

// OIDCProvider implements IdentityVerifier using github.com/coreos/go-oidc/v3 and golang.org/x/oauth2.
type OIDCProvider struct {
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
}

// NewOIDCProvider discovers the OIDC issuer and constructs an OIDCProvider.
func NewOIDCProvider(ctx context.Context, issuerURL, clientID, clientSecret, redirectURL string) (*OIDCProvider, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("discover oidc provider %q: %w", issuerURL, err)
	}
	oauthCfg := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: clientID})
	return &OIDCProvider{
		oauth2Config: oauthCfg,
		verifier:     verifier,
	}, nil
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
	var claims struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("parse oidc claims: %w", err)
	}
	if strings.TrimSpace(claims.Email) == "" {
		return nil, errors.New("oidc id_token missing email claim")
	}
	displayName := strings.TrimSpace(claims.Name)
	if displayName == "" {
		displayName = claims.Email
	}
	return &VerifiedIdentity{
		Subject: idToken.Subject,
		Email:   strings.TrimSpace(claims.Email),
		Name:    displayName,
	}, nil
}

// UserStore defines the persistence operations required for user authentication and provisioning.
type UserStore interface {
	GetUserByID(ctx context.Context, id uuid.UUID) (sqlc.User, error)
	GetUserByOIDCSub(ctx context.Context, sub string) (sqlc.User, error)
	GetUserByEmail(ctx context.Context, email string) (sqlc.User, error)
	CreateUser(ctx context.Context, arg sqlc.CreateUserParams) (sqlc.User, error)
	UpdateUserOIDC(ctx context.Context, id uuid.UUID, oidcSub, displayName string) (sqlc.User, error)
	GetSystemSettings(ctx context.Context) (sqlc.SystemSettings, error)
	UpsertSystemSettings(ctx context.Context, s sqlc.SystemSettings) (sqlc.SystemSettings, error)
}

// Service manages OIDC login, pre-created user resolution, bootstrap seeding, and audit logging.
type Service struct {
	store          UserStore
	audit          *audit.Logger
	sessions       *scs.SessionManager
	verifier       IdentityVerifier
	bootstrapEmail string
}

// NewService constructs an authentication Service.
func NewService(store UserStore, auditLogger *audit.Logger, sessions *scs.SessionManager, verifier IdentityVerifier, bootstrapEmail string) *Service {
	return &Service{
		store:          store,
		audit:          auditLogger,
		sessions:       sessions,
		verifier:       verifier,
		bootstrapEmail: strings.TrimSpace(bootstrapEmail),
	}
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
			DefaultAssigneeID:      u.ID,
			DisplayTimezone:        displayTimezone,
			IMAPInboxMailbox:       "INBOX",
			IMAPArchiveMailbox:     "Archive",
			IMAPJunkMailbox:        "Junk",
			IMAPSentMailbox:        "Sent",
			SubjectMatchWindowDays: 30,
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
	return u, settings, nil
}

// PrecreateUser allows an admin to pre-create an assignee user before their first OIDC login.
func (s *Service) PrecreateUser(ctx context.Context, actorID uuid.UUID, email, displayName, role string) (sqlc.User, error) {
	email = strings.TrimSpace(email)
	displayName = strings.TrimSpace(displayName)
	if email == "" || displayName == "" {
		return sqlc.User{}, errors.New("email and display_name are required")
	}
	if role != "agent" && role != "admin" {
		role = "agent"
	}
	u, err := s.store.CreateUser(ctx, sqlc.CreateUserParams{
		Email:       email,
		DisplayName: displayName,
		Role:        role,
		IsActive:    true,
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
func (s *Service) ResolveAndLoginUser(ctx context.Context, id *VerifiedIdentity, ipAddr, userAgent string) (sqlc.User, error) {
	// 1. Lookup by oidc_sub
	u, err := s.store.GetUserByOIDCSub(ctx, id.Subject)
	if errors.Is(err, pgx.ErrNoRows) {
		// 2. Lookup by email (matches pre-created users)
		existing, emailErr := s.store.GetUserByEmail(ctx, id.Email)
		switch {
		case emailErr == nil:
			oldSnap := existing
			u, err = s.store.UpdateUserOIDC(ctx, existing.ID, id.Subject, id.Name)
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
				OIDCSub:     &sub,
				Email:       id.Email,
				DisplayName: id.Name,
				Role:        role,
				IsActive:    true,
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
	} else if err != nil {
		return sqlc.User{}, fmt.Errorf("lookup user by oidc_sub: %w", err)
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
	if err != nil {
		http.Error(w, "user provisioning failed", http.StatusInternalServerError)
		return
	}

	if err := s.sessions.RenewToken(ctx); err != nil {
		http.Error(w, "session renewal failed", http.StatusInternalServerError)
		return
	}
	s.sessions.Put(ctx, SessionKeyUserID, user.ID.String())
	http.Redirect(w, r, "/threads", http.StatusSeeOther)
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
