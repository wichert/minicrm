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

// TestListThreadsSortOrders verifies the allow-listed sort orders against real
// PostgreSQL: "newest" orders by last_message_at DESC (index-orderable literal
// ORDER BY) and "longest_waiting" by last activity ASC — the time of the most
// recent email on the thread in either direction. The sort value only selects
// between two static query variants in Go, so an unknown value must fall back
// to newest rather than alter the query shape.
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
	// Thread A opened earliest but had activity most recently (newest
	// last_message_at). Thread B's only email is much older, so B has been
	// waiting longest even though it was opened later than A. This is what
	// distinguishes "longest waiting" from the old opened_at ordering.
	a, err := q.CreateThread(ctx, sqlc.Thread{AccountID: "support", SubjectNormalized: "sort-a", SubjectDisplay: "Sort A", Status: sqlc.ThreadStatusNew, CustomerEmail: "a@example.com", LastMessageAt: newest.Add(-1 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	b, err := q.CreateThread(ctx, sqlc.Thread{AccountID: "support", SubjectNormalized: "sort-b", SubjectDisplay: "Sort B", Status: sqlc.ThreadStatusNew, CustomerEmail: "b@example.com", LastMessageAt: newest.Add(-48 * time.Hour)})
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

	byWaiting, err := q.ListThreads(ctx, sqlc.ListThreadsFilter{
		AllowedAccounts: []string{"support"},
		OpenOnly:        openOnly,
		Sort:            "longest_waiting",
		Limit:           10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byWaiting) != 2 || byWaiting[0].ID != b.ID || byWaiting[1].ID != a.ID {
		t.Fatalf("longest_waiting order = [%s, %s], want [B, A] (B waited longest)", idsOf(byWaiting)[0], idsOf(byWaiting)[1])
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

// TestListCustomerHistoryReturnsLatestPreview guards the "other conversations"
// panel's row-2 preamble. ListCustomerHistory projects the latest-message preview
// (body text, sanitized HTML, direction) and the message count on top of the
// thread metadata, so the scanner arity only matches against real PostgreSQL —
// a fake store would happily return whatever struct the test built.
func TestListCustomerHistoryReturnsLatestPreview(t *testing.T) {
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

	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	mk := func(subject string, last time.Time) sqlc.Thread {
		th, err := q.CreateThread(ctx, sqlc.Thread{
			AccountID: "support", SubjectNormalized: subject, SubjectDisplay: subject,
			Status: sqlc.ThreadStatusCompleted, CustomerEmail: "repeat@example.com",
			LastMessageAt: last,
		})
		if err != nil {
			t.Fatal(err)
		}
		return th
	}
	current := mk("history-current", base)
	older := mk("history-older", base.Add(-72*time.Hour))
	newer := mk("history-newer", base.Add(72*time.Hour))
	// A thread for a different customer must never leak into the panel.
	other := mk("history-other-customer", base)
	if _, err := pool.Exec(ctx, "UPDATE threads SET customer_email = 'other@example.com' WHERE id = $1", other.ID); err != nil {
		t.Fatal(err)
	}

	emails := []sqlc.Email{
		{ThreadID: older.ID, AccountID: "support", Direction: "inbound", FromAddress: "repeat@example.com",
			Subject: "history-older", MessageID: "<hist-older-1@test>", SentAt: base.Add(-72 * time.Hour),
			BodyText: "Older thread body text", BodyHTMLSanitized: "<p>Older thread body text</p>"},
		{ThreadID: older.ID, AccountID: "support", Direction: "outbound", FromAddress: "agent@example.com",
			Subject: "history-older", MessageID: "<hist-older-2@test>", SentAt: base.Add(-71 * time.Hour),
			BodyText: "Newest reply on the older thread", BodyHTMLSanitized: "<p>Newest reply on the older thread</p>"},
		{ThreadID: newer.ID, AccountID: "support", Direction: "inbound", FromAddress: "repeat@example.com",
			Subject: "history-newer", MessageID: "<hist-newer-1@test>", SentAt: base.Add(72 * time.Hour),
			BodyText: "Newer thread body text", BodyHTMLSanitized: "<p>Newer thread body text</p>"},
		{ThreadID: other.ID, AccountID: "support", Direction: "inbound", FromAddress: "other@example.com",
			Subject: "history-other-customer", MessageID: "<hist-other-1@test>", SentAt: base,
			BodyText: "Different customer body", BodyHTMLSanitized: "<p>Different customer body</p>"},
	}
	for _, e := range emails {
		if _, err := q.CreateEmail(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	history, err := q.ListCustomerHistory(ctx, []string{"support"}, "repeat@example.com", current.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history = %d threads, want 2 (current and other-customer excluded)", len(history))
	}
	// Ordered by last activity, newest first: a sibling newer than the thread
	// being read is exactly why the panel is now called "other conversations".
	if history[0].ID != newer.ID || history[1].ID != older.ID {
		t.Fatalf("history order = [%s, %s], want [newer, older]", history[0].ID, history[1].ID)
	}
	if got := history[0].LatestBodyText; got != "Newer thread body text" {
		t.Errorf("newer thread preview = %q, want the latest body text", got)
	}
	if got := history[0].LatestDirection; got != "inbound" {
		t.Errorf("newer thread direction = %q, want inbound", got)
	}
	// The older thread's preview is its most recent email, i.e. our own reply.
	if got := history[1].LatestBodyText; got != "Newest reply on the older thread" {
		t.Errorf("older thread preview = %q, want its latest message", got)
	}
	if got := history[1].LatestDirection; got != "outbound" {
		t.Errorf("older thread direction = %q, want outbound", got)
	}
	if history[1].MessageCount != 2 {
		t.Errorf("older thread message count = %d, want 2", history[1].MessageCount)
	}
}
