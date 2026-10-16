// Package views provides type-safe server-rendered HTML templates and timezone formatting for MiniCRM.
package views

import (
	"html/template"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// ThreadSummaryItem holds display fields for a single row in the inbox list.
type ThreadSummaryItem struct {
	Thread        sqlc.Thread
	AssigneeName  string
	FormattedTime string
}

// InboxViewData holds data for rendering the threads inbox view.
type InboxViewData struct {
	CurrentUser    sqlc.User
	Threads        []ThreadSummaryItem
	StatusFilter   string
	AssigneeFilter string
	Users          []sqlc.User
	DisplayTZ      string
}

// EmailItemView holds an email, its formatted timestamp, and its attachments.
type EmailItemView struct {
	Email         sqlc.Email
	FormattedSent string
	Attachments   []sqlc.Attachment
}

// EventItemView holds a thread audit event and its formatted timestamp.
type EventItemView struct {
	Event         sqlc.ThreadEvent
	FormattedTime string
}

// ThreadDetailViewData holds data for rendering a single conversation thread.
type ThreadDetailViewData struct {
	CurrentUser   sqlc.User
	Thread        sqlc.Thread
	AssigneeName  string
	ReopensThread *sqlc.Thread
	Emails        []EmailItemView
	Events        []EventItemView
	Users         []sqlc.User
	DisplayTZ     string
}

// SettingsViewData holds data for the admin settings and user management page.
type SettingsViewData struct {
	CurrentUser sqlc.User
	Settings    sqlc.SystemSettings
	Users       []sqlc.User
	AuditLogs   []sqlc.AuditLog
}

// FormatTimestamp converts t into the configured IANA displayTimezone.
func FormatTimestamp(t time.Time, displayTimezone string) string {
	loc, err := time.LoadLocation(displayTimezone)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04:05 MST")
}

var (
	inboxFullTmpl = template.Must(template.New("inbox").Parse(layoutHeader + inboxBody + layoutFooter))
	inboxPartTmpl = template.Must(template.New("inbox_partial").Parse(inboxTablePartial))
	detailTmpl    = template.Must(template.New("detail").Parse(layoutHeader + threadDetailBody + layoutFooter))
	settingsTmpl  = template.Must(template.New("settings").Parse(layoutHeader + settingsBody + layoutFooter))
	emailBodyTmpl = template.Must(template.New("email_body").Parse(isolatedEmailBodyHTML))
)

// RenderInbox renders either the full inbox page or the HTMX partial thread table.
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

// RenderSettings renders the admin settings and user pre-creation view.
func RenderSettings(w io.Writer, data SettingsViewData) error {
	return settingsTmpl.Execute(w, data)
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
  <meta name="referrer" content="no-referrer">
  <title>MiniCRM</title>
  <link rel="stylesheet" href="/static/app.css">
  <script src="/static/htmx.min.js" defer></script>
</head>
<body>
  <header class="navbar">
    <div>
      <strong><a href="/threads">MiniCRM</a></strong>
      <span> | <a href="/threads">Threads</a></span>
      {{if eq .CurrentUser.Role "admin"}}<span> | <a href="/settings">Settings</a></span>{{end}}
    </div>
    <div>
      <span>{{.CurrentUser.DisplayName}} ({{.CurrentUser.Email}})</span>
      <form method="post" action="/auth/logout" style="display:inline">
        <button type="submit">Logout</button>
      </form>
    </div>
  </header>
  <main class="container">
`

const layoutFooter = `
  </main>
</body>
</html>
`

const inboxTablePartial = `
<div id="threads-table">
  <table>
    <thead>
      <tr>
        <th>Subject</th>
        <th>Customer</th>
        <th>Status</th>
        <th>Assignee</th>
        <th>Last Message ({{.DisplayTZ}})</th>
      </tr>
    </thead>
    <tbody>
      {{range .Threads}}
      <tr>
        <td><a href="/threads/{{.Thread.ID}}">{{.Thread.SubjectDisplay}}</a></td>
        <td>{{.Thread.CustomerName}} &lt;{{.Thread.CustomerEmail}}&gt;</td>
        <td><span class="badge">{{.Thread.Status}}</span></td>
        <td>{{.AssigneeName}}</td>
        <td>{{.FormattedTime}}</td>
      </tr>
      {{else}}
      <tr><td colspan="5">No threads found.</td></tr>
      {{end}}
    </tbody>
  </table>
</div>
`

const inboxBody = `
<h1>Conversation Threads</h1>
<form method="get" action="/threads" class="card" hx-get="/threads" hx-target="#threads-table">
  <label>Status:
    <select name="status" data-hx-get="/threads">
      <option value="">All</option>
      <option value="new" {{if eq .StatusFilter "new"}}selected{{end}}>new</option>
      <option value="in_progress" {{if eq .StatusFilter "in_progress"}}selected{{end}}>in-progress</option>
      <option value="completed" {{if eq .StatusFilter "completed"}}selected{{end}}>completed</option>
      <option value="spam" {{if eq .StatusFilter "spam"}}selected{{end}}>spam</option>
    </select>
  </label>
  <button type="submit">Filter</button>
</form>
` + inboxTablePartial

const threadDetailBody = `
<div class="card">
  <h1>{{.Thread.SubjectDisplay}}</h1>
  <p>
    <strong>Status:</strong> <span class="badge">{{.Thread.Status}}</span> |
    <strong>Customer:</strong> {{.Thread.CustomerName}} &lt;{{.Thread.CustomerEmail}}&gt; |
    <strong>Assignee:</strong> {{.AssigneeName}} ({{.Thread.AssignmentSource}})
  </p>

  {{if .ReopensThread}}
  <div class="banner-reopen">
    <strong>Completed Thread Found:</strong>
    This email is a reply to already completed thread
    <a href="/threads/{{.ReopensThread.ID}}">{{.ReopensThread.SubjectDisplay}} ({{.ReopensThread.ID}})</a>.
    <form method="post" action="/threads/{{.Thread.ID}}/reopen-merge" style="margin-top:0.5rem">
      <button type="submit">Reopen completed thread &amp; merge this email into existing thread</button>
    </form>
  </div>
  {{end}}

  <div style="display:flex; gap:1rem; align-items:center; flex-wrap:wrap;">
    {{if eq .Thread.Status "new"}}
    <form method="post" action="/threads/{{.Thread.ID}}/transition">
      <input type="hidden" name="status" value="in_progress">
      <button type="submit">Start Progress (new -&gt; in-progress)</button>
    </form>
    <form method="post" action="/threads/{{.Thread.ID}}/transition">
      <input type="hidden" name="status" value="spam">
      <button type="submit">Mark as Spam (Move to Junk)</button>
    </form>
    {{end}}
    {{if eq .Thread.Status "in_progress"}}
    <form method="post" action="/threads/{{.Thread.ID}}/transition">
      <input type="hidden" name="status" value="completed">
      <button type="submit">Complete Thread (Move to Archive)</button>
    </form>
    {{end}}

    <form method="post" action="/threads/{{.Thread.ID}}/assign">
      <label>Assignee:
        <select name="assignee_id">
          {{range .Users}}
          <option value="{{.ID}}" {{if eq .ID $.Thread.AssigneeID}}selected{{end}}>{{.DisplayName}} ({{.Email}})</option>
          {{end}}
        </select>
      </label>
      <button type="submit">Assign</button>
    </form>
  </div>
</div>

<h2>Messages</h2>
{{range .Emails}}
<article class="card">
  <header>
    <strong>{{.Email.Direction | printf "%s"}}:</strong> {{.Email.FromName}} &lt;{{.Email.FromAddress}}&gt;
    <span> — {{.FormattedSent}}</span>
  </header>
  <iframe
    class="email-frame"
    src="/emails/{{.Email.ID}}/body"
    sandbox="allow-popups allow-popups-to-escape-sandbox"
    referrerpolicy="no-referrer"
    credentialless
    loading="lazy">
  </iframe>
  {{if .Attachments}}
  <div>
    <strong>Attachments:</strong>
    <ul>
      {{range .Attachments}}
      <li><a href="/attachments/{{.ID}}/download">{{.Filename}} ({{.SizeBytes}} bytes)</a></li>
      {{end}}
    </ul>
  </div>
  {{end}}
</article>
{{end}}

<section class="card">
  <h3>Send Reply</h3>
  <form method="post" action="/threads/{{.Thread.ID}}/reply" enctype="multipart/form-data">
    <div>
      <textarea name="body_text" rows="5" style="width:100%" required placeholder="Write your reply..."></textarea>
    </div>
    <div style="margin-top:0.5rem">
      <label>Attachment: <input type="file" name="attachment"></label>
    </div>
    <div style="margin-top:0.5rem">
      <button type="submit">Send Reply via SMTP</button>
    </div>
  </form>
</section>

<section class="card">
  <h3>Audit Timeline ({{.DisplayTZ}})</h3>
  <ul>
    {{range .Events}}
    <li><code>{{.FormattedTime}}</code> — <strong>{{.Event.EventType}}</strong></li>
    {{end}}
  </ul>
</section>
`

const settingsBody = `
<h1>System Settings</h1>
<form method="post" action="/settings" class="card">
  <div>
    <label>Default Assignee:
      <select name="default_assignee_id" required>
        {{range .Users}}
        <option value="{{.ID}}" {{if eq .ID $.Settings.DefaultAssigneeID}}selected{{end}}>{{.DisplayName}} ({{.Email}})</option>
        {{end}}
      </select>
    </label>
  </div>
  <div>
    <label>Display Timezone (IANA):
      <input type="text" name="display_timezone" value="{{.Settings.DisplayTimezone}}" required>
    </label>
  </div>
  <div>
    <label>IMAP INBOX Folder:
      <input type="text" name="imap_inbox_mailbox" value="{{.Settings.IMAPInboxMailbox}}" required>
    </label>
  </div>
  <div>
    <label>IMAP Archive Folder:
      <input type="text" name="imap_archive_mailbox" value="{{.Settings.IMAPArchiveMailbox}}" required>
    </label>
  </div>
  <div>
    <label>IMAP Junk Folder:
      <input type="text" name="imap_junk_mailbox" value="{{.Settings.IMAPJunkMailbox}}" required>
    </label>
  </div>
  <div>
    <label>IMAP Sent Folder:
      <input type="text" name="imap_sent_mailbox" value="{{.Settings.IMAPSentMailbox}}" required>
    </label>
  </div>
  <div>
    <label>Subject Match Window (days):
      <input type="number" name="subject_match_window_days" value="{{.Settings.SubjectMatchWindowDays}}" min="1" max="365" required>
    </label>
  </div>
  <button type="submit">Save Settings</button>
</form>

<h2>Pre-Create User (Assignee)</h2>
<form method="post" action="/settings/users" class="card">
  <label>Email: <input type="email" name="email" required></label>
  <label>Display Name: <input type="text" name="display_name" required></label>
  <label>Role:
    <select name="role">
      <option value="agent">agent</option>
      <option value="admin">admin</option>
    </select>
  </label>
  <button type="submit">Pre-Create User</button>
</form>
`

const isolatedEmailBodyHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'none'; base-uri 'none'; object-src 'none'">
  <meta name="referrer" content="no-referrer">
</head>
<body style="font-family: system-ui, sans-serif; margin: 0.75rem; word-wrap: break-word;">
  {{if .HasHTML}}
    <div>{{.SanitizedHTML}}</div>
  {{else}}
    <pre style="white-space: pre-wrap; font-family: inherit; margin: 0;">{{.PlainText}}</pre>
  {{end}}
</body>
</html>
`
