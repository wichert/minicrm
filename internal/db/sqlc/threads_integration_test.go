package sqlc_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wichert/minicrm/internal/db/migrations"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// TestGetThreadByIDScansOpenedAt guards the threadColumns/scanThreadWithOpenedAt
// contract: GetThreadByID projects one extra column (opened_at) on top of the shared
// threadColumns list, so the row scanner must expect exactly that arity. A mismatch
// only surfaces against real PostgreSQL, not in unit tests with fakes.
func TestGetThreadByIDScansOpenedAt(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to a disposable PostgreSQL database")
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
	schema := "threads_test_" + uuid.NewString()[:8]
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

	q := sqlc.New(pool)
	created, err := q.CreateThread(ctx, sqlc.Thread{
		AccountID:         "support",
		SubjectNormalized: "opened-at",
		SubjectDisplay:    "Opened at",
		Status:            sqlc.ThreadStatusNew,
		CustomerEmail:     "customer@example.com",
		CustomerName:      "Customer",
	})
	if err != nil {
		t.Fatal(err)
	}

	// No emails yet: opened_at falls back to last_message_at so the caller never
	// has to special-case a zero value.
	got, err := q.GetThreadByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetThreadByID: %v", err)
	}
	if got.OpenedAt.IsZero() {
		t.Fatal("opened_at must fall back to last_message_at when there are no emails")
	}
	if !got.OpenedAt.Equal(created.LastMessageAt) {
		t.Fatalf("opened_at fallback = %v, want %v", got.OpenedAt, created.LastMessageAt)
	}

	opened := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Microsecond)
	emailID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO emails (id,thread_id,account_id,direction,message_id,imap_mailbox,imap_uidvalidity,imap_uid,from_address,subject,subject_normalized,sent_at) VALUES ($1,$2,'support','inbound',$3,'INBOX',1,7,'customer@example.com','Opened at','opened-at',$4)`, emailID, created.ID, emailID.String(), opened); err != nil {
		t.Fatal(err)
	}

	got, err = q.GetThreadByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetThreadByID with email: %v", err)
	}
	if !got.OpenedAt.Equal(opened) {
		t.Fatalf("opened_at = %v, want %v", got.OpenedAt, opened)
	}
	// The shared columns must still round-trip correctly alongside the extra one.
	if got.ID != created.ID || got.AccountID != "support" || got.Status != sqlc.ThreadStatusNew {
		t.Fatalf("unexpected thread after scan: %+v", got)
	}

	// LockThreadByID still projects the plain threadColumns list and must keep working.
	locked, err := q.LockThreadByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("LockThreadByID: %v", err)
	}
	if locked.ID != created.ID {
		t.Fatalf("LockThreadByID returned %v, want %v", locked.ID, created.ID)
	}
}

// TestListThreadsSortOrders verifies the W7 allow-listed sort orders against real
// PostgreSQL: "newest" orders by last_message_at DESC and "oldest_open" by the
// earliest inbound message time (opened_at) ASC. The sort value is a bound
// parameter folded into a fixed ORDER BY expression, so an unknown value must
// fall back to newest rather than alter the query shape.
func TestListThreadsSortOrders(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to a disposable PostgreSQL database")
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
	schema := "threads_test_" + uuid.NewString()[:8]
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
	q := sqlc.New(pool)

	newest := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// Thread A opened earliest but replied to last (newest last_message_at).
	// Thread B opened later but never received a follow-up.
	a, err := q.CreateThread(ctx, sqlc.Thread{AccountID: "support", SubjectNormalized: "sort-a", SubjectDisplay: "Sort A", Status: sqlc.ThreadStatusNew, CustomerEmail: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := q.CreateThread(ctx, sqlc.Thread{AccountID: "support", SubjectNormalized: "sort-b", SubjectDisplay: "Sort B", Status: sqlc.ThreadStatusNew, CustomerEmail: "b@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	emails := []sqlc.Email{
		{ThreadID: a.ID, AccountID: "support", Direction: "inbound", FromAddress: "a@example.com", Subject: "Sort A", MessageID: "<sort-a-0@test>", SentAt: newest.Add(-72 * time.Hour)},
		{ThreadID: a.ID, AccountID: "support", Direction: "inbound", FromAddress: "a@example.com", Subject: "Sort A", MessageID: "<sort-a-1@test>", SentAt: newest.Add(-1 * time.Hour)},
		{ThreadID: b.ID, AccountID: "support", Direction: "inbound", FromAddress: "b@example.com", Subject: "Sort B", MessageID: "<sort-b-0@test>", SentAt: newest.Add(-48 * time.Hour)},
	}
	for _, e := range emails {
		if _, err := q.CreateEmail(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	openOnly := true
	byNewest, err := q.ListThreads(ctx, sqlc.ListThreadsFilter{
		AllowedAccounts: []string{"support"},
		OpenOnly:        openOnly,
		Sort:            "newest",
		Limit:           10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byNewest) != 2 || byNewest[0].ID != a.ID || byNewest[1].ID != b.ID {
		t.Fatalf("newest order = [%s, %s], want [A, B]", idsOf(byNewest)[0], idsOf(byNewest)[1])
	}

	byOldest, err := q.ListThreads(ctx, sqlc.ListThreadsFilter{
		AllowedAccounts: []string{"support"},
		OpenOnly:        openOnly,
		Sort:            "oldest_open",
		Limit:           10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byOldest) != 2 || byOldest[0].ID != a.ID || byOldest[1].ID != b.ID {
		t.Fatalf("oldest_open order = [%s, %s], want [A, B] (A opened first)", idsOf(byOldest)[0], idsOf(byOldest)[1])
	}

	// An unknown sort value must fall back to newest, never interpolate.
	byUnknown, err := q.ListThreads(ctx, sqlc.ListThreadsFilter{
		AllowedAccounts: []string{"support"},
		OpenOnly:        openOnly,
		Sort:            "invalid'; DROP TABLE threads; --",
		Limit:           10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byUnknown) != 2 || byUnknown[0].ID != a.ID {
		t.Fatalf("unknown sort must fall back to newest, got [%s, %s]", idsOf(byUnknown)[0], idsOf(byUnknown)[1])
	}
}

func idsOf(threads []sqlc.Thread) []string {
	out := make([]string, len(threads))
	for i, t := range threads {
		out[i] = t.ID.String()
	}
	return out
}
