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
	"golang.org/x/oauth2"
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

type refreshStatus int

const (
	refreshOK refreshStatus = iota
	refreshPermanentFail
	refreshTransientFail
)

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
		if err != nil || !user.IsActive || len(user.AllowedAccounts) == 0 {
			_ = s.sessions.Destroy(ctx)
			s.handleUnauthenticated(ctx, w, r)
			return
		}

		sessionKey := s.sessionTrackingKey(ctx, user.ID)
		s.syncContextFromLatestCreds(ctx, sessionKey, user.ID)

		user, status := s.refreshSessionIfNeeded(ctx, r, sessionKey, user)
		switch status {
		case refreshPermanentFail:
			s.deleteSessionCreds(sessionKey)
			_ = s.sessions.Destroy(ctx)
			s.handleUnauthenticated(ctx, w, r)
			return
		case refreshTransientFail:
			w.Header().Set("Retry-After", "5")
			http.Error(w, "authentication service temporarily unavailable", http.StatusServiceUnavailable)
			return
		}

		csw := &credSyncResponseWriter{
			ResponseWriter: w,
			syncFn: func() {
				s.syncContextFromLatestCreds(ctx, sessionKey, user.ID)
			},
		}
		ctx = WithUser(ctx, user)
		next.ServeHTTP(csw, r.WithContext(ctx))
		csw.ensureSynced()
	})
}

type credSyncResponseWriter struct {
	http.ResponseWriter
	syncFn func()
	synced bool
}

func (w *credSyncResponseWriter) ensureSynced() {
	if !w.synced {
		w.synced = true
		if w.syncFn != nil {
			w.syncFn()
		}
	}
}

func (w *credSyncResponseWriter) WriteHeader(code int) {
	w.ensureSynced()
	w.ResponseWriter.WriteHeader(code)
}

func (w *credSyncResponseWriter) Write(b []byte) (int, error) {
	w.ensureSynced()
	return w.ResponseWriter.Write(b)
}

func (w *credSyncResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// sessionCredsCodec wraps an scs.Codec so concurrent session commits can never overwrite
// a newer rotated refresh token generation with an older request's session snapshot.
type sessionCredsCodec struct {
	base scs.Codec
	svc  *Service
}

func (c *sessionCredsCodec) Encode(deadline time.Time, values map[string]any) ([]byte, error) {
	if c.svc != nil && values != nil {
		if sid, ok := values[SessionKeySessionID].(string); ok && sid != "" {
			if latest, found := c.svc.lookupSessionCreds(sid); found {
				valGen, _ := values[SessionKeyRefreshGen].(int64)
				if latest.gen > valGen {
					values[SessionKeyRefreshGen] = latest.gen
					values[SessionKeyRefreshToken] = append([]byte(nil), latest.encRefreshToken...)
					values[SessionKeyIDTokenExpiry] = latest.idTokenExpiry
					values[SessionKeyRefreshExpiry] = latest.refreshExpiry
					if latest.refreshDeadline.After(deadline) {
						deadline = latest.refreshDeadline
					}
				}
			}
		}
	}
	return c.base.Encode(deadline, values)
}

func (c *sessionCredsCodec) Decode(b []byte) (time.Time, map[string]any, error) {
	deadline, values, err := c.base.Decode(b)
	if err != nil || c.svc == nil || values == nil {
		return deadline, values, err
	}
	if sid, ok := values[SessionKeySessionID].(string); ok && sid != "" {
		valGen, _ := values[SessionKeyRefreshGen].(int64)
		if latest, found := c.svc.lookupSessionCreds(sid); found && latest.gen > valGen {
			values[SessionKeyRefreshGen] = latest.gen
			values[SessionKeyRefreshToken] = append([]byte(nil), latest.encRefreshToken...)
			values[SessionKeyIDTokenExpiry] = latest.idTokenExpiry
			values[SessionKeyRefreshExpiry] = latest.refreshExpiry
			if latest.refreshDeadline.After(deadline) {
				deadline = latest.refreshDeadline
			}
		}
	}
	return deadline, values, nil
}

func (s *Service) sessionTrackingKey(ctx context.Context, userID uuid.UUID) string {
	if sid := s.sessions.GetString(ctx, SessionKeySessionID); sid != "" {
		return sid
	}
	if tok := s.sessions.Token(ctx); tok != "" {
		return tok
	}
	return userID.String()
}

func (s *Service) lookupSessionCreds(sessionKey string) (versionedSessionCreds, bool) {
	if sessionKey == "" {
		return versionedSessionCreds{}, false
	}
	s.sessionCredsMu.Lock()
	defer s.sessionCredsMu.Unlock()
	v, ok := s.sessionCreds[sessionKey]
	return v, ok
}

func (s *Service) recordAndApplySessionCreds(
	ctx context.Context,
	sessionKey string,
	userID uuid.UUID,
	encRefresh []byte,
	idExpUnix int64,
	refreshExpUnix int64,
	refreshDeadline time.Time,
) {
	ctxGen := s.sessions.GetInt64(ctx, SessionKeyRefreshGen)
	s.sessionCredsMu.Lock()
	nextGen := ctxGen + 1
	if existing, ok := s.sessionCreds[sessionKey]; ok && existing.gen >= nextGen {
		nextGen = existing.gen + 1
	}
	creds := versionedSessionCreds{
		userID:          userID,
		gen:             nextGen,
		encRefreshToken: append([]byte(nil), encRefresh...),
		idTokenExpiry:   idExpUnix,
		refreshExpiry:   refreshExpUnix,
		refreshDeadline: refreshDeadline,
		updatedAt:       time.Now().UTC(),
	}
	if sessionKey != "" {
		s.sessionCreds[sessionKey] = creds
	}
	s.sessionCredsMu.Unlock()

	s.applyCredsToContext(ctx, creds)
}

func (s *Service) syncContextFromLatestCreds(ctx context.Context, sessionKey string, userID uuid.UUID) {
	latest, ok := s.lookupSessionCreds(sessionKey)
	if !ok || latest.userID != userID {
		return
	}
	ctxGen := s.sessions.GetInt64(ctx, SessionKeyRefreshGen)
	if latest.gen <= ctxGen {
		return
	}
	s.applyCredsToContext(ctx, latest)
}

func (s *Service) applyCredsToContext(ctx context.Context, creds versionedSessionCreds) {
	s.sessions.Put(ctx, SessionKeyRefreshGen, creds.gen)
	s.sessions.Put(ctx, SessionKeyRefreshToken, append([]byte(nil), creds.encRefreshToken...))
	s.sessions.Put(ctx, SessionKeyIDTokenExpiry, creds.idTokenExpiry)
	s.sessions.Put(ctx, SessionKeyRefreshExpiry, creds.refreshExpiry)
	if !creds.refreshDeadline.IsZero() {
		s.sessions.SetDeadline(ctx, creds.refreshDeadline)
	}
}

func (s *Service) clearSessionCreds(ctx context.Context, sessionKey string) {
	s.deleteSessionCreds(sessionKey)
	s.sessions.Remove(ctx, SessionKeyRefreshGen)
	s.sessions.Remove(ctx, SessionKeyRefreshToken)
	s.sessions.Remove(ctx, SessionKeyIDTokenExpiry)
	s.sessions.Remove(ctx, SessionKeyRefreshExpiry)
}

func (s *Service) deleteSessionCreds(sessionKey string) {
	if sessionKey == "" {
		return
	}
	s.sessionCredsMu.Lock()
	delete(s.sessionCreds, sessionKey)
	s.sessionCredsMu.Unlock()
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

func isPermanentAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var perm *PermanentAuthError
	if errors.As(err, &perm) {
		return true
	}
	if errors.Is(err, ErrNoAccountAccess) || errors.Is(err, ErrUserInactive) || errors.Is(err, ErrOIDCSubjectMismatch) {
		return true
	}
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) && retrieveErr.Response != nil {
		code := retrieveErr.Response.StatusCode
		if code == http.StatusBadRequest || code == http.StatusUnauthorized {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "invalid_token") ||
		strings.Contains(msg, "unauthorized_client")
}

func (s *Service) refreshSessionIfNeeded(ctx context.Context, r *http.Request, sessionKey string, user sqlc.User) (sqlc.User, refreshStatus) {
	encToken := s.sessions.GetBytes(ctx, SessionKeyRefreshToken)
	if len(encToken) == 0 {
		return user, refreshOK
	}
	idExpUnix := s.sessions.GetInt64(ctx, SessionKeyIDTokenExpiry)
	now := time.Now().UTC()
	if idExpUnix > 0 && now.Unix() < idExpUnix {
		return user, refreshOK
	}

	refresher, ok := s.verifier.(TokenRefresher)
	if !ok || refresher == nil {
		return user, refreshOK
	}

	refreshToken, err := s.tokenCipher.Decrypt(encToken, user.ID)
	if err != nil {
		return sqlc.User{}, refreshPermanentFail
	}

	prevRefreshDeadline := time.Unix(s.sessions.GetInt64(ctx, SessionKeyRefreshExpiry), 0).UTC()
	outcome, err := s.executeSingleflightRefresh(ctx, refresher, user, refreshToken, prevRefreshDeadline, r.RemoteAddr, r.UserAgent())
	if err != nil {
		if isPermanentAuthFailure(err) {
			return sqlc.User{}, refreshPermanentFail
		}
		// If the provider rotated the refresh token before a transient local DB/audit sync
		// failure occurred, persist the rotated refresh token now (with idTokenExpiry = now)
		// so the next request retries synchronization using the rotated token.
		if outcome.result != nil && outcome.result.RefreshToken != "" {
			if newEnc, encErr := s.tokenCipher.Encrypt(outcome.result.RefreshToken, user.ID); encErr == nil {
				s.recordAndApplySessionCreds(ctx, sessionKey, user.ID, newEnc, now.Unix(), outcome.refreshDeadline.Unix(), outcome.refreshDeadline)
			}
		}
		return sqlc.User{}, refreshTransientFail
	}
	if outcome.updatedUser != nil {
		user = *outcome.updatedUser
	}

	newEnc, err := s.tokenCipher.Encrypt(outcome.result.RefreshToken, user.ID)
	if err != nil {
		return sqlc.User{}, refreshTransientFail
	}
	s.recordAndApplySessionCreds(ctx, sessionKey, user.ID, newEnc, outcome.idTokenExpiry.Unix(), outcome.refreshDeadline.Unix(), outcome.refreshDeadline)
	return user, refreshOK
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

	type sfPair struct {
		outcome cachedRefreshOutcome
		err     error
	}

	val, _, _ := s.refreshGroup.Do(sfKey, func() (any, error) {
		if cached, ok := s.lookupRecentRefresh(sfKey); ok {
			return sfPair{outcome: cached}, nil
		}

		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshCallTimeout)
		defer cancel()

		var (
			res             *RefreshResult
			refreshDeadline time.Time
			fromPendingSync bool
		)
		if pending, ok := s.lookupPendingSync(sfKey); ok {
			res = pending.result
			refreshDeadline = pending.refreshDeadline
			fromPendingSync = true
		} else {
			var err error
			res, err = refresher.RefreshAndVerify(refreshCtx, refreshToken)
			if err != nil {
				return sfPair{err: err}, nil
			}
			if res == nil {
				return sfPair{err: errors.New("nil refresh result")}, nil
			}
		}

		now := time.Now().UTC()
		if res.RefreshToken == "" {
			res.RefreshToken = refreshToken
		}
		if refreshDeadline.IsZero() || !refreshDeadline.After(now) {
			refreshDeadline = res.RefreshTokenExpiry
			if refreshDeadline.IsZero() || !refreshDeadline.After(now) {
				if prevRefreshDeadline.After(now) {
					refreshDeadline = prevRefreshDeadline
				} else {
					refreshDeadline = now.Add(s.sessions.Lifetime)
				}
			}
		}

		var syncedUser *sqlc.User
		if res.Identity != nil {
			u, syncErr := s.syncRefreshedIdentity(refreshCtx, user, res.Identity, ipAddr, userAgent)
			if syncErr != nil {
				partial := cachedRefreshOutcome{
					result:          res,
					idTokenExpiry:   now,
					refreshDeadline: refreshDeadline,
					cachedAt:        now,
				}
				if !isPermanentAuthFailure(syncErr) {
					rotatedKey := tokenSingleflightKey(user.ID, res.RefreshToken)
					s.storePendingSync(rotatedKey, pendingSyncState{
						result:          res,
						refreshDeadline: refreshDeadline,
						cachedAt:        now,
					})
				} else if fromPendingSync {
					s.deletePendingSync(sfKey)
				}
				return sfPair{outcome: partial, err: syncErr}, nil
			}
			syncedUser = &u
		}
		if fromPendingSync {
			s.deletePendingSync(sfKey)
		}

		idExp := res.IDTokenExpiry
		if idExp.IsZero() || !idExp.After(now) {
			idExp = now.Add(defaultIDTokenRefreshWindow)
		}

		outcome := cachedRefreshOutcome{
			result:          res,
			updatedUser:     syncedUser,
			idTokenExpiry:   idExp,
			refreshDeadline: refreshDeadline,
			cachedAt:        now,
		}
		s.storeRecentRefresh(sfKey, outcome)
		return sfPair{outcome: outcome}, nil
	})
	pair := val.(sfPair)
	return pair.outcome, pair.err
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

func (s *Service) lookupPendingSync(key string) (pendingSyncState, bool) {
	s.recentRefreshMu.Lock()
	defer s.recentRefreshMu.Unlock()

	now := time.Now().UTC()
	for k, v := range s.pendingSyncs {
		if !v.refreshDeadline.IsZero() && now.After(v.refreshDeadline) {
			delete(s.pendingSyncs, k)
		}
	}
	v, ok := s.pendingSyncs[key]
	return v, ok
}

func (s *Service) storePendingSync(key string, state pendingSyncState) {
	s.recentRefreshMu.Lock()
	defer s.recentRefreshMu.Unlock()
	s.pendingSyncs[key] = state
}

func (s *Service) deletePendingSync(key string) {
	s.recentRefreshMu.Lock()
	defer s.recentRefreshMu.Unlock()
	delete(s.pendingSyncs, key)
}

func (s *Service) syncRefreshedIdentity(ctx context.Context, user sqlc.User, id *VerifiedIdentity, ipAddr, userAgent string) (sqlc.User, error) {
	sub := strings.TrimSpace(id.Subject)
	if sub == "" || user.OIDCSub == nil || strings.TrimSpace(*user.OIDCSub) == "" || *user.OIDCSub != sub {
		return sqlc.User{}, NewPermanentAuthError(ErrOIDCSubjectMismatch)
	}

	displayName := user.DisplayName
	if strings.TrimSpace(id.Name) != "" {
		displayName = strings.TrimSpace(id.Name)
	}

	allowedAccounts := user.AllowedAccounts
	if id.GroupsPresent || len(id.Groups) > 0 {
		allowedAccounts = s.ResolveAccounts(id.Groups)
	}

	if len(allowedAccounts) == 0 {
		if len(user.AllowedAccounts) > 0 || user.DisplayName != displayName {
			oldSnap := user
			updated, err := s.store.UpdateUserOIDC(ctx, user.ID, sub, displayName, []string{})
			if err != nil {
				return sqlc.User{}, fmt.Errorf("persist revoked user account access: %w", err)
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
		}
		return sqlc.User{}, NewPermanentAuthError(ErrNoAccountAccess)
	}

	if slices.Equal(user.AllowedAccounts, allowedAccounts) && user.DisplayName == displayName {
		return user, nil
	}
	oldSnap := user
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
