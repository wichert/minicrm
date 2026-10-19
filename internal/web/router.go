package web

import (
	"context"
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/web/static"
)

// WithChiParam attaches a URL parameter to r's chi.Context.
func WithChiParam(r *http.Request, key, val string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, val)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// NewRouter constructs the chi HTTP router with security headers, CSRF protection, and session management.
func NewRouter(srv *Server, authSvc *auth.Service, sessions *scs.SessionManager) http.Handler {
	r := chi.NewRouter()
	r.Use(SecurityHeadersMiddleware)

	csrf := http.NewCrossOriginProtection()
	r.Use(csrf.Handler)
	r.Use(sessions.LoadAndSave)

	// Static embedded assets
	fileServer := http.FileServer(http.FS(static.FS))
	r.Handle("/static/*", http.StripPrefix("/static/", fileServer))
	// Browsers still probe /favicon.ico; point it at the embedded SVG icon.
	r.Get("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon.svg", http.StatusMovedPermanently)
	})

	// Public OAuth2 / OIDC routes
	r.Get("/auth/login", authSvc.HandleLogin)
	r.Get("/auth/callback", authSvc.HandleCallback)

	// Authenticated routes
	r.Group(func(authed chi.Router) {
		authed.Use(authSvc.RequireAuth)

		authed.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/threads", http.StatusSeeOther)
		})
		authed.Post("/auth/logout", authSvc.HandleLogout)

		authed.Get("/threads", srv.HandleListThreads)
		authed.Get("/threads/{id}", srv.HandleViewThread)
		authed.Post("/threads/{id}/transition", srv.HandleTransitionThread)
		authed.Post("/threads/{id}/assign", srv.HandleAssignThread)
		authed.Post("/threads/{id}/reopen-merge", srv.HandleReopenMerge)
		authed.Post("/threads/{id}/reply", srv.HandleReplyThread)

		authed.Get("/emails/{id}/body", srv.HandleIsolatedEmailBody)
		authed.Get("/attachments/{id}/download", srv.HandleDownloadAttachment)

		// Admin-only settings routes
		authed.Group(func(admin chi.Router) {
			admin.Use(srv.requireAdmin)
			admin.Get("/settings", srv.HandleGetSettings)
			admin.Post("/settings", srv.HandleUpdateSettings)
			admin.Post("/settings/users", srv.HandlePrecreateUser)
		})
	})

	return r
}
