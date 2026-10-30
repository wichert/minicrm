package e2e_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/auth/devidm"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/web"
	"github.com/wichert/minicrm/internal/workflow"
)

// W4 master-detail: the split document and its client-side behaviour. A real
// browser resize test (Playwright setViewportSize across 1280px) is not possible
// in this suite — the repo's e2e layer is Go/HTTP only (docs/testing.md) — so the
// resize contract is verified at the document level: the layout switch must live
// solely in the single @media rule in app.css, and the inert module in app.js
// must listen to matchMedia change events so an in-place resize toggles the
// hidden pane without a reload.
func TestEndToEndSplitMasterDetailAssets(t *testing.T) {
	srv := startSelfContainedSplitE2EServer(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}

	resp, err := client.Get(srv + "/static/app.css")
	if err != nil {
		t.Fatalf("GET /static/app.css: %v", err)
	}
	cssBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("read app.css: status=%d err=%v", resp.StatusCode, err)
	}
	css := string(cssBytes)
	if !strings.Contains(css, "@media (min-width: 1280px)") {
		t.Error("app.css must contain the 1280px split media query")
	}
	// The brief forbids new breakpoints for the layout: the split layout must reuse
	// the existing 1280px query that W3 already uses for one-line rows. The list
	// column is sized from the remaining columns (subject/snippet, meta line and a
	// slim actions cell) after the status/age/select columns were dropped from the
	// pane table.
	if !strings.Contains(css, "grid-template-columns: clamp(360px, 34vw, 520px) minmax(0, 1fr)") {
		t.Error("app.css must lay out the split panes with the shared 1280px media query")
	}

	jsResp, err := client.Get(srv + "/static/app.js")
	if err != nil {
		t.Fatalf("GET /static/app.js: %v", err)
	}
	jsBytes, err := io.ReadAll(jsResp.Body)
	jsResp.Body.Close()
	if err != nil || jsResp.StatusCode != http.StatusOK {
		t.Fatalf("read app.js: status=%d err=%v", jsResp.StatusCode, err)
	}
	js := string(jsBytes)
	if !strings.Contains(js, `matchMedia("(min-width: 1280px)")`) {
		t.Error("app.js must toggle inert on the split list via the 1280px matchMedia query")
	}
	// app.js is a minified esbuild bundle (frontend/src/app.js → internal/web/static/app.js),
	// so identifiers are mangled: assert on the stable DOM contract instead of a
	// function name. The inert module marks the hidden pane with data-inert-pane
	// and re-applies on the matchMedia "change" event.
	if !strings.Contains(js, `"change"`) || !strings.Contains(js, "data-inert-pane") {
		t.Error("app.js inert module must react to matchMedia change events (resize without reload)")
	}
	// The back link must stay available in single-pane mode: hiding it in split mode
	// is a CSS rule, and neither the template nor the script may set aria-hidden or
	// tabindex on it (that would strand keyboard and screen-reader users on mobile).
	if !strings.Contains(css, ".split-layout:not(.split-single) .back-link") {
		t.Error("app.css must hide the back link only in split mode via `.split-layout:not(.split-single) .back-link`")
	}
	if strings.Contains(js, `backLink.setAttribute("aria-hidden"`) || strings.Contains(js, `back.setAttribute("aria-hidden"`) {
		t.Error("app.js must not toggle aria-hidden on the back link; CSS owns its visibility")
	}
	// W4 defect: `.container-wide` zeroed the top margin, putting the inbox and the
	// split view flush against the navbar. The wide variant may change the width
	// only; vertical rhythm comes from `.container`'s 1.75rem auto margin.
	if idx := strings.Index(css, ".container-wide {"); idx >= 0 {
		rule := css[idx:]
		if end := strings.Index(rule, "}"); end >= 0 {
			rule = rule[:end]
		}
		if strings.Contains(rule, "margin-top: 0") {
			t.Error(".container-wide must not zero the top margin again")
		}
		if !strings.Contains(rule, "max-width: min(1400px, 100%)") {
			t.Error(".container-wide must keep its width cap")
		}
	} else {
		t.Error("app.css is missing .container-wide")
	}
}

// startSelfContainedSplitE2EServer boots the full MiniCRM router (same shape as
// startSelfContainedE2EServers) and returns its base URL.
func startSelfContainedSplitE2EServer(t *testing.T) string {
	t.Helper()

	idm, err := devidm.NewServer(devidm.Config{
		ClientID:     "minicrm-e2e",
		ClientSecret: "minicrm-e2e-secret",
		Subject:      "dev-admin-sub",
		Email:        "admin@example.com",
		Name:         "Dev Admin",
		Groups:       []string{"crm-support", "crm-sales"},
	})
	if err != nil {
		t.Fatalf("devidm.NewServer: %v", err)
	}
	idmSrv := httptest.NewServer(idm.Handler())
	t.Cleanup(idmSrv.Close)
	idm.SetIssuerURL(idmSrv.URL)

	store := newE2EMemStore()
	auditLogger := audit.NewLogger(store, nil)
	attStore, err := storage.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.NewStore: %v", err)
	}
	sessions := auth.NewSessionManager(false)

	var handler http.Handler
	crmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(crmSrv.Close)

	ctx := context.Background()
	oidcProvider, err := auth.NewOIDCProvider(
		ctx,
		idmSrv.URL,
		"minicrm-e2e",
		"minicrm-e2e-secret",
		crmSrv.URL+"/auth/callback",
		"groups",
		[]string{"openid", "profile", "email", "groups"},
	)
	if err != nil {
		t.Fatalf("auth.NewOIDCProvider: %v", err)
	}

	authSvc := auth.NewService(store, auditLogger, sessions, oidcProvider, "admin@example.com",
		auth.AccountAccessRule{AccountID: "support", AccountName: "Support", OIDCGroup: "crm-support"},
		auth.AccountAccessRule{AccountID: "sales", AccountName: "Sales", OIDCGroup: "crm-sales"},
	)
	if _, _, err := authSvc.SeedBootstrapAdminAndSettings(ctx, "admin@example.com", "Dev Admin", "UTC"); err != nil {
		t.Fatalf("SeedBootstrapAdminAndSettings: %v", err)
	}
	wfSvc := workflow.NewService(store, auditLogger, nil)
	webSrv := web.NewServer(store, authSvc, wfSvc, nil, attStore, auditLogger, sessions, []web.AccountConfig{
		{ID: "support", Name: "Support", FromAddress: "support@example.com"},
		{ID: "sales", Name: "Sales", FromAddress: "sales@example.com"},
	})
	handler = web.NewRouter(webSrv, authSvc, sessions)

	return crmSrv.URL
}
