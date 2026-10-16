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

	s.audit.LogDataAccess(ctx, "system_settings", "1")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.RenderSettings(w, views.SettingsViewData{
		CurrentUser: user,
		Settings:    settings,
		Users:       users,
		AuditLogs:   logs,
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

	defaultAssigneeID, err := uuid.Parse(strings.TrimSpace(r.FormValue("default_assignee_id")))
	if err != nil {
		http.Error(w, "invalid default_assignee_id", http.StatusBadRequest)
		return
	}
	assigneeUser, err := s.store.GetUserByID(ctx, defaultAssigneeID)
	if err != nil || !assigneeUser.IsActive {
		http.Error(w, "default assignee must be an active user", http.StatusBadRequest)
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
		DefaultAssigneeID:      defaultAssigneeID,
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

	if _, err := s.auth.PrecreateUser(ctx, user.ID, email, displayName, role); err != nil {
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
