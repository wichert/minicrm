package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/mail"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/web/views"
	"github.com/wichert/minicrm/internal/workflow"
)

// AccountConfig describes a configured mail account for display and option lists.
type AccountConfig struct {
	ID          string
	Name        string
	FromAddress string
}

// HandlerStore defines the persistence operations required by the web Server handlers.
type HandlerStore interface {
	ListThreads(ctx context.Context, f sqlc.ListThreadsFilter) ([]sqlc.Thread, error)
	GetThreadByID(ctx context.Context, id uuid.UUID) (sqlc.Thread, error)
	ListEmailsByThread(ctx context.Context, threadID uuid.UUID) ([]sqlc.Email, error)
	GetEmailByID(ctx context.Context, id uuid.UUID) (sqlc.Email, error)
	ListAttachmentsByEmail(ctx context.Context, emailID uuid.UUID) ([]sqlc.Attachment, error)
	GetAttachmentByID(ctx context.Context, id uuid.UUID) (sqlc.Attachment, error)
	ListThreadEvents(ctx context.Context, threadID uuid.UUID) ([]sqlc.ThreadEvent, error)
	ListUsers(ctx context.Context) ([]sqlc.User, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (sqlc.User, error)
	GetSystemSettings(ctx context.Context) (sqlc.SystemSettings, error)
	UpsertSystemSettings(ctx context.Context, s sqlc.SystemSettings) (sqlc.SystemSettings, error)
	ListAuditLogs(ctx context.Context, limit int) ([]sqlc.AuditLog, error)
	GetAccountSettings(ctx context.Context, accountID string) (sqlc.AccountSettings, error)
	ListAccountSettings(ctx context.Context) ([]sqlc.AccountSettings, error)
	UpsertAccountSettings(ctx context.Context, s sqlc.AccountSettings) (sqlc.AccountSettings, error)
}

// ErrThreadNotFound is returned when a thread does not exist or is not accessible to the user.
var ErrThreadNotFound = errors.New("thread not found")

const (
	flashSessionKey  = "minicrm_flash"
	threadListLimit  = 100
	flashKindSuccess = "success"
	flashKindError   = "error"
)

// Server holds dependencies for MiniCRM HTTP handlers.
type Server struct {
	store       HandlerStore
	auth        *auth.Service
	workflow    *workflow.Service
	outbox      *mail.OutboxWorker
	attachments *storage.Store
	audit       *audit.Logger
	sessions    *scs.SessionManager
	accounts    []AccountConfig
}

// NewServer constructs a web Server.
func NewServer(
	store HandlerStore,
	authSvc *auth.Service,
	wfSvc *workflow.Service,
	outbox *mail.OutboxWorker,
	attachments *storage.Store,
	auditLogger *audit.Logger,
	sessions *scs.SessionManager,
	accounts []AccountConfig,
) *Server {
	return &Server{
		store:       store,
		auth:        authSvc,
		workflow:    wfSvc,
		outbox:      outbox,
		attachments: attachments,
		audit:       auditLogger,
		sessions:    sessions,
		accounts:    accounts,
	}
}

// setFlash stores a one-shot notice in the SCS session for the next rendered page.
func (s *Server) setFlash(r *http.Request, kind, message string) {
	if s.sessions == nil || strings.TrimSpace(message) == "" {
		return
	}
	s.sessions.Put(r.Context(), flashSessionKey, kind+"\n"+message)
}

// popFlash consumes and returns the pending session notice.
func (s *Server) popFlash(r *http.Request) views.Flash {
	if s.sessions == nil {
		return views.Flash{}
	}
	raw := s.sessions.PopString(r.Context(), flashSessionKey)
	if raw == "" {
		return views.Flash{}
	}
	kind, message, ok := strings.Cut(raw, "\n")
	if !ok {
		return views.Flash{Kind: flashKindSuccess, Message: raw}
	}
	if kind == "" {
		kind = flashKindSuccess
	}
	return views.Flash{Kind: kind, Message: message}
}

// renderError renders the styled application error page with the given status code.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, heading, message string) {
	user, _ := auth.UserFromContext(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = views.RenderError(w, views.ErrorViewData{
		Title:       heading,
		ActiveNav:   "",
		CurrentUser: user,
		StatusCode:  status,
		Heading:     heading,
		Message:     message,
	})
}

// requireAdmin renders a styled 403 instead of a bare plain-text response.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := auth.UserFromContext(r.Context())
		if !ok || user.Role != "admin" {
			s.renderError(w, r, http.StatusForbidden, "Access denied", "You need administrator rights to view this page.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// accountName returns the display name of a configured mail account.
func (s *Server) accountName(accountID string) string {
	for _, a := range s.accounts {
		if a.ID == accountID {
			return a.Name
		}
	}
	return accountID
}

// accountFromAddress returns the outbound from address of a configured mail account.
func (s *Server) accountFromAddress(accountID string) string {
	for _, a := range s.accounts {
		if a.ID == accountID {
			return a.FromAddress
		}
	}
	return ""
}

// accessibleAccounts returns the configured accounts the user may access.
func (s *Server) accessibleAccounts(user sqlc.User) []views.AccountOption {
	out := make([]views.AccountOption, 0, len(s.accounts))
	for _, a := range s.accounts {
		if user.HasAccountAccess(a.ID) {
			out = append(out, views.AccountOption{ID: a.ID, Name: a.Name})
		}
	}
	return out
}

// loadAuthorizedThread loads a thread and verifies the user may access its mail account.
func (s *Server) loadAuthorizedThread(ctx context.Context, user sqlc.User, threadID uuid.UUID) (sqlc.Thread, error) {
	thread, err := s.store.GetThreadByID(ctx, threadID)
	if err != nil {
		return sqlc.Thread{}, ErrThreadNotFound
	}
	if !user.HasAccountAccess(thread.AccountID) {
		return sqlc.Thread{}, ErrThreadNotFound
	}
	return thread, nil
}

// assigneeFilterForUser returns the active users that may be offered as an inbox assignee filter.
func assigneeFilterUsers(allUsers []sqlc.User, current sqlc.User) []sqlc.User {
	out := make([]sqlc.User, 0, len(allUsers))
	for _, u := range allUsers {
		if !u.IsActive {
			continue
		}
		for _, accountID := range current.AllowedAccounts {
			if u.HasAccountAccess(accountID) {
				out = append(out, u)
				break
			}
		}
	}
	return out
}

// HandleListThreads renders the filterable thread inbox.
func (s *Server) HandleListThreads(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)

	statusStr := strings.TrimSpace(r.URL.Query().Get("status"))
	var statusFilter *sqlc.ThreadStatus
	switch sqlc.ThreadStatus(statusStr) {
	case sqlc.ThreadStatusNew, sqlc.ThreadStatusInProgress, sqlc.ThreadStatusCompleted, sqlc.ThreadStatusSpam:
		st := sqlc.ThreadStatus(statusStr)
		statusFilter = &st
	default:
		// Unknown or empty status values fall back to "All statuses" instead of reaching PostgreSQL.
		statusStr = ""
	}

	accountFilter := strings.TrimSpace(r.URL.Query().Get("account"))
	var accountIDFilter *string
	if accountFilter != "" && user.HasAccountAccess(accountFilter) {
		accountIDFilter = &accountFilter
	} else {
		accountFilter = ""
	}

	assigneeFilter := strings.TrimSpace(r.URL.Query().Get("assignee"))
	var assigneeID *uuid.UUID
	unassigned := false
	switch assigneeFilter {
	case "":
	case "me":
		id := user.ID
		assigneeID = &id
	case "unassigned":
		unassigned = true
	default:
		if parsed, err := uuid.Parse(assigneeFilter); err == nil {
			assigneeID = &parsed
		} else {
			assigneeFilter = ""
		}
	}

	var threads []sqlc.Thread
	if len(user.AllowedAccounts) > 0 {
		var err error
		threads, err = s.store.ListThreads(ctx, sqlc.ListThreadsFilter{
			Status:          statusFilter,
			AssigneeID:      assigneeID,
			Unassigned:      unassigned,
			AccountID:       accountIDFilter,
			AllowedAccounts: user.AllowedAccounts,
			Limit:           threadListLimit,
		})
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "Inbox unavailable", "We could not load the conversation threads. Please try again.")
			return
		}
	}
	users, _ := s.store.ListUsers(ctx)
	userNames := make(map[uuid.UUID]string, len(users))
	for _, u := range users {
		userNames[u.ID] = u.DisplayName
	}
	displayTZ := s.currentDisplayTimezone(ctx)

	items := make([]views.ThreadSummaryItem, 0, len(threads))
	for _, th := range threads {
		assigneeName := ""
		if th.AssigneeID != nil {
			assigneeName = userNames[*th.AssigneeID]
		}
		items = append(items, views.ThreadSummaryItem{
			Thread:        th,
			AssigneeName:  assigneeName,
			AccountName:   s.accountName(th.AccountID),
			FormattedTime: views.FormatTimestamp(th.LastMessageAt, displayTZ),
		})
	}

	s.audit.LogDataAccess(ctx, "thread_list", "inbox")

	partial := r.Header.Get("HX-Request") == "true"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.RenderInbox(w, views.InboxViewData{
		Title:          "Threads",
		Flash:          s.popFlash(r),
		ActiveNav:      "threads",
		CurrentUser:    user,
		Threads:        items,
		StatusFilter:   statusStr,
		AssigneeFilter: assigneeFilter,
		Users:          assigneeFilterUsers(users, user),
		DisplayTZ:      displayTZ,
		ShowAccountUI:  user.HasMultipleAccounts(),
		AccountFilter:  accountFilter,
		Accounts:       s.accessibleAccounts(user),
		Truncated:      len(threads) >= threadListLimit,
		Limit:          threadListLimit,
	}, partial); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Inbox unavailable", "We could not render the conversation threads.")
	}
}

// HandleViewThread renders a single conversation thread and its messages.
func (s *Server) HandleViewThread(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Invalid thread", "That conversation link is not valid.")
		return
	}

	thread, err := s.loadAuthorizedThread(ctx, user, threadID)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Thread not found", "This conversation does not exist or you do not have access to its mail account.")
		return
	}

	s.audit.LogDataAccess(ctx, "thread", thread.ID.String())
	s.renderThreadDetail(w, r, user, thread, http.StatusOK, nil, nil, "")
}

// renderThreadDetail loads a thread's messages and activity and renders the detail page.
// headerErrors render in the thread header card (transition/assign/merge failures); replyErrors
// render next to the reply field so a reply failure is never shown twice.
func (s *Server) renderThreadDetail(w http.ResponseWriter, r *http.Request, user sqlc.User, thread sqlc.Thread, status int, headerErrors, replyErrors []string, replyDraft string) {
	ctx := r.Context()

	var reopensThread *sqlc.Thread
	reopenCompletedAt := ""
	reopenCompletedBy := ""
	if thread.ReopensThreadID != nil {
		if prev, err := s.store.GetThreadByID(ctx, *thread.ReopensThreadID); err == nil {
			reopensThread = &prev
		}
	}

	emails, err := s.store.ListEmailsByThread(ctx, thread.ID)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Thread unavailable", "We could not load this conversation's messages.")
		return
	}
	displayTZ := s.currentDisplayTimezone(ctx)
	emailViews := make([]views.EmailItemView, 0, len(emails))
	for _, em := range emails {
		atts, _ := s.store.ListAttachmentsByEmail(ctx, em.ID)
		emailViews = append(emailViews, views.EmailItemView{
			Email:         em,
			FormattedSent: views.FormatTimestamp(em.SentAt, displayTZ),
			Attachments:   atts,
		})
	}

	allUsers, _ := s.store.ListUsers(ctx)
	userNames := make(map[uuid.UUID]string, len(allUsers))
	for _, u := range allUsers {
		userNames[u.ID] = u.DisplayName
	}
	// Only users with access to the thread's account may be offered as assignees.
	users := make([]sqlc.User, 0, len(allUsers))
	for _, u := range allUsers {
		if u.IsActive && u.HasAccountAccess(thread.AccountID) {
			users = append(users, u)
		}
	}
	assigneeName := ""
	if thread.AssigneeID != nil {
		assigneeName = userNames[*thread.AssigneeID]
	}

	rawEvents, _ := s.store.ListThreadEvents(ctx, thread.ID)
	eventViews := make([]views.EventItemView, 0, len(rawEvents))
	for _, ev := range rawEvents {
		actorName := ""
		if ev.ActorUserID != nil {
			actorName = userNames[*ev.ActorUserID]
		}
		eventViews = append(eventViews, views.EventItemView{
			Event:         ev,
			FormattedTime: views.FormatTimestamp(ev.OccurredAt, displayTZ),
			Summary:       views.FormatThreadEventSummary(ev, actorName, userNames),
		})
	}

	// A reopened thread carries both its own "created" event and the reparented follow-up thread's
	// events. Label that merged block so the duplicated "opened this thread" line reads as history.
	if sectionIndex := mergedFollowUpEventIndex(rawEvents); sectionIndex >= 0 {
		eventViews[sectionIndex].Section = "Follow-up thread"
	}

	if reopensThread != nil {
		prevEvents, _ := s.store.ListThreadEvents(ctx, reopensThread.ID)
		for _, ev := range prevEvents {
			if ev.EventType != "status_changed" || !eventTargetsStatus(ev.NewValue, string(sqlc.ThreadStatusCompleted)) {
				continue
			}
			reopenCompletedAt = views.FormatTimestamp(ev.OccurredAt, displayTZ)
			if ev.ActorUserID != nil {
				reopenCompletedBy = userNames[*ev.ActorUserID]
			}
		}
	}

	replyAllowed := thread.Status != sqlc.ThreadStatusSpam && thread.Status != sqlc.ThreadStatusCompleted

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if err := views.RenderThreadDetail(w, views.ThreadDetailViewData{
		Title:              thread.SubjectDisplay,
		Flash:              s.popFlash(r),
		ActiveNav:          "threads",
		CurrentUser:        user,
		Thread:             thread,
		AssigneeName:       assigneeName,
		ReopensThread:      reopensThread,
		ReopenCompletedAt:  reopenCompletedAt,
		ReopenCompletedBy:  reopenCompletedBy,
		Emails:             emailViews,
		Events:             eventViews,
		Users:              users,
		DisplayTZ:          displayTZ,
		ShowAccountUI:      user.HasMultipleAccounts(),
		AccountName:        s.accountName(thread.AccountID),
		AccountFromAddress: s.accountFromAddress(thread.AccountID),
		ReplyAllowed:       replyAllowed,
		ReplyDraft:         replyDraft,
		Errors:             headerErrors,
		ReplyErrors:        replyErrors,
	}); err != nil && status == http.StatusOK {
		s.renderError(w, r, http.StatusInternalServerError, "Thread unavailable", "We could not render this conversation.")
	}
}

// mergedFollowUpEventIndex returns the index of the reparented follow-up thread's "created" event
// in a merged thread's event list, or -1 when the thread was never reopened and merged.
func mergedFollowUpEventIndex(events []sqlc.ThreadEvent) int {
	followUpStart := -1
	created := 0
	for i, ev := range events {
		if ev.EventType != "created" {
			continue
		}
		created++
		if created == 2 {
			followUpStart = i
			break
		}
	}
	if followUpStart < 0 {
		return -1
	}
	for i := followUpStart + 1; i < len(events); i++ {
		if events[i].EventType == "reopen_merged" {
			return followUpStart
		}
	}
	return -1
}

// eventTargetsStatus reports whether a status_changed event's new_value contains status.
func eventTargetsStatus(raw json.RawMessage, status string) bool {
	values := make(map[string]string)
	if err := json.Unmarshal(raw, &values); err != nil {
		return false
	}
	return values["status"] == status
}

// HandleIsolatedEmailBody serves the sanitized email body inside a sandboxed iframe with strict CSP.
func (s *Server) HandleIsolatedEmailBody(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	emailID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid email id", http.StatusBadRequest)
		return
	}
	user, _ := auth.UserFromContext(ctx)
	emailRec, err := s.store.GetEmailByID(ctx, emailID)
	if err != nil || !user.HasAccountAccess(emailRec.AccountID) {
		http.Error(w, "email not found", http.StatusNotFound)
		return
	}

	s.audit.LogDataAccess(ctx, "email_body", emailRec.ID.String())
	SetIsolatedEmailBodyHeaders(w)
	if err := views.RenderIsolatedEmailBody(w, emailRec); err != nil {
		http.Error(w, "render email body failed", http.StatusInternalServerError)
	}
}

// HandleTransitionThread transitions a thread's workflow status.
func (s *Server) HandleTransitionThread(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Invalid thread", "That conversation link is not valid.")
		return
	}
	if err := parseBoundedForm(w, r, 64<<10); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Invalid request", "The submitted form could not be read.")
		return
	}
	thread, err := s.loadAuthorizedThread(ctx, user, threadID)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Thread not found", "This conversation does not exist or you do not have access to its mail account.")
		return
	}
	targetStatus := sqlc.ThreadStatus(strings.TrimSpace(r.FormValue("status")))
	if err := s.workflow.Transition(ctx, threadID, targetStatus, &user.ID); err != nil {
		if errors.Is(err, workflow.ErrInvalidTransition) {
			s.renderThreadDetail(w, r, user, thread, http.StatusUnprocessableEntity,
				[]string{"That status change is not allowed from the thread's current state."}, nil, "")
			return
		}
		s.renderError(w, r, http.StatusInternalServerError, "Update failed", "The status change could not be saved. Please try again.")
		return
	}
	s.setFlash(r, flashKindSuccess, fmt.Sprintf("Thread marked as %s.", views.FormatStatus(targetStatus)))
	redirectToThread(w, r, threadID)
}

// HandleAssignThread changes the assignee of a thread.
func (s *Server) HandleAssignThread(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Invalid thread", "That conversation link is not valid.")
		return
	}
	if err := parseBoundedForm(w, r, 64<<10); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Invalid request", "The submitted form could not be read.")
		return
	}
	thread, err := s.loadAuthorizedThread(ctx, user, threadID)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Thread not found", "This conversation does not exist or you do not have access to its mail account.")
		return
	}
	switch thread.Status {
	case sqlc.ThreadStatusSpam:
		s.renderThreadDetail(w, r, user, thread, http.StatusUnprocessableEntity,
			[]string{"Spam threads cannot be reassigned."}, nil, "")
		return
	case sqlc.ThreadStatusCompleted:
		s.renderThreadDetail(w, r, user, thread, http.StatusUnprocessableEntity,
			[]string{"Completed threads cannot be reassigned. Reopen and merge the customer's follow-up reply first."}, nil, "")
		return
	}
	assigneeID, err := uuid.Parse(strings.TrimSpace(r.FormValue("assignee_id")))
	if err != nil {
		s.setFlash(r, flashKindError, "Choose an assignee before assigning this thread.")
		redirectToThread(w, r, threadID)
		return
	}
	if err := s.workflow.Reassign(ctx, threadID, assigneeID, &user.ID); err != nil {
		message := "The thread could not be assigned. Please try again."
		if errors.Is(err, workflow.ErrAssigneeNoAccountAccess) {
			message = "That person does not have access to this thread's mail account."
		}
		s.renderThreadDetail(w, r, user, thread, http.StatusUnprocessableEntity, []string{message}, nil, "")
		return
	}
	assigneeName := ""
	if assignee, err := s.store.GetUserByID(ctx, assigneeID); err == nil {
		assigneeName = assignee.DisplayName
	}
	s.setFlash(r, flashKindSuccess, fmt.Sprintf("Thread assigned to %s.", assigneeName))
	redirectToThread(w, r, threadID)
}

// HandleReopenMerge merges a follow-up thread into its referenced completed thread and reopens it.
func (s *Server) HandleReopenMerge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Invalid thread", "That conversation link is not valid.")
		return
	}
	thread, err := s.loadAuthorizedThread(ctx, user, threadID)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Thread not found", "This conversation does not exist or you do not have access to its mail account.")
		return
	}
	reopenedID, err := s.workflow.ReopenAndMerge(ctx, threadID, &user.ID)
	if err != nil {
		if errors.Is(err, workflow.ErrCannotReopenMerge) {
			s.renderThreadDetail(w, r, user, thread, http.StatusUnprocessableEntity,
				[]string{"This follow-up can no longer be reopened and merged."}, nil, "")
			return
		}
		s.renderError(w, r, http.StatusInternalServerError, "Merge failed", "The thread could not be reopened and merged. Please try again.")
		return
	}
	s.setFlash(r, flashKindSuccess, "Thread reopened and merged into the original conversation.")
	redirectToThread(w, r, reopenedID)
}

// HandleReplyThread composes and queues an outbound reply via SMTP.
func (s *Server) HandleReplyThread(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Invalid thread", "That conversation link is not valid.")
		return
	}
	thread, err := s.loadAuthorizedThread(ctx, user, threadID)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Thread not found", "This conversation does not exist or you do not have access to its mail account.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	// #nosec G120 -- request body is bounded via http.MaxBytesReader above
	if err := r.ParseMultipartForm(16 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		// #nosec G120 -- request body is bounded via http.MaxBytesReader above
		_ = r.ParseForm()
	}
	bodyText := strings.TrimSpace(r.FormValue("body_text"))

	// Terminal threads cannot receive outbound mail: spam is a dead end, and a completed
	// thread must be reopened via Reopen & Merge before it can be discussed again.
	switch thread.Status {
	case sqlc.ThreadStatusSpam:
		s.audit.LogDataAccess(ctx, "thread", thread.ID.String())
		s.renderThreadDetail(w, r, user, thread, http.StatusUnprocessableEntity,
			nil, []string{"Replies are disabled on spam threads."}, bodyText)
		return
	case sqlc.ThreadStatusCompleted:
		s.audit.LogDataAccess(ctx, "thread", thread.ID.String())
		s.renderThreadDetail(w, r, user, thread, http.StatusUnprocessableEntity,
			nil, []string{"This thread is completed. Reopen and merge the customer's follow-up reply before sending a reply."}, bodyText)
		return
	}

	var uploaded []mail.OutgoingAttachmentInput
	if r.MultipartForm != nil {
		if files := r.MultipartForm.File["attachment"]; len(files) > 0 {
			for _, fh := range files {
				f, err := fh.Open()
				if err != nil {
					continue
				}
				data, err := io.ReadAll(io.LimitReader(f, 16<<20))
				_ = f.Close()
				if err == nil && len(data) > 0 {
					uploaded = append(uploaded, mail.OutgoingAttachmentInput{
						Filename:    fh.Filename,
						ContentType: fh.Header.Get("Content-Type"),
						Data:        data,
					})
				}
			}
		}
	}

	if _, err := s.outbox.ComposeAndQueueReply(ctx, mail.ComposeReplyParams{
		ThreadID:     threadID,
		SenderUserID: user.ID,
		BodyText:     bodyText,
		Attachments:  uploaded,
	}); err != nil {
		s.audit.LogDataAccess(ctx, "thread", thread.ID.String())
		s.renderThreadDetail(w, r, user, thread, http.StatusUnprocessableEntity, nil, []string{err.Error()}, bodyText)
		return
	}
	s.setFlash(r, flashKindSuccess, "Reply queued for delivery.")
	redirectToThread(w, r, threadID)
}

// HandleDownloadAttachment streams an attachment file from local disk with hardened download headers.
func (s *Server) HandleDownloadAttachment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	attID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid attachment id", http.StatusBadRequest)
		return
	}
	user, _ := auth.UserFromContext(ctx)
	att, err := s.store.GetAttachmentByID(ctx, attID)
	if err != nil {
		http.Error(w, "attachment not found", http.StatusNotFound)
		return
	}
	// Attachments are only served when the user has access to the mail account of the
	// email/thread the attachment belongs to, even when the blob is shared across accounts.
	ownerEmail, err := s.store.GetEmailByID(ctx, att.EmailID)
	if err != nil || !user.HasAccountAccess(ownerEmail.AccountID) {
		http.Error(w, "attachment not found", http.StatusNotFound)
		return
	}
	f, err := s.attachments.Open(ctx, att.StorageRelPath)
	if err != nil {
		http.Error(w, "attachment file unavailable", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()

	s.audit.LogDataAccess(ctx, "attachment", att.ID.String())
	SetAttachmentDownloadHeaders(w, att.Filename, att.ContentType)
	_, _ = io.Copy(w, f)
}

func (s *Server) currentDisplayTimezone(ctx context.Context) string {
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil || settings.DisplayTimezone == "" {
		return "UTC"
	}
	return settings.DisplayTimezone
}

func parseBoundedForm(w http.ResponseWriter, r *http.Request, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err == nil && mediaType == "multipart/form-data" {
		// #nosec G120 -- request body is bounded via http.MaxBytesReader above
		return r.ParseMultipartForm(maxBytes)
	}
	return r.ParseForm()
}

func redirectToThread(w http.ResponseWriter, r *http.Request, threadID uuid.UUID) {
	target := fmt.Sprintf("/threads/%s", url.PathEscape(threadID.String()))
	// #nosec G710 -- target is a fixed internal route constructed from a validated uuid.UUID
	http.Redirect(w, r, target, http.StatusSeeOther)
}
