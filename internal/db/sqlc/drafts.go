package sqlc

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"log/slog"
)

// ErrDraftConflict indicates that a shared draft has changed since it was read.
var ErrDraftConflict = errors.New("draft changed in another session")

// ReplyDraft is an immutable version of a shared draft.
type ReplyDraft struct {
	ThreadID   uuid.UUID `json:"thread_id"`
	AccountID  string    `json:"-"`
	Revision   uuid.UUID `json:"revision"`
	Body       string    `json:"body"`
	SessionID  string    `json:"-"`
	OccurredAt time.Time `json:"occurred_at"`
	Discarded  bool      `json:"discarded"`
}

// GetReplyDraft returns the most recent version including discard tombstones.
func (q *Queries) GetReplyDraft(ctx context.Context, threadID uuid.UUID) (ReplyDraft, error) {
	var d ReplyDraft
	err := q.db.QueryRow(ctx, `SELECT thread_id, account_id, revision, body, session_id, occurred_at, discarded FROM reply_drafts WHERE thread_id=$1 ORDER BY id DESC LIMIT 1`, threadID).
		Scan(&d.ThreadID, &d.AccountID, &d.Revision, &d.Body, &d.SessionID, &d.OccurredAt, &d.Discarded)
	return d, err
}

// AppendReplyDraft appends a revision; caller must hold the thread row lock.
func (q *Queries) AppendReplyDraft(ctx context.Context, thread Thread, body, session string, discard bool) (ReplyDraft, error) {
	d := ReplyDraft{ThreadID: thread.ID, AccountID: thread.AccountID, Revision: uuid.New(), Body: body, SessionID: session, Discarded: discard}
	err := q.db.QueryRow(ctx, `INSERT INTO reply_drafts (thread_id,account_id,revision,body,session_id,discarded) VALUES ($1,$2,$3,$4,$5,$6) RETURNING occurred_at`, d.ThreadID, d.AccountID, d.Revision, d.Body, d.SessionID, d.Discarded).Scan(&d.OccurredAt)
	return d, err
}

// ClearReplyDraft appends a tombstone and records a discard event. Caller holds the thread lock.
func (q *Queries) ClearReplyDraft(ctx context.Context, thread Thread, actor *uuid.UUID, reason string) error {
	d, err := q.CurrentReplyDraft(ctx, thread.ID)
	if err != nil {
		return err
	}
	if d.Revision == uuid.Nil || d.Discarded || d.Body == "" {
		return nil
	}
	if _, err := q.AppendReplyDraft(ctx, thread, "", "system", true); err != nil {
		return err
	}
	value, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	if err := q.InsertThreadEvent(ctx, ThreadEvent{ThreadID: thread.ID, ActorUserID: actor, EventType: "draft_discarded", NewValue: value}); err != nil {
		return err
	}
	slog.InfoContext(ctx, "data_change", "event_category", "data_change", "resource_type", "thread", "resource_id", thread.ID.String(), "event_type", "draft_discarded")
	return nil
}

// CurrentReplyDraft returns a zero-revision empty draft when none has been saved.
func (q *Queries) CurrentReplyDraft(ctx context.Context, threadID uuid.UUID) (ReplyDraft, error) {
	d, err := q.GetReplyDraft(ctx, threadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReplyDraft{ThreadID: threadID}, nil
	}
	return d, err
}
