package assignment_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/assignment"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

type fakeSettingsStore struct {
	accountSettings map[string]sqlc.AccountSettings
	users           map[uuid.UUID]sqlc.User
}

func (f *fakeSettingsStore) GetAccountSettings(_ context.Context, accountID string) (sqlc.AccountSettings, error) {
	s, ok := f.accountSettings[accountID]
	if !ok {
		return sqlc.AccountSettings{}, pgx.ErrNoRows
	}
	return s, nil
}

func (f *fakeSettingsStore) GetUserByID(_ context.Context, id uuid.UUID) (sqlc.User, error) {
	u, ok := f.users[id]
	if !ok {
		return sqlc.User{}, pgx.ErrNoRows
	}
	return u, nil
}

type stubRuleResolver struct {
	matchID uuid.UUID
}

func (s *stubRuleResolver) Name() string { return "rule" }

func (s *stubRuleResolver) Resolve(_ context.Context, input assignment.EmailInput) (*assignment.Candidate, error) {
	if input.FromAddress == "vip@acme.com" {
		return &assignment.Candidate{
			AssigneeID: s.matchID,
			Source:     "rule",
			Confidence: 1.0,
			Reason:     "vip domain rule",
		}, nil
	}
	return nil, nil
}

func ptr(id uuid.UUID) *uuid.UUID { return &id }

func TestAssignmentEngineDefaultAndChain(t *testing.T) {
	defaultUser := sqlc.User{ID: uuid.New(), Email: "default@example.com", IsActive: true, AllowedAccounts: []string{"support"}}
	vipUser := sqlc.User{ID: uuid.New(), Email: "vip-agent@example.com", IsActive: true, AllowedAccounts: []string{"support"}}
	store := &fakeSettingsStore{
		accountSettings: map[string]sqlc.AccountSettings{
			"support": {AccountID: "support", DefaultAssigneeID: ptr(defaultUser.ID)},
		},
		users: map[uuid.UUID]sqlc.User{
			defaultUser.ID: defaultUser,
			vipUser.ID:     vipUser,
		},
	}

	engine := assignment.NewEngine(
		0.7,
		&stubRuleResolver{matchID: vipUser.ID},
		assignment.NewDefaultAssigneeResolver(store),
	)

	cand1, err := engine.Resolve(context.Background(), assignment.EmailInput{AccountID: "support", FromAddress: "customer@other.com"})
	if err != nil {
		t.Fatalf("Resolve default: %v", err)
	}
	if cand1.AssigneeID != defaultUser.ID || cand1.Source != "default" {
		t.Fatalf("got %+v, want defaultUser", cand1)
	}

	cand2, err := engine.Resolve(context.Background(), assignment.EmailInput{AccountID: "support", FromAddress: "vip@acme.com"})
	if err != nil {
		t.Fatalf("Resolve rule: %v", err)
	}
	if cand2.AssigneeID != vipUser.ID || cand2.Source != "rule" {
		t.Fatalf("got %+v, want vipUser via rule", cand2)
	}
}

func TestDefaultResolverPerAccount(t *testing.T) {
	supportUser := sqlc.User{ID: uuid.New(), Email: "support@example.com", IsActive: true, AllowedAccounts: []string{"support"}}
	salesUser := sqlc.User{ID: uuid.New(), Email: "sales@example.com", IsActive: true, AllowedAccounts: []string{"sales"}}
	store := &fakeSettingsStore{
		accountSettings: map[string]sqlc.AccountSettings{
			"support": {AccountID: "support", DefaultAssigneeID: ptr(supportUser.ID)},
			"sales":   {AccountID: "sales", DefaultAssigneeID: ptr(salesUser.ID)},
		},
		users: map[uuid.UUID]sqlc.User{
			supportUser.ID: supportUser,
			salesUser.ID:   salesUser,
		},
	}
	resolver := assignment.NewDefaultAssigneeResolver(store)
	ctx := context.Background()

	for _, tc := range []struct {
		account string
		want    uuid.UUID
	}{
		{"support", supportUser.ID},
		{"sales", salesUser.ID},
	} {
		cand, err := resolver.Resolve(ctx, assignment.EmailInput{AccountID: tc.account})
		if err != nil {
			t.Fatalf("%s: %v", tc.account, err)
		}
		if cand == nil || cand.AssigneeID != tc.want {
			t.Fatalf("%s: got %+v, want %s", tc.account, cand, tc.want)
		}
	}
}

func TestDefaultResolverUnassignedWhenAccountHasNoDefault(t *testing.T) {
	user := sqlc.User{ID: uuid.New(), Email: "support@example.com", IsActive: true, AllowedAccounts: []string{"support"}}
	ctx := context.Background()

	t.Run("no account_settings row", func(t *testing.T) {
		store := &fakeSettingsStore{accountSettings: map[string]sqlc.AccountSettings{}}
		resolver := assignment.NewDefaultAssigneeResolver(store)
		cand, err := resolver.Resolve(ctx, assignment.EmailInput{AccountID: "sales"})
		if err != nil || cand != nil {
			t.Fatalf("got cand=%+v err=%v, want nil candidate and nil error", cand, err)
		}
	})

	t.Run("null default assignee", func(t *testing.T) {
		store := &fakeSettingsStore{accountSettings: map[string]sqlc.AccountSettings{
			"support": {AccountID: "support", DefaultAssigneeID: nil},
		}}
		resolver := assignment.NewDefaultAssigneeResolver(store)
		cand, err := resolver.Resolve(ctx, assignment.EmailInput{AccountID: "support"})
		if err != nil || cand != nil {
			t.Fatalf("got cand=%+v err=%v, want nil candidate and nil error", cand, err)
		}
	})

	t.Run("inactive default assignee", func(t *testing.T) {
		inactive := sqlc.User{ID: user.ID, Email: user.Email, IsActive: false, AllowedAccounts: []string{"support"}}
		store := &fakeSettingsStore{
			accountSettings: map[string]sqlc.AccountSettings{
				"support": {AccountID: "support", DefaultAssigneeID: ptr(inactive.ID)},
			},
			users: map[uuid.UUID]sqlc.User{inactive.ID: inactive},
		}
		resolver := assignment.NewDefaultAssigneeResolver(store)
		cand, err := resolver.Resolve(ctx, assignment.EmailInput{AccountID: "support"})
		if err != nil || cand != nil {
			t.Fatalf("got cand=%+v err=%v, want nil candidate and nil error", cand, err)
		}
	})

	t.Run("assignee without account access", func(t *testing.T) {
		other := sqlc.User{ID: user.ID, Email: user.Email, IsActive: true, AllowedAccounts: []string{"sales"}}
		store := &fakeSettingsStore{
			accountSettings: map[string]sqlc.AccountSettings{
				"support": {AccountID: "support", DefaultAssigneeID: ptr(other.ID)},
			},
			users: map[uuid.UUID]sqlc.User{other.ID: other},
		}
		resolver := assignment.NewDefaultAssigneeResolver(store)
		cand, err := resolver.Resolve(ctx, assignment.EmailInput{AccountID: "support"})
		if err != nil || cand != nil {
			t.Fatalf("got cand=%+v err=%v, want nil candidate and nil error", cand, err)
		}
	})

	t.Run("engine reports no assignee available", func(t *testing.T) {
		store := &fakeSettingsStore{accountSettings: map[string]sqlc.AccountSettings{}}
		engine := assignment.NewEngine(0.5, assignment.NewDefaultAssigneeResolver(store))
		_, err := engine.Resolve(ctx, assignment.EmailInput{AccountID: "sales"})
		if !errors.Is(err, assignment.ErrNoAssigneeAvailable) {
			t.Fatalf("got err=%v, want ErrNoAssigneeAvailable", err)
		}
	})
}
