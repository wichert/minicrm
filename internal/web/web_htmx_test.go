package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// The results region is the unit of the htmx swap: the poll and the filter
// selects both replace it (hx-swap="outerHTML"), so the fragment must return the
// element itself plus out-of-band updates for the toolbar pieces outside it.
func TestInboxFragmentReturnsRegionAndOOBToolbar(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Fragment thread", Status: sqlc.ThreadStatusNew,
		AssigneeID: &multiUser.ID, CustomerEmail: "frag@client.example", LastMessageAt: time.Now().UTC(),
	}

	req := httptest.NewRequest(http.MethodGet, "/threads?q=fragment", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fragment status = %d, want 200", rec.Code)
	}
	frag := rec.Body.String()

	for _, want := range []string{
		`data-inbox-results`,
		`id="inbox-results-region"`,
		`hx-get="/threads?q=fragment"`,
		`class="thread-link"`,
		`id="inbox-results-count" hx-swap-oob="true"`,
		`id="inbox-tabs" aria-label="Thread status" hx-swap-oob="true"`,
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment is missing %q: %s", want, frag)
		}
	}
	// A fragment must never re-render page chrome: htmx would splice it into the
	// results region.
	for _, unwanted := range []string{"<!DOCTYPE html>", "data-inbox-toolbar", "data-pull-indicator", `class="filter-sticky"`} {
		if strings.Contains(frag, unwanted) {
			t.Errorf("fragment must not contain %q: %s", unwanted, frag)
		}
	}
}

// Filter selects drive the same swap; without JS the form must still submit.
func TestInboxFilterSelectsAreProgressive(t *testing.T) {
	_, _, router, _, _ := buildAccountTestServer(t)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`hx-get="/threads"`,
		`hx-target="#inbox-results-region"`,
		`hx-swap="outerHTML"`,
		`hx-push-url="true"`,
		`hx-include="closest form"`,
		`data-hx-get="/threads"`,
		`<form method="get" action="/threads" class="filter-inline">`,
		`<noscript><button type="submit"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox filter markup is missing %q", want)
		}
	}
	// The poll contract lives on the region element itself.
	for _, want := range []string{`hx-trigger="every 60s, refresh from:body"`, `hx-push-url="false"`} {
		if !strings.Contains(body, want) {
			t.Errorf("results region is missing %q", want)
		}
	}
	// htmx must be loaded before app.js uses window.htmx.
	htmxIdx := strings.Index(body, `/static/htmx.min.js`)
	appIdx := strings.Index(body, `/static/app.js`)
	if htmxIdx < 0 || appIdx < 0 || htmxIdx > appIdx {
		t.Errorf("htmx.min.js must be loaded before app.js (htmx at %d, app at %d)", htmxIdx, appIdx)
	}
}

// The split pane has its own fragment endpoint so a poll never swaps the whole
// detail document into the list pane.
func TestSplitPaneFragmentEndpoint(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)
	openID := uuid.New()
	otherID := uuid.New()
	now := time.Now().UTC()
	store.threads[openID] = sqlc.Thread{
		ID: openID, AccountID: "support", SubjectDisplay: "Open thread", Status: sqlc.ThreadStatusNew,
		AssigneeID: &multiUser.ID, CustomerEmail: "open@client.example", LastMessageAt: now, OpenedAt: now,
	}
	store.threads[otherID] = sqlc.Thread{
		ID: otherID, AccountID: "support", SubjectDisplay: "Pane sibling", Status: sqlc.ThreadStatusNew,
		AssigneeID: &multiUser.ID, CustomerEmail: "sib@client.example", LastMessageAt: now.Add(-time.Hour), OpenedAt: now,
	}

	req := httptest.NewRequest(http.MethodGet, "/threads/"+openID.String()+"/list?status=new", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pane fragment status = %d, want 200", rec.Code)
	}
	frag := rec.Body.String()
	for _, want := range []string{
		`id="split-inbox-results-region"`,
		`hx-get="/threads/` + openID.String() + `?status=new"`,
		`id="split-inbox-results-count" hx-swap-oob="true"`,
		`id="split-inbox-tabs" aria-label="Thread status" hx-swap-oob="true"`,
		"Pane sibling",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("pane fragment is missing %q: %s", want, frag)
		}
	}
	// The pane keeps its no-bulk-bar rule and must not leak the whole page.
	if strings.Contains(frag, `id="bulk-spam-form"`) {
		t.Errorf("pane fragment must not render the bulk bar: %s", frag)
	}
	if strings.Contains(frag, "<!DOCTYPE html>") || strings.Contains(frag, `data-split-detail`) {
		t.Errorf("pane fragment must not render the detail page: %s", frag)
	}

	// A plain navigation to the fragment URL is redirected to the thread page, so
	// the endpoint never becomes a divergent way to read a list.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+openID.String()+"/list?status=new", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("non-htmx request status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/threads/"+openID.String()) {
		t.Fatalf("non-htmx request must redirect to the thread, got %q", loc)
	}

	// Unknown thread: 404, not an empty list.
	rec = httptest.NewRecorder()
	missing := httptest.NewRequest(http.MethodGet, "/threads/"+uuid.New().String()+"/list", nil)
	missing.Header.Set("HX-Request", "true")
	router.ServeHTTP(rec, missing)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown thread status = %d, want 404", rec.Code)
	}
}

// Access to the pane is the account-access check of the thread page itself: the
// pane lists the queue around the thread, so a user without access to the
// thread's account must not be able to read it (AGENTS.md invariant 5).
func TestSplitPaneFragmentEnforcesAccountAccess(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	otherAccountID := uuid.New()
	store.threads[otherAccountID] = sqlc.Thread{
		ID: otherAccountID, AccountID: "billing", SubjectDisplay: "Billing thread", Status: sqlc.ThreadStatusNew,
		CustomerEmail: "billing@client.example", LastMessageAt: time.Now().UTC(),
	}
	req := httptest.NewRequest(http.MethodGet, "/threads/"+otherAccountID.String()+"/list", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("inaccessible account status = %d, want 404", rec.Code)
	}
}

// The detail page's pane points its poll at the pane endpoint, not at the detail
// page (which would swap a whole document into the <aside>).
func TestSplitPanePollsItsOwnEndpoint(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	now := time.Now().UTC()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Poll source", Status: sqlc.ThreadStatusNew,
		AssigneeID: &multiUser.ID, CustomerEmail: "poll@client.example", LastMessageAt: now, OpenedAt: now,
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String()+"?status=new", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `hx-get="/threads/`+threadID.String()+`?status=new"`) {
		t.Errorf("pane region must poll the pane endpoint: %s", body)
	}
	if !strings.Contains(body, `hx-get="/threads/`+threadID.String()+`/list"`) {
		t.Errorf("pane filter selects must target the pane endpoint: %s", body)
	}
	if strings.Contains(body, `hx-push-url="true"`) {
		t.Error("pane must not push the fragment endpoint into the address bar")
	}
}

// A non-htmx request to the pane endpoint must land on the thread page, and must
// be built from the route uuid + allowlisted filter params so a request line
// such as `GET //other.example/...` cannot turn it into an open redirect.
func TestSplitPaneRedirectIsNotOpenRedirect(t *testing.T) {
	_, store, router, multiUser, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	now := time.Now().UTC()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Redirect target", Status: sqlc.ThreadStatusNew,
		AssigneeID: &multiUser.ID, CustomerEmail: "redir@client.example", LastMessageAt: now, OpenedAt: now,
	}

	req := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String()+"/list?status=new&nolist=1&evil=1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	want := "/threads/" + threadID.String() + "?status=new"
	if loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
}
