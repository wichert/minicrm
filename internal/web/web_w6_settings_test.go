package web_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/mail"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/web"
	"github.com/wichert/minicrm/internal/web/views"
	"github.com/wichert/minicrm/internal/workflow"
)

// buildW6SettingsServer builds a web.Server exposing the admin settings and
// full-activity handlers over a test store, with the admin authenticated.
func buildW6SettingsServer(t *testing.T) (*web.Server, *testWebStore, http.Handler) {
	t.Helper()
	store := newTestWebStore()
	auditLogger := audit.NewLogger(store, nil)
	attStore, err := storage.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	wfSvc := workflow.NewService(store, auditLogger, nil)
	outbox := mail.NewOutboxWorker(outboxStoreAdapter{store}, attStore, []mail.AccountSenderConfig{
		{AccountID: "support", FromAddress: "support@example.com", FromName: "Support", Transport: &noopSMTP{}},
	}, auditLogger, "example.com", nil, nil)
	sm := auth.NewSessionManager(false)
	authSvc := auth.NewService(store, auditLogger, sm, nil, "admin@example.com")
	srv := web.NewServer(store, authSvc, wfSvc, outbox, attStore, auditLogger, sm, testAccounts())

	router := web.SecurityHeadersMiddleware(sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithUser(r.Context(), store.adminUser))
		switch r.URL.Path {
		case "/settings":
			srv.HandleGetSettings(w, r)
		case "/settings/activity":
			srv.HandleGetActivity(w, r)
		default:
			http.NotFound(w, r)
		}
	})))
	return srv, store, router
}

// W6: settings page must render the sticky section nav with anchor links.
func TestRenderSettingsSectionNav(t *testing.T) {
	admin := sqlc.User{ID: uuid.New(), Email: "admin@example.com", DisplayName: "Admin", Role: "admin", IsActive: true}
	var buf bytes.Buffer
	err := views.RenderSettings(&buf, views.SettingsViewData{
		CurrentUser: admin,
		Settings:    sqlc.SystemSettings{ID: 1, DisplayTimezone: "UTC", IMAPInboxMailbox: "INBOX"},
	})
	if err != nil {
		t.Fatalf("RenderSettings: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		`class="settings-section-nav"`,
		`href="#settings-routing"`,
		`href="#settings-folders"`,
		`href="#settings-team"`,
		`href="#settings-activity"`,
		`id="settings-routing"`,
		`id="settings-folders"`,
		`id="settings-team"`,
		`id="settings-activity"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in settings page: %s", want, body)
		}
	}
}

// W6: team table uses data-label cells and account chips; audit table is shared.
func TestRenderSettingsCardMarkup(t *testing.T) {
	admin := sqlc.User{
		ID: uuid.New(), Email: "admin@example.com", DisplayName: "Admin",
		Role: "admin", IsActive: true, AllowedAccounts: []string{"support"},
	}
	var buf bytes.Buffer
	err := views.RenderSettings(&buf, views.SettingsViewData{
		CurrentUser:  admin,
		Settings:     sqlc.SystemSettings{ID: 1, DisplayTimezone: "UTC", IMAPInboxMailbox: "INBOX"},
		Users:        []sqlc.User{admin},
		Accounts:     []views.AccountOption{{ID: "support", Name: "Customer Support"}},
		AccountNames: map[string]string{"support": "Customer Support"},
		AuditLogs: []views.AuditEntryView{
			{FormattedTime: "2026-01-01 00:00:00 UTC", ActorName: "Admin", Label: "Settings updated", TargetName: "System settings"},
		},
	})
	if err != nil {
		t.Fatalf("RenderSettings: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		`class="settings-table settings-users-table"`,
		`class="settings-table settings-audit-table"`,
		`data-label="Account access"`,
		`class="account-chip"`,
		`View full activity`,
		`href="/settings/activity"`,
		`class="badge badge-role"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in settings page: %s", want, body)
		}
	}
	// Single source of truth: exactly one audit table instance in the page.
	if got := strings.Count(body, "settings-audit-table"); got != 1 {
		t.Fatalf("expected exactly one audit table instance, got %d", got)
	}
}

// W6: /settings/activity renders the shared audit table paginated at 50/page.
func TestHandleGetActivity(t *testing.T) {
	_, store, router := buildW6SettingsServer(t)
	store.auditLogs = nil
	for i := 0; i < 60; i++ {
		store.auditLogs = append(store.auditLogs, sqlc.AuditLog{
			ID:         uuid.New(),
			EventType:  "settings_updated",
			OccurredAt: time.Now().UTC().Add(time.Duration(i) * time.Minute),
		})
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/activity", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d", rec.Code)
	}
	body := rec.Body.String()
	if got := strings.Count(body, `data-label="Time"`); got != 50 {
		t.Fatalf("expected 50 entries on page 1, got %d", got)
	}
	if !strings.Contains(body, `href="/settings/activity?page=2"`) {
		t.Fatal("expected next-page link on page 1")
	}

	// Page 2 shows the remaining 10 entries and no further next link.
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/settings/activity?page=2", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("page 2 status %d", rec2.Code)
	}
	body2 := rec2.Body.String()
	if got := strings.Count(body2, `data-label="Time"`); got != 10 {
		t.Fatalf("expected 10 entries on page 2, got %d", got)
	}
	if strings.Contains(body2, `page=3`) {
		t.Fatal("page 2 must not offer a next page")
	}
	if !strings.Contains(body2, `page=1`) {
		t.Fatal("expected prev-page link on page 2")
	}

	// Invalid page falls back to page 1.
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/settings/activity?page=0", nil))
	if rec3.Code != http.StatusOK || !strings.Contains(rec3.Body.String(), `page=2`) {
		t.Fatalf("invalid page must fall back to page 1: %d", rec3.Code)
	}
}

// W6: the settings page renders at most 10 activity entries server-side.
func TestRenderSettingsBoundedActivity(t *testing.T) {
	_, store, router := buildW6SettingsServer(t)
	store.auditLogs = nil
	for i := 0; i < 30; i++ {
		store.auditLogs = append(store.auditLogs, sqlc.AuditLog{
			ID:         uuid.New(),
			EventType:  "settings_updated",
			OccurredAt: time.Now().UTC().Add(time.Duration(i) * time.Minute),
		})
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d", rec.Code)
	}
	if got := strings.Count(rec.Body.String(), `data-label="Time"`); got != 10 {
		t.Fatalf("expected exactly 10 activity entries on settings page, got %d", got)
	}
}
