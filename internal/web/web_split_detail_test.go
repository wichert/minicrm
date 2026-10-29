package web_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wichert/minicrm/internal/auth"
	sqlc "github.com/wichert/minicrm/internal/db/sqlc"
	web "github.com/wichert/minicrm/internal/web"
	"github.com/wichert/minicrm/internal/web/static"
)

// Render-level coverage for the W4 split master-detail document. Only markup
// semantics are asserted; no CSS-presence or computed-geometry checks.
func TestThreadDetailRendersSplitListPane(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	otherID := uuid.New()
	agentID := store.adminUser.ID
	store.threads[threadID] = sqlc.Thread{
		ID:             threadID,
		AccountID:      "support",
		SubjectDisplay: "Split pane thread",
		Status:         sqlc.ThreadStatusNew,
		AssigneeID:     &agentID,
		CustomerEmail:  "customer@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}
	store.threads[otherID] = sqlc.Thread{
		ID:             otherID,
		AccountID:      "support",
		SubjectDisplay: "Other open thread",
		Status:         sqlc.ThreadStatusNew,
		CustomerEmail:  "other@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
	}
	store.emails[uuid.New()] = sqlc.Email{
		ID:          uuid.New(),
		ThreadID:    threadID,
		AccountID:   "support",
		Direction:   "inbound",
		FromAddress: "customer@example.com",
		Subject:     "Split pane thread",
		BodyText:    "Hello from the customer",
		SentAt:      time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}

	req := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String(), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-split-layout`) {
		t.Error("detail page missing data-split-layout wrapper")
	}
	if !strings.Contains(body, `data-split-list`) || !strings.Contains(body, `data-split-detail`) {
		t.Error("detail page missing split list/detail panes")
	}
	if !strings.Contains(body, "Split pane thread") {
		t.Error("detail page missing the open thread")
	}
	// The list pane is really rendered: it must contain a row link for another thread,
	// addressed by path (the only route that renders a detail).
	if !strings.Contains(body, `href="/threads/`+otherID.String()+`"`) {
		t.Error("split list pane missing row link for another thread")
	}
	// The pane keeps the inbox chrome (toolbar, filters) working.
	if !strings.Contains(body, `data-inbox-toolbar`) || !strings.Contains(body, `filter-inline`) {
		t.Error("split list pane missing inbox toolbar/filter markup")
	}
	// Split layout needs the wide container.
	if !strings.Contains(body, `container-wide`) {
		t.Error("detail page with list pane must use the wide container")
	}
	// Exactly one top-level heading: the thread subject. The list pane must not
	// contribute a second h1 (it uses an sr-only h2).
	if got := strings.Count(body, "<h1"); got != 1 {
		t.Errorf("detail page must have exactly one h1, got %d", got)
	}
	// The back link must be a plain focusable anchor in the markup; its visibility
	// in split mode is CSS-only, so it must carry no baked-in AT suppression.
	backIdx := strings.Index(body, `class="back-link"`)
	if backIdx < 0 {
		t.Fatal("detail page missing back link")
	}
	endIdx := strings.Index(body[backIdx:], ">")
	if endIdx < 0 {
		t.Fatal("malformed back link markup")
	}
	backTag := body[backIdx : backIdx+endIdx]
	if strings.Contains(backTag, "aria-hidden") || strings.Contains(backTag, "tabindex") {
		t.Errorf("back link must not hardcode aria-hidden/tabindex: %q", backTag)
	}
}

func TestThreadDetailNolistRendersSinglePane(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	otherID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID:             threadID,
		AccountID:      "support",
		SubjectDisplay: "Single pane thread",
		Status:         sqlc.ThreadStatusNew,
		CustomerEmail:  "customer@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}
	store.threads[otherID] = sqlc.Thread{
		ID:             otherID,
		AccountID:      "support",
		SubjectDisplay: "Other open thread",
		Status:         sqlc.ThreadStatusNew,
		CustomerEmail:  "other@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
	}

	req := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String()+"?nolist=1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail?nolist=1 status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `data-split-list`) {
		t.Error("?nolist=1 must omit the list pane")
	}
	if !strings.Contains(body, "Single pane thread") {
		t.Error("?nolist=1 must keep the thread detail")
	}
	if strings.Contains(body, `href="/threads/`+otherID.String()+`"`) {
		t.Error("?nolist=1 must not render other threads' row links")
	}
	// Single-pane pages keep the back link visible at every width: the split-single
	// modifier must exempt it from the wide-only hide rule.
	if !strings.Contains(body, `split-layout split-single`) {
		t.Error("?nolist=1 page must carry the split-single modifier class")
	}
}

// Spec W4 §4: row links in the pane must carry the current inbox query so a
// filtered queue survives opening a thread, and must address the thread by PATH
// (/threads/{id}). Addressing it as /threads?thread=<id> matched GET /threads
// instead of GET /threads/{id}, so a pane row click re-rendered the list and the
// pane never opened the thread.
func TestSplitPaneRowLinksCarryInboxQuery(t *testing.T) {
	_, store, router, admin, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	otherID := uuid.New()
	agentID := store.adminUser.ID
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Anchor thread",
		Status: sqlc.ThreadStatusNew, AssigneeID: &agentID, CustomerEmail: "customer@example.com",
		LastMessageAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}
	store.threads[otherID] = sqlc.Thread{
		ID: otherID, AccountID: "support", SubjectDisplay: "Linked row thread",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "other@example.com",
		LastMessageAt: time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
	}
	store.emails[uuid.New()] = sqlc.Email{
		ID: uuid.New(), ThreadID: threadID, AccountID: "support", FromAddress: "customer@example.com",
		Subject: "Anchor thread", BodyText: "hi", SentAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}
	store.emails[uuid.New()] = sqlc.Email{
		ID: uuid.New(), ThreadID: otherID, AccountID: "support", FromAddress: "other@example.com",
		Subject: "Linked row thread", BodyText: "hi", SentAt: time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
	}

	// Open the anchor thread from a filtered queue (the query mirrors what a row
	// link appends).
	req := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String()+"?status=new", nil)
	req = req.WithContext(auth.WithUser(req.Context(), admin))
	req = web.WithChiParam(req, "id", threadID.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail with query status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	wantHref := `href="/threads/` + otherID.String() + `?status=new"`
	if !strings.Contains(body, wantHref) {
		t.Errorf("pane row links must address the thread by path and carry the inbox query; missing %s in detail page", wantHref)
	}
	// The row's data-row-href (the click target in app.js) must use the same form.
	if !strings.Contains(body, `data-row-href="/threads/`+otherID.String()+`?status=new"`) {
		t.Error("pane row data-row-href must address the thread by path and carry the inbox query")
	}
	// The broken addressing mechanism must never come back: /threads?thread=<id>
	// matched the list route, so clicking a pane row reloaded the inbox.
	if strings.Contains(body, "/threads?status=new&amp;thread="+otherID.String()) {
		t.Error("pane row links must not address the thread as /threads?...&thread=<id>")
	}
	if strings.Contains(body, "thread="+otherID.String()) {
		t.Errorf("pane row links must not put the thread id in the query: %s", body)
	}

	// The emitted link must round-trip: following it renders the OTHER thread's
	// detail and keeps the filter in the pane (same path-addressed shape again).
	req2 := httptest.NewRequest(http.MethodGet, "/threads/"+otherID.String()+"?status=new", nil)
	req2 = req2.WithContext(auth.WithUser(req2.Context(), admin))
	req2 = web.WithChiParam(req2, "id", threadID.String())
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET pane row link status = %d, want 200", rec2.Code)
	}
	body2 := rec2.Body.String()
	if !strings.Contains(body2, "<h1>Linked row thread") {
		t.Error("following a pane row link must render that thread's detail")
	}
	// Round-trip: the pane on the new page still shows status=new and the anchor
	// thread is linkable from it (with the filter preserved).
	if !strings.Contains(body2, `href="/threads/`+threadID.String()+`?status=new"`) {
		t.Error("pane row links must keep the inbox filters across a round trip")
	}
}

// Legacy deep links that still address the thread as ?thread=<id> keep working:
// the override is no longer how row links work, but old URLs must not break.
func TestViewThreadKeepsThreadQueryOverride(t *testing.T) {
	_, store, router, admin, _ := buildAccountTestServer(t)
	pathID := uuid.New()
	queryID := uuid.New()
	for id, subject := range map[uuid.UUID]string{pathID: "Path thread", queryID: "Query thread"} {
		store.threads[id] = sqlc.Thread{
			ID: id, AccountID: "support", SubjectDisplay: subject,
			Status: sqlc.ThreadStatusNew, CustomerEmail: "c@example.com",
			LastMessageAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/threads/"+pathID.String()+"?thread="+queryID.String(), nil)
	req = req.WithContext(auth.WithUser(req.Context(), admin))
	req = web.WithChiParam(req, "id", pathID.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with ?thread= override status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<h1>Query thread") {
		t.Error("the legacy ?thread= override must still identify the rendered thread")
	}
}

// Regression guard: restructuring the detail template must not lose the Reopen &
// Merge banner, the reply composer, or attachment markup.
func TestThreadDetailSplitKeepsReopenComposerAndAttachments(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	completedID := uuid.New()
	followUpID := uuid.New()
	emailID := uuid.New()
	attachmentID := uuid.New()
	agentID := store.adminUser.ID
	store.threads[completedID] = sqlc.Thread{
		ID:             completedID,
		AccountID:      "support",
		SubjectDisplay: "Previous Ticket",
		Status:         sqlc.ThreadStatusCompleted,
		AssigneeID:     &agentID,
		CustomerEmail:  "client@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}
	store.threads[followUpID] = sqlc.Thread{
		ID:              followUpID,
		AccountID:       "support",
		SubjectDisplay:  "Re: Previous Ticket",
		Status:          sqlc.ThreadStatusNew,
		AssigneeID:      &agentID,
		CustomerEmail:   "client@example.com",
		ReopensThreadID: &completedID,
		LastMessageAt:   time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
	}
	store.emails[emailID] = sqlc.Email{
		ID:                emailID,
		ThreadID:          followUpID,
		AccountID:         "support",
		Direction:         "inbound",
		FromAddress:       "client@example.com",
		Subject:           "Re: Previous Ticket",
		BodyHTMLSanitized: "<p>Safe sanitized paragraph</p>",
		SentAt:            time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
	}
	store.attachments[attachmentID] = sqlc.Attachment{
		ID:             attachmentID,
		EmailID:        emailID,
		Filename:       "invoice.pdf",
		ContentType:    "application/pdf",
		SizeBytes:      128,
		SHA256Hex:      "abc",
		StorageRelPath: "ab/abc",
	}

	req := httptest.NewRequest(http.MethodGet, "/threads/"+followUpID.String(), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Reopen &amp; merge") {
		t.Error("split detail page lost the Reopen & Merge banner")
	}
	if !strings.Contains(body, "reply-card") || !strings.Contains(body, `name="body_text"`) {
		t.Error("split detail page lost the reply composer")
	}
	if !strings.Contains(body, "/attachments/"+attachmentID.String()) {
		t.Error("split detail page lost attachment markup")
	}
	if !strings.Contains(body, `sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"`) {
		t.Error("split detail page lost the sandboxed email iframe")
	}
}

// W4 defect: `.container-wide` overrode `.container`'s top margin with 0, so the
// inbox and the split view sat flush against the ink navbar. The heading spacing
// must come back from the shared `.container` rhythm, and the wide variant must
// keep its width cap and horizontal centering (it was added to fix W2's
// horizontal overflow, not the vertical rhythm).
func TestWideContainerKeepsTopSpacing(t *testing.T) {
	css, err := static.FS.ReadFile("app.css")
	if err != nil {
		t.Fatal(err)
	}
	sheet := string(css)

	if !strings.Contains(sheet, ".container {\n  width: 100%;\n  max-width: 1120px;\n  margin: 1.75rem auto;") {
		t.Error("app.css must keep the shared .container top-margin rhythm (1.75rem)")
	}
	wideIdx := strings.Index(sheet, ".container-wide {")
	if wideIdx < 0 {
		t.Fatal("app.css is missing .container-wide")
	}
	rule := sheet[wideIdx:]
	if end := strings.Index(rule, "}"); end >= 0 {
		rule = rule[:end]
	}
	if !strings.Contains(rule, "max-width: min(1400px, 100%)") {
		t.Errorf(".container-wide must keep its width cap: %q", rule)
	}
	if regexp.MustCompile(`margin-top:\s*0[;\s]`).MatchString(rule) {
		t.Errorf(".container-wide must not zero out the top margin: %q", rule)
	}
}
