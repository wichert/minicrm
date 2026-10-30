// Package views provides type-safe server-rendered HTML templates and timezone formatting for MiniCRM.
package views

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"mime"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// AccountOption is a mail account selectable or displayable in the UI.
type AccountOption struct {
	ID   string
	Name string
}

// Flash is a one-shot notice rendered at the top of the main content area.
type Flash struct {
	Kind    string
	Message string
}

// ThreadSummaryItem holds display fields for a single row in the inbox list.
type ThreadSummaryItem struct {
	Thread          sqlc.Thread
	AssigneeName    string
	AccountName     string
	FormattedTime   string
	FullTime        string
	OpenedFullTime  string
	RelativeTime    string
	WaitLabel       string
	WaitUrgency     string
	LatestSnippet   string
	LatestDirection string
	MessageCount    int
	CanAssignToMe   bool
	// IsCurrent marks the thread the detail page is showing so the pane row reads as
	// selected (aria-current plus a background in app.css).
	IsCurrent bool
}

// InboxViewData holds data for rendering the threads inbox view.
type InboxViewData struct {
	Title          string
	AppName        string
	AppShortName   string
	Flash          Flash
	ActiveNav      string
	CurrentUser    sqlc.User
	Threads        []ThreadSummaryItem
	Counts         sqlc.ThreadCounts
	StatusFilter   string
	AssigneeFilter string
	Users          []sqlc.User
	DisplayTZ      string
	// ShowAccountUI is true when the current user can access more than one mail account.
	ShowAccountUI bool
	AccountFilter string
	Accounts      []AccountOption
	Search        string
	NextURL       string
	RefreshURL    string
	// PartialURL is the htmx endpoint that returns just the results region for
	// this list (the current URL on /threads, the pane endpoint on the split
	// view). The region's polling contract and the filter selects both use it.
	PartialURL string
	// FilterURL is the htmx endpoint the filter selects call with the form's
	// fields. It is a bare path (no query) because htmx appends the included
	// form fields as query parameters for a GET.
	FilterURL string
	// ShowAssigneeUI is true when more than one team member can be assigned, so the
	// assignee filter is only rendered when it can change the result.
	ShowAssigneeUI bool
	// ClearSearchURL is the current inbox URL with the search term removed.
	ClearSearchURL string
	// WideLayout widens the page container for the thread list; the detail page keeps
	// the narrower reading column.
	WideLayout bool
	// AssignableUsers maps thread UUID (string) to the users who may be assigned
	// that thread: only active users with access to the thread's account. The row
	// menu renders "Assign to…" entries from this server-filtered list; filtering
	// by account access always happens server-side (AGENTS.md invariant 5).
	AssignableUsers map[string][]sqlc.User
	// Sort is the allow-listed sort key of the current list ("newest" or
	// "longest_waiting"); unknown query values collapse to "newest".
	Sort string
	// SortTruncated is true when the longest_waiting sort was capped at the page
	// limit without cursor pagination; the view renders a truncation notice.
	SortTruncated bool
	// TotalCount, when > 0, is the total number of matching threads known to the
	// handler; the pagination footer shows "N Threads · Load older". Zero means
	// the total is unknown and the footer falls back to "More threads".
	TotalCount int
	// PaneLinksBaseURL is the inbox URL whose query string the split pane appends to
	// every row link. It is set only on the detail page's pane (W4) so that opening
	// a thread keeps the queue's filters in the link, and the pane re-renders with
	// the same filters instead of falling back to the default open inbox.
	PaneLinksBaseURL string
	// PaneMode marks the list as the narrow W4 split pane rather than the full inbox
	// page: the pane table drops the bulk-select, status and age columns (the active
	// tab implies status and the age rides in the meta line) and its rows lay out on
	// two lines instead of one.
	PaneMode bool
}

// EmailItemView holds an email, its formatted timestamp, and its attachments.
type EmailItemView struct {
	Email           sqlc.Email
	FormattedSent   string
	FullSent        string
	Snippet         string
	Attachments     []sqlc.Attachment
	HasRemoteImages bool
	IsLatest        bool
}

// EventItemView holds a formatted thread audit event sentence.
type EventItemView struct {
	Event         sqlc.ThreadEvent
	FormattedTime string
	FullTime      string
	Summary       string
	// Section, when set, renders a subheading before this event to distinguish merged history.
	Section string
}

// ThreadDetailViewData holds data for rendering a single conversation thread.
type ThreadDetailViewData struct {
	WideLayout    bool
	Title         string
	AppName       string
	AppShortName  string
	Flash         Flash
	ActiveNav     string
	CurrentUser   sqlc.User
	BackURL       string
	Thread        sqlc.Thread
	AssigneeName  string
	WaitLabel     string
	WaitUrgency   string
	ReopensThread *sqlc.Thread
	// ReopenCompletedAt and ReopenCompletedBy describe when/by whom the referenced thread was completed.
	ReopenCompletedAt string
	ReopenCompletedBy string
	Emails            []EmailItemView
	Events            []EventItemView
	Users             []sqlc.User
	DisplayTZ         string
	// ShowAccountUI is true when the current user can access more than one mail account.
	ShowAccountUI      bool
	AccountName        string
	AccountFromAddress string
	ReplyAllowed       bool
	ReplyDraft         string
	DraftRevision      string
	DraftFormatting    string
	PreviousThreads    []ThreadSummaryItem
	// Errors are transition, assignment, or merge errors rendered in the thread header card.
	Errors []string
	// ReplyErrors are reply-composition errors rendered next to the reply field only.
	ReplyErrors []string
	// List, when non-nil, renders the inbox list pane beside the thread for the
	// W4 split layout on wide viewports (nil with ?nolist=1 or on list failure).
	List *InboxViewData
}

// AccountSettingsView pairs a configured mail account with its current settings.
type AccountSettingsView struct {
	Account AccountOption
	// DefaultAssigneeValue is the currently selected user UUID as a form value ("" for none).
	DefaultAssigneeValue string
}

// NewUserForm holds typed values for the add-team-member form so validation errors do not lose input.
type NewUserForm struct {
	Email           string
	DisplayName     string
	Role            string
	AllowedAccounts []string
}

// AuditEntryView is a human-readable row for the settings activity table.
type AuditEntryView struct {
	FormattedTime string
	FullTime      string
	ActorName     string
	TargetName    string
	Label         string
}

// SettingsViewData holds data for the admin settings and user management page.
type SettingsViewData struct {
	WideLayout      bool
	Title           string
	AppName         string
	AppShortName    string
	Flash           Flash
	ActiveNav       string
	CurrentUser     sqlc.User
	Settings        sqlc.SystemSettings
	Users           []sqlc.User
	AuditLogs       []AuditEntryView
	Accounts        []AccountOption
	AccountSettings []AccountSettingsView
	AccountNames    map[string]string
	NewUser         NewUserForm
	Errors          []string
}

// ErrorViewData holds data for the styled error page.
type ErrorViewData struct {
	WideLayout   bool
	Title        string
	AppName      string
	AppShortName string
	Flash        Flash
	ActiveNav    string
	CurrentUser  sqlc.User
	StatusCode   int
	Heading      string
	Message      string
}

// FormatTimestamp converts t into the configured IANA displayTimezone without repeating
// the timezone suffix or seconds (the active timezone is configured and shown in Settings).
func FormatTimestamp(t time.Time, displayTimezone string) string {
	loc, err := time.LoadLocation(displayTimezone)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04")
}

// FormatDateOnly converts t into the configured IANA displayTimezone and renders
// just the calendar date (no clock time). Compact lists use it where the exact
// time is noise and the relative ordering is already obvious from the order.
func FormatDateOnly(t time.Time, displayTimezone string) string {
	if t.IsZero() {
		return ""
	}
	loc, err := time.LoadLocation(displayTimezone)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02")
}

// FormatTimestampFull converts t into the configured IANA displayTimezone with full seconds
// and timezone abbreviation for hover tooltips.
func FormatTimestampFull(t time.Time, displayTimezone string) string {
	loc, err := time.LoadLocation(displayTimezone)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04:05 MST")
}

// FormatRelativeTime returns a compact relative age string (e.g. "2h ago", "14d ago").
func FormatRelativeTime(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < time.Minute {
		return "Just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1d ago"
	}
	return fmt.Sprintf("%dd ago", days)
}

// FormatWaitDuration computes how long a New thread has been waiting for attention
// and returns a human label plus an urgency tier ("fresh", "warning", "urgent").
func FormatWaitDuration(since, now time.Time) (string, string) {
	if since.IsZero() {
		return "Waiting", "fresh"
	}
	d := now.Sub(since)
	if d < time.Minute {
		return "Waiting <1m", "fresh"
	}
	if d < time.Hour {
		return fmt.Sprintf("Waiting %dm", int(d.Minutes())), "fresh"
	}
	if d < 4*time.Hour {
		return fmt.Sprintf("Waiting %dh", int(d.Hours())), "fresh"
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("Waiting %dh", int(d.Hours())), "warning"
	}
	days := int(d.Hours() / 24)
	return fmt.Sprintf("Waiting %dd", days), "urgent"
}

// ThreadAgeBadge derives the age badge label and urgency tier ("fresh", "warning", "urgent")
// for an open thread (new or in_progress) using its opened_at timestamp and the configured
// IANA display timezone. Calendar-day buckets ("Today", "This week", "Overdue") are computed
// in displayTimezone rather than UTC wall-clock time.
func ThreadAgeBadge(status sqlc.ThreadStatus, openedAt, now time.Time, displayTimezone string) (string, string) {
	if status != sqlc.ThreadStatusNew && status != sqlc.ThreadStatusInProgress {
		return "", ""
	}
	if openedAt.IsZero() {
		// Defensible default: an unknown open time is treated as today so a missing
		// timestamp never inflates a thread's age or mislabels it as overdue.
		return "Today", "fresh"
	}
	loc, err := time.LoadLocation(displayTimezone)
	if err != nil {
		loc = time.UTC
	}
	openedLocal := openedAt.In(loc)
	nowLocal := now.In(loc)
	openedDay := time.Date(openedLocal.Year(), openedLocal.Month(), openedLocal.Day(), 0, 0, 0, 0, time.UTC)
	nowDay := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, time.UTC)
	days := int(nowDay.Sub(openedDay) / (24 * time.Hour))
	switch {
	case days <= 0:
		return "Today", "fresh"
	case days <= 7:
		return fmt.Sprintf("Open %dd", days), "warning"
	default:
		return fmt.Sprintf("Overdue · %dd", days), "urgent"
	}
}

// PaneAgeLabel shortens a wait badge label for the split pane's compact age
// element ("Today", "3d", "12d"). It reuses the exact bucketing of
// ThreadAgeBadge — the buckets are derived from the same label so the two can
// never disagree — and simply strips the words the pane does not have room
// for. Labels that match no bucket (future-proofing) pass through unchanged.
func PaneAgeLabel(waitLabel string) string {
	switch waitLabel {
	case "Today":
		return "Today"
	case "Open 1d", "Open 2d", "Open 3d", "Open 4d", "Open 5d", "Open 6d", "Open 7d":
		return strings.TrimPrefix(waitLabel, "Open ")
	default:
		// "Overdue · Nd" or an unexpected label: keep the day count, drop words.
		if days, ok := strings.CutPrefix(waitLabel, "Overdue · "); ok {
			return days
		}
		return waitLabel
	}
}

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

// ExtractEmailSnippet extracts a clean single-line preview blurb from an email's plain text
// or sanitized HTML fallback.
func ExtractEmailSnippet(bodyText, bodyHTML string, maxRunes int) string {
	raw := strings.TrimSpace(bodyText)
	if raw == "" && strings.TrimSpace(bodyHTML) != "" {
		raw = html.UnescapeString(htmlTagRe.ReplaceAllString(bodyHTML, " "))
	}
	if raw == "" {
		return ""
	}
	// Prefer non-quoted lines when present.
	lines := strings.Split(raw, "\n")
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ">") {
			continue
		}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		kept = lines
	}
	joined := strings.Join(kept, " ")
	collapsed := strings.Join(strings.Fields(joined), " ")
	if maxRunes <= 0 || utf8.RuneCountInString(collapsed) <= maxRunes {
		return collapsed
	}
	runes := []rune(collapsed)
	return strings.TrimSpace(string(runes[:maxRunes])) + "…"
}

// FormatStatus returns the single human-readable label for a workflow status.
func FormatStatus(status sqlc.ThreadStatus) string {
	switch status {
	case sqlc.ThreadStatusNew:
		return "New"
	case sqlc.ThreadStatusInProgress:
		return "In progress"
	case sqlc.ThreadStatusCompleted:
		return "Completed"
	case sqlc.ThreadStatusSpam:
		return "Spam"
	default:
		return strings.TrimSpace(strings.ReplaceAll(string(status), "_", " "))
	}
}

// FormatRole returns the human-readable label for a user role.
func FormatRole(role string) string {
	if role == "admin" {
		return "Admin"
	}
	return "Agent"
}

// FormatAuditEvent returns a human-readable label for a system audit event type.
func FormatAuditEvent(eventType string) string {
	switch eventType {
	case "settings_updated":
		return "Settings updated"
	case "user_precreated":
		return "User pre-created"
	case "user_created":
		return "User created"
	case "user_updated":
		return "User updated"
	case "user_login":
		return "Signed in"
	case "user_logout":
		return "Signed out"
	case "user_login_failed":
		return "Sign-in failed"
	default:
		return humanizeEvent(eventType)
	}
}

// FormatFileSize renders a byte count as a human-readable size.
func FormatFileSize(size int64) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%d B", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
}

// FormatThreadEventSummary turns a raw thread event into a human-readable sentence.
func FormatThreadEventSummary(ev sqlc.ThreadEvent, actorName string, userNames map[uuid.UUID]string) string {
	if strings.TrimSpace(actorName) == "" {
		actorName = "System"
	}
	oldValues := decodeEventValues(ev.OldValue)
	newValues := decodeEventValues(ev.NewValue)
	statusName := func(raw any) string {
		text, _ := raw.(string)
		if text == "" {
			return ""
		}
		return FormatStatus(sqlc.ThreadStatus(text))
	}
	// assigneeTarget returns the assignee name and whether the actor assigned the thread to themselves.
	assigneeTarget := func(raw any) (string, bool) {
		text, _ := raw.(string)
		id, err := uuid.Parse(text)
		if err != nil {
			return "", false
		}
		if ev.ActorUserID != nil && *ev.ActorUserID == id {
			return "", true
		}
		if name, ok := userNames[id]; ok && name != "" {
			return name, false
		}
		return "another operator", false
	}

	switch ev.EventType {
	case "created":
		if origin, _ := newValues["origin"].(string); origin == "outbound" {
			return "A message sent from another mail client opened this thread."
		}
		if actorName == "System" {
			return "The customer opened this thread."
		}
		return fmt.Sprintf("%s opened this thread.", actorName)
	case "status_changed":
		from, to := statusName(oldValues["status"]), statusName(newValues["status"])
		if from != "" && to != "" {
			return fmt.Sprintf("%s changed status from %s to %s.", actorName, from, to)
		}
		return fmt.Sprintf("%s changed the status.", actorName)
	case "assigned", "reassigned":
		name, toSelf := assigneeTarget(newValues["assignee_id"])
		switch {
		case toSelf:
			name = "themselves"
		case name == "":
			name = "another operator"
		}
		switch source, _ := newValues["source"].(string); source {
		case "reply":
			return fmt.Sprintf("%s assigned this thread to %s when replying.", actorName, name)
		case "default":
			return fmt.Sprintf("%s auto-assigned this thread to %s.", actorName, name)
		default:
			return fmt.Sprintf("%s assigned this thread to %s.", actorName, name)
		}
	case "reply_sent":
		return fmt.Sprintf("%s sent a reply.", actorName)
	case "reply_sent_external":
		return "A reply was sent from another mail client."
	case "reply_received":
		return "The customer replied."
	case "reopen_merged":
		return fmt.Sprintf("%s reopened and merged a follow-up reply.", actorName)
	default:
		return fmt.Sprintf("%s: %s.", actorName, humanizeEvent(ev.EventType))
	}
}

func decodeEventValues(raw json.RawMessage) map[string]any {
	values := make(map[string]any)
	if len(raw) == 0 {
		return values
	}
	_ = json.Unmarshal(raw, &values)
	return values
}

func humanizeEvent(eventType string) string {
	text := strings.TrimSpace(strings.ReplaceAll(eventType, "_", " "))
	if text == "" {
		return "Event"
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

func statusBadgeClass(status sqlc.ThreadStatus) string {
	switch status {
	case sqlc.ThreadStatusNew:
		return "badge badge-new"
	case sqlc.ThreadStatusInProgress:
		return "badge badge-in-progress"
	case sqlc.ThreadStatusCompleted:
		return "badge badge-completed"
	case sqlc.ThreadStatusSpam:
		return "badge badge-spam"
	default:
		return "badge"
	}
}

func directionClass(direction string) string {
	switch direction {
	case "inbound":
		return "inbound"
	case "outbound":
		return "outbound"
	default:
		return "other"
	}
}

func directionLabel(direction string) string {
	switch direction {
	case "inbound":
		return "Customer"
	case "outbound":
		return "Team reply"
	default:
		return "Message"
	}
}

func directionBadgeClass(direction string) string {
	switch direction {
	case "inbound":
		return "badge badge-inbound"
	case "outbound":
		return "badge badge-outbound"
	default:
		return "badge"
	}
}

func hasAccount(accounts []string, accountID string) bool {
	for _, id := range accounts {
		if id == accountID {
			return true
		}
	}
	return false
}

func assignmentLabel(source string) string {
	switch source {
	case "reply":
		return "assigned via reply"
	case "default":
		return "auto-assigned"
	case "manual":
		return ""
	default:
		return ""
	}
}

// ClassifyAttachment determines whether an attachment is safe to render inline in the browser
// and returns its canonical MIME type, whether it is an inline-renderable image, and whether
// it is safe to view in a browser tab. Non-allowlisted types (including HTML and SVG) return
// isViewable = false so they are only ever served as downloads.
func ClassifyAttachment(contentType, filename string) (safeContentType string, isImage bool, isViewable bool) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || mediaType == "" {
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	} else {
		mediaType = strings.ToLower(mediaType)
	}
	if mediaType == "" || mediaType == "application/octet-stream" {
		switch strings.ToLower(filepath.Ext(filename)) {
		case ".png":
			mediaType = "image/png"
		case ".jpg", ".jpeg":
			mediaType = "image/jpeg"
		case ".gif":
			mediaType = "image/gif"
		case ".webp":
			mediaType = "image/webp"
		case ".pdf":
			mediaType = "application/pdf"
		case ".txt", ".log", ".csv":
			mediaType = "text/plain"
		}
	}
	switch mediaType {
	case "image/png":
		return "image/png", true, true
	case "image/jpeg", "image/jpg":
		return "image/jpeg", true, true
	case "image/gif":
		return "image/gif", true, true
	case "image/webp":
		return "image/webp", true, true
	case "application/pdf":
		return "application/pdf", false, true
	case "text/plain", "text/csv":
		return "text/plain; charset=utf-8", false, true
	default:
		return "", false, false
	}
}

// IsImageAttachment reports whether att is a safe raster image that can be previewed inline.
func IsImageAttachment(att sqlc.Attachment) bool {
	_, isImage, _ := ClassifyAttachment(att.ContentType, att.Filename)
	return isImage
}

// IsViewableAttachment reports whether att can be safely opened inline in a browser tab.
func IsViewableAttachment(att sqlc.Attachment) bool {
	_, _, isViewable := ClassifyAttachment(att.ContentType, att.Filename)
	return isViewable
}

// HasImageAttachments reports whether atts contains at least one safe inline-previewable image.
func HasImageAttachments(atts []sqlc.Attachment) bool {
	for _, att := range atts {
		if IsImageAttachment(att) {
			return true
		}
	}
	return false
}

// AttachmentKindLabel returns a user-friendly label for an attachment's file type.
func AttachmentKindLabel(att sqlc.Attachment) string {
	safeType, isImage, _ := ClassifyAttachment(att.ContentType, att.Filename)
	if isImage {
		return "Image"
	}
	switch safeType {
	case "application/pdf":
		return "PDF document"
	case "text/plain; charset=utf-8":
		return "Text file"
	default:
		return "File"
	}
}

func attachmentInlineTokens(att sqlc.Attachment) []string {
	var tokens []string
	if att.ContentID != "" {
		tokens = append(tokens, "cid:"+att.ContentID)
	}
	if att.Filename != "" {
		tokens = append(tokens, "cid:"+att.Filename, att.Filename)
	}
	return tokens
}

func resolveAndFilterInlineImages(sanitizedHTML string, atts []sqlc.Attachment) (string, []sqlc.Attachment) {
	if sanitizedHTML == "" || len(atts) == 0 {
		return sanitizedHTML, atts
	}
	out := sanitizedHTML
	listed := make([]sqlc.Attachment, 0, len(atts))

	for _, att := range atts {
		if !IsImageAttachment(att) {
			listed = append(listed, att)
			continue
		}
		viewURL := "/attachments/" + att.ID.String() + "/view"
		used := false
		for _, token := range attachmentInlineTokens(att) {
			doubleAttr := `src="` + token + `"`
			singleAttr := `src='` + token + `'`
			if strings.Contains(out, doubleAttr) || strings.Contains(out, singleAttr) {
				out = strings.ReplaceAll(out, doubleAttr, `src="`+viewURL+`"`)
				out = strings.ReplaceAll(out, singleAttr, `src='`+viewURL+`'`)
				used = true
			}
		}
		if !used {
			listed = append(listed, att)
		}
	}
	return out, listed
}

// FilterListedAttachments returns only the attachments that should be shown in the message's
// attachment list, omitting image attachments that are already rendered inline via an <img>
// element in sanitizedHTML.
func FilterListedAttachments(sanitizedHTML string, atts []sqlc.Attachment) []sqlc.Attachment {
	_, listed := resolveAndFilterInlineImages(sanitizedHTML, atts)
	return listed
}

// ResolveInlineCIDImages rewrites cid:<ContentID> (or inline filename) image sources in
// sanitizedHTML to point at /attachments/<id>/view for matching image attachments belonging
// to the same email.
func ResolveInlineCIDImages(sanitizedHTML string, atts []sqlc.Attachment) string {
	resolved, _ := resolveAndFilterInlineImages(sanitizedHTML, atts)
	return resolved
}

var templateFuncs = template.FuncMap{
	"statusLabel":       FormatStatus,
	"statusBadgeClass":  statusBadgeClass,
	"directionLabel":    directionLabel,
	"directionClass":    directionClass,
	"directionBadge":    directionBadgeClass,
	"formatSize":        FormatFileSize,
	"assignmentLabel":   assignmentLabel,
	"roleLabel":         FormatRole,
	"auditEventLabel":   FormatAuditEvent,
	"hasAccount":        hasAccount,
	"isImageAttachment": IsImageAttachment,

	"isViewableAttachment": IsViewableAttachment,
	"hasImageAttachments":  HasImageAttachments,
	"attachmentKind":       AttachmentKindLabel,
	"paneAgeLabel":         PaneAgeLabel,
	"threadDetailURL":      ThreadDetailURL,
	"inboxFilterForm":      inboxFilterFormArgs,
}

// ThreadDetailURL builds a thread detail link from a base inbox URL (whose query
// string carries the pane's filters) and the thread id. The thread is always
// addressed by path — /threads/{id} is the only route that renders the detail
// (GET /threads/{id} -> HandleViewThread); addressing it as /threads?thread={id}
// matched GET /threads instead, which re-rendered the list and made pane rows
// dead links. With an empty base the link is the bare /threads/{id}; otherwise
// the base's query string (never its path) is carried along, so opening a thread
// from a filtered queue keeps the queue's filters.
func ThreadDetailURL(base string, threadID fmt.Stringer) string {
	link := "/threads/" + threadID.String()
	query := ""
	if i := strings.IndexByte(base, '?'); i >= 0 {
		query = base[i+1:]
	} else if base != "" {
		// Base without a query (a bare "/threads"): nothing to carry.
		return link
	}
	if query == "" {
		return link
	}
	return link + "?" + query
}

// inboxFilterFormData parameterizes the shared inbox filter form partial: the
// three placements (full inbox, split top bar, split pane) render the same
// controls but target different URLs. Data holds the list's filter values and
// visibility flags; ClearSearchURL is the placement-specific "clear search"
// link (on the split page both bars clear back to the thread path).
type inboxFilterFormData struct {
	Data           InboxViewData
	Action         string
	ClearSearchURL string
}

// inboxFilterFormArgs is a template helper that adapts the list data (passed as
// a value on the inbox page, a pointer inside ThreadDetailViewData.List) plus
// the placement's form action and clear-search URL into the shape the shared
// inbox_filter_form partial renders.
func inboxFilterFormArgs(data any, action, clearSearchURL string) inboxFilterFormData {
	var listData InboxViewData
	switch v := data.(type) {
	case InboxViewData:
		listData = v
	case *InboxViewData:
		if v != nil {
			listData = *v
		}
	}
	return inboxFilterFormData{Data: listData, Action: action, ClearSearchURL: clearSearchURL}
}

// The template sets below concatenate the same files in the same order the
// former string constants did, so rendered output is byte-identical. Markup lives
// in templates/*.html (embedded by templates.go) so it gets editor support back.
var (
	inboxFullTmpl = mustTemplates("inbox", "inbox_filter_form.html", "inbox_table.html", "inbox_tabs.html", "inbox_results.html", "layout_header.html", "inbox_body.html", "layout_footer.html")
	inboxPartTmpl = mustTemplates("inbox_partial", "inbox_filter_form.html", "inbox_table.html", "inbox_tabs.html", "inbox_results.html")
	detailTmpl    = mustTemplates("detail", "inbox_filter_form.html", "inbox_table.html", "inbox_tabs.html", "inbox_results.html", "layout_header.html", "thread_detail.html", "layout_footer.html")
	settingsTmpl  = mustTemplates("settings", "layout_header.html", "settings_audit_table.html", "settings_body.html", "layout_footer.html")
	activityTmpl  = mustTemplates("activity", "layout_header.html", "settings_audit_table.html", "activity_body.html", "layout_footer.html")
	errorTmpl     = mustTemplates("error", "layout_header.html", "error_body.html", "layout_footer.html")
	emailBodyTmpl = mustTemplates("email_body", "email_body.html")
)

// RenderInbox renders the full inbox page.
func RenderInbox(w io.Writer, data InboxViewData) error {
	return inboxFullTmpl.Execute(w, data)
}

// RenderInboxResults renders the htmx fragment for an in-place list swap: the
// contents of the results region (bulk bar unless the list is the split pane,
// the table, and the pagination footer), followed by out-of-band updates for the
// results count and the status tab strip so the pills stay current without
// re-rendering the toolbar. It is what GET /threads and GET /threads/{id}/list
// return when the request carries the HX-Request header.
func RenderInboxResults(w io.Writer, data InboxViewData) error {
	return inboxPartTmpl.ExecuteTemplate(w, "inbox_results_fragment", data)
}

// RenderThreadDetail renders the thread detail view including sandboxed email iframes and reopen banner.
func RenderThreadDetail(w io.Writer, data ThreadDetailViewData) error {
	return detailTmpl.Execute(w, data)
}

// RenderSettings renders the admin settings and user management view.
func RenderSettings(w io.Writer, data SettingsViewData) error {
	return settingsTmpl.Execute(w, data)
}

// ActivityViewData holds data for the admin full-activity page.
type ActivityViewData struct {
	WideLayout   bool
	Title        string
	AppName      string
	AppShortName string
	Flash        Flash
	ActiveNav    string
	CurrentUser  sqlc.User
	AuditLogs    []AuditEntryView
	// Page is the 1-based page number being shown.
	Page int
	// HasPrev/HasNext drive the pagination controls.
	HasPrev bool
	HasNext bool
	// PagePrev/PageNext are the target page numbers for the links.
	PagePrev int
	PageNext int
}

// RenderActivity renders the admin full-activity page.
func RenderActivity(w io.Writer, data ActivityViewData) error {
	return activityTmpl.Execute(w, data)
}

// RenderError renders the styled error page.
func RenderError(w io.Writer, data ErrorViewData) error {
	return errorTmpl.Execute(w, data)
}

// IsolatedEmailBodyData holds the sanitized HTML or plain text for the isolated iframe endpoint.
type IsolatedEmailBodyData struct {
	EmailID       uuid.UUID
	SanitizedHTML template.HTML
	PlainText     string
	HasHTML       bool
}

// RenderIsolatedEmailBody renders a single email's content for the sandboxed iframe endpoint.
func RenderIsolatedEmailBody(w io.Writer, email sqlc.Email) error {
	data := IsolatedEmailBodyData{
		EmailID:   email.ID,
		PlainText: email.BodyText,
		HasHTML:   email.BodyHTMLSanitized != "",
		// #nosec G203 -- BodyHTMLSanitized is sanitized via strict bluemonday allowlist policy and served inside a sandboxed iframe with CSP script-src 'none'
		SanitizedHTML: template.HTML(email.BodyHTMLSanitized),
	}
	return emailBodyTmpl.Execute(w, data)
}
