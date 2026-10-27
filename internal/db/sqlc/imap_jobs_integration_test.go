package sqlc_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/wichert/minicrm/internal/db/migrations"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// This test requires a disposable database: TEST_DATABASE_URL.
func TestIMAPJobsOrderedAcrossWorkers(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrations.Up(ctx, db, "support"); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	threadID, emailID := uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO threads (id, account_id, subject_normalized, subject_display, customer_email) VALUES ($1,'support','integration','integration','integration@example.com')`, threadID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM threads WHERE id=$1`, threadID) })
	_, err = pool.Exec(ctx, `INSERT INTO emails (id,thread_id,account_id,direction,message_id,imap_mailbox,imap_uidvalidity,imap_uid,from_address,subject,subject_normalized,sent_at) VALUES ($1,$2,'support','inbound',$3,'INBOX',1,42,'a@example.com','integration','integration',now())`, emailID, threadID, emailID.String())
	if err != nil {
		t.Fatal(err)
	}
	q := sqlc.New(pool)
	source := "INBOX"
	first, err := q.EnqueueIMAPJob(ctx, sqlc.IMAPJob{AccountID: "support", JobType: "move_to_junk", EmailID: emailID, SourceMailbox: &source, TargetMailbox: "Junk"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.EnqueueIMAPJob(ctx, sqlc.IMAPJob{AccountID: "support", JobType: "restore_from_junk", EmailID: emailID, TargetMailbox: "INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan sqlc.IMAPJob, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, e := q.ClaimNextIMAPJob(ctx)
			if e != nil {
				errs <- e
			} else {
				results <- job
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for job := range results {
		if job.ID != first.ID {
			t.Fatalf("later job claimed: %s", job.ID)
		}
	}
	if len(errs) != 1 || !errors.Is(<-errs, pgx.ErrNoRows) {
		t.Fatal("second worker did not wait")
	}
	if err := q.CompleteIMAPJob(ctx, first.ID, emailID, "Junk"); err != nil {
		t.Fatal(err)
	}
	job, err := q.ClaimNextIMAPJob(ctx)
	if err != nil || job.ID != second.ID {
		t.Fatalf("restore not runnable: %+v %v", job, err)
	}
}
