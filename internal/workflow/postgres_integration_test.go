package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/migrations"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/workflow"
)

// Set TEST_DATABASE_URL to run against a disposable PostgreSQL database.
func TestPostgresCompleteFromNewOrderingAndConcurrency(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "workflow_test_" + uuid.NewString()[:8]
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") }()
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	db := stdlib.OpenDB(*config.ConnConfig)
	defer db.Close()
	if err := migrations.Up(ctx, db, "support"); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO system_settings(id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	q := sqlc.New(pool)
	svc := workflow.NewService(workflow.QueriesAdapter{Queries: q}, audit.NewLogger(q, nil), nil)
	newThread := func() uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO threads(id,account_id,subject_normalized,subject_display,customer_email) VALUES ($1,'support','subject','subject','test@example.com')`, id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := newThread()
	if err := svc.CompleteFromNew(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	events, err := q.ListThreadEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || !events[0].OccurredAt.Equal(events[1].OccurredAt) {
		t.Fatalf("expected two same-transaction events: %+v", events)
	}
	var first, second map[string]string
	if err := json.Unmarshal(events[0].NewValue, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(events[1].NewValue, &second); err != nil {
		t.Fatal(err)
	}
	if first["status"] != "in_progress" || second["status"] != "completed" {
		t.Fatalf("wrong order: %v %v", first, second)
	}
	// A terminal transition must invalidate even a never-saved revision.
	id = newThread()
	if err := svc.Transition(ctx, id, sqlc.ThreadStatusSpam, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := q.SaveReplyDraft(ctx, id, uuid.Nil, "stale", "", "session", false, nil); !errors.Is(err, sqlc.ErrDraftConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}

	// Reopen preserves the follow-up draft and its historical revisions.
	target, source := newThread(), newThread()
	if _, err := pool.Exec(ctx, `UPDATE threads SET status='completed' WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE threads SET reopens_thread_id=$1 WHERE id=$2`, target, source); err != nil {
		t.Fatal(err)
	}
	original, err := q.SaveReplyDraft(ctx, source, uuid.Nil, "draft to preserve", "", "session", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReopenAndMerge(ctx, source, nil); err != nil {
		t.Fatal(err)
	}
	draft, err := q.GetReplyDraft(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Body != original.Body || draft.Revision == original.Revision {
		t.Fatalf("unexpected merged draft: %+v", draft)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reply_drafts WHERE thread_id=$1 AND revision=$2`, target, original.Revision).Scan(&count); err != nil || count != 1 {
		t.Fatalf("source history missing: count=%d err=%v", count, err)
	}

	// Distinct active drafts must never overwrite one another during merge.
	target, source = newThread(), newThread()
	if _, err := q.SaveReplyDraft(ctx, target, uuid.Nil, "target draft", "", "session", false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := q.SaveReplyDraft(ctx, source, uuid.Nil, "source draft", "", "session", false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE threads SET status='completed' WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE threads SET reopens_thread_id=$1 WHERE id=$2`, target, source); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReopenAndMerge(ctx, source, nil); err == nil {
		t.Fatal("merge overwrote distinct drafts")
	}
	if _, err := q.GetThreadByID(ctx, source); err != nil {
		t.Fatalf("source deleted on failed merge: %v", err)
	}

	assignee := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,display_name,allowed_accounts) VALUES ($1,$2,'Agent',ARRAY['support'])`, assignee, assignee.String()+"@example.com"); err != nil {
		t.Fatal(err)
	}
	// Hold the thread row while closing it; a concurrent reassign must wait and
	// validate the committed state rather than act on its earlier snapshot.
	id = newThread()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE threads SET status='spam' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- svc.Reassign(ctx, id, assignee, nil) }()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, workflow.ErrInvalidTransition) {
		t.Fatalf("closed thread reassigned: %v", err)
	}
	closed, err := q.GetThreadByID(ctx, id)
	if err != nil || closed.AssigneeID != nil {
		t.Fatalf("unexpected closed assignment: %+v %v", closed, err)
	}

	for i := 0; i < 20; i++ {
		id = newThread()
		var wg sync.WaitGroup
		wg.Add(2)
		errs := make([]error, 2)
		go func() { defer wg.Done(); errs[0] = svc.CompleteFromNew(ctx, id, nil) }()
		go func() { defer wg.Done(); errs[1] = svc.Transition(ctx, id, sqlc.ThreadStatusSpam, nil) }()
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("exactly one operation must succeed: %v", errs)
		}
		for _, err := range errs {
			if err != nil && !errors.Is(err, workflow.ErrInvalidTransition) {
				t.Fatal(err)
			}
		}
		thread, err := q.GetThreadByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if errs[0] == nil && thread.Status != sqlc.ThreadStatusCompleted {
			t.Fatalf("completed overwritten: %s", thread.Status)
		}
	}
}
