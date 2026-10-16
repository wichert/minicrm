package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

type authCtxKey string

const currentUserKey authCtxKey = "minicrm_current_user"

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

// NewSessionManager creates an SCS SessionManager with secure cookie settings.
func NewSessionManager(secureCookie bool) *scs.SessionManager {
	sm := scs.New()
	sm.Lifetime = 12 * time.Hour
	sm.IdleTimeout = 2 * time.Hour
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

// RequireAuth returns HTTP middleware that verifies an active session and loads the user.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := sqlc.WithRequestMeta(r.Context(), uuid.NewString(), r.RemoteAddr)
		rawID := s.sessions.GetString(ctx, SessionKeyUserID)
		if rawID == "" {
			http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
			return
		}
		userID, err := uuid.Parse(rawID)
		if err != nil {
			_ = s.sessions.Destroy(ctx)
			http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
			return
		}
		user, err := s.store.GetUserByID(ctx, userID)
		if err != nil || !user.IsActive {
			_ = s.sessions.Destroy(ctx)
			http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
			return
		}
		ctx = WithUser(ctx, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
