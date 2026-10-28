package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/web"
)

// newHealthTestRouter builds a router whose server reports the supplied probe result.
func newHealthTestRouter(t *testing.T, probe web.HealthCheck) http.Handler {
	t.Helper()
	srv, store, _, _, _ := buildAccountTestServer(t)
	if probe != nil {
		srv.WithHealthCheck(probe)
	}
	sm := auth.NewSessionManager(false)
	authSvc := auth.NewService(store, audit.NewLogger(store, nil), sm, nil, "admin@example.com")
	return web.NewRouter(srv, authSvc, sm)
}

// /healthz must be reachable without authentication so orchestrators can probe it.
func TestHealthzIsPublic(t *testing.T) {
	router := newHealthTestRouter(t, func(context.Context) error { return nil })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("unexpected Content-Type: %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("unexpected Cache-Control: %q, want no-store", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"ok"`) || !strings.Contains(body, `"database":"ok"`) {
		t.Fatalf("unexpected health body: %s", body)
	}
}

// A failing dependency must produce 503 so load balancers stop routing traffic.
func TestHealthzReportsUnavailableDependency(t *testing.T) {
	router := newHealthTestRouter(t, func(context.Context) error { return errors.New("connection refused") })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"unavailable"`) || !strings.Contains(body, "connection refused") {
		t.Fatalf("unexpected health body: %s", body)
	}
}

// Without a registered probe the endpoint still answers, reporting a healthy process.
func TestHealthzWithoutProbe(t *testing.T) {
	router := newHealthTestRouter(t, nil)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("unexpected health body: %s", body)
	}
}
