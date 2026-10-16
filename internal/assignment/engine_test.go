package assignment_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/assignment"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

type fakeSettingsStore struct {
	defaultID uuid.UUID
	users     map[uuid.UUID]sqlc.User
}

func (f *fakeSettingsStore) GetSystemSettings(_ context.Context) (sqlc.SystemSettings, error) {
	return sqlc.SystemSettings{DefaultAssigneeID: f.defaultID}, nil
}

func (f *fakeSettingsStore) GetUserByID(_ context.Context, id uuid.UUID) (sqlc.User, error) {
	u, ok := f.users[id]
	if !ok {
		return sqlc.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeSettingsStore) ListUsers(_ context.Context) ([]sqlc.User, error) {
	var out []sqlc.User
	for _, u := range f.users {
		out = append(out, u)
	}
	return out, nil
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

func TestAssignmentEngineDefaultAndChain(t *testing.T) {
	defaultUser := sqlc.User{ID: uuid.New(), Email: "default@example.com", IsActive: true}
	vipUser := sqlc.User{ID: uuid.New(), Email: "vip-agent@example.com", IsActive: true}
	store := &fakeSettingsStore{
		defaultID: defaultUser.ID,
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

	cand1, err := engine.Resolve(context.Background(), assignment.EmailInput{FromAddress: "customer@other.com"})
	if err != nil {
		t.Fatalf("Resolve default: %v", err)
	}
	if cand1.AssigneeID != defaultUser.ID || cand1.Source != "default" {
		t.Fatalf("got %+v, want defaultUser", cand1)
	}

	cand2, err := engine.Resolve(context.Background(), assignment.EmailInput{FromAddress: "vip@acme.com"})
	if err != nil {
		t.Fatalf("Resolve rule: %v", err)
	}
	if cand2.AssigneeID != vipUser.ID || cand2.Source != "rule" {
		t.Fatalf("got %+v, want vipUser via rule", cand2)
	}
}
