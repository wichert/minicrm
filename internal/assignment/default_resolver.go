package assignment

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// SettingsStore is the persistence interface required by DefaultAssigneeResolver.
type SettingsStore interface {
	GetAccountSettings(ctx context.Context, accountID string) (sqlc.AccountSettings, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (sqlc.User, error)
}

// DefaultAssigneeResolver assigns incoming threads to the configured default assignee of
// the mail account the thread belongs to.
//
// When the account has no default assignee configured (no account_settings row, a NULL
// default_assignee_id, or an assignee that is inactive or lacks access to the account) it
// resolves no candidate at all, so the thread is created in the unassigned state.
type DefaultAssigneeResolver struct {
	store SettingsStore
}

// NewDefaultAssigneeResolver creates a DefaultAssigneeResolver.
func NewDefaultAssigneeResolver(store SettingsStore) *DefaultAssigneeResolver {
	return &DefaultAssigneeResolver{store: store}
}

// Name returns the resolver identifier.
func (r *DefaultAssigneeResolver) Name() string {
	return "default"
}

// Resolve returns the configured per-account default assignee, or nil when the account has none.
func (r *DefaultAssigneeResolver) Resolve(ctx context.Context, input EmailInput) (*Candidate, error) {
	if input.AccountID == "" {
		return nil, errors.New("default assignee resolver requires an account ID")
	}
	settings, err := r.store.GetAccountSettings(ctx, input.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load account_settings for %q: %w", input.AccountID, err)
	}
	if settings.DefaultAssigneeID == nil {
		return nil, nil
	}
	user, err := r.store.GetUserByID(ctx, *settings.DefaultAssigneeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load default assignee: %w", err)
	}
	if !user.IsActive || !user.HasAccountAccess(input.AccountID) {
		return nil, nil
	}
	return &Candidate{
		AssigneeID: user.ID,
		Source:     "default",
		Confidence: 1.0,
		Reason:     "configured default assignee for account " + input.AccountID,
	}, nil
}
