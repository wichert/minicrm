package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wichert/minicrm/internal/auth"
	sqlc "github.com/wichert/minicrm/internal/db/sqlc"
	web "github.com/wichert/minicrm/internal/web"
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
	// The list pane is really rendered: it must contain a row link for another thread
	// (row links carry the pane base query: ?thread=<id> identifies the thread).
	if !strings.Contains(body, `thread=`+otherID.String()) {
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
// filtered queue survives opening a thread; the ?thread=<id> query parameter —
// not the path segment — identifies the thread on such links.
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
	wantHref := `href="/threads?status=new&amp;thread=` + otherID.String() + `"`
	if !strings.Contains(body, wantHref) {
		t.Errorf("pane row links must carry the inbox query; missing %s in detail page", wantHref)
	}
	if strings.Contains(body, `href="/threads/`+otherID.String()+`"`) {
		t.Error("pane row links must not use the bare /threads/{id} form when a pane base is set")
	}

	// Follow the appended link: ?thread= must route to the right thread and the
	// response must render that thread's detail (not the anchor's).
	req2 := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String()+"?status=new&thread="+otherID.String(), nil)
	req2 = req2.WithContext(auth.WithUser(req2.Context(), admin))
	req2 = web.WithChiParam(req2, "id", threadID.String())
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET ?thread= status = %d, want 200", rec2.Code)
	}
	body2 := rec2.Body.String()
	if !strings.Contains(body2, "Linked row thread") {
		t.Error("?thread= query must identify the rendered thread")
	}
	if strings.Count(body2, "Anchor thread") > strings.Count(body2, "Linked row thread") && !strings.Contains(body2, "<h1>Linked row thread") {
		t.Error("?thread= must override the path segment as the thread source")
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
