package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	sqlc "github.com/wichert/minicrm/internal/db/sqlc"
)

// Render-level coverage for the W3 bulk actions bar (no CSS/geometry asserts).
func TestInboxBulkBarRenderSemantics(t *testing.T) {
	_, store, router, _, _ := buildAccountTestServer(t)
	threadID := uuid.New()
	store.threads[threadID] = sqlc.Thread{
		ID: threadID, AccountID: "support", SubjectDisplay: "Bulk bar",
		Status: sqlc.ThreadStatusNew, CustomerEmail: "bulk@client.example", LastMessageAt: time.Now().UTC(),
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/threads", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /threads status = %d", rec.Code)
	}
	body := rec.Body.String()

	// (a) Every row renders a data-row-select button inside its row menu
	// (regression guard: the Select binding must have targets in the rows).
	if !strings.Contains(body, `data-row-select`) {
		t.Fatalf("expected a data-row-select button in each row menu: %s", body)
	}
	if strings.Count(body, "row-menu-popover") < 1 || !strings.Contains(body, `data-row-select aria-label="Select Bulk bar for bulk actions"`) {
		t.Fatalf("expected the row-menu Select control for the rendered thread: %s", body)
	}

	// (b) The Clear control exists in the bar.
	if !strings.Contains(body, `data-bulk-clear`) {
		t.Fatalf("expected the Clear control in the bulk bar: %s", body)
	}

	// (c) The instructional sentence is gone and the aria-live count span is present.
	if strings.Contains(body, "Select conversations to assign") {
		t.Fatalf("instructional placeholder sentence must be gone: %s", body)
	}
	if !strings.Contains(body, `<span data-selected-count aria-live="polite"></span>`) {
		t.Fatalf("expected the empty aria-live count span: %s", body)
	}

	// (d) No-JS parity: the bar and checkboxes are plain markup, not gated on
	// a JS-added class server-side.
	if !strings.Contains(body, `id="bulk-spam-form"`) || !strings.Contains(body, `data-select-thread`) {
		t.Fatalf("expected the inline bulk bar and checkboxes without JS gating: %s", body)
	}
	if strings.Contains(body, `class="js`) || strings.Contains(body, `"js"`) {
		t.Fatalf("server markup must not hardcode the html.js class: %s", body)
	}
}
