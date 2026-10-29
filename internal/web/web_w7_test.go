package web_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	sqlc "github.com/wichert/minicrm/internal/db/sqlc"
)

// W7 row-menu parity: every open thread's ⋯ menu must carry the Start working
// shortcut (new only), the per-thread "Assign to…" submenu filtered server-side
// to active users with access to the thread's own account, and the quick
// complete/spam buttons.
func TestRowMenuParityAndAccountFilteredAssignTo(t *testing.T) {
	_, store, router, multiUser, singleUser := buildAccountTestServer(t)
	supportID := uuid.New()
	secretID := uuid.New()
	store.threads[supportID] = sqlc.Thread{ID: supportID, AccountID: "support", SubjectDisplay: "Support row", Status: sqlc.ThreadStatusNew, CustomerEmail: "s@x.example", LastMessageAt: time.Now().UTC()}
	store.threads[secretID] = sqlc.Thread{ID: secretID, AccountID: "secret", SubjectDisplay: "Secret row", Status: sqlc.ThreadStatusNew, CustomerEmail: "p@x.example", LastMessageAt: time.Now().UTC()}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ">Start working<") {
		t.Error("row menu must offer Start working for new threads")
	}
	if !strings.Contains(body, "row-menu-submenu") || !strings.Contains(body, "Assign to&hellip;") {
		t.Error("row menu must offer the Assign to… submenu")
	}
	if !strings.Contains(body, "row-quick-complete") || !strings.Contains(body, "row-quick-spam") {
		t.Error("row menu must offer quick complete/spam buttons")
	}
	// Submenu entries are per-thread and account-scoped: the multi-account admin
	// sees both agents on the support thread.
	rowIdx := strings.Index(body, "Support row")
	submenuIdx := strings.Index(body[rowIdx:], "row-menu-submenu")
	if submenuIdx < 0 {
		t.Fatal("missing submenu for support row")
	}
	submenu := body[rowIdx+submenuIdx:]
	end := strings.Index(submenu, "</details>")
	if end < 0 {
		t.Fatal("malformed submenu")
	}
	submenu = submenu[:end]
	for _, name := range []string{multiUser.DisplayName, singleUser.DisplayName} {
		if !strings.Contains(submenu, name) {
			t.Errorf("support-row submenu must list %s", name)
		}
	}

	// The "as=single" persona has access only to the support account, so the
	// secret-account thread must never render (404-independent: the inbox only
	// lists accessible accounts) and its submenu would not leak names.
	singleRec := httptest.NewRecorder()
	router.ServeHTTP(singleRec, httptest.NewRequest(http.MethodGet, "/threads?account=support&status=new", nil))
	singleBody := singleRec.Body.String()
	if strings.Contains(singleBody, "Secret row") {
		t.Error("threads of inaccessible accounts must not render")
	}
	if strings.Count(singleBody, "row-menu-submenu") == 0 {
		t.Error("support rows must still offer the Assign to… submenu")
	}
}

// parseInboxListFilters accepts only the allow-listed sort values; anything else
// falls back to newest instead of reaching the store.
func TestParseInboxListFiltersSortAllowList(t *testing.T) {
	_, _, router, _, _ := buildAccountTestServer(t)
	cases := []struct {
		raw  string
		want string
	}{
		{"", "newest"},
		{"newest", "newest"},
		{"oldest_open", "oldest_open"},
		{"Newest", "newest"},         // case-sensitive: unknown falls back
		{"; DROP TABLE x", "newest"}, // injection attempt falls back
		{"oldest", "newest"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?sort="+url.QueryEscape(tc.raw), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("sort %q: status = %d", tc.raw, rec.Code)
		}
		// The sort select must reflect the resolved value.
		body := rec.Body.String()
		oldestSelected := strings.Contains(body, `value="oldest_open" selected`)
		if tc.want == "oldest_open" && !oldestSelected {
			t.Errorf("sort %q: oldest_open not selected in sort control", tc.raw)
		}
		if tc.want == "newest" && oldestSelected {
			t.Errorf("sort %q: must fall back to newest, but oldest_open is selected", tc.raw)
		}
	}
}

// The inbox footer must show "N threads · Load older" when more pages exist and
// a truncation notice when the oldest_open sort fills its page.
func TestInboxFooterAndSortTruncation(t *testing.T) {
	testInboxPageSize = 2
	t.Cleanup(func() { testInboxPageSize = 0 })
	_, store, router, _, _ := buildAccountTestServer(t)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		id := uuid.New()
		store.threads[id] = sqlc.Thread{ID: id, AccountID: "support", SubjectDisplay: fmt.Sprintf("Page %d", i), Status: sqlc.ThreadStatusNew, CustomerEmail: "p@x.example", LastMessageAt: now.Add(-time.Duration(i) * time.Hour)}
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads?status=new", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Load older</a>") {
		t.Fatal("paged inbox must show the Load older link")
	}
	if !strings.Contains(body, "list-footer-count") {
		t.Fatal("footer must carry the list-footer-count label")
	}

	// oldest_open has no cursor pagination: a full page is capped with a notice.
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/threads?status=new&sort=oldest_open", nil))
	body2 := rec2.Body.String()
	if !strings.Contains(body2, "list-truncated") {
		t.Fatal("full oldest_open page must show the truncation notice")
	}
	if strings.Contains(body2, "Load older</a>") {
		t.Fatal("oldest_open must not offer cursor pagination")
	}
	if strings.Count(body2, `class="thread-link"`) != 2 {
		t.Fatalf("oldest_open page must be capped to the page size, got %d links", strings.Count(body2, `class="thread-link"`))
	}
}
