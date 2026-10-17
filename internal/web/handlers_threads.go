package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

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
	ID   string
	Name string
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

// Server holds dependencies for MiniCRM HTTP handlers.
type Server struct {
	store       HandlerStore
	auth        *auth.Service
	workflow    *workflow.Service
	outbox      *mail.OutboxWorker
	attachments *storage.Store
	audit       *audit.Logger
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
	accounts []AccountConfig,
) *Server {
	return &Server{
		store:       store,
		auth:        authSvc,
		workflow:    wfSvc,
		outbox:      outbox,
		attachments: attachments,
		audit:       auditLogger,
		accounts:    accounts,
	}
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

// HandleListThreads renders the filterable thread inbox.
func (s *Server) HandleListThreads(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)

	statusStr := strings.TrimSpace(r.URL.Query().Get("status"))
	var statusFilter *sqlc.ThreadStatus
	if statusStr != "" {
		st := sqlc.ThreadStatus(statusStr)
		statusFilter = &st
	}

	accountFilter := strings.TrimSpace(r.URL.Query().Get("account"))
	var accountIDFilter *string
	if accountFilter != "" && user.HasAccountAccess(accountFilter) {
		accountIDFilter = &accountFilter
	} else {
		accountFilter = ""
	}

	var threads []sqlc.Thread
	if len(user.AllowedAccounts) > 0 {
		var err error
		threads, err = s.store.ListThreads(ctx, sqlc.ListThreadsFilter{
			Status:          statusFilter,
			AccountID:       accountIDFilter,
			AllowedAccounts: user.AllowedAccounts,
			Limit:           100,
		})
		if err != nil {
			http.Error(w, "failed to list threads", http.StatusInternalServerError)
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
		CurrentUser:   user,
		Threads:       items,
		StatusFilter:  statusStr,
		Users:         users,
		DisplayTZ:     displayTZ,
		ShowAccountUI: user.HasMultipleAccounts(),
		AccountFilter: accountFilter,
		Accounts:      s.accessibleAccounts(user),
	}, partial); err != nil {
		http.Error(w, "template render error", http.StatusInternalServerError)
	}
}

// HandleViewThread renders a single conversation thread and its messages.
func (s *Server) HandleViewThread(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid thread id", http.StatusBadRequest)
		return
	}

	thread, err := s.loadAuthorizedThread(ctx, user, threadID)
	if err != nil {
		http.Error(w, "thread not found", http.StatusNotFound)
		return
	}

	var reopensThread *sqlc.Thread
	if thread.ReopensThreadID != nil {
		if prev, err := s.store.GetThreadByID(ctx, *thread.ReopensThreadID); err == nil {
			reopensThread = &prev
		}
	}

	emails, err := s.store.ListEmailsByThread(ctx, threadID)
	if err != nil {
		http.Error(w, "failed to load thread emails", http.StatusInternalServerError)
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

	rawEvents, _ := s.store.ListThreadEvents(ctx, threadID)
	eventViews := make([]views.EventItemView, 0, len(rawEvents))
	for _, ev := range rawEvents {
		eventViews = append(eventViews, views.EventItemView{
			Event:         ev,
			FormattedTime: views.FormatTimestamp(ev.OccurredAt, displayTZ),
		})
	}

	allUsers, _ := s.store.ListUsers(ctx)
	// Only users with access to the thread's account may be offered as assignees.
	users := make([]sqlc.User, 0, len(allUsers))
	for _, u := range allUsers {
		if u.IsActive && u.HasAccountAccess(thread.AccountID) {
			users = append(users, u)
		}
	}
	assigneeName := ""
	if thread.AssigneeID != nil {
		for _, u := range allUsers {
			if u.ID == *thread.AssigneeID {
				assigneeName = u.DisplayName
				break
			}
		}
	}

	s.audit.LogDataAccess(ctx, "thread", thread.ID.String())

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.RenderThreadDetail(w, views.ThreadDetailViewData{
		CurrentUser:   user,
		Thread:        thread,
		AssigneeName:  assigneeName,
		ReopensThread: reopensThread,
		Emails:        emailViews,
		Events:        eventViews,
		Users:         users,
		DisplayTZ:     displayTZ,
		ShowAccountUI: user.HasMultipleAccounts(),
		AccountName:   s.accountName(thread.AccountID),
	}); err != nil {
		http.Error(w, "template render error", http.StatusInternalServerError)
	}
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
		http.Error(w, "invalid thread id", http.StatusBadRequest)
		return
	}
	if err := parseBoundedForm(w, r, 64<<10); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if _, err := s.loadAuthorizedThread(ctx, user, threadID); err != nil {
		http.Error(w, "thread not found", http.StatusNotFound)
		return
	}
	targetStatus := sqlc.ThreadStatus(strings.TrimSpace(r.FormValue("status")))
	if err := s.workflow.Transition(ctx, threadID, targetStatus, &user.ID); err != nil {
		if errors.Is(err, workflow.ErrInvalidTransition) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, "transition failed", http.StatusInternalServerError)
		return
	}
	redirectToThread(w, r, threadID)
}

// HandleAssignThread changes the assignee of a thread.
func (s *Server) HandleAssignThread(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid thread id", http.StatusBadRequest)
		return
	}
	if err := parseBoundedForm(w, r, 64<<10); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if _, err := s.loadAuthorizedThread(ctx, user, threadID); err != nil {
		http.Error(w, "thread not found", http.StatusNotFound)
		return
	}
	assigneeID, err := uuid.Parse(strings.TrimSpace(r.FormValue("assignee_id")))
	if err != nil {
		http.Error(w, "invalid assignee_id", http.StatusBadRequest)
		return
	}
	if err := s.workflow.Reassign(ctx, threadID, assigneeID, &user.ID); err != nil {
		if errors.Is(err, workflow.ErrAssigneeNoAccountAccess) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	redirectToThread(w, r, threadID)
}

// HandleReopenMerge merges a follow-up thread into its referenced completed thread and reopens it.
func (s *Server) HandleReopenMerge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid thread id", http.StatusBadRequest)
		return
	}
	if _, err := s.loadAuthorizedThread(ctx, user, threadID); err != nil {
		http.Error(w, "thread not found", http.StatusNotFound)
		return
	}
	reopenedID, err := s.workflow.ReopenAndMerge(ctx, threadID, &user.ID)
	if err != nil {
		if errors.Is(err, workflow.ErrCannotReopenMerge) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, "reopen and merge failed", http.StatusInternalServerError)
		return
	}
	redirectToThread(w, r, reopenedID)
}

// HandleReplyThread composes and queues an outbound reply via SMTP.
func (s *Server) HandleReplyThread(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	threadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid thread id", http.StatusBadRequest)
		return
	}
	if _, err := s.loadAuthorizedThread(ctx, user, threadID); err != nil {
		http.Error(w, "thread not found", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	// #nosec G120 -- request body is bounded via http.MaxBytesReader above
	if err := r.ParseMultipartForm(16 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		// #nosec G120 -- request body is bounded via http.MaxBytesReader above
		_ = r.ParseForm()
	}
	bodyText := strings.TrimSpace(r.FormValue("body_text"))

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
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
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
	return r.ParseForm()
}

func redirectToThread(w http.ResponseWriter, r *http.Request, threadID uuid.UUID) {
	target := fmt.Sprintf("/threads/%s", url.PathEscape(threadID.String()))
	// #nosec G710 -- target is a fixed internal route constructed from a validated uuid.UUID
	http.Redirect(w, r, target, http.StatusSeeOther)
}
