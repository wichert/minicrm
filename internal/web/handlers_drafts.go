package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// HandleDraft reads or saves an account-authorized shared draft.
func (s *Server) HandleDraft(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.loadAuthorizedThread(r.Context(), user, id); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		d, err := s.store.GetReplyDraft(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			d = sqlc.ReplyDraft{ThreadID: id}
		} else if err != nil {
			http.Error(w, "draft unavailable", http.StatusInternalServerError)
			return
		}
		s.audit.LogDataAccess(r.Context(), "reply_draft", id.String())
		_ = json.NewEncoder(w).Encode(d)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var input struct {
		Revision uuid.UUID `json:"revision"`
		Body     string    `json:"body"`
		Discard  bool      `json:"discard"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid draft", http.StatusBadRequest)
		return
	}
	session := ""
	if s.sessions != nil {
		session = s.sessions.GetString(r.Context(), auth.SessionKeySessionID)
	}
	d, err := s.store.SaveReplyDraft(r.Context(), id, input.Revision, input.Body, session, input.Discard, &user.ID)
	if errors.Is(err, sqlc.ErrDraftConflict) {
		http.Error(w, "Draft changed in another session. Copy your text before reloading.", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "Could not save draft", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(d)
}
