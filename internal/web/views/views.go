// Package views provides type-safe server-rendered HTML templates and timezone formatting for MiniCRM.
package views

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

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
	Thread        sqlc.Thread
	AssigneeName  string
	AccountName   string
	FormattedTime string
}

// InboxViewData holds data for rendering the threads inbox view.
type InboxViewData struct {
	Title          string
	Flash          Flash
	ActiveNav      string
	CurrentUser    sqlc.User
	Threads        []ThreadSummaryItem
	StatusFilter   string
	AssigneeFilter string
	Users          []sqlc.User
	DisplayTZ      string
	// ShowAccountUI is true when the current user can access more than one mail account.
	ShowAccountUI bool
	AccountFilter string
	Accounts      []AccountOption
	// Truncated is true when the store returned exactly the query limit.
	Truncated bool
	Limit     int
}

// EmailItemView holds an email, its formatted timestamp, and its attachments.
type EmailItemView struct {
	Email         sqlc.Email
	FormattedSent string
	Attachments   []sqlc.Attachment
}

// EventItemView holds a formatted thread audit event sentence.
type EventItemView struct {
	Event         sqlc.ThreadEvent
	FormattedTime string
	Summary       string
	// Section, when set, renders a subheading before this event to distinguish merged history.
	Section string
}

// ThreadDetailViewData holds data for rendering a single conversation thread.
type ThreadDetailViewData struct {
	Title         string
	Flash         Flash
	ActiveNav     string
	CurrentUser   sqlc.User
	Thread        sqlc.Thread
	AssigneeName  string
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
	// Errors are transition, assignment, or merge errors rendered in the thread header card.
	Errors []string
	// ReplyErrors are reply-composition errors rendered next to the reply field only.
	ReplyErrors []string
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
	ActorName     string
	TargetName    string
	Label         string
}

// SettingsViewData holds data for the admin settings and user management page.
type SettingsViewData struct {
	Title           string
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
	Title       string
	Flash       Flash
	ActiveNav   string
	CurrentUser sqlc.User
	StatusCode  int
	Heading     string
	Message     string
}

// FormatTimestamp converts t into the configured IANA displayTimezone.
func FormatTimestamp(t time.Time, displayTimezone string) string {
	loc, err := time.LoadLocation(displayTimezone)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04:05 MST")
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

var templateFuncs = template.FuncMap{
	"statusLabel":      FormatStatus,
	"statusBadgeClass": statusBadgeClass,
	"directionLabel":   directionLabel,
	"directionClass":   directionClass,
	"directionBadge":   directionBadgeClass,
	"formatSize":       FormatFileSize,
	"assignmentLabel":  assignmentLabel,
	"roleLabel":        FormatRole,
	"auditEventLabel":  FormatAuditEvent,
	"hasAccount":       hasAccount,
}

var (
	inboxFullTmpl = template.Must(template.New("inbox").Funcs(templateFuncs).Parse(layoutHeader + inboxBody + layoutFooter))
	inboxPartTmpl = template.Must(template.New("inbox_partial").Funcs(templateFuncs).Parse(inboxTablePartial))
	detailTmpl    = template.Must(template.New("detail").Funcs(templateFuncs).Parse(layoutHeader + threadDetailBody + layoutFooter))
	settingsTmpl  = template.Must(template.New("settings").Funcs(templateFuncs).Parse(layoutHeader + settingsBody + layoutFooter))
	errorTmpl     = template.Must(template.New("error").Funcs(templateFuncs).Parse(layoutHeader + errorBody + layoutFooter))
	emailBodyTmpl = template.Must(template.New("email_body").Funcs(templateFuncs).Parse(isolatedEmailBodyHTML))
)

// RenderInbox renders either the full inbox page or the partial thread table.
func RenderInbox(w io.Writer, data InboxViewData, partialOnly bool) error {
	if partialOnly {
		return inboxPartTmpl.Execute(w, data)
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
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light">
  <meta name="referrer" content="no-referrer">
  <title>{{if .Title}}{{.Title}} — MiniCRM{{else}}MiniCRM{{end}}</title>
  <link rel="icon" type="image/svg+xml" href="/static/favicon.svg">
  <link rel="stylesheet" href="/static/app.css">
  <script src="/static/app.js" defer></script>
</head>
<body>
  <a class="skip-link" href="#main-content">Skip to content</a>
  <header>
    <nav class="navbar" aria-label="Main">
      <a class="navbar-brand" href="/threads">MiniCRM</a>
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
  <main id="main-content" class="container" tabindex="-1">
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

const inboxTablePartial = `
{{if .Threads}}
<div class="table-wrap">
  <table class="threads-table">
    <thead>
      <tr>
        {{if .ShowAccountUI}}<th scope="col">Account</th>{{end}}
        <th scope="col">Subject</th>
        <th scope="col">Customer</th>
        <th scope="col">Status</th>
        <th scope="col">Assignee</th>
        <th scope="col">Last message ({{.DisplayTZ}})</th>
      </tr>
    </thead>
    <tbody>
      {{range .Threads}}
      <tr>
        {{if $.ShowAccountUI}}<td><span class="badge badge-account">{{.AccountName}}</span></td>{{end}}
        <td><a href="/threads/{{.Thread.ID}}">{{.Thread.SubjectDisplay}}</a></td>
        <td>
          <span class="customer-name">{{if .Thread.CustomerName}}{{.Thread.CustomerName}}{{else}}Unknown customer{{end}}</span>
          <span class="customer-email">{{.Thread.CustomerEmail}}</span>
        </td>
        <td><span class="{{statusBadgeClass .Thread.Status}}">{{statusLabel .Thread.Status}}</span></td>
        <td>{{if .AssigneeName}}{{.AssigneeName}}{{else}}<span class="muted">Unassigned</span>{{end}}</td>
        <td class="nowrap">{{.FormattedTime}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
</div>
{{else}}
<p class="card empty-state">No threads match these filters.</p>
{{end}}
`

const inboxBody = `
<div class="page-heading">
  <h1>Threads</h1>
</div>
<form method="get" action="/threads" class="card filter-bar">
  <label class="field">Status
    <select name="status" data-hx-get="/threads">
      <option value="">All statuses</option>
      <option value="new" {{if eq .StatusFilter "new"}}selected{{end}}>New</option>
      <option value="in_progress" {{if eq .StatusFilter "in_progress"}}selected{{end}}>In progress</option>
      <option value="completed" {{if eq .StatusFilter "completed"}}selected{{end}}>Completed</option>
      <option value="spam" {{if eq .StatusFilter "spam"}}selected{{end}}>Spam</option>
    </select>
  </label>
  <label class="field">Assignee
    <select name="assignee" data-hx-get="/threads">
      <option value="" {{if eq .AssigneeFilter ""}}selected{{end}}>Anyone</option>
      <option value="me" {{if eq .AssigneeFilter "me"}}selected{{end}}>Assigned to me</option>
      <option value="unassigned" {{if eq .AssigneeFilter "unassigned"}}selected{{end}}>Unassigned</option>
      {{range .Users}}
      <option value="{{.ID}}" {{if eq .ID.String $.AssigneeFilter}}selected{{end}}>{{.DisplayName}}</option>
      {{end}}
    </select>
  </label>
  {{if .ShowAccountUI}}
  <label class="field">Account
    <select name="account" data-hx-get="/threads">
      <option value="">All accounts</option>
      {{range .Accounts}}
      <option value="{{.ID}}" {{if eq .ID $.AccountFilter}}selected{{end}}>{{.Name}}</option>
      {{end}}
    </select>
  </label>
  {{end}}
  <noscript><button type="submit" class="btn btn-secondary">Filter</button></noscript>
</form>
<p class="results-count">
  Showing {{len .Threads}} thread{{if ne (len .Threads) 1}}s{{end}}{{if .Truncated}} (first {{.Limit}}; narrow the filters to see more){{end}}
</p>
` + inboxTablePartial

const threadDetailBody = `
<div class="card thread-header-card">
  <div class="thread-header">
    <div>
      <h1>{{.Thread.SubjectDisplay}}</h1>
      <ul class="meta-list">
        <li><span class="meta-label">Status</span> <span class="{{statusBadgeClass .Thread.Status}}">{{statusLabel .Thread.Status}}</span></li>
        {{if .ShowAccountUI}}<li><span class="meta-label">Account</span> <span class="badge badge-account">{{.AccountName}}</span></li>{{end}}
        <li><span class="meta-label">Customer</span> <strong>{{if .Thread.CustomerName}}{{.Thread.CustomerName}}{{else}}Unknown customer{{end}}</strong> <span class="customer-email">{{.Thread.CustomerEmail}}</span></li>
        <li><span class="meta-label">Assignee</span>
          {{if .Thread.AssigneeID}}{{.AssigneeName}}{{with assignmentLabel .Thread.AssignmentSource}} <span class="muted">({{.}})</span>{{end}}{{else}}<span class="muted">Unassigned</span>{{end}}
        </li>
      </ul>
    </div>
    {{if .ReplyAllowed}}<a class="btn btn-secondary" href="#reply">Reply</a>{{end}}
  </div>

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
    {{if eq .Thread.Status "new"}}
    <form method="post" action="/threads/{{.Thread.ID}}/transition" class="inline-form">
      <input type="hidden" name="status" value="in_progress">
      <button type="submit" class="btn btn-primary">Start working</button>
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
    <p class="helper">This thread is marked as spam. Replies and assignment are disabled.</p>
    {{else}}
      {{if eq .Thread.Status "completed"}}
      <p class="helper">This thread is completed. Replies and assignment are disabled until a follow-up reply is reopened and merged.</p>
      {{else}}
      {{if not .Thread.AssigneeID}}
      <form method="post" action="/threads/{{.Thread.ID}}/assign" class="inline-form">
        <input type="hidden" name="assignee_id" value="{{.CurrentUser.ID}}">
        <button type="submit" class="btn btn-secondary">Assign to me</button>
      </form>
      {{end}}
      <form method="post" action="/threads/{{.Thread.ID}}/assign" class="assign-form">
        <label class="field" for="assignee-{{.Thread.ID}}">Assignee
          <select id="assignee-{{.Thread.ID}}" name="assignee_id">
            <option value="" {{if not .Thread.AssigneeID}}selected{{end}} disabled>-- Unassigned --</option>
            {{range .Users}}
            <option value="{{.ID}}" {{if and $.Thread.AssigneeID (eq .ID.String $.Thread.AssigneeID.String)}}selected{{end}}>{{.DisplayName}} ({{.Email}})</option>
            {{end}}
          </select>
        </label>
        <button type="submit" class="btn btn-secondary">Assign</button>
      </form>
      {{end}}
    {{end}}
  </div>
</div>

<h2>Messages</h2>
{{range .Emails}}
<article class="card message message-{{directionClass .Email.Direction}}">
  <header class="message-header">
    <span class="{{directionBadge .Email.Direction}}">{{directionLabel .Email.Direction}}</span>
    <span class="message-from"><strong>{{if .Email.FromName}}{{.Email.FromName}}{{else}}{{.Email.FromAddress}}{{end}}</strong> &lt;{{.Email.FromAddress}}&gt;</span>
    <time class="muted">{{.FormattedSent}}</time>
  </header>
  <iframe
    class="email-frame"
    src="/emails/{{.Email.ID}}/body"
    title="Email message from {{.Email.FromAddress}}"
    sandbox="allow-popups allow-popups-to-escape-sandbox"
    referrerpolicy="no-referrer"
    loading="lazy">
  </iframe>
  <div class="message-actions">
    <button type="button" class="btn btn-ghost btn-small" data-frame-expand>Expand</button>
  </div>
  {{if .Attachments}}
  <div class="attachments">
    <strong>Attachments</strong>
    <ul>
      {{range .Attachments}}
      <li><a href="/attachments/{{.ID}}/download">{{.Filename}}</a> <span class="muted">{{formatSize .SizeBytes}}</span></li>
      {{end}}
    </ul>
  </div>
  {{end}}
</article>
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
  <h2>Send reply</h2>
  <dl class="reply-context">
    <div><dt>From</dt><dd>{{.AccountName}} &lt;{{.AccountFromAddress}}&gt;</dd></div>
    <div><dt>To</dt><dd>{{if .Thread.CustomerName}}{{.Thread.CustomerName}} {{end}}&lt;{{.Thread.CustomerEmail}}&gt;</dd></div>
  </dl>
  {{if eq .Thread.Status "new"}}
  <p class="helper">Sending a reply assigns this thread to you and moves it to In progress.</p>
  {{end}}
  {{if .ReplyErrors}}
  <div class="form-errors" role="alert">
    <ul>{{range .ReplyErrors}}<li>{{.}}</li>{{end}}</ul>
  </div>
  {{end}}
  <form method="post" action="/threads/{{.Thread.ID}}/reply" enctype="multipart/form-data" class="reply-form">
    <div class="field">
      <label for="reply-body">Message</label>
      <textarea id="reply-body" class="reply-textarea" name="body_text" rows="6" required placeholder="Write your reply...">{{.ReplyDraft}}</textarea>
    </div>
    <div class="field">
      <label for="reply-attachment">Attachment (optional)</label>
      <input id="reply-attachment" type="file" name="attachment">
    </div>
    <button type="submit" class="btn btn-primary">Send reply</button>
  </form>
</section>
{{end}}

<section class="card">
  <h2>Activity ({{.DisplayTZ}})</h2>
  <ol class="timeline">
    {{range .Events}}
    {{if .Section}}<li class="timeline-section"><h3>{{.Section}}</h3></li>{{end}}
    <li><time class="muted">{{.FormattedTime}}</time> <span>{{.Summary}}</span></li>
    {{else}}
    <li class="muted">No activity recorded yet.</li>
    {{end}}
  </ol>
</section>
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

{{if .Errors}}
<div class="form-errors" role="alert">
  <ul>{{range .Errors}}<li>{{.}}</li>{{end}}</ul>
</div>
{{end}}

<form method="post" action="/settings" class="card">
  <h2>Mailbox settings</h2>
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
    </label>
    <label class="field">Subject match window (days)
      <input type="number" name="subject_match_window_days" value="{{.Settings.SubjectMatchWindowDays}}" min="1" max="365" required>
    </label>
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
  <button type="submit" class="btn btn-primary">Save settings</button>
</form>

<h2>Team members</h2>
<div class="card">
  <div class="table-wrap">
    <table>
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
          <td>{{.DisplayName}}</td>
          <td>{{.Email}}</td>
          <td>{{roleLabel .Role}}</td>
          <td>
            {{if .AllowedAccounts}}
              {{range $index, $account := .AllowedAccounts}}{{if $index}}, {{end}}{{index $.AccountNames $account}}{{end}}
            {{else}}
              <span class="muted">No account access</span>
            {{end}}
          </td>
          <td>{{if .IsActive}}<span class="badge badge-active">Active</span>{{else}}<span class="badge badge-inactive">Inactive</span>{{end}}</td>
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

<h2>Recent activity</h2>
<div class="card">
  <div class="table-wrap">
    <table>
      <thead>
        <tr>
          <th scope="col">Time</th>
          <th scope="col">Actor</th>
          <th scope="col">Event</th>
          <th scope="col">Target</th>
        </tr>
      </thead>
      <tbody>
        {{range .AuditLogs}}
        <tr>
          <td class="nowrap">{{.FormattedTime}}</td>
          <td>{{.ActorName}}</td>
          <td>{{.Label}}</td>
          <td>{{if .TargetName}}{{.TargetName}}{{else}}<span class="muted">—</span>{{end}}</td>
        </tr>
        {{else}}
        <tr><td colspan="4" class="empty-state">No activity recorded yet.</td></tr>
        {{end}}
      </tbody>
    </table>
  </div>
</div>
`

const isolatedEmailBodyHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'none'; base-uri 'none'; object-src 'none'">
  <meta name="referrer" content="no-referrer">
  <title>Email message</title>
  <style>
    html, body { margin: 0; padding: 0; background: #ffffff; color: #0f172a; }
    body { font-family: system-ui, -apple-system, sans-serif; font-size: 15px; line-height: 1.55; padding: 0.75rem 1rem; overflow-wrap: anywhere; }
    img { max-width: 100%; height: auto; }
    table { border-collapse: collapse; max-width: 100%; }
    th, td { border: 1px solid #e2e8f0; padding: 0.35rem 0.5rem; text-align: left; }
    blockquote { margin: 0.75rem 0; padding-left: 0.75rem; border-left: 3px solid #e2e8f0; color: #475569; }
    pre { white-space: pre-wrap; font-family: inherit; margin: 0; }
    a { color: #2563eb; }
  </style>
</head>
<body>
  {{if .HasHTML}}
    <div>{{.SanitizedHTML}}</div>
  {{else}}
    <pre>{{.PlainText}}</pre>
  {{end}}
</body>
</html>
`
