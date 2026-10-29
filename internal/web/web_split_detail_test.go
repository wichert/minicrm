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

// W-rows: the split pane renders a dedicated two-line row (subject + customer,
// then the preamble at full width with the compact age at its trailing edge).
// The full inbox keeps the one-line row. Only markup semantics are asserted;
// appearance is verified from screenshots in the plan's W-rows-3 step.
func TestSplitPaneRowsRenderTwoLinePaneRowMarkup(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	otherID := uuid.New()
	agentID := store.adminUser.ID
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Pane row thread",
		Status: sqlc.ThreadStatusNew, AssigneeID: &agentID,
		CustomerName: "Ann Lee", CustomerEmail: "ann@example.com",
		LastMessageAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		OpenedAt:      time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		MessageCount:  3, HasDraft: true,
		LatestBodyText: "Hi, we moved offices last week and the invoice address changed",
	}
	store.threads[otherID] = sqlc.Thread{
		ID: otherID, AccountID: "support", SubjectDisplay: "Pane neighbour thread",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "no-name@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
		OpenedAt:       time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
		MessageCount:   1,
		LatestBodyText: "Outbound reply text here", LatestDirection: "outbound",
	}
	store.emails[uuid.New()] = sqlc.Email{
		ID: uuid.New(), ThreadID: threadID, AccountID: "support", FromAddress: "ann@example.com",
		Subject: "Pane row thread", BodyText: "Hi, we moved offices last week and the invoice address changed",
		SentAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}
	store.emails[uuid.New()] = sqlc.Email{
		ID: uuid.New(), ThreadID: otherID, AccountID: "support", FromAddress: "agent@example.com",
		Subject: "Pane neighbour thread", BodyText: "Outbound reply text here",
		SentAt: time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
	}

	// Split pane (detail page with list pane, status filter matching the seeded rows).
	req := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String()+"?status=new", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	paneIdx := strings.Index(body, `data-split-list`)
	detailIdx := strings.Index(body, `data-split-detail`)
	if paneIdx < 0 || detailIdx < 0 || detailIdx < paneIdx {
		t.Fatalf("pane/detail markers not in expected order")
	}
	pane := body[paneIdx:detailIdx]

	// The pane row is the three-part structure: .pane-row > .pane-row-top with
	// the subject link and customer span, then the separate .pane-row-snippet.
	rowIdx := strings.Index(pane, `<div class="pane-row">`)
	if rowIdx < 0 {
		t.Fatal("pane rows must render the .pane-row two-line structure")
	}
	rowEnd := strings.Index(pane[rowIdx:], "</td>")
	if rowEnd < 0 {
		t.Fatal("malformed pane row markup")
	}
	row := pane[rowIdx : rowIdx+rowEnd]
	topIdx := strings.Index(row, `<div class="pane-row-top">`)
	if topIdx < 0 {
		t.Fatal("pane row must open with .pane-row-top")
	}
	topEnd := strings.Index(row[topIdx:], "</div>")
	if topEnd < 0 {
		t.Fatal("malformed .pane-row-top")
	}
	top := row[topIdx : topIdx+topEnd]
	if !strings.Contains(top, `<a class="thread-link" href="/threads/`+otherID.String()) &&
		!strings.Contains(top, `<a class="thread-link" href="/threads/`+threadID.String()) {
		t.Errorf("pane row 1 must carry the subject link: %s", top)
	}
	custIdx := strings.Index(top, `class="pane-customer"`)
	if custIdx < 0 {
		t.Fatal("pane row 1 must carry the customer span")
	}
	// The subject link and the customer must both sit in row 1 (before the snippet).
	snipIdx := strings.Index(row, `class="pane-row-snippet"`)
	if snipIdx < 0 {
		t.Fatal("pane row must render the preamble as .pane-row-snippet")
	}
	if snipIdx < custIdx {
		t.Error("the customer span must be part of row 1, before the snippet")
	}
	// The snippet is its own element, not a sibling inside the same line container.
	if !strings.Contains(row, `pane-row-snippet">Hi, we moved offices`) {
		t.Error("the preamble must render inside .pane-row-snippet, separate from the subject link")
	}
	if !strings.Contains(row, `class="badge"`) || !strings.Contains(row, `class="count-pill"`) {
		t.Error("pane row 1 must keep the draft badge and the message-count pill")
	}
	// The compact age rides the snippet line with its full text in the title.
	if !strings.Contains(row, `class="wait-badge wait-`) || !strings.Contains(row, `pane-age`) {
		t.Error("pane rows must keep the age badge with the pane-age compact class")
	}
	ageIdx := strings.Index(row, `pane-age`)
	titleIdx := strings.Index(row, `title="Opened `)
	if ageIdx < 0 || titleIdx < 0 || titleIdx < ageIdx {
		t.Error("the compact age must keep the full opened timestamp in its title")
	}
	// The pane drops assignee, account and the separate meta line.
	if strings.Contains(row, `thread-meta`) || strings.Contains(row, `Unassigned`) {
		t.Error("pane rows must not render the assignee/meta line")
	}
	if strings.Contains(row, "Customer Support") {
		t.Error("pane rows must not render the account name")
	}
	// The selected row state survives the layout change.
	if !strings.Contains(pane, `aria-current="true"`) || !strings.Contains(pane, `thread-row-current`) {
		t.Error("the open thread's pane row must keep aria-current and thread-row-current")
	}

	// Customer name on row 1; the neighbour falls back to the email.
	if !strings.Contains(pane, `pane-customer" title="ann@example.com">Ann Lee`) {
		t.Error("pane row 1 must show the customer name with the email as title")
	}
	if !strings.Contains(pane, `>no-name@example.com</span>`) {
		t.Error("a customer without a name must fall back to the email on pane row 1")
	}

	// Outbound latest message: the You: marker stays on the preamble line.
	if !strings.Contains(pane, `<span class="snippet-you">You:</span> Outbound reply text here`) {
		t.Error("an outbound latest message must keep the You: prefix on the pane preamble")
	}

	// A thread with no message text still renders the placeholder in the snippet.
	emptyID := uuid.New()
	store.threads[emptyID] = sqlc.Thread{
		ID: emptyID, AccountID: "support", SubjectDisplay: "Pane empty thread",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "quiet@example.com",
		LastMessageAt: time.Date(2026, 1, 15, 8, 0, 0, 0, time.UTC),
		OpenedAt:      time.Date(2026, 1, 15, 8, 0, 0, 0, time.UTC),
	}
	req = httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String()+"?status=new", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail with empty-text row status = %d, want 200", rec.Code)
	}
	body = rec.Body.String()
	paneIdx = strings.Index(body, `data-split-list`)
	detailIdx = strings.Index(body, `data-split-detail`)
	if paneIdx < 0 || detailIdx < 0 || detailIdx < paneIdx {
		t.Fatalf("pane/detail markers not in expected order on second load")
	}
	pane = body[paneIdx:detailIdx]
	if !strings.Contains(pane, `pane-row-snippet"><em>(No message text)</em>`) {
		t.Error("a thread with no body text must render the placeholder inside .pane-row-snippet")
	}
}

// The two-line pane row is pane-only: the full inbox at /threads keeps the
// one-line row markup and must never emit the pane structure.
func TestFullInboxDoesNotRenderPaneRow(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Inbox row thread",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "ann@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		OpenedAt:       time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		LatestBodyText: "Inbox snippet text",
	}
	store.emails[uuid.New()] = sqlc.Email{
		ID: uuid.New(), ThreadID: threadID, AccountID: "support", FromAddress: "ann@example.com",
		Subject: "Inbox row thread", BodyText: "Inbox snippet text",
		SentAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /threads status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `pane-row`) || strings.Contains(body, `pane-customer`) || strings.Contains(body, `pane-age`) {
		t.Error("the full inbox must not render the pane-row markup")
	}
	for _, want := range []string{`class="thread-rowline"`, `class="thread-line"`, `class="thread-snippet"`, `class="thread-meta"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the full inbox must keep the one-line row markup: missing %s", want)
		}
	}
}

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
	// The pane keeps the queue selector and its filter row (rendered once, above
	// the split, since a 400px column cannot hold four filter controls).
	if !strings.Contains(body, `data-inbox-toolbar`) || !strings.Contains(body, `filter-inline`) {
		t.Error("detail page missing the inbox toolbar/filter markup")
	}
	if !strings.Contains(body, `data-split-filters`) {
		t.Error("detail page must render the split filter row above the panes")
	}
	// No bulk bar in the pane: it is preserved on the full inbox.
	if strings.Contains(body, `id="bulk-spam-form"`) {
		t.Error("the split pane must not render the bulk-actions bar")
	}
	// The open thread's row reads as selected.
	if !strings.Contains(body, `aria-current="true"`) {
		t.Error("the open thread's pane row must be marked aria-current")
	}
	if !strings.Contains(body, `thread-row-current`) {
		t.Error("the open thread's pane row must carry the current-row class")
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
	if !strings.Contains(body2, `aria-current="true"`) || !strings.Contains(body2, `thread-row-current`) {
		t.Error("the newly opened thread's row must be the marked current row after the round trip")
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

// W4 pane density: the split-pane table drops the bulk-select, status and age
// columns (the active tab implies the status and the age rides inline in the meta
// line) while the full inbox keeps all three.
func TestSplitPaneTableOmitsStatusAgeAndSelectColumns(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	otherID := uuid.New()
	agentID := store.adminUser.ID
	for id, subject := range map[uuid.UUID]string{threadID: "Pane open thread", otherID: "Pane other thread"} {
		store.threads[id] = sqlc.Thread{
			ID: id, AccountID: "support", SubjectDisplay: subject,
			Status: sqlc.ThreadStatusNew, AssigneeID: &agentID,
			CustomerEmail: "customer@example.com",
			LastMessageAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
			OpenedAt:      time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		}
	}
	emailID := uuid.New()
	store.emails[emailID] = sqlc.Email{
		ID: emailID, ThreadID: threadID, AccountID: "support", FromAddress: "customer@example.com",
		Subject: "Pane open thread", BodyText: "hi", SentAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}

	// Full inbox: everything stays.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /threads status = %d, want 200", rec.Code)
	}
	inbox := rec.Body.String()
	for _, want := range []string{
		`class="threads-table"`,
		`<th scope="col">Status</th>`,
		`<th scope="col">Age</th>`,
		`class="select-cell"`,
		`id="bulk-spam-form"`,
	} {
		if !strings.Contains(inbox, want) {
			t.Errorf("full inbox must keep %q", want)
		}
	}
	if strings.Contains(inbox, `threads-table-pane`) {
		t.Error("full inbox table must not carry the pane variant class")
	}

	// Split pane: the three columns are gone, the pane variant class is present,
	// and the age still reaches the row through the meta line.
	req := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String()+"?status=new", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	paneIdx := strings.Index(body, `data-split-list`)
	detailIdx := strings.Index(body, `data-split-detail`)
	if paneIdx < 0 || detailIdx < 0 || detailIdx < paneIdx {
		t.Fatalf("pane/detail markers not in expected order")
	}
	pane := body[paneIdx:detailIdx]
	if !strings.Contains(pane, `threads-table-pane`) {
		t.Error("pane table must carry the threads-table-pane variant")
	}
	for _, unwanted := range []string{`<th scope="col">Status</th>`, `<th scope="col">Age</th>`, `class="select-cell"`, `class="status-cell"`, `class="age-cell"`, `id="bulk-spam-form"`, `data-select-thread`} {
		if strings.Contains(pane, unwanted) {
			t.Errorf("pane must not render %q", unwanted)
		}
	}
	// The row still shows the age inline in the meta line.
	if !strings.Contains(pane, `<span class="wait-badge`) {
		t.Error("pane rows must keep the age inline in the meta line")
	}
	// The pane is a reading queue: no row menu, no hover quick actions and no
	// bulk-select entry. Acting on a thread belongs to the detail page or the
	// full inbox (/threads), which is one click away. Scope this to the results
	// table — the pane's "More" tab strip legitimately uses data-row-menu.
	resultsIdx := strings.Index(pane, `data-inbox-results`)
	if resultsIdx < 0 {
		t.Fatal("pane must render its results region")
	}
	results := pane[resultsIdx:]
	for _, unwanted := range []string{`data-row-menu`, `data-row-select`, `row-quick-complete`, `row-quick-spam`, `class="actions-cell"`, `<th scope="col" class="actions-cell">`} {
		if strings.Contains(results, unwanted) {
			t.Errorf("pane rows must not render the action affordance %q", unwanted)
		}
	}
}

// W4 defect: `.container-wide` overrode `.container`'s top margin with 0, so the
// inbox and the split view sat flush against the ink navbar. The heading spacing
// must come back from the shared `.container` rhythm, and the wide variant must
// keep its width cap and horizontal centering.
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
	// The pane's sticky offsets must agree with that spacing: it sticks below the
	// viewport top and is allowed to scroll to a viewport minus the 1rem it keeps
	// on each side.
	if !strings.Contains(sheet, "top: 1rem;") || !strings.Contains(sheet, "max-height: calc(100vh - 2rem);") {
		t.Error("app.css must keep the paired split-list sticky top/max-height")
	}
}

// W4 pane density is expressed in app.css only for what the template cannot say:
// a wider list column, two-line rows, a selected row, a slim actions cell, and the
// wide-only filter row that replaced the pane's own duplicate chrome.
func TestSplitPaneStylesAreWideOnly(t *testing.T) {
	css, err := static.FS.ReadFile("app.css")
	if err != nil {
		t.Fatal(err)
	}
	sheet := string(css)

	// The pane is wider than before (four filter controls no longer live in it and
	// the table is three columns instead of five).
	if !strings.Contains(sheet, "grid-template-columns: clamp(360px, 34vw, 520px) minmax(0, 1fr)") {
		t.Error("app.css must widen the split list column")
	}
	// The current row reads as selected.
	if !strings.Contains(sheet, `.split-list .thread-row[aria-current="true"]`) {
		t.Error("app.css must style the selected pane row")
	}
	if !strings.Contains(sheet, ".split-list .thread-row-current td:first-child") {
		t.Error("app.css must draw the selected row's accent on the first cell")
	}
	// Pane rows use the W-rows two-line layout (the 1280px one-line rule is for
	// the full-width inbox only; the pane gets its own .pane-row markup).
	if !strings.Contains(sheet, ".split-list .pane-row {") ||
		!strings.Contains(sheet, ".split-list .pane-row-top {") ||
		!strings.Contains(sheet, ".split-list .pane-row-snippet {") {
		t.Error("app.css must style the pane's two-line pane-row layout")
	}
	// The pane must not keep the old one-line row overrides: their selectors
	// would be dead now that the pane emits .pane-row markup instead.
	if strings.Contains(sheet, ".split-list .thread-rowline") ||
		strings.Contains(sheet, ".split-list .thread-line") ||
		strings.Contains(sheet, ".split-list .thread-meta") {
		t.Error("app.css must drop the retired .split-list one-line row overrides")
	}
	// The pane renders no actions cell, and no row hover highlight: a row that
	// lit up under the cursor suggested an action surface that is not there.
	if !strings.Contains(sheet, ".split-list .thread-row:hover") ||
		!strings.Contains(sheet, "background: transparent;") {
		t.Error("app.css must suppress the split pane's row hover highlight")
	}
	if !strings.Contains(sheet, ".split-list .row-quick-actions,") {
		t.Error("app.css must keep the pane's quick actions hidden")
	}
	// The split filter row is wide-only: rendered above the split, hidden below
	// 1280px where the pane does not exist.
	if !strings.Contains(sheet, ".split-filters {\n  display: none;") {
		t.Error("app.css must hide the split filter row by default (below 1280px)")
	}
	if !strings.Contains(sheet, "@media (max-width: 1279.98px) {\n  .split-filters {\n    display: none;") {
		t.Error("app.css must keep an explicit below-1280px rule for .split-filters")
	}
	// The pane hides its own table header and the redundant results count.
	if !strings.Contains(sheet, ".split-list .threads-table thead {") {
		t.Error("app.css must hide the pane's table header row")
	}
	if !strings.Contains(sheet, ".split-list .results-count {") {
		t.Error("app.css must drop the pane's results count")
	}
}

// On the completed/spam tabs there is no wait badge (ThreadAgeBadge returns an
// empty label for those statuses), so the pane's trailing age is a relative
// age. That string is short enough to fit the pane-age badge's max-width;
// the previous absolute timestamp ("2026-01-15 10:00") overflowed the space
// the preamble reserves for it and drew under the snippet's last line.
func TestSplitPaneTrailingAgeStaysCompactWithoutWaitBadge(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Completed pane thread",
		Status:         sqlc.ThreadStatusCompleted,
		CustomerName:   "Ann Lee",
		LastMessageAt:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		OpenedAt:       time.Date(2026, 1, 10, 10, 0, 0, 0, time.UTC),
		MessageCount:   2,
		LatestBodyText: "Completed thread preamble text",
	}
	store.emails[uuid.New()] = sqlc.Email{
		ID: uuid.New(), ThreadID: threadID, AccountID: "support", FromAddress: "ann@example.com",
		Subject: "Completed pane thread", BodyText: "Completed thread preamble text",
		SentAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}

	req := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String()+"?status=completed", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET completed detail status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	paneIdx := strings.Index(body, `data-split-list`)
	detailIdx := strings.Index(body, `data-split-detail`)
	if paneIdx < 0 || detailIdx < 0 || detailIdx < paneIdx {
		t.Fatalf("pane/detail markers not in expected order")
	}
	pane := body[paneIdx:detailIdx]

	if strings.Contains(pane, `class="wait-badge`) {
		t.Error("a completed thread must not render a wait badge in the pane")
	}
	// The trailing age element must still be present and must be a relative
	// age, not an absolute timestamp that overflows the reserved space.
	ageMarker := `class="pane-age"`
	ageIdx := strings.Index(pane, ageMarker)
	if ageIdx < 0 {
		t.Fatal("a completed pane row must still render a trailing age element")
	}
	tagEnd := strings.Index(pane[ageIdx:], ">")
	if tagEnd < 0 {
		t.Fatal("malformed pane-age element")
	}
	textEnd := strings.Index(pane[ageIdx+tagEnd:], "</time>")
	if textEnd < 0 {
		t.Fatal("malformed pane-age element: missing closing tag")
	}
	label := strings.TrimSpace(pane[ageIdx+tagEnd+1 : ageIdx+tagEnd+textEnd])
	if label == "" {
		t.Fatal("the pane-age element must not be empty")
	}
	if strings.Contains(label, ":") {
		t.Errorf("the pane's trailing age must stay compact, got an absolute timestamp %q", label)
	}
	if !strings.Contains(label, "ago") && label != "Just now" {
		t.Errorf("the pane's trailing age must be a relative age, got %q", label)
	}
}

// The split pane's results-status line must not carry a ticking age label. The
// eyebrow still renders so the manual "Refresh" affordance stays reachable, but
// the live status text is left empty and app.js never paints it inside the pane
// (a "Updated just now" beside a queue the user is reading is noise).
func TestSplitPaneHasNoUpdatedStatusLabel(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Quiet pane thread",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "quiet@example.com",
		LastMessageAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		OpenedAt:      time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		MessageCount:  1, LatestBodyText: "Quiet pane preamble",
	}
	store.emails[uuid.New()] = sqlc.Email{
		ID: uuid.New(), ThreadID: threadID, AccountID: "support", FromAddress: "quiet@example.com",
		Subject: "Quiet pane thread", BodyText: "Quiet pane preamble",
		SentAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}

	req := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String()+"?status=new", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	paneIdx := strings.Index(body, `data-split-list`)
	detailIdx := strings.Index(body, `data-split-detail`)
	if paneIdx < 0 || detailIdx < 0 || detailIdx < paneIdx {
		t.Fatalf("pane/detail markers not in expected order")
	}
	pane := body[paneIdx:detailIdx]

	// The manual refresh control survives so the queue can be reloaded on demand.
	if !strings.Contains(pane, `data-refresh-link`) {
		t.Error("the pane must keep its manual refresh affordance")
	}
	// The live status span must exist (app.js targets it) but render no text.
	if !strings.Contains(pane, `data-refresh-status`) {
		t.Fatal("the pane must keep the data-refresh-status hook")
	}
	statusIdx := strings.Index(pane, `data-refresh-status`)
	open := strings.Index(pane[statusIdx:], ">")
	if open < 0 {
		t.Fatal("malformed data-refresh-status element")
	}
	closeIdx := strings.Index(pane[statusIdx+open:], "</span>")
	if closeIdx < 0 {
		t.Fatal("malformed data-refresh-status element: missing closing tag")
	}
	label := strings.TrimSpace(pane[statusIdx+open+1 : statusIdx+open+closeIdx])
	if label != "" && !strings.EqualFold(label, "Refresh") {
		t.Errorf("the pane's status span must not carry an age label, got %q", label)
	}
	// And the split pane markup must not contain the phrase the auto-refresh used
	// to paint into it.
	if strings.Contains(pane, "Updated just now") {
		t.Error("the pane must not render an 'Updated just now' label")
	}
}

// The retired "Oldest open" sort is replaced by "Longest waiting": the select
// offers only the new key and no longer mentions the old label anywhere.
func TestSortSelectOffersLongestWaiting(t *testing.T) {
	_, _, router, _, _ := buildAccountTestServer(t)

	for _, path := range []string{"/threads?status=new", "/threads?status=new&sort=longest_waiting"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Longest waiting") {
			t.Errorf("%s: sort control must offer Longest waiting", path)
		}
		if strings.Contains(body, "Oldest open") || strings.Contains(body, `value="oldest_open"`) {
			t.Errorf("%s: the retired Oldest open sort must be gone", path)
		}
	}

	// The default stays newest, and the new key round-trips.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=new", nil))
	if !strings.Contains(rec.Body.String(), `value="newest" selected`) {
		t.Error("the default sort must stay Newest")
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=new&sort=longest_waiting", nil))
	if !strings.Contains(rec.Body.String(), `value="longest_waiting" selected`) {
		t.Error("sort=longest_waiting must select the new option")
	}
}

// Filter-navigation consistency (plans/filter-navigation-consistency.md): row
// links on the filtered inbox list must carry the queue's query string onto the
// detail URL, so the split view re-renders the same filtered queue instead of
// falling back to the default open inbox.
func TestListRowLinksCarryInboxQueryIntoDetailURL(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	mineID := uuid.New()
	otherID := uuid.New()
	store.threads[mineID] = sqlc.Thread{
		ID: mineID, AccountID: "support", SubjectDisplay: "Queue match",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "queue@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		LatestBodyText: "Latest inbound text",
	}
	store.threads[otherID] = sqlc.Thread{
		ID: otherID, AccountID: "support", SubjectDisplay: "Queue neighbour",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "neighbour@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
		LatestBodyText: "Neighbour text",
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=new&q=Queue", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /threads?status=new&q=Queue status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The href is HTML-escaped (& -> &amp;) but must keep the queue's filters.
	for _, want := range []string{
		`href="/threads/` + mineID.String() + `?status=new&amp;q=Queue"`,
		`href="/threads/` + otherID.String() + `?status=new&amp;q=Queue"`,
		`data-row-href="/threads/` + mineID.String() + `?status=new&amp;q=Queue"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list row links must carry the queue query onto the detail path; missing %s", want)
		}
	}
}

// The split view's two filter bars (top data-split-filters and the pane's
// sticky bar) must both submit to the open thread's path: changing a pane
// filter re-renders detail+pane in plain HTML (no-JS included) instead of
// navigating to the plain inbox. The carried filters must show as selected.
func TestSplitPaneFilterFormsTargetThreadPath(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	otherID := uuid.New()
	agentID := store.adminUser.ID
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Anchor thread",
		Status: sqlc.ThreadStatusNew, AssigneeID: &agentID, CustomerEmail: "anchor@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		LatestBodyText: "Anchor text",
	}
	store.threads[otherID] = sqlc.Thread{
		ID: otherID, AccountID: "support", SubjectDisplay: "Needle thread",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "needle@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
		LatestBodyText: "Needle text",
	}

	req := httptest.NewRequest(http.MethodGet, "/threads/"+threadID.String()+"?status=new&sort=longest_waiting&account=support&q=needle", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail with filters status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// No GET filter form may target the plain inbox on the split page: both bars
	// submit to the thread path so a filter change keeps the conversation open.
	if got := strings.Count(body, `action="/threads"`); got != 0 {
		t.Errorf("split page must not contain a filter form action=%q (count %d)", "/threads", got)
	}
	if got := strings.Count(body, `form method="get" action="/threads/`+threadID.String()+`"`); got != 2 {
		t.Errorf("both split filter bars must target the thread path, found %d forms", got)
	}

	paneIdx := strings.Index(body, `data-split-list`)
	detailIdx := strings.Index(body, `data-split-detail`)
	if paneIdx < 0 || detailIdx < 0 || detailIdx < paneIdx {
		t.Fatal("pane/detail markers not in expected order")
	}
	pane := body[paneIdx:detailIdx]

	// The pane's controls show the carried filters as selected.
	for _, want := range []string{
		`name="q" value="needle"`,
		`value="longest_waiting" selected`,
		`<option value="support" selected>Customer Support</option>`,
		`name="status" value="new"`,
	} {
		if !strings.Contains(pane, want) {
			t.Errorf("pane filter bar must carry the queue filters; missing %s", want)
		}
	}
	// The enhancement marker matches the form target, so a select change submits
	// the enclosing form to the thread path (app.js submits el.form).
	if !strings.Contains(pane, `data-hx-get="/threads/`+threadID.String()+`"`) {
		t.Error("pane filter controls must mark data-hx-get with the thread path")
	}
	if strings.Contains(pane, `data-hx-get="/threads"`) {
		t.Error("pane filter controls must not point data-hx-get at the plain inbox")
	}
	// Clearing the search keeps the remaining filters and stays on the split view.
	wantClear := `class="search-clear" href="/threads/` + threadID.String() + `?account=support&amp;sort=longest_waiting&amp;status=new"`
	if !strings.Contains(pane, wantClear) {
		t.Errorf("pane search-clear must stay on the thread path with the remaining filters; missing %s", wantClear)
	}
	// The pane refresh link re-renders the split view with the same filters.
	wantRefresh := `href="/threads/` + threadID.String() + `?status=new&amp;sort=longest_waiting&amp;account=support&amp;q=needle"`
	if !strings.Contains(pane, wantRefresh) {
		t.Errorf("pane refresh link must re-render the split view; missing %s", wantRefresh)
	}
}

// Deep-link fallback: a detail URL without any filter query (bookmark, Back
// reload, legacy ?thread= link) must populate the pane from the session's
// last-visited inbox queue instead of the bare default open inbox.
func TestDetailDeepLinkUsesSessionInboxFilters(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	newID := uuid.New()
	completedID := uuid.New()
	store.threads[newID] = sqlc.Thread{
		ID: newID, AccountID: "support", SubjectDisplay: "Fresh thread",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "fresh@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		LatestBodyText: "Fresh text",
	}
	store.threads[completedID] = sqlc.Thread{
		ID: completedID, AccountID: "support", SubjectDisplay: "Archived thread",
		Status: sqlc.ThreadStatusCompleted, CustomerEmail: "archived@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
		LatestBodyText: "Archived text",
	}

	// Visit a filtered queue so the handler records it as the last inbox URL.
	first := httptest.NewRequest(http.MethodGet, "/threads?status=completed&sort=longest_waiting", nil)
	firstRec := httptest.NewRecorder()
	router.ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("GET filtered inbox status = %d, want 200", firstRec.Code)
	}

	// Then open a thread deep link without a query, carrying the session cookie.
	req := httptest.NewRequest(http.MethodGet, "/threads/"+newID.String(), nil)
	for _, cookie := range firstRec.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET deep-linked detail status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	paneIdx := strings.Index(body, `data-split-list`)
	detailIdx := strings.Index(body, `data-split-detail`)
	if paneIdx < 0 || detailIdx < 0 || detailIdx < paneIdx {
		t.Fatal("deep-linked detail must still render the split panes")
	}
	pane := body[paneIdx:detailIdx]

	// The pane queue comes from the session's last inbox URL, not the default.
	if !strings.Contains(pane, `href="/threads/`+completedID.String()+`?status=completed&amp;sort=longest_waiting"`) {
		t.Errorf("deep-linked pane must show the session's completed queue with carried filters: %s", pane)
	}
	if strings.Contains(pane, `<a class="thread-link" href="/threads/`+newID.String()) {
		t.Errorf("deep-linked pane must not fall back to the default open queue: %s", pane)
	}
	// The carried filters show in the pane's controls and hidden status field.
	for _, want := range []string{
		`name="status" value="completed"`,
		`value="longest_waiting" selected`,
	} {
		if !strings.Contains(pane, want) {
			t.Errorf("deep-linked pane controls must reflect the session filters; missing %s", want)
		}
	}
	// The pane form still targets the thread path so filters keep the split view.
	if !strings.Contains(pane, `form method="get" action="/threads/`+newID.String()+`"`) {
		t.Error("deep-linked pane filter form must target the thread path")
	}
	// The refresh link stays on the split view with the restored queue.
	if !strings.Contains(pane, `href="/threads/`+newID.String()+`?status=completed&amp;sort=longest_waiting"`) {
		t.Error("deep-linked pane refresh link must carry the restored filters")
	}
}

// The pane's "Load older" cursor link must page the pane in place: it carries
// the next-page cursor onto the thread path instead of linking to the plain
// inbox (which would leave the split view).
func TestSplitPaneLoadOlderKeepsThreadPath(t *testing.T) {
	srv, store, router, _, _ := buildAccountTestServer(t)
	srv.WithInboxPageSize(1)
	firstID := uuid.New()
	secondID := uuid.New()
	store.threads[firstID] = sqlc.Thread{
		ID: firstID, AccountID: "support", SubjectDisplay: "Newest thread",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "newest@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		LatestBodyText: "Newest text",
	}
	store.threads[secondID] = sqlc.Thread{
		ID: secondID, AccountID: "support", SubjectDisplay: "Older thread",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "older@example.com",
		LastMessageAt:  time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
		LatestBodyText: "Older text",
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads/"+firstID.String()+"?status=new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	paneIdx := strings.Index(body, `data-split-list`)
	detailIdx := strings.Index(body, `data-split-detail`)
	if paneIdx < 0 || detailIdx < 0 || detailIdx < paneIdx {
		t.Fatal("split panes not rendered")
	}
	pane := body[paneIdx:detailIdx]
	if !strings.Contains(pane, `class="btn btn-secondary" href="/threads/`+firstID.String()+`?`) {
		t.Errorf("pane Load older link must page the pane in place, got: %s", pane)
	}
	if strings.Contains(pane, `href="/threads?status=new&amp;`) {
		t.Error("pane Load older link must not point at the plain inbox")
	}
}
