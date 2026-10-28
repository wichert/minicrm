//go:build seedintegration

package mail_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/wichert/minicrm/internal/config"
	"github.com/wichert/minicrm/internal/db/migrations"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/mail"
	"github.com/wichert/minicrm/internal/seed"
	"github.com/wichert/minicrm/internal/storage"
)

// TestSeedPipeline runs APPEND, sync, workflow transitions, and job drain against a
// disposable database. Set TEST_DATABASE_URL and pass -tags seedintegration.
func TestSeedPipeline(t *testing.T) {
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

	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(yamlPath, []byte(pipelineYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	accounts := []config.MailAccountConfig{
		{ID: "support", Name: "Customer Support", SMTPFromAddress: "support@example.com", SMTPFromName: "Customer Support", IMAPUsername: "support@example.com"},
		{ID: "sales", Name: "Sales", SMTPFromAddress: "sales@example.com", SMTPFromName: "Sales", IMAPUsername: "sales@example.com"},
	}
	doc, err := seed.LoadFile(yamlPath, dir, accounts)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	fakes := map[string]*fakeIMAPServer{
		"support": newFakeIMAPServer(),
		"sales":   newFakeIMAPServer(),
	}
	summary, err := seed.Run(ctx, seed.Options{
		File:        doc,
		Accounts:    accounts,
		Queries:     sqlc.New(pool),
		Pool:        pool,
		Attachments: store,
		Dial: func(ctx context.Context, acct config.MailAccountConfig) (mail.IMAPClient, error) {
			return fakes[acct.ID], nil
		},
		JobTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Threads != 3 || summary.Messages < 4 {
		t.Fatalf("summary threads=%d messages=%d", summary.Threads, summary.Messages)
	}
	if summary.ByAccount["support"].Spam != 1 || summary.ByAccount["support"].Done != 1 {
		t.Fatalf("support summary %+v", summary.ByAccount["support"])
	}
	if summary.ByAccount["sales"].Open != 1 {
		t.Fatalf("sales summary %+v", summary.ByAccount["sales"])
	}
	if summary.JobsDrained < 2 {
		t.Fatalf("jobs drained = %d", summary.JobsDrained)
	}
	if len(fakes["support"].mailboxes["Junk"]) == 0 || len(fakes["support"].mailboxes["Archive"]) == 0 {
		t.Fatalf("junk=%d archive=%d", len(fakes["support"].mailboxes["Junk"]), len(fakes["support"].mailboxes["Archive"]))
	}

	again, err := seed.Run(ctx, seed.Options{
		File: doc, Accounts: accounts, Queries: sqlc.New(pool), Pool: pool, Attachments: store,
		Dial: func(ctx context.Context, acct config.MailAccountConfig) (mail.IMAPClient, error) {
			t.Fatal("dial should not run when already seeded")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Skipped {
		t.Fatal("expected second run to skip")
	}
}

const pipelineYAML = `
accounts: [support, sales]
users:
  - email: alex@example.com
    display_name: Alex Agent
    accounts: [support, sales]
  - email: sam@example.com
    display_name: Sam Support
    accounts: [support]
settings:
  timezone: Europe/Amsterdam
  default_assignees:
    support: sam@example.com
    sales: alex@example.com
threads:
  - id: login
    account: support
    customer: alice@customer.example
    customer_name: Alice Example
    subject: Cannot log in
    status: completed
    assigned_to: sam@example.com
    messages:
      - date: 2026-10-05T09:15:00Z
        body: Help
      - date: 2026-10-05T11:40:00Z
        direction: outbound
        body: Try caps lock
  - id: junk-mail
    account: support
    customer: winner@lottery.example
    customer_name: Prize Desk
    subject: You won a prize
    status: spam
    messages:
      - date: 2026-10-06T09:15:00Z
        body: Claim now
  - id: quote
    account: sales
    customer: dave@prospect.example
    customer_name: Dave Prospect
    subject: Pricing for 50 seats
    status: in_progress
    assigned_source: default
    messages:
      - date: 2026-10-07T09:15:00Z
        body: Please quote 50 seats
`
