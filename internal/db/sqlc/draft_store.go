package sqlc

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
)

// SaveReplyDraft validates the revision under the thread row lock and appends a version.
func (q *Queries) SaveReplyDraft(ctx context.Context, id, expected uuid.UUID, body, formatting, session string, discard bool, actor *uuid.UUID) (ReplyDraft, error) {
	tx, err := q.Begin(ctx)
	if err != nil {
		return ReplyDraft{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tq := q.WithTx(tx)
	thread, err := tq.LockThreadByID(ctx, id)
	if err != nil {
		return ReplyDraft{}, err
	}
	current, err := tq.CurrentReplyDraft(ctx, id)
	if err != nil {
		return ReplyDraft{}, err
	}
	if current.Revision != expected {
		return ReplyDraft{}, ErrDraftConflict
	}
	if !discard && thread.Status != ThreadStatusNew && thread.Status != ThreadStatusInProgress {
		return ReplyDraft{}, errors.New("thread cannot receive replies")
	}
	if discard {
		body = ""
		formatting = ""
	}
	if current.Revision != uuid.Nil && !current.Discarded && current.Body != "" && (discard || body == "") {
		if err := tq.ClearReplyDraft(ctx, thread, actor, "discarded"); err != nil {
			return ReplyDraft{}, err
		}
	}
	d, err := tq.AppendReplyDraft(ctx, thread, body, formatting, session, discard)
	if err != nil {
		return ReplyDraft{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReplyDraft{}, err
	}
	slog.InfoContext(ctx, "data_change", "event_category", "data_change", "resource_type", "reply_draft", "resource_id", id.String(), "discarded", discard, "actor_user_id", ActorFromContext(ctx))
	return d, nil
}
