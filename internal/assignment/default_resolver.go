package assignment

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// SettingsStore is the persistence interface required by DefaultAssigneeResolver.
type SettingsStore interface {
	GetSystemSettings(ctx context.Context) (sqlc.SystemSettings, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (sqlc.User, error)
	ListUsers(ctx context.Context) ([]sqlc.User, error)
}

// DefaultAssigneeResolver assigns incoming threads to the configured system default assignee.
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

// Resolve returns the configured default_assignee_id if active, or falls back to the first active user.
func (r *DefaultAssigneeResolver) Resolve(ctx context.Context, _ EmailInput) (*Candidate, error) {
	settings, err := r.store.GetSystemSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("load system_settings: %w", err)
	}
	user, err := r.store.GetUserByID(ctx, settings.DefaultAssigneeID)
	if err == nil && user.IsActive {
		return &Candidate{
			AssigneeID: user.ID,
			Source:     "default",
			Confidence: 1.0,
			Reason:     "configured default assignee",
		}, nil
	}

	users, err := r.store.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list fallback users: %w", err)
	}
	for _, u := range users {
		if u.IsActive {
			return &Candidate{
				AssigneeID: u.ID,
				Source:     "default",
				Confidence: 1.0,
				Reason:     "fallback active user",
			}, nil
		}
	}
	return nil, ErrNoAssigneeAvailable
}
