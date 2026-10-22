package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

type authCtxKey string

const (
	currentUserKey         authCtxKey = "minicrm_current_user"
	defaultSessionLifetime            = 7 * 24 * time.Hour
	refreshCallTimeout                = 10 * time.Second
	expiredEmailFrameCSP              = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; " +
		"frame-ancestors 'self'; sandbox allow-same-origin allow-popups allow-popups-to-escape-sandbox; " +
		"form-action 'none'; base-uri 'none'; object-src 'none'"
	expiredEmailFrameBody = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Session expired</title>
  <style>
    html, body { margin: 0; padding: 0; background: #ffffff; color: #475569; font-family: system-ui, -apple-system, sans-serif; font-size: 14px; }
    body { padding: 0.75rem 1rem; }
  </style>
</head>
<body data-session-expired="true">
  <p>Your session has expired.</p>
</body>
</html>`
)

// WithUser attaches an authenticated User to ctx and sets the sqlc actor ID.
func WithUser(ctx context.Context, u sqlc.User) context.Context {
	ctx = context.WithValue(ctx, currentUserKey, u)
	ctx = sqlc.WithActor(ctx, u.ID)
	return ctx
}

// UserFromContext retrieves the authenticated User from ctx.
func UserFromContext(ctx context.Context) (sqlc.User, bool) {
	u, ok := ctx.Value(currentUserKey).(sqlc.User)
	return u, ok
}

// NewSessionManager creates an SCS SessionManager with secure cookie settings and default PWA-friendly lifetime.
func NewSessionManager(secureCookie bool) *scs.SessionManager {
	return NewSessionManagerWithTimeouts(secureCookie, defaultSessionLifetime, 0)
}

// NewSessionManagerWithTimeouts creates an SCS SessionManager with explicit lifetime and idle timeout bounds.
func NewSessionManagerWithTimeouts(secureCookie bool, lifetime, idleTimeout time.Duration) *scs.SessionManager {
	if lifetime <= 0 {
		lifetime = defaultSessionLifetime
	}
	if idleTimeout < 0 {
		idleTimeout = 0
	}
	sm := scs.New()
	sm.Lifetime = lifetime
	sm.IdleTimeout = idleTimeout
	sm.HashTokenInStore = true
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Secure = secureCookie
	if secureCookie {
		sm.Cookie.Name = "__Host-minicrm_session"
	} else {
		sm.Cookie.Name = "minicrm_session"
	}
	return sm
}

// RequireAuth returns HTTP middleware that verifies an active session, transparently refreshes
// expired OIDC tokens when a refresh_token is stored, and loads the authenticated user.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := sqlc.WithRequestMeta(r.Context(), uuid.NewString(), r.RemoteAddr)
		rawID := s.sessions.GetString(ctx, SessionKeyUserID)
		if rawID == "" {
			s.handleUnauthenticated(ctx, w, r)
			return
		}
		userID, err := uuid.Parse(rawID)
		if err != nil {
			_ = s.sessions.Destroy(ctx)
			s.handleUnauthenticated(ctx, w, r)
			return
		}
		user, err := s.store.GetUserByID(ctx, userID)
		if err != nil || !user.IsActive {
			_ = s.sessions.Destroy(ctx)
			s.handleUnauthenticated(ctx, w, r)
			return
		}

		user, ok := s.refreshSessionIfNeeded(ctx, r, user)
		if !ok {
			_ = s.sessions.Destroy(ctx)
			s.handleUnauthenticated(ctx, w, r)
			return
		}

		ctx = WithUser(ctx, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Service) handleUnauthenticated(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if isEmailFrameRequest(r) {
		h := w.Header()
		h.Set("Content-Security-Policy", expiredEmailFrameCSP)
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "private, no-store, max-age=0")
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(expiredEmailFrameBody))
		return
	}

	if r.Method == http.MethodGet && shouldSaveReturnTo(r.URL.Path) {
		if safe := sanitizeReturnTo(r.URL.RequestURI()); safe != "" {
			s.sessions.Put(ctx, SessionKeyReturnTo, safe)
		}
	}
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
}

func isEmailFrameRequest(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Dest"), "iframe") {
		return true
	}
	return strings.HasPrefix(r.URL.Path, "/emails/") && strings.HasSuffix(r.URL.Path, "/body")
}

func shouldSaveReturnTo(path string) bool {
	if path == "" || path == "/" {
		return false
	}
	if strings.HasPrefix(path, "/auth/") || strings.HasPrefix(path, "/emails/") || strings.HasPrefix(path, "/attachments/") || strings.HasPrefix(path, "/static/") {
		return false
	}
	return true
}

func sanitizeReturnTo(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return ""
	}
	if strings.ContainsAny(raw, "\r\n") {
		return ""
	}
	if strings.HasPrefix(raw, "/auth/") {
		return ""
	}
	return raw
}

func (s *Service) refreshSessionIfNeeded(ctx context.Context, r *http.Request, user sqlc.User) (sqlc.User, bool) {
	encToken := s.sessions.GetBytes(ctx, SessionKeyRefreshToken)
	if len(encToken) == 0 {
		return user, true
	}
	idExpUnix := s.sessions.GetInt64(ctx, SessionKeyIDTokenExpiry)
	now := time.Now().UTC()
	if idExpUnix > 0 && now.Unix() < idExpUnix {
		return user, true
	}

	refresher, ok := s.verifier.(TokenRefresher)
	if !ok || refresher == nil {
		return user, true
	}

	refreshToken, err := s.tokenCipher.Decrypt(encToken, user.ID)
	if err != nil {
		return sqlc.User{}, false
	}

	prevRefreshDeadline := time.Unix(s.sessions.GetInt64(ctx, SessionKeyRefreshExpiry), 0).UTC()
	outcome, err := s.executeSingleflightRefresh(ctx, refresher, user, refreshToken, prevRefreshDeadline, r.RemoteAddr, r.UserAgent())
	if err != nil {
		return sqlc.User{}, false
	}
	if outcome.updatedUser != nil {
		user = *outcome.updatedUser
	}

	newEnc, err := s.tokenCipher.Encrypt(outcome.result.RefreshToken, user.ID)
	if err != nil {
		return sqlc.User{}, false
	}
	s.sessions.Put(ctx, SessionKeyRefreshToken, newEnc)
	s.sessions.Put(ctx, SessionKeyIDTokenExpiry, outcome.idTokenExpiry.Unix())
	s.sessions.Put(ctx, SessionKeyRefreshExpiry, outcome.refreshDeadline.Unix())
	s.sessions.SetDeadline(ctx, outcome.refreshDeadline)
	return user, true
}

func (s *Service) executeSingleflightRefresh(
	ctx context.Context,
	refresher TokenRefresher,
	user sqlc.User,
	refreshToken string,
	prevRefreshDeadline time.Time,
	ipAddr string,
	userAgent string,
) (cachedRefreshOutcome, error) {
	sfKey := tokenSingleflightKey(user.ID, refreshToken)

	if cached, ok := s.lookupRecentRefresh(sfKey); ok {
		return cached, nil
	}

	val, err, _ := s.refreshGroup.Do(sfKey, func() (any, error) {
		if cached, ok := s.lookupRecentRefresh(sfKey); ok {
			return cached, nil
		}

		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshCallTimeout)
		defer cancel()

		res, err := refresher.RefreshAndVerify(refreshCtx, refreshToken)
		if err != nil {
			return cachedRefreshOutcome{}, err
		}
		if res == nil {
			return cachedRefreshOutcome{}, errors.New("nil refresh result")
		}
		var syncedUser *sqlc.User
		if res.Identity != nil {
			u, err := s.syncRefreshedIdentity(refreshCtx, user, res.Identity, ipAddr, userAgent)
			if err != nil {
				return cachedRefreshOutcome{}, err
			}
			syncedUser = &u
		}
		now := time.Now().UTC()
		if res.RefreshToken == "" {
			res.RefreshToken = refreshToken
		}
		idExp := res.IDTokenExpiry
		if idExp.IsZero() || !idExp.After(now) {
			idExp = now.Add(defaultIDTokenRefreshWindow)
		}
		refreshDeadline := res.RefreshTokenExpiry
		if refreshDeadline.IsZero() || !refreshDeadline.After(now) {
			if prevRefreshDeadline.After(now) {
				refreshDeadline = prevRefreshDeadline
			} else {
				refreshDeadline = now.Add(s.sessions.Lifetime)
			}
		}

		outcome := cachedRefreshOutcome{
			result:          res,
			updatedUser:     syncedUser,
			idTokenExpiry:   idExp,
			refreshDeadline: refreshDeadline,
			cachedAt:        now,
		}
		s.storeRecentRefresh(sfKey, outcome)
		return outcome, nil
	})
	if err != nil {
		return cachedRefreshOutcome{}, err
	}
	return val.(cachedRefreshOutcome), nil
}

func (s *Service) lookupRecentRefresh(key string) (cachedRefreshOutcome, bool) {
	s.recentRefreshMu.Lock()
	defer s.recentRefreshMu.Unlock()

	now := time.Now().UTC()
	for k, v := range s.recentRefreshes {
		if now.Sub(v.cachedAt) > recentRefreshCacheTTL {
			delete(s.recentRefreshes, k)
		}
	}
	v, ok := s.recentRefreshes[key]
	return v, ok
}

func (s *Service) storeRecentRefresh(key string, outcome cachedRefreshOutcome) {
	s.recentRefreshMu.Lock()
	defer s.recentRefreshMu.Unlock()
	s.recentRefreshes[key] = outcome
}

func (s *Service) syncRefreshedIdentity(ctx context.Context, user sqlc.User, id *VerifiedIdentity, ipAddr, userAgent string) (sqlc.User, error) {
	allowedAccounts := s.ResolveAccounts(id.Groups)
	if len(allowedAccounts) == 0 {
		return sqlc.User{}, ErrNoAccountAccess
	}
	displayName := user.DisplayName
	if strings.TrimSpace(id.Name) != "" {
		displayName = strings.TrimSpace(id.Name)
	}
	if slices.Equal(user.AllowedAccounts, allowedAccounts) && user.DisplayName == displayName {
		return user, nil
	}
	oldSnap := user
	sub := derefOr(user.OIDCSub, id.Subject)
	updated, err := s.store.UpdateUserOIDC(ctx, user.ID, sub, displayName, allowedAccounts)
	if err != nil {
		return sqlc.User{}, fmt.Errorf("update user on token refresh: %w", err)
	}
	if err := s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
		ActorUserID:  &updated.ID,
		TargetUserID: &updated.ID,
		EventType:    "user_updated",
		IPAddress:    ipAddr,
		UserAgent:    userAgent,
		OldValue:     oldSnap,
		NewValue:     updated,
	}); err != nil {
		return sqlc.User{}, err
	}
	return updated, nil
}

// RequireAdmin returns HTTP middleware that restricts access to users with role == "admin".
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFromContext(r.Context())
		if !ok || user.Role != "admin" {
			http.Error(w, "admin role required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
