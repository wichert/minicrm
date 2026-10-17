package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/web/views"
)

// HandleGetSettings renders the admin settings and user management page.
func (s *Server) HandleGetSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)

	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		http.Error(w, "failed to load settings", http.StatusInternalServerError)
		return
	}
	users, _ := s.store.ListUsers(ctx)
	logs, _ := s.store.ListAuditLogs(ctx, 50)

	accounts := make([]views.AccountOption, 0, len(s.accounts))
	accountViews := make([]views.AccountSettingsView, 0, len(s.accounts))
	for _, a := range s.accounts {
		accounts = append(accounts, views.AccountOption{ID: a.ID, Name: a.Name})
		view := views.AccountSettingsView{Account: views.AccountOption{ID: a.ID, Name: a.Name}}
		if as, err := s.store.GetAccountSettings(ctx, a.ID); err == nil {
			view.DefaultAssigneeID = as.DefaultAssigneeID
		}
		accountViews = append(accountViews, view)
	}

	s.audit.LogDataAccess(ctx, "system_settings", "1")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.RenderSettings(w, views.SettingsViewData{
		CurrentUser:     user,
		Settings:        settings,
		Users:           users,
		AuditLogs:       logs,
		Accounts:        accounts,
		AccountSettings: accountViews,
	}); err != nil {
		http.Error(w, "template render error", http.StatusInternalServerError)
	}
}

// HandleUpdateSettings validates and updates system_settings, recording a settings_updated audit log.
func (s *Server) HandleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	if err := parseBoundedForm(w, r, 64<<10); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	displayTZ := strings.TrimSpace(r.FormValue("display_timezone"))
	if displayTZ == "" {
		displayTZ = "UTC"
	}
	if _, err := time.LoadLocation(displayTZ); err != nil {
		http.Error(w, "invalid IANA display_timezone", http.StatusBadRequest)
		return
	}

	windowDays, err := strconv.Atoi(strings.TrimSpace(r.FormValue("subject_match_window_days")))
	if err != nil || windowDays <= 0 {
		windowDays = 30
	}

	oldSettings, _ := s.store.GetSystemSettings(ctx)

	updated, err := s.store.UpsertSystemSettings(ctx, sqlc.SystemSettings{
		ID:                     1,
		DisplayTimezone:        displayTZ,
		IMAPInboxMailbox:       defaultOrTrim(r.FormValue("imap_inbox_mailbox"), "INBOX"),
		IMAPArchiveMailbox:     defaultOrTrim(r.FormValue("imap_archive_mailbox"), "Archive"),
		IMAPJunkMailbox:        defaultOrTrim(r.FormValue("imap_junk_mailbox"), "Junk"),
		IMAPSentMailbox:        defaultOrTrim(r.FormValue("imap_sent_mailbox"), "Sent"),
		SubjectMatchWindowDays: windowDays,
	})
	if err != nil {
		http.Error(w, "failed to save settings", http.StatusInternalServerError)
		return
	}

	_ = s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
		ActorUserID: &user.ID,
		EventType:   "settings_updated",
		IPAddress:   r.RemoteAddr,
		UserAgent:   r.UserAgent(),
		OldValue:    oldSettings,
		NewValue:    updated,
	})

	// Per-account default assignees. An empty selection leaves the account without a
	// default assignee, so new threads for it stay unassigned.
	for _, account := range s.accounts {
		field := "account_default_assignee_" + account.ID
		raw := strings.TrimSpace(r.FormValue(field))
		oldAccountSettings, _ := s.store.GetAccountSettings(ctx, account.ID)

		var assigneeID *uuid.UUID
		if raw != "" {
			parsed, err := uuid.Parse(raw)
			if err != nil {
				http.Error(w, "invalid default assignee for account "+account.ID, http.StatusBadRequest)
				return
			}
			assigneeUser, err := s.store.GetUserByID(ctx, parsed)
			if err != nil || !assigneeUser.IsActive || !assigneeUser.HasAccountAccess(account.ID) {
				http.Error(w, "default assignee must be an active user with access to account "+account.ID, http.StatusBadRequest)
				return
			}
			assigneeID = &parsed
		}

		saved, err := s.store.UpsertAccountSettings(ctx, sqlc.AccountSettings{
			AccountID:         account.ID,
			DefaultAssigneeID: assigneeID,
		})
		if err != nil {
			http.Error(w, "failed to save account settings", http.StatusInternalServerError)
			return
		}
		_ = s.audit.LogSystemEvent(ctx, audit.SystemEventParams{
			ActorUserID: &user.ID,
			EventType:   "settings_updated",
			IPAddress:   r.RemoteAddr,
			UserAgent:   r.UserAgent(),
			OldValue:    oldAccountSettings,
			NewValue:    saved,
		})

		// Pick up threads that were waiting for a default assignee.
		if assigneeID != nil {
			if _, err := s.workflow.AssignUnassignedThreadsForAccount(ctx, account.ID, *assigneeID, &user.ID); err != nil {
				http.Error(w, "failed to assign waiting threads", http.StatusInternalServerError)
				return
			}
		}
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// HandlePrecreateUser allows an admin to pre-create an assignee user before their first OIDC login.
func (s *Server) HandlePrecreateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	if err := parseBoundedForm(w, r, 64<<10); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	displayName := strings.TrimSpace(r.FormValue("display_name"))
	role := strings.TrimSpace(r.FormValue("role"))

	known := make(map[string]struct{}, len(s.accounts))
	for _, a := range s.accounts {
		known[a.ID] = struct{}{}
	}
	selected := make([]string, 0, len(s.accounts))
	for _, id := range r.Form["allowed_accounts"] {
		if _, ok := known[id]; !ok {
			http.Error(w, "unknown account "+id, http.StatusBadRequest)
			return
		}
		selected = append(selected, id)
	}

	if _, err := s.auth.PrecreateUser(ctx, user.ID, email, displayName, role, selected...); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func defaultOrTrim(val, fallback string) string {
	v := strings.TrimSpace(val)
	if v == "" {
		return fallback
	}
	return v
}
