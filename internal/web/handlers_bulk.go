package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/workflow"
)

func (s *Server) redirectBulkBack(w http.ResponseWriter, r *http.Request) {
	back := sanitizeInboxBackURL(r.FormValue("return_to"), r.Host)
	if back == "" {
		back = "/threads"
	}
	// #nosec G710 -- back is restricted to the local inbox.
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// HandleBulkAssign revalidates access and assignee eligibility for every selection.
func (s *Server) HandleBulkAssign(w http.ResponseWriter, r *http.Request) {
	if err := parseBoundedForm(w, r, 128<<10); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	ids := r.Form["thread_id"]
	if len(ids) == 0 {
		s.setFlash(r, flashKindError, "Select at least one conversation.")
		s.redirectBulkBack(w, r)
		return
	}
	if len(ids) > 500 {
		s.setFlash(r, flashKindError, "Select between 1 and 500 conversations.")
		s.redirectBulkBack(w, r)
		return
	}
	assignee, err := uuid.Parse(strings.TrimSpace(r.FormValue("assignee_id")))
	if err != nil {
		s.setFlash(r, flashKindError, "Choose an assignee.")
		s.redirectBulkBack(w, r)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	success, failed := 0, 0
	seen := make(map[uuid.UUID]bool, len(ids))
	var failures []string
	for _, raw := range ids {
		id, err := uuid.Parse(raw)
		if err != nil || seen[id] {
			failed++
			failures = append(failures, "Invalid or duplicate selection")
			continue
		}
		seen[id] = true
		thread, err := s.loadAuthorizedThread(r.Context(), user, id)
		if err != nil {
			failed++
			failures = append(failures, id.String()+": unavailable")
			continue
		}
		if thread.Status != sqlc.ThreadStatusNew && thread.Status != sqlc.ThreadStatusInProgress {
			failed++
			failures = append(failures, id.String()+": no longer open")
			continue
		}
		if err := s.workflow.Reassign(r.Context(), id, assignee, &user.ID); err != nil {
			failed++
			reason := "could not assign"
			if errors.Is(err, workflow.ErrAssigneeNoAccountAccess) {
				reason = "assignee lacks account access"
			}
			failures = append(failures, id.String()+": "+reason)
			continue
		}
		success++
	}
	message := fmt.Sprintf("%d assigned; %d failed.", success, failed)
	if len(failures) > 0 {
		message += " " + strings.Join(failures, "; ")
	}
	s.setFlash(r, flashKindSuccess, message)
	s.redirectBulkBack(w, r)
}

// HandleBulkSpam revalidates every selected conversation independently.
func (s *Server) HandleBulkSpam(w http.ResponseWriter, r *http.Request) {
	if err := parseBoundedForm(w, r, 128<<10); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	ids := r.Form["thread_id"]
	if len(ids) == 0 {
		s.setFlash(r, flashKindError, "Select at least one conversation.")
		s.redirectBulkBack(w, r)
		return
	}
	if len(ids) > 500 {
		s.setFlash(r, flashKindError, "Select between 1 and 500 conversations.")
		s.redirectBulkBack(w, r)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	success, failed := 0, 0
	seen := make(map[uuid.UUID]bool, len(ids))
	var failures []string
	for _, raw := range ids {
		id, err := uuid.Parse(raw)
		if err != nil || seen[id] {
			failed++
			failures = append(failures, "Invalid or duplicate selection")
			continue
		}
		seen[id] = true
		thread, err := s.loadAuthorizedThread(r.Context(), user, id)
		if err != nil {
			failed++
			failures = append(failures, id.String()+": unavailable")
			continue
		}
		if thread.Status != sqlc.ThreadStatusNew && thread.Status != sqlc.ThreadStatusInProgress {
			failed++
			failures = append(failures, id.String()+": no longer open")
			continue
		}
		if err := s.workflow.Transition(r.Context(), id, sqlc.ThreadStatusSpam, &user.ID); err != nil {
			failed++
			failures = append(failures, id.String()+": could not queue")
			continue
		}
		success++
	}
	message := fmt.Sprintf("%d marked as spam and queued for Junk; %d failed.", success, failed)
	if len(failures) > 0 {
		message += " " + strings.Join(failures, "; ")
	}
	s.setFlash(r, flashKindSuccess, message)
	s.redirectBulkBack(w, r)
}

// HandleBulkComplete completes selected threads, using the audited CompleteFromNew composite for new threads.
func (s *Server) HandleBulkComplete(w http.ResponseWriter, r *http.Request) {
	if err := parseBoundedForm(w, r, 64<<10); err != nil {
		http.Error(w, "Invalid form submission", http.StatusBadRequest)
		return
	}
	ids := r.Form["thread_id"]
	if len(ids) == 0 {
		s.setFlash(r, flashKindError, "Select at least one conversation.")
		s.redirectBulkBack(w, r)
		return
	}
	if len(ids) > 500 {
		s.setFlash(r, flashKindError, "Select between 1 and 500 conversations.")
		s.redirectBulkBack(w, r)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	success, failed := 0, 0
	seen := make(map[uuid.UUID]bool, len(ids))
	var failures []string
	for _, raw := range ids {
		id, err := uuid.Parse(raw)
		if err != nil || seen[id] {
			failed++
			failures = append(failures, "Invalid or duplicate selection")
			continue
		}
		seen[id] = true
		thread, err := s.loadAuthorizedThread(r.Context(), user, id)
		if err != nil {
			failed++
			failures = append(failures, id.String()+": unavailable")
			continue
		}
		switch thread.Status {
		case sqlc.ThreadStatusNew:
			err = s.workflow.CompleteFromNew(r.Context(), id, &user.ID)
		case sqlc.ThreadStatusInProgress:
			err = s.workflow.Transition(r.Context(), id, sqlc.ThreadStatusCompleted, &user.ID)
		default:
			failed++
			failures = append(failures, id.String()+": no longer open")
			continue
		}
		if err != nil {
			failed++
			failures = append(failures, id.String()+": could not complete")
			continue
		}
		success++
	}
	message := fmt.Sprintf("%d completed and queued for Archive; %d failed.", success, failed)
	if len(failures) > 0 {
		message += " " + strings.Join(failures, "; ")
	}
	s.setFlash(r, flashKindSuccess, message)
	s.redirectBulkBack(w, r)
}
