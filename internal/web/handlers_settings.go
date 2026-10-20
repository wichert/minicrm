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

// settingsForm holds typed form values so a validation error re-renders without losing input.
type settingsForm struct {
	Settings        *sqlc.SystemSettings
	AccountDefaults map[string]string
	NewUser         views.NewUserForm
}

// HandleGetSettings renders the admin settings and user management page.
func (s *Server) HandleGetSettings(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	s.renderSettings(w, r, user, http.StatusOK, nil, nil)
}

// renderSettings builds and renders the settings page, optionally using typed form values.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, user sqlc.User, status int, form *settingsForm, formErrors []string) {
	ctx := r.Context()

	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Settings unavailable", "We could not load the system settings.")
		return
	}
	users, _ := s.store.ListUsers(ctx)
	logs, _ := s.store.ListAuditLogs(ctx, 50)

	accounts := make([]views.AccountOption, 0, len(s.accounts))
	accountNames := make(map[string]string, len(s.accounts))
	accountViews := make([]views.AccountSettingsView, 0, len(s.accounts))
	for _, a := range s.accounts {
		accounts = append(accounts, views.AccountOption{ID: a.ID, Name: a.Name})
		accountNames[a.ID] = a.Name
		view := views.AccountSettingsView{Account: views.AccountOption{ID: a.ID, Name: a.Name}}
		if as, err := s.store.GetAccountSettings(ctx, a.ID); err == nil && as.DefaultAssigneeID != nil {
			view.DefaultAssigneeValue = as.DefaultAssigneeID.String()
		}
		accountViews = append(accountViews, view)
	}

	userNames := make(map[uuid.UUID]string, len(users))
	for _, u := range users {
		userNames[u.ID] = u.DisplayName
	}

	auditEntries := make([]views.AuditEntryView, 0, len(logs))
	for _, entry := range logs {
		actorName := "System"
		if entry.ActorUserID != nil {
			if name := userNames[*entry.ActorUserID]; name != "" {
				actorName = name
			}
		}
		targetName := ""
		if entry.TargetUserID != nil {
			targetName = userNames[*entry.TargetUserID]
		}
		label := views.FormatAuditEvent(entry.EventType)
		if label == "Settings updated" {
			targetName = "System settings"
		}
		auditEntries = append(auditEntries, views.AuditEntryView{
			FormattedTime: views.FormatTimestamp(entry.OccurredAt, settings.DisplayTimezone),
			FullTime:      views.FormatTimestampFull(entry.OccurredAt, settings.DisplayTimezone),
			ActorName:     actorName,
			TargetName:    targetName,
			Label:         label,
		})
	}

	newUser := views.NewUserForm{Role: "agent"}
	if form != nil {
		if form.Settings != nil {
			settings = *form.Settings
		}
		if form.AccountDefaults != nil {
			for i := range accountViews {
				accountViews[i].DefaultAssigneeValue = form.AccountDefaults[accountViews[i].Account.ID]
			}
		}
		newUser = form.NewUser
		if newUser.Role == "" {
			newUser.Role = "agent"
		}
	}

	s.audit.LogDataAccess(ctx, "system_settings", "1")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if err := views.RenderSettings(w, views.SettingsViewData{
		Title:           "Settings",
		Flash:           s.popFlash(r),
		ActiveNav:       "settings",
		CurrentUser:     user,
		Settings:        settings,
		Users:           users,
		AuditLogs:       auditEntries,
		Accounts:        accounts,
		AccountSettings: accountViews,
		AccountNames:    accountNames,
		NewUser:         newUser,
		Errors:          formErrors,
	}); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Settings unavailable", "We could not render the settings page.")
	}
}

// settingsFormFromRequest captures submitted settings values for re-rendering on error.
func settingsFormFromRequest(r *http.Request) *settingsForm {
	windowDays, err := strconv.Atoi(strings.TrimSpace(r.FormValue("subject_match_window_days")))
	if err != nil {
		windowDays = 0
	}
	settings := sqlc.SystemSettings{
		ID:                     1,
		DisplayTimezone:        strings.TrimSpace(r.FormValue("display_timezone")),
		IMAPInboxMailbox:       defaultOrTrim(r.FormValue("imap_inbox_mailbox"), "INBOX"),
		IMAPArchiveMailbox:     defaultOrTrim(r.FormValue("imap_archive_mailbox"), "Archive"),
		IMAPJunkMailbox:        defaultOrTrim(r.FormValue("imap_junk_mailbox"), "Junk"),
		IMAPSentMailbox:        defaultOrTrim(r.FormValue("imap_sent_mailbox"), "Sent"),
		SubjectMatchWindowDays: windowDays,
	}
	defaults := make(map[string]string)
	for key, values := range r.Form {
		const prefix = "account_default_assignee_"
		if !strings.HasPrefix(key, prefix) || len(values) == 0 {
			continue
		}
		defaults[strings.TrimPrefix(key, prefix)] = strings.TrimSpace(values[0])
	}
	return &settingsForm{Settings: &settings, AccountDefaults: defaults}
}

// HandleUpdateSettings validates and updates system_settings, recording a settings_updated audit log.
func (s *Server) HandleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	if err := parseBoundedForm(w, r, 64<<10); err != nil {
		s.renderSettings(w, r, user, http.StatusBadRequest, nil, []string{"The submitted form could not be read."})
		return
	}
	form := settingsFormFromRequest(r)

	displayTZ := form.Settings.DisplayTimezone
	if displayTZ == "" {
		displayTZ = "UTC"
		form.Settings.DisplayTimezone = "UTC"
	}
	if _, err := time.LoadLocation(displayTZ); err != nil {
		s.renderSettings(w, r, user, http.StatusBadRequest, form, []string{"Enter a valid IANA display timezone, for example Europe/Amsterdam."})
		return
	}

	windowDays := form.Settings.SubjectMatchWindowDays
	if windowDays <= 0 {
		windowDays = 30
		form.Settings.SubjectMatchWindowDays = 30
	}

	oldSettings, _ := s.store.GetSystemSettings(ctx)

	updated, err := s.store.UpsertSystemSettings(ctx, sqlc.SystemSettings{
		ID:                     1,
		DisplayTimezone:        displayTZ,
		IMAPInboxMailbox:       form.Settings.IMAPInboxMailbox,
		IMAPArchiveMailbox:     form.Settings.IMAPArchiveMailbox,
		IMAPJunkMailbox:        form.Settings.IMAPJunkMailbox,
		IMAPSentMailbox:        form.Settings.IMAPSentMailbox,
		SubjectMatchWindowDays: windowDays,
	})
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Save failed", "The settings could not be saved. Please try again.")
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
		raw := strings.TrimSpace(r.FormValue("account_default_assignee_" + account.ID))
		oldAccountSettings, _ := s.store.GetAccountSettings(ctx, account.ID)

		var assigneeID *uuid.UUID
		if raw != "" {
			parsed, err := uuid.Parse(raw)
			if err != nil {
				s.renderSettings(w, r, user, http.StatusBadRequest, form,
					[]string{"Choose a valid default assignee for " + account.Name + "."})
				return
			}
			assigneeUser, err := s.store.GetUserByID(ctx, parsed)
			if err != nil || !assigneeUser.IsActive || !assigneeUser.HasAccountAccess(account.ID) {
				s.renderSettings(w, r, user, http.StatusBadRequest, form,
					[]string{"The default assignee for " + account.Name + " must be an active team member with access to that account."})
				return
			}
			assigneeID = &parsed
		}

		saved, err := s.store.UpsertAccountSettings(ctx, sqlc.AccountSettings{
			AccountID:         account.ID,
			DefaultAssigneeID: assigneeID,
		})
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "Save failed", "The account settings could not be saved. Please try again.")
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
				s.renderError(w, r, http.StatusInternalServerError, "Save failed", "Threads waiting for an assignee could not be updated.")
				return
			}
		}
	}

	s.setFlash(r, flashKindSuccess, "Settings saved.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// newUserFormFromRequest captures submitted add-team-member values for re-rendering on error.
func newUserFormFromRequest(r *http.Request) views.NewUserForm {
	return views.NewUserForm{
		Email:           strings.TrimSpace(r.FormValue("email")),
		DisplayName:     strings.TrimSpace(r.FormValue("display_name")),
		Role:            strings.TrimSpace(r.FormValue("role")),
		AllowedAccounts: r.Form["allowed_accounts"],
	}
}

// HandlePrecreateUser allows an admin to pre-create an assignee user before their first OIDC login.
func (s *Server) HandlePrecreateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := auth.UserFromContext(ctx)
	if err := parseBoundedForm(w, r, 64<<10); err != nil {
		s.renderSettings(w, r, user, http.StatusBadRequest, nil, []string{"The submitted form could not be read."})
		return
	}
	newUser := newUserFormFromRequest(r)

	known := make(map[string]struct{}, len(s.accounts))
	for _, a := range s.accounts {
		known[a.ID] = struct{}{}
	}
	selected := make([]string, 0, len(s.accounts))
	for _, id := range newUser.AllowedAccounts {
		if _, ok := known[id]; !ok {
			s.renderSettings(w, r, user, http.StatusBadRequest, &settingsForm{NewUser: newUser},
				[]string{"One of the selected mail accounts is not configured."})
			return
		}
		selected = append(selected, id)
	}
	newUser.AllowedAccounts = selected

	if _, err := s.auth.PrecreateUser(ctx, user.ID, newUser.Email, newUser.DisplayName, newUser.Role, selected...); err != nil {
		s.renderSettings(w, r, user, http.StatusUnprocessableEntity, &settingsForm{NewUser: newUser},
			[]string{err.Error()})
		return
	}
	s.setFlash(r, flashKindSuccess, "User "+newUser.Email+" created.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func defaultOrTrim(val, fallback string) string {
	v := strings.TrimSpace(val)
	if v == "" {
		return fallback
	}
	return v
}
