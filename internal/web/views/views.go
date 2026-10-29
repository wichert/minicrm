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

var (
	inboxFullTmpl = template.Must(template.New("inbox").Funcs(templateFuncs).Parse(inboxFilterForm + inboxTablePartial + layoutHeader + inboxBody + layoutFooter))
	inboxPartTmpl = template.Must(template.New("inbox_partial").Funcs(templateFuncs).Parse(inboxFilterForm + inboxTablePartial))
	detailTmpl    = template.Must(template.New("detail").Funcs(templateFuncs).Parse(inboxFilterForm + inboxTablePartial + layoutHeader + threadDetailBody + layoutFooter))
	settingsTmpl  = template.Must(template.New("settings").Funcs(templateFuncs).Parse(layoutHeader + settingsAuditTablePartial + settingsBody + layoutFooter))
	activityTmpl  = template.Must(template.New("activity").Funcs(templateFuncs).Parse(layoutHeader + settingsAuditTablePartial + activityBody + layoutFooter))
	errorTmpl     = template.Must(template.New("error").Funcs(templateFuncs).Parse(layoutHeader + errorBody + layoutFooter))
	emailBodyTmpl = template.Must(template.New("email_body").Funcs(templateFuncs).Parse(isolatedEmailBodyHTML))
)

// RenderInbox renders either the full inbox page or the partial thread table.
func RenderInbox(w io.Writer, data InboxViewData, partialOnly bool) error {
	if partialOnly {
		return inboxPartTmpl.ExecuteTemplate(w, "inbox_table", data)
	}
	return inboxFullTmpl.Execute(w, data)
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

const layoutHeader = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
  <meta name="color-scheme" content="light">
  <meta name="theme-color" content="#14142b">
  <meta name="apple-mobile-web-app-capable" content="yes">
  <meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
  <meta name="apple-mobile-web-app-title" content="{{if .AppShortName}}{{.AppShortName}}{{else if .AppName}}{{.AppName}}{{else}}MiniCRM{{end}}">
  <meta name="referrer" content="no-referrer">
  <title>{{if .Title}}{{.Title}} — {{end}}{{if .AppName}}{{.AppName}}{{else}}MiniCRM{{end}}</title>
  <link rel="icon" type="image/svg+xml" href="/static/favicon.svg">
  <link rel="apple-touch-icon" href="/static/apple-touch-icon.png">
  <link rel="manifest" href="/manifest.webmanifest">
  <link rel="stylesheet" href="/static/app.css">
  <script src="/static/editor.js" defer></script>
  <script src="/static/app.js" defer></script>
</head>
<body>
  <a class="skip-link" href="#main-content">Skip to content</a>
  <header>
    <nav class="navbar" aria-label="Main">
      <a class="navbar-brand" href="/threads"><svg class="brand-mark" viewBox="0 0 32 32" width="24" height="24" aria-hidden="true" focusable="false"><rect width="32" height="32" rx="8" fill="#6366f1"/><path d="M7 11.5h18v10a2 2 0 0 1-2 2H9a2 2 0 0 1-2-2z" fill="none" stroke="#fff" stroke-width="2" stroke-linejoin="round"/><path d="m7.5 12 8.5 6.5L24.5 12" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg><span>{{if .AppShortName}}{{.AppShortName}}{{else if .AppName}}{{.AppName}}{{else}}MiniCRM{{end}}</span></a>
      <ul class="nav-links">
        <li><a href="/threads"{{if eq .ActiveNav "threads"}} aria-current="page"{{end}}>Threads</a></li>
        {{if eq .CurrentUser.Role "admin"}}<li><a href="/settings"{{if eq .ActiveNav "settings"}} aria-current="page"{{end}}>Settings</a></li>{{end}}
      </ul>
      <div class="navbar-user">
        <span class="navbar-user-name">{{.CurrentUser.DisplayName}}</span>
        <span class="navbar-user-email">{{.CurrentUser.Email}}</span>
        <form method="post" action="/auth/logout">
          <button type="submit" class="btn btn-ghost btn-small">Log out</button>
        </form>
      </div>
    </nav>
  </header>
  <main id="main-content" class="container{{if .WideLayout}} container-wide{{end}}" tabindex="-1">
    {{if .Flash.Message}}<div class="flash flash-{{.Flash.Kind}}" role="status">
      <span>{{.Flash.Message}}</span>
      <button type="button" class="flash-dismiss" data-flash-dismiss aria-label="Dismiss notice">×</button>
    </div>{{end}}
`

const layoutFooter = `
  </main>
  <dialog id="confirm-dialog" class="confirm-dialog" aria-labelledby="confirm-dialog-title" aria-describedby="confirm-dialog-message">
    <form method="dialog">
      <h2 id="confirm-dialog-title">Confirm action</h2>
      <p id="confirm-dialog-message" data-confirm-message></p>
      <div class="dialog-actions">
        <button type="button" class="btn btn-primary" data-confirm-cancel>Cancel</button>
        <button type="submit" class="btn btn-danger" data-confirm-accept>Confirm</button>
      </div>
    </form>
  </dialog>
</body>
</html>
`

const inboxTablePartial = `{{define "inbox_table"}}
{{if .Threads}}
<div class="table-wrap">
  {{/* PaneMode (W4 split list) drops the bulk-select, status, age and actions
       columns: the active tab already states the status, the age rides at the
       end of the pane row, and acting on a thread belongs to the detail page or
       the full inbox. The narrow pane spends its whole width on the subject,
       customer and preamble. */}}
  <table class="threads-table{{if .PaneMode}} threads-table-pane{{end}}">
    <thead>
      <tr>
        {{if and (not .PaneMode) (ne .StatusFilter "completed") (ne .StatusFilter "spam")}}<th scope="col" class="select-cell"><label class="select-label"><input type="checkbox" data-select-all aria-label="Select all on page"></label></th>{{end}}
        <th scope="col">Thread</th>
        {{if not .PaneMode}}<th scope="col">Status</th>
        <th scope="col">Age</th>{{end}}
        {{if and (not .PaneMode) (ne .StatusFilter "completed") (ne .StatusFilter "spam")}}<th scope="col" class="actions-cell"><span class="sr-only">Actions</span></th>{{end}}
      </tr>
    </thead>
    <tbody>
      {{$paneBase := .PaneLinksBaseURL}}{{$refreshURL := .RefreshURL}}{{$assignable := .AssignableUsers}}{{range .Threads}}{{$row := .}}{{$rowUsers := index $assignable .Thread.ID.String}}
      {{/* The open thread reads as selected in the pane: aria-current for assistive
           tech plus a background/border-left in app.css. */}}
      <tr class="thread-row{{if eq .Thread.Status "new"}} thread-row-new{{end}}{{if and $.PaneMode .IsCurrent}} thread-row-current{{end}}" data-row-href="{{threadDetailURL $paneBase .Thread.ID}}"{{if and $.PaneMode .IsCurrent}} aria-current="true"{{end}}>
        {{if and (not $.PaneMode) (ne $.StatusFilter "completed") (ne $.StatusFilter "spam")}}<td class="select-cell"><label class="select-label"><input type="checkbox" name="thread_id" value="{{.Thread.ID}}" form="bulk-spam-form" data-select-thread data-status="{{.Thread.Status}}" data-account="{{.AccountName}}" aria-label="Select {{.Thread.SubjectDisplay}}"></label></td>{{end}}
        <td class="thread-cell">
          {{if $.PaneMode}}{{/* W-rows pane row: row 1 is the subject (a link, up to
              two lines) beside the customer, row 2 is the message preamble on its
              own full-width line with the compact age at its trailing edge.
              Assignee, account and the absolute date are dropped from the pane —
              they live in the detail header, and the tab strip + filters already
              carry status/account. The age keeps its full text in its title attribute.
              No-JS story: this is plain HTML, no behaviour depends on it. */}}
          <div class="pane-row">
            <div class="pane-row-top">
              <a class="thread-link" href="{{threadDetailURL $paneBase .Thread.ID}}" title="{{.Thread.SubjectDisplay}}">{{.Thread.SubjectDisplay}}</a>{{if .Thread.HasDraft}} <span class="badge">Draft</span>{{end}}{{if gt .MessageCount 1}} <span class="count-pill" title="{{.MessageCount}} messages">{{.MessageCount}}</span>{{end}}
              <span class="pane-customer" title="{{if .Thread.CustomerEmail}}{{.Thread.CustomerEmail}}{{end}}">{{if .Thread.CustomerName}}{{.Thread.CustomerName}}{{else if .Thread.CustomerEmail}}{{.Thread.CustomerEmail}}{{else}}Unknown customer{{end}}</span>
            </div>
            <p class="pane-row-snippet">{{if .LatestSnippet}}{{if eq .LatestDirection "outbound"}}<span class="snippet-you">You:</span> {{end}}{{.LatestSnippet}}{{else}}<em>(No message text)</em>{{end}}{{if .WaitLabel}}<span class="wait-badge wait-{{.WaitUrgency}} pane-age" title="Opened {{.OpenedFullTime}}">{{paneAgeLabel .WaitLabel}}</span>{{else}}<time class="pane-age" title="{{.FullTime}}">{{.RelativeTime}}</time>{{end}}</p>
          </div>
          {{else}}
          <div class="thread-rowline">
            <span class="thread-line">
              <a class="thread-link" href="{{threadDetailURL $paneBase .Thread.ID}}" title="{{.Thread.SubjectDisplay}}">{{.Thread.SubjectDisplay}}</a>{{if .Thread.HasDraft}} <span class="badge">Draft</span>{{end}}{{if gt .MessageCount 1}} <span class="count-pill" title="{{.MessageCount}} messages">{{.MessageCount}}</span>{{end}}
              <span class="thread-snippet">{{if .LatestSnippet}}{{if eq .LatestDirection "outbound"}}<span class="snippet-you">You:</span> {{end}}{{.LatestSnippet}}{{else}}<em>(No message text)</em>{{end}}</span>
            </span>
            <span class="thread-meta" title="{{if .Thread.CustomerEmail}}{{.Thread.CustomerEmail}}{{end}}">{{if .Thread.CustomerName}}{{.Thread.CustomerName}}{{else if .Thread.CustomerEmail}}{{.Thread.CustomerEmail}}{{else}}Unknown customer{{end}}{{if $.ShowAccountUI}} &middot; {{.AccountName}}{{end}} &middot; {{if .AssigneeName}}{{.AssigneeName}}{{else}}Unassigned{{end}}{{if or (eq $.StatusFilter "completed") (eq $.StatusFilter "spam")}}<time title="{{.FullTime}}">{{.FormattedTime}}</time>{{else}}<time title="{{.FullTime}}">{{.RelativeTime}}</time>{{end}}</span>
          </div>
          {{end}}
        </td>
        {{if not $.PaneMode}}<td class="status-cell">
          <span class="{{statusBadgeClass .Thread.Status}}">{{statusLabel .Thread.Status}}</span>
        </td>
        <td class="nowrap age-cell">
          {{if .WaitLabel}}<span class="wait-badge wait-{{.WaitUrgency}}" title="Opened {{.OpenedFullTime}}">{{.WaitLabel}}</span>
          {{else if or (eq $.StatusFilter "completed") (eq $.StatusFilter "spam")}}<time title="{{.FullTime}}">{{.FormattedTime}}</time>
          {{else}}<time title="{{.FullTime}}">{{.RelativeTime}}</time>{{end}}
        </td>{{end}}
        {{/* W-rows follow-up: the split pane is a reading queue, not an action
             surface. Row menus and hover quick actions stay on the full inbox
             (/threads) and on the detail page, which is one click away. */}}
        {{if and (not $.PaneMode) (ne $.StatusFilter "completed") (ne $.StatusFilter "spam")}}
        <td class="actions-cell">
          {{if or (eq .Thread.Status "new") (eq .Thread.Status "in_progress")}}
          <details class="row-menu" data-row-menu>
            <summary class="row-menu-trigger" aria-label="Actions for {{.Thread.SubjectDisplay}}" title="Actions">
              <span aria-hidden="true">&#8943;</span>
            </summary>
            <div class="row-menu-popover">
              <button type="button" class="row-menu-item" data-row-select aria-label="Select {{.Thread.SubjectDisplay}} for bulk actions">Select</button>
              {{if eq .Thread.Status "new"}}
              <form method="post" action="/threads/{{.Thread.ID}}/transition" class="row-menu-form">
                <input type="hidden" name="status" value="in_progress">
                <input type="hidden" name="return_to" value="{{$.RefreshURL}}">
                <button type="submit" class="row-menu-item" aria-label="Start working on {{.Thread.SubjectDisplay}}">Start working</button>
              </form>
              {{end}}
              {{if .CanAssignToMe}}<form method="post" action="/threads/{{.Thread.ID}}/assign" class="row-menu-form">
                <input type="hidden" name="assignee_id" value="{{$.CurrentUser.ID}}">
                <input type="hidden" name="return_to" value="{{$.RefreshURL}}">
                <button type="submit" class="row-menu-item" aria-label="Assign {{.Thread.SubjectDisplay}} to me">Assign to me</button>
              </form>{{end}}
              {{if $rowUsers}}
              <details class="row-menu-submenu">
                <summary class="row-menu-item" aria-label="Assign {{.Thread.SubjectDisplay}} to a team member">Assign to&hellip;</summary>
                <div class="row-menu-popover row-menu-submenu-popover">
                  {{range $rowUsers}}
                  <form method="post" action="/threads/{{$row.Thread.ID}}/assign" class="row-menu-form">
                    <input type="hidden" name="assignee_id" value="{{.ID}}">
                    <input type="hidden" name="return_to" value="{{$refreshURL}}">
                    <button type="submit" class="row-menu-item" aria-label="Assign {{$row.Thread.SubjectDisplay}} to {{.DisplayName}}">{{.DisplayName}}</button>
                  </form>
                  {{end}}
                </div>
              </details>
              {{end}}
            </div>
          </details>
          {{if or (eq .Thread.Status "new") (eq .Thread.Status "in_progress")}}
          <!-- W7 spec: hover quick actions live directly in the actions cell,
               outside the ⋯ popover, so they work without opening the menu.
               Each form is self-contained (own action + hidden inputs). -->
          <div class="row-quick-actions" aria-label="Quick actions">
            <form method="post" action="/threads/{{.Thread.ID}}/transition" class="row-menu-form" data-confirm="Mark this conversation as completed and move its messages to Archive?" data-confirm-action="Complete thread">
              <input type="hidden" name="status" value="completed">
              {{if eq .Thread.Status "new"}}<input type="hidden" name="complete_from_new" value="1">{{end}}
              <input type="hidden" name="return_to" value="{{$.RefreshURL}}">
              <button type="submit" class="row-quick-btn row-quick-complete" aria-label="Complete {{.Thread.SubjectDisplay}}" title="Complete"><span aria-hidden="true">&#10003;</span></button>
            </form>
            <form method="post" action="/threads/{{.Thread.ID}}/transition" class="row-menu-form" data-confirm="Mark this conversation as spam and move its messages to Junk?" data-confirm-action="Mark as spam">
              <input type="hidden" name="status" value="spam">
              <input type="hidden" name="return_to" value="{{$.RefreshURL}}">
              <button type="submit" class="row-quick-btn row-quick-spam" aria-label="Mark {{.Thread.SubjectDisplay}} as spam" title="Mark as spam"><span aria-hidden="true">&#10005;</span></button>
            </form>
          </div>
          {{end}}
          {{end}}
        </td>
        {{end}}
      </tr>
      {{end}}
    </tbody>
  </table>
</div>
{{else}}
<p class="card empty-state">No threads match these filters.</p>
{{end}}
{{end}}
`

// inboxFilterForm is the shared filter bar used by all three placements: the
// full inbox page, the split view's top bar (data-split-filters) and the split
// pane's sticky bar. Action selects where the plain GET form submits (no-JS
// path): the full inbox posts to /threads, the split bars post to the open
// thread's path so applying a filter re-renders the split view in place.
const inboxFilterForm = `{{define "inbox_filter_form"}}
<form method="get" action="{{.Action}}" class="filter-inline">
	<input type="hidden" name="status" value="{{.Data.StatusFilter}}">
	<label class="inline-field search-field">
		<span class="search-icon" aria-hidden="true">
			<svg viewBox="0 0 16 16" width="14" height="14" focusable="false"><circle cx="7" cy="7" r="4.5" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="m10.5 10.5 3 3" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>
		</span>
		<span class="sr-only">Customer or subject</span>
		<input type="search" name="q" value="{{.Data.Search}}" placeholder="Search customer or subject" aria-label="Customer or subject">
		{{if .Data.Search}}<a class="search-clear" href="{{.ClearSearchURL}}" aria-label="Clear search">&times;</a>{{end}}
	</label>
	{{if .Data.ShowAssigneeUI}}
	<label class="inline-field inline-field-compact">
		<span class="sr-only">Assignee</span>
		<select name="assignee" data-hx-get="{{.Action}}" aria-label="Assignee">
			<option value="" {{if eq .Data.AssigneeFilter ""}}selected{{end}}>Anyone</option>
			<option value="me" {{if eq .Data.AssigneeFilter "me"}}selected{{end}}>Assigned to me</option>
			<option value="unassigned" {{if eq .Data.AssigneeFilter "unassigned"}}selected{{end}}>Unassigned</option>
			{{range .Data.Users}}
			<option value="{{.ID}}" {{if eq .ID.String $.Data.AssigneeFilter}}selected{{end}}>{{.DisplayName}}</option>
			{{end}}
		</select>
	</label>
	{{end}}
	{{if .Data.ShowAccountUI}}
	<label class="inline-field inline-field-compact">
		<span class="sr-only">Account</span>
		<select name="account" data-hx-get="{{.Action}}" aria-label="Account">
			<option value="">All accounts</option>
			{{range .Data.Accounts}}
			<option value="{{.ID}}" {{if eq .ID $.Data.AccountFilter}}selected{{end}}>{{.Name}}</option>
			{{end}}
		</select>
	</label>
	{{end}}
	<label class="inline-field inline-field-compact">
		<span class="sr-only">Sort</span>
		<select name="sort" data-hx-get="{{.Action}}" aria-label="Sort threads">
			<option value="newest" {{if ne .Data.Sort "longest_waiting"}}selected{{end}}>Newest</option>
			<option value="longest_waiting" {{if eq .Data.Sort "longest_waiting"}}selected{{end}}>Longest waiting</option>
		</select>
	</label>
	<noscript><button type="submit" class="btn btn-secondary btn-small">Filter</button></noscript>
</form>
{{end}}`

const inboxBody = `
<h1 class="sr-only">Threads</h1>
<div class="inbox-toolbar" data-inbox-toolbar>
  <div class="tab-strip" data-tab-strip>
  <nav class="tabs" aria-label="Thread status">
    <a class="tab" href="/threads?status=attention&amp;assignee={{.AssigneeFilter}}&amp;account={{.AccountFilter}}&amp;q={{.Search}}"{{if eq .StatusFilter "attention"}} aria-current="true"{{end}}>Needs attention <span class="count-pill">{{.Counts.NeedsAttention}}</span></a>
    <a class="tab" href="/threads?status=new&amp;assignee={{.AssigneeFilter}}&amp;account={{.AccountFilter}}&amp;q={{.Search}}"{{if eq .StatusFilter "new"}} aria-current="true"{{end}}>New <span class="count-pill">{{.Counts.New}}</span></a>
    <a class="tab" href="/threads?status=open&amp;assignee=me&amp;account={{.AccountFilter}}&amp;q={{.Search}}"{{if and (eq .StatusFilter "open") (eq .AssigneeFilter "me")}} aria-current="true"{{end}}>Mine <span class="count-pill">{{.Counts.Mine}}</span></a>
    <details class="row-menu tab-more" data-row-menu{{if or (eq .StatusFilter "in_progress") (eq .StatusFilter "completed") (eq .StatusFilter "spam") (eq .StatusFilter "all")}} data-active-more="true"{{end}}>
      <summary class="tab tab-more-trigger">More{{if eq .StatusFilter "in_progress"}}: In progress <span class="count-pill">{{.Counts.InProgress}}</span>{{else if eq .StatusFilter "completed"}}: Completed <span class="count-pill">{{.Counts.Completed}}</span>{{else if eq .StatusFilter "spam"}}: Spam <span class="count-pill">{{.Counts.Spam}}</span>{{else if eq .StatusFilter "all"}}: All <span class="count-pill">{{.Counts.All}}</span>{{end}}</summary>
      <div class="row-menu-popover tab-more-popover">
        <a class="row-menu-item tab-more-item" href="/threads?status=in_progress&amp;assignee={{.AssigneeFilter}}&amp;account={{.AccountFilter}}&amp;q={{.Search}}"{{if eq .StatusFilter "in_progress"}} aria-current="true"{{end}}><span>In progress</span> <span class="count-pill">{{.Counts.InProgress}}</span></a>
        <a class="row-menu-item tab-more-item" href="/threads?status=completed&amp;assignee={{.AssigneeFilter}}&amp;account={{.AccountFilter}}&amp;q={{.Search}}"{{if eq .StatusFilter "completed"}} aria-current="true"{{end}}><span>Completed</span> <span class="count-pill">{{.Counts.Completed}}</span></a>
        <a class="row-menu-item tab-more-item" href="/threads?status=spam&amp;assignee={{.AssigneeFilter}}&amp;account={{.AccountFilter}}&amp;q={{.Search}}"{{if eq .StatusFilter "spam"}} aria-current="true"{{end}}><span>Spam</span> <span class="count-pill">{{.Counts.Spam}}</span></a>
        <a class="row-menu-item tab-more-item" href="/threads?status=all&amp;assignee={{.AssigneeFilter}}&amp;account={{.AccountFilter}}&amp;q={{.Search}}"{{if eq .StatusFilter "all"}} aria-current="true"{{end}}><span>All</span> <span class="count-pill">{{.Counts.All}}</span></a>
      </div>
    </details>
  </nav>
  </div>
  <div class="results-status">
    <p class="results-count">{{len .Threads}} thread{{if ne (len .Threads) 1}}s{{end}}</p>
    <a class="refresh-meta" href="{{.RefreshURL}}" data-refresh-link title="Refresh the list"><span data-refresh-status aria-live="polite">Refresh</span></a>
  </div>
</div>
<span class="pull-indicator" data-pull-indicator aria-hidden="true"></span>
<div class="filter-sticky">
  {{template "inbox_filter_form" (inboxFilterForm . "/threads" .ClearSearchURL)}}
</div>
<div data-inbox-results>
{{if and .Threads (ne .StatusFilter "completed") (ne .StatusFilter "spam")}}
<form id="bulk-spam-form" method="post" action="/threads/bulk-spam" class="bulk-actions" data-has-selection="false" data-confirm="Mark selected conversations as spam and queue moves to Junk?" data-confirm-action="Mark as spam">
  <input type="hidden" name="return_to" value="{{.RefreshURL}}">
  <label class="mobile-select-all"><input type="checkbox" data-select-all-mobile> Select all on page</label>
  <span data-selected-count aria-live="polite"></span>
  <div class="bulk-actions-controls">
    <label class="inline-field">Assign selected to
      <select name="assignee_id" aria-label="Assignee for selected conversations" data-bulk-assignee disabled aria-disabled="true">
        <option value="">Choose assignee</option>
        {{range .Users}}<option value="{{.ID}}">{{.DisplayName}}</option>{{end}}
      </select>
    </label>
    <button type="submit" formaction="/threads/bulk-assign" class="btn btn-secondary btn-small" data-bulk-action="assign" disabled aria-disabled="true">Assign selected</button>
    <button type="submit" formaction="/threads/bulk-complete" class="btn btn-secondary btn-small" data-bulk-action="complete" data-confirm="Mark selected conversations as completed and queue moves to Archive?" data-confirm-action="Complete selected" data-confirm-submit disabled aria-disabled="true">Complete selected</button>
    <button type="submit" class="btn btn-danger btn-small" data-bulk-action="spam" data-confirm-submit disabled aria-disabled="true">Mark selected as spam</button>
    <button type="button" class="btn btn-secondary btn-small" data-bulk-clear>Clear</button>
  </div>
</form>
{{end}}
{{template "inbox_table" .}}
{{if .SortTruncated}}<p class="list-truncated">Longest waiting shows at most 100 threads. Use search or filters to narrow the queue.</p>{{end}}
{{if .NextURL}}<p class="list-footer"><span class="list-footer-count">{{if .TotalCount}}{{.TotalCount}} threads{{else}}More threads{{end}}</span> <a class="btn btn-secondary" href="{{.NextURL}}">Load older</a></p>{{end}}
</div>
`

const threadDetailBody = `
{{if .List}}
{{/* W4: the filter row lives once, above the whole split layout, instead of inside
     the clamp(360px..520px) list pane where four controls wrapped onto three lines.
     It is shown only at >=1280px (app.css .split-filters): below that the pane is
     hidden anyway and the standalone detail page keeps its today's chrome. The form
     posts back to the open thread's own path, so applying a filter re-renders the
     pane without leaving the conversation — no-JS included. */}}
<div class="split-filters" data-split-filters>
  {{template "inbox_filter_form" (inboxFilterForm .List (threadDetailURL "" .Thread.ID) .List.ClearSearchURL)}}
</div>
{{end}}
<div class="split-layout{{if not .List}} split-single{{end}}" data-split-layout>
{{if .List}}
  <aside class="split-list" data-split-list id="split-list" aria-label="Thread list">
  <h2 class="sr-only">Threads</h2>
  <div class="inbox-toolbar" data-inbox-toolbar>
    <div class="tab-strip" data-tab-strip>
    <nav class="tabs" aria-label="Thread status">
      <a class="tab" href="/threads?status=attention&assignee={{.List.AssigneeFilter}}&account={{.List.AccountFilter}}&q={{.List.Search}}"{{if eq .List.StatusFilter "attention"}} aria-current="true"{{end}}>Needs attention <span class="count-pill">{{.List.Counts.NeedsAttention}}</span></a>
      <a class="tab" href="/threads?status=new&assignee={{.List.AssigneeFilter}}&account={{.List.AccountFilter}}&q={{.List.Search}}"{{if eq .List.StatusFilter "new"}} aria-current="true"{{end}}>New <span class="count-pill">{{.List.Counts.New}}</span></a>
      <a class="tab" href="/threads?status=open&assignee=me&account={{.List.AccountFilter}}&q={{.List.Search}}"{{if and (eq .List.StatusFilter "open") (eq .List.AssigneeFilter "me")}} aria-current="true"{{end}}>Mine <span class="count-pill">{{.List.Counts.Mine}}</span></a>
      <details class="row-menu tab-more" data-row-menu{{if or (eq .List.StatusFilter "in_progress") (eq .List.StatusFilter "completed") (eq .List.StatusFilter "spam") (eq .List.StatusFilter "all")}} data-active-more="true"{{end}}>
        <summary class="tab tab-more-trigger">More{{if eq .List.StatusFilter "in_progress"}}: In progress <span class="count-pill">{{.List.Counts.InProgress}}</span>{{else if eq .List.StatusFilter "completed"}}: Completed <span class="count-pill">{{.List.Counts.Completed}}</span>{{else if eq .List.StatusFilter "spam"}}: Spam <span class="count-pill">{{.List.Counts.Spam}}</span>{{else if eq .List.StatusFilter "all"}}: All <span class="count-pill">{{.List.Counts.All}}</span>{{end}}</summary>
        <div class="row-menu-popover tab-more-popover">
          <a class="row-menu-item tab-more-item" href="/threads?status=in_progress&assignee={{.List.AssigneeFilter}}&account={{.List.AccountFilter}}&q={{.List.Search}}"{{if eq .List.StatusFilter "in_progress"}} aria-current="true"{{end}}><span>In progress</span> <span class="count-pill">{{.List.Counts.InProgress}}</span></a>
          <a class="row-menu-item tab-more-item" href="/threads?status=completed&assignee={{.List.AssigneeFilter}}&account={{.List.AccountFilter}}&q={{.List.Search}}"{{if eq .List.StatusFilter "completed"}} aria-current="true"{{end}}><span>Completed</span> <span class="count-pill">{{.List.Counts.Completed}}</span></a>
          <a class="row-menu-item tab-more-item" href="/threads?status=spam&assignee={{.List.AssigneeFilter}}&account={{.List.AccountFilter}}&q={{.List.Search}}"{{if eq .List.StatusFilter "spam"}} aria-current="true"{{end}}><span>Spam</span> <span class="count-pill">{{.List.Counts.Spam}}</span></a>
          <a class="row-menu-item tab-more-item" href="/threads?status=all&assignee={{.List.AssigneeFilter}}&account={{.List.AccountFilter}}&q={{.List.Search}}"{{if eq .List.StatusFilter "all"}} aria-current="true"{{end}}><span>All</span> <span class="count-pill">{{.List.Counts.All}}</span></a>
        </div>
      </details>
    </nav>
    </div>
    <div class="results-status">
      <p class="results-count">{{len .List.Threads}} thread{{if ne (len .List.Threads) 1}}s{{end}}</p>
      <a class="refresh-meta" href="{{.List.RefreshURL}}" data-refresh-link title="Refresh the list"><span data-refresh-status aria-live="polite">Refresh</span></a>
    </div>
  </div>
  <div class="filter-sticky">
    {{/* The form submits to the open thread's path (not /threads): applying a
         pane filter re-renders the split view with the new filter instead of
         navigating to the plain inbox — the same behaviour as the top
         data-split-filters bar above, and it works without JS. The handler
         sets ClearSearchURL to a thread-path URL so clearing the search stays
         in the split view too. */}}
    {{template "inbox_filter_form" (inboxFilterForm .List (threadDetailURL "" .Thread.ID) .List.ClearSearchURL)}}
  </div>
  <span class="pull-indicator" data-pull-indicator aria-hidden="true"></span>
  {{/* No bulk bar in the pane: selection chrome plus a 5-button bar cannot fit a
       narrow column without eating the subject. Bulk actions stay on the plain
       inbox (/threads), which is one click away via the "/threads" nav link and
       is the whole page below 1280px. */}}
  <div data-inbox-results>
  {{template "inbox_table" .List}}
  {{if .List.SortTruncated}}<p class="list-truncated">Longest waiting shows at most 100 threads. Use search or filters to narrow the queue.</p>{{end}}
  {{if .List.NextURL}}<p class="list-footer"><span class="list-footer-count">{{if .List.TotalCount}}{{.List.TotalCount}} threads{{else}}More threads{{end}}</span> <a class="btn btn-secondary" href="{{.List.NextURL}}">Load older</a></p>{{end}}
  </div>
  </aside>
{{end}}
  <div class="split-detail" data-split-detail>
<div class="thread-top-bar thread-detail">
  <a class="back-link" href="{{if .BackURL}}{{.BackURL}}{{else}}/threads{{end}}">&larr; Back to threads</a>
  {{/* Refresh re-requests this page. In the split view it reuses the pane's
       filter-carrying thread URL (List.RefreshURL) so reloading keeps the queue
       the user is reading; without a pane it is the bare thread path. No-JS: it
       is a plain link either way. */}}
  <a class="btn btn-secondary btn-small" href="{{if .List}}{{.List.RefreshURL}}{{else}}/threads/{{.Thread.ID}}{{end}}">Refresh</a>
</div>
<div class="card thread-header-card">
  <div class="thread-header">
    <div>
      <h1>{{.Thread.SubjectDisplay}}</h1>
      <ul class="meta-list">
        <li><span class="{{statusBadgeClass .Thread.Status}}">{{statusLabel .Thread.Status}}</span></li>
        {{if .WaitLabel}}<li><span class="wait-badge wait-{{.WaitUrgency}}">{{.WaitLabel}}</span></li>{{end}}
        {{if .ShowAccountUI}}<li><span class="badge badge-account">{{.AccountName}}</span></li>{{end}}
        <li><span class="meta-label">Customer</span> <strong>{{if .Thread.CustomerName}}{{.Thread.CustomerName}}{{else}}Unknown customer{{end}}</strong> <span class="customer-email">{{.Thread.CustomerEmail}}</span></li>
        {{if or (eq .Thread.Status "completed") (eq .Thread.Status "spam")}}
        <li><span class="meta-label">Assignee</span> {{if .Thread.AssigneeID}}{{.AssigneeName}}{{else}}<span class="muted">Unassigned</span>{{end}}</li>
        {{end}}
      </ul>
    </div>
  </div>

  {{if .PreviousThreads}}
  <details class="previous-conversations">
    <summary>{{len .PreviousThreads}} other conversation{{if ne (len .PreviousThreads) 1}}s{{end}}</summary>
    <p class="helper">Other conversations from this email address</p>
    <ul class="previous-conversations-list">
      {{range .PreviousThreads}}
      <li class="previous-conversation-row">
        <div class="previous-conversation-main">
          <div class="previous-conversation-head">
            <a class="previous-conversation-link" href="/threads/{{.Thread.ID}}" target="_blank" rel="noopener" title="Opens in a new tab so your reply stays here.">{{.Thread.SubjectDisplay}}</a>
            <span class="previous-conversation-meta">
              <span class="{{statusBadgeClass .Thread.Status}}">{{statusLabel .Thread.Status}}</span>
              <time class="muted previous-conversation-time" title="{{.FullTime}}">{{.FormattedTime}}</time>
              {{if $.ShowAccountUI}}<span class="badge badge-account">{{.AccountName}}</span>{{end}}
            </span>
          </div>
          <p class="previous-conversation-snippet">{{if .LatestSnippet}}{{if eq .LatestDirection "outbound"}}<span class="snippet-you">You:</span> {{end}}{{.LatestSnippet}}{{else}}<em>(No message text)</em>{{end}}</p>
        </div>
      </li>
      {{end}}
    </ul>
  </details>
  {{end}}

  {{if .ReopensThread}}
  <div class="banner banner-reopen">
    <h2>Reply to a completed thread</h2>
    <p>
      This email replies to the completed thread
      <a href="/threads/{{.ReopensThread.ID}}">{{.ReopensThread.SubjectDisplay}}</a>{{if .ReopenCompletedAt}}, completed {{.ReopenCompletedAt}}{{end}}{{if .ReopenCompletedBy}} by {{.ReopenCompletedBy}}{{end}}.
    </p>
    <p><strong>Reopen &amp; merge</strong> moves this email into the original thread and returns it to In progress; this temporary thread is removed.</p>
    <form method="post" action="/threads/{{.Thread.ID}}/reopen-merge" class="inline-form">
      <button type="submit" class="btn btn-primary">Reopen &amp; merge into the original thread</button>
    </form>
    <p class="helper">Or keep this email as a separate new thread and use Start working below.</p>
  </div>
  {{end}}

  {{if .Errors}}
  <div class="form-errors" role="alert">
    <ul>{{range .Errors}}<li>{{.}}</li>{{end}}</ul>
  </div>
  {{end}}

  <div class="actions-bar">
    <div class="actions-group">
      {{if eq .Thread.Status "new"}}
      <form method="post" action="/threads/{{.Thread.ID}}/transition" class="inline-form">
        <input type="hidden" name="status" value="in_progress">
        <button type="submit" class="btn btn-primary">Start working</button>
      </form>
      <form method="post" action="/threads/{{.Thread.ID}}/transition" class="inline-form" data-confirm="Mark this thread complete and move its messages to the Archive folder?" data-confirm-action="Move to Archive">
        <input type="hidden" name="status" value="completed">
        <input type="hidden" name="complete_from_new" value="1">
        <button type="submit" class="btn btn-primary">Mark complete</button>
      </form>
      <form method="post" action="/threads/{{.Thread.ID}}/transition" class="inline-form" data-confirm="Mark this thread as spam and move its messages to the Junk folder?" data-confirm-action="Move to Junk">
        <input type="hidden" name="status" value="spam">
        <button type="submit" class="btn btn-danger">Mark as spam</button>
      </form>
      {{end}}
      {{if eq .Thread.Status "in_progress"}}
      <form method="post" action="/threads/{{.Thread.ID}}/transition" class="inline-form" data-confirm="Mark this thread complete and move its messages to the Archive folder?" data-confirm-action="Move to Archive">
        <input type="hidden" name="status" value="completed">
        <button type="submit" class="btn btn-primary">Mark complete</button>
      </form>
      {{end}}
      {{if eq .Thread.Status "spam"}}
      <form method="post" action="/threads/{{.Thread.ID}}/transition" class="inline-form" data-confirm="Return this conversation to In progress and queue restoration from Junk?" data-confirm-action="Restore">
        <input type="hidden" name="status" value="in_progress"><input type="hidden" name="recover_spam" value="1">
        <button type="submit" class="btn btn-secondary">Not spam</button>
      </form>
      <p class="helper">This thread is marked as spam. Replies and assignment are disabled.</p>
      {{end}}
      {{if eq .Thread.Status "completed"}}
      <p class="helper">This thread is completed. Replies and assignment are disabled until a follow-up reply is reopened and merged.</p>
      {{end}}
    </div>
    {{if and (ne .Thread.Status "spam") (ne .Thread.Status "completed")}}
    <div class="actions-group actions-assign">
      {{if not .Thread.AssigneeID}}
      <form method="post" action="/threads/{{.Thread.ID}}/assign" class="inline-form">
        <input type="hidden" name="assignee_id" value="{{.CurrentUser.ID}}">
        <button type="submit" class="btn btn-secondary">Assign to me</button>
      </form>
      {{end}}
      <form method="post" action="/threads/{{.Thread.ID}}/assign" class="assign-form">
        <label class="inline-field" for="assignee-{{.Thread.ID}}">Assignee
          <select id="assignee-{{.Thread.ID}}" name="assignee_id" data-auto-submit>
            <option value="" {{if not .Thread.AssigneeID}}selected{{end}} disabled>Unassigned</option>
            {{range .Users}}
            <option value="{{.ID}}" {{if and $.Thread.AssigneeID (eq .ID.String $.Thread.AssigneeID.String)}}selected{{end}}>{{.DisplayName}}</option>
            {{end}}
          </select>
        </label>
        <noscript><button type="submit" class="btn btn-secondary">Assign</button></noscript>
      </form>
      {{with assignmentLabel .Thread.AssignmentSource}}{{if $.Thread.AssigneeID}}<span class="muted assign-note">({{.}})</span>{{end}}{{end}}
    </div>
    {{end}}
  </div>
</div>

{{if .ReplyAllowed}}
<button type="button" class="reply-sticky-bar" data-open-reply-sheet aria-controls="reply" aria-expanded="false">Reply</button>
{{end}}

<div class="section-heading">
  <h2>Messages ({{len .Emails}})</h2>
  {{if gt (len .Emails) 1}}<button type="button" class="btn btn-ghost btn-small" data-toggle-messages aria-pressed="false">Expand all</button>{{end}}
</div>
{{range .Emails}}
<details class="card message message-{{directionClass .Email.Direction}}"{{if .IsLatest}} open{{end}}>
  <summary class="message-header">
    <span class="{{directionBadge .Email.Direction}}">{{directionLabel .Email.Direction}}</span>
    <span class="message-from"><strong>{{if .Email.FromName}}{{.Email.FromName}}{{else}}{{.Email.FromAddress}}{{end}}</strong><span class="message-address"> &lt;{{.Email.FromAddress}}&gt;</span></span>
    <span class="message-snippet">{{if .Snippet}}{{.Snippet}}{{else}}<em>(No message content)</em>{{end}}</span>
    {{if .Attachments}}<span class="attachment-chip" title="{{len .Attachments}} attachment(s)">&#128206; {{len .Attachments}}</span>{{end}}
    <time class="muted message-time" title="{{.FullSent}}">{{.FormattedSent}}</time>
  </summary>
  {{if .HasRemoteImages}}
  <div class="remote-image-notice">
    <p>Remote images blocked for your privacy. Loading them may tell the sender you opened this message.</p>
    <button type="button" class="btn btn-secondary btn-small" data-load-images data-image-prefix="/emails/{{.Email.ID}}/images/">Load images for this message</button>
  </div>
  {{end}}
  <iframe
    class="email-frame"
    src="/emails/{{.Email.ID}}/body"
    title="Email message from {{.Email.FromAddress}}"
    sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"
    referrerpolicy="no-referrer"
    loading="lazy">
  </iframe>
  {{if .Attachments}}
  <div class="attachments">
    <ul class="attachment-grid" aria-label="Attachments">
      {{range .Attachments}}
      <li class="attachment-card">
        {{if isImageAttachment .}}
        <a class="attachment-thumb attachment-thumb-image" href="/attachments/{{.ID}}/view" target="_blank" rel="noopener" tabindex="-1" aria-hidden="true">
          <img class="attachment-preview-img" src="/attachments/{{.ID}}/view" alt="{{.Filename}}" loading="lazy">
        </a>
        {{else}}
        <span class="attachment-thumb attachment-thumb-doc" aria-hidden="true">
          <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><path d="M8 13h8"/><path d="M8 17h5"/></svg>
        </span>
        {{end}}
        <div class="attachment-meta">
          {{if isViewableAttachment .}}
          <a class="attachment-name" href="/attachments/{{.ID}}/view" target="_blank" rel="noopener" title="{{.Filename}}">{{.Filename}}</a>
          {{else}}
          <a class="attachment-name" href="/attachments/{{.ID}}/download" title="{{.Filename}}">{{.Filename}}</a>
          {{end}}
          <span class="attachment-sub muted">{{attachmentKind .}} &middot; {{formatSize .SizeBytes}}</span>
        </div>
        <div class="attachment-actions">
          {{if isViewableAttachment .}}
          <a class="btn-icon" href="/attachments/{{.ID}}/view" target="_blank" rel="noopener" title="View {{.Filename}}" aria-label="View {{.Filename}}">
            <svg viewBox="0 0 24 24" width="17" height="17" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/></svg>
          </a>
          {{end}}
          <a class="btn-icon" href="/attachments/{{.ID}}/download" title="Download {{.Filename}}" aria-label="Download {{.Filename}}">
            <svg viewBox="0 0 24 24" width="17" height="17" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>
          </a>
        </div>
      </li>
      {{end}}
    </ul>
  </div>
  {{end}}
</details>
{{end}}

{{if not .ReplyAllowed}}
{{if or .ReplyDraft .ReplyErrors}}
<section class="card" id="reply-draft">
  <h2>Draft not sent</h2>
  {{if .ReplyDraft}}
  <p class="helper">This thread cannot receive a reply, so your draft has not been sent. Copy it if you need it elsewhere.</p>
  {{else}}
  <p class="helper">This thread cannot receive a reply.</p>
  {{end}}
  {{if .ReplyErrors}}
  <div class="form-errors" role="alert">
    <ul>{{range .ReplyErrors}}<li>{{.}}</li>{{end}}</ul>
  </div>
  {{end}}
  {{if .ReplyDraft}}
  <label class="field" for="preserved-draft">Draft</label>
  <textarea id="preserved-draft" name="preserved_draft" rows="6" readonly>{{.ReplyDraft}}</textarea>
  {{end}}
</section>
{{end}}
{{end}}

{{if .ReplyAllowed}}
<section class="card reply-card" id="reply">
  <button type="button" class="reply-sheet-close" data-close-reply-sheet aria-label="Close reply">&times;</button>
  <h2>Send reply</h2>
  <details class="reply-context-details" open>
    <summary>{{.AccountName}} &rarr; {{if .Thread.CustomerName}}{{.Thread.CustomerName}}{{else}}{{.Thread.CustomerEmail}}{{end}}</summary>
    <dl class="reply-context">
      <div><dt>From</dt><dd>{{.AccountName}} &lt;{{.AccountFromAddress}}&gt;</dd></div>
      <div><dt>To</dt><dd>{{if .Thread.CustomerName}}{{.Thread.CustomerName}} {{end}}&lt;{{.Thread.CustomerEmail}}&gt;</dd></div>
    </dl>
  </details>
  {{if eq .Thread.Status "new"}}
  <p class="helper">If this thread is unassigned, sending will assign it to you. Sending a reply also moves a new thread to In progress.</p>
  {{end}}
  {{if .ReplyErrors}}
  <div class="form-errors" role="alert">
    <ul>{{range .ReplyErrors}}<li>{{.}}</li>{{end}}</ul>
  </div>
  {{end}}
  <form method="post" action="/threads/{{.Thread.ID}}/reply" enctype="multipart/form-data" class="reply-form" data-draft-url="/threads/{{.Thread.ID}}/draft">
    <input type="hidden" name="draft_revision" value="{{.DraftRevision}}">
    <input type="hidden" name="body_formatting" value="{{.DraftFormatting}}" disabled>
    <div class="field">
      <label for="reply-body">Message</label>
      <textarea id="reply-body" class="reply-textarea" name="body_text" rows="6" required placeholder="Write your reply...">{{.ReplyDraft}}</textarea>
      <div data-editor-controls hidden>
        <div role="toolbar" aria-label="Reply formatting">
          <button type="button" class="btn btn-secondary btn-small" data-editor-bold aria-pressed="false">Bold</button>
          <button type="button" class="btn btn-secondary btn-small" data-editor-italic aria-pressed="false">Italic</button>
          <button type="button" class="btn btn-secondary btn-small" data-editor-plain>Use plain text</button>
        </div>
        <div data-editor></div>
      </div>
    </div>
    <div class="reply-footer">
      <button type="button" class="btn btn-secondary" data-save-draft>Save draft</button>
      <button type="button" class="btn btn-secondary" data-discard-draft>Discard draft</button>
      <span role="status" aria-live="polite" data-draft-status></span>
      <label class="file-field" for="reply-attachment">Attachment (optional)
        <input id="reply-attachment" type="file" name="attachment">
      </label>
      <button type="submit" class="btn btn-primary">Send reply</button>
    </div>
  </form>
</section>
{{end}}

<details class="card activity-card">
  <summary><h2>Activity ({{len .Events}})</h2></summary>
  <ol class="timeline">
    {{range .Events}}
    {{if .Section}}<li class="timeline-section"><h3>{{.Section}}</h3></li>{{end}}
    <li><time class="muted" title="{{.FullTime}}">{{.FormattedTime}}</time> <span>{{.Summary}}</span></li>
    {{else}}
    <li class="muted">No activity recorded yet.</li>
    {{end}}
  </ol>
</details>
`

const errorBody = `
<div class="error-page">
  <p class="error-code">{{.StatusCode}}</p>
  <h1>{{.Heading}}</h1>
  <p>{{.Message}}</p>
  <a class="btn btn-primary" href="/threads">Back to threads</a>
</div>
`

const settingsBody = `
<div class="page-heading">
  <h1>Settings</h1>
</div>

<nav class="settings-section-nav" aria-label="Settings sections">
  <a href="#settings-routing">Routing &amp; display</a>
  <a href="#settings-folders">Folders</a>
  <a href="#settings-team">Team</a>
  <a href="#settings-activity">Activity</a>
</nav>

{{if .Errors}}
<div class="form-errors" role="alert">
  <ul>{{range .Errors}}<li>{{.}}</li>{{end}}</ul>
</div>
{{end}}

<form method="post" action="/settings" class="card" id="settings-routing">
  <h2>Routing &amp; display</h2>
  {{range .AccountSettings}}
  <fieldset>
    <legend>{{.Account.Name}}</legend>
    <label class="field">Default assignee
      <select name="account_default_assignee_{{.Account.ID}}">
        <option value="" {{if eq .DefaultAssigneeValue ""}}selected{{end}}>-- Unassigned --</option>
        {{$current := .DefaultAssigneeValue}}
        {{$acct := .Account.ID}}
        {{range $.Users}}
        {{if and .IsActive (.HasAccountAccess $acct)}}
        <option value="{{.ID}}" {{if eq .ID.String $current}}selected{{end}}>{{.DisplayName}} ({{.Email}})</option>
        {{end}}
        {{end}}
      </select>
    </label>
  </fieldset>
  {{end}}
  <div class="form-grid">
    <label class="field">Display timezone (IANA)
      <input type="text" name="display_timezone" value="{{.Settings.DisplayTimezone}}" required>
      <span class="helper">All times in MiniCRM are shown in this timezone.</span>
    </label>
    <label class="field">Subject match window (days)
      <input type="number" name="subject_match_window_days" value="{{.Settings.SubjectMatchWindowDays}}" min="1" max="365" required>
    </label>
    <label class="field">Needs attention threshold (days)
      <input type="number" name="attention_age_days" value="{{.Settings.AttentionAgeDays}}" min="1" max="365" required>
      <span class="helper">Open threads older than this appear in Needs attention.</span>
    </label>
  </div>
  <fieldset id="settings-folders">
    <legend>IMAP folder mapping</legend>
  <div class="form-grid">
    <label class="field">IMAP INBOX folder
      <input type="text" name="imap_inbox_mailbox" value="{{.Settings.IMAPInboxMailbox}}" required>
    </label>
    <label class="field">IMAP Archive folder
      <input type="text" name="imap_archive_mailbox" value="{{.Settings.IMAPArchiveMailbox}}" required>
    </label>
    <label class="field">IMAP Junk folder
      <input type="text" name="imap_junk_mailbox" value="{{.Settings.IMAPJunkMailbox}}" required>
    </label>
    <label class="field">IMAP Sent folder
      <input type="text" name="imap_sent_mailbox" value="{{.Settings.IMAPSentMailbox}}" required>
    </label>
  </div>
  </fieldset>
  <button type="submit" class="btn btn-primary">Save settings</button>
</form>

<h2 id="settings-team">Team members</h2>
<div class="card">
  <div class="table-wrap">
    <table class="settings-table settings-users-table">
      <thead>
        <tr>
          <th scope="col">Name</th>
          <th scope="col">Email</th>
          <th scope="col">Role</th>
          <th scope="col">Account access</th>
          <th scope="col">Status</th>
        </tr>
      </thead>
      <tbody>
        {{range .Users}}
        <tr>
          <td data-label="Name" class="settings-user-name"><span class="settings-user-name-text">{{.DisplayName}}</span>{{if .IsActive}}<span class="badge badge-active">Active</span>{{else}}<span class="badge badge-inactive">Inactive</span>{{end}}<span class="badge badge-role">{{roleLabel .Role}}</span></td>
          <td data-label="Email" class="settings-user-email">{{.Email}}</td>
          <td data-label="Role" class="settings-user-role">{{roleLabel .Role}}</td>
          <td data-label="Account access" class="settings-user-accounts">
            {{if .AllowedAccounts}}
              {{range .AllowedAccounts}}<span class="account-chip">{{index $.AccountNames .}}</span>{{end}}
            {{else}}
              <span class="muted">No account access</span>
            {{end}}
          </td>
          <td data-label="Status" class="settings-user-status">{{if .IsActive}}Active{{else}}Inactive{{end}}</td>
        </tr>
        {{else}}
        <tr><td colspan="5" class="empty-state">No team members yet.</td></tr>
        {{end}}
      </tbody>
    </table>
  </div>
</div>

<h2>Add team member</h2>
<form method="post" action="/settings/users" class="card">
  <div class="form-grid">
    <label class="field">Email
      <input type="email" name="email" value="{{.NewUser.Email}}" required>
    </label>
    <label class="field">Display name
      <input type="text" name="display_name" value="{{.NewUser.DisplayName}}" required>
    </label>
    <label class="field">Role
      <select name="role">
        <option value="agent" {{if ne .NewUser.Role "admin"}}selected{{end}}>Agent</option>
        <option value="admin" {{if eq .NewUser.Role "admin"}}selected{{end}}>Admin</option>
      </select>
    </label>
  </div>
  <fieldset>
    <legend>Account access</legend>
    <div class="checkbox-row">
      {{range .Accounts}}
      <label class="checkbox"><input type="checkbox" name="allowed_accounts" value="{{.ID}}" {{if hasAccount $.NewUser.AllowedAccounts .ID}}checked{{end}}> {{.Name}}</label>
      {{end}}
    </div>
  </fieldset>
  <button type="submit" class="btn btn-primary">Create user</button>
</form>

<h2 id="settings-activity">Recent activity</h2>
<div class="card settings-recent">
  <div class="table-wrap">
    {{template "settings_audit_table" .AuditLogs}}
  </div>
  <div class="settings-activity-footer">
    <a class="settings-activity-link" href="/settings/activity">View full activity</a>
  </div>
</div>
`

// settingsAuditTablePartial is the single source of truth for the recent-activity
// table, shared by the settings page (bounded render) and /settings/activity
// (paginated). On mobile the same markup transforms into stacked cards via CSS:
// each td carries data-label and status/role also appear inside the name cell
// (line 1), so the transformation stays CSS-only per the W6 plan.
const settingsAuditTablePartial = `{{define "settings_audit_table"}}
<table class="settings-table settings-audit-table">
  <thead>
    <tr>
      <th scope="col">Time</th>
      <th scope="col">Actor</th>
      <th scope="col">Event</th>
      <th scope="col">Target</th>
    </tr>
  </thead>
  <tbody>
    {{range .}}
    <tr>
      <td data-label="Time" class="nowrap"><time title="{{.FullTime}}">{{.FormattedTime}}</time></td>
      <td data-label="Actor">{{.ActorName}}</td>
      <td data-label="Event">{{.Label}}</td>
      <td data-label="Target">{{if .TargetName}}{{.TargetName}}{{else}}<span class="muted">—</span>{{end}}</td>
    </tr>
    {{else}}
    <tr><td colspan="4" class="empty-state">No activity recorded yet.</td></tr>
    {{end}}
  </tbody>
</table>
{{end}}`

// activityBody is the admin full-activity page: the shared audit table plus
// pagination links. Admin-only route (requireAdmin in router.go).
const activityBody = `
<div class="page-heading page-heading-row">
  <h1>Full activity</h1>
  <a class="btn btn-ghost btn-small" href="/settings">Back to settings</a>
</div>

<div class="card">
  <div class="table-wrap">
    {{template "settings_audit_table" .AuditLogs}}
  </div>
  <div class="activity-pagination">
    {{if .HasPrev}}<a class="btn btn-ghost btn-small" href="/settings/activity?page={{.PagePrev}}">&larr; Newer</a>{{else}}<span></span>{{end}}
    <span class="activity-page-label muted">Page {{.Page}}</span>
    {{if .HasNext}}<a class="btn btn-ghost btn-small" href="/settings/activity?page={{.PageNext}}">Older &rarr;</a>{{else}}<span></span>{{end}}
  </div>
</div>
`

const isolatedEmailBodyHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="responsive-embedded-sizing">
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'none'; base-uri 'none'; object-src 'none'">
  <meta name="referrer" content="no-referrer">
  <title>Email message</title>
  <style>
    html, body { margin: 0; padding: 0; background: #ffffff; color: #0f172a; }
    body { display: flow-root; font-family: system-ui, -apple-system, sans-serif; font-size: 15px; line-height: 1.55; padding: 0.75rem 1rem; overflow-wrap: anywhere; }
    img { max-width: 100%; height: auto; }
    table { border-collapse: collapse; max-width: 100%; }
    th, td { border: 1px solid #e2e8f0; padding: 0.35rem 0.5rem; text-align: left; }
    blockquote { margin: 0.75rem 0; padding-left: 0.75rem; border-left: 3px solid #e2e8f0; color: #475569; }
    pre { white-space: pre-wrap; font-family: inherit; margin: 0; }
    a { color: #4338ca; }
    .empty-body { color: #64748b; font-style: italic; margin: 0; }
  </style>
</head>
<body>
  {{if .HasHTML}}
    <div>{{.SanitizedHTML}}</div>
  {{else if .PlainText}}
    <pre>{{.PlainText}}</pre>
  {{else}}
    <p class="empty-body">(No message content)</p>
  {{end}}
</body>
</html>
`
