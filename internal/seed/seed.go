package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wichert/minicrm/internal/assignment"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/config"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/mail"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/threading"
	"github.com/wichert/minicrm/internal/workflow"
)

// Dialer opens an authenticated IMAP session for one configured account.
type Dialer func(ctx context.Context, acct config.MailAccountConfig) (mail.IMAPClient, error)

// Options controls a seed run.
type Options struct {
	File        *File
	Accounts    []config.MailAccountConfig
	Queries     *sqlc.Queries
	Pool        *pgxpool.Pool
	Attachments *storage.Store
	Logger      *slog.Logger
	Dial        Dialer
	SchemaReady func(ctx context.Context) error
	JobTimeout  time.Duration
	// Now overrides the clock used only for logging; message dates come from YAML.
	SkipIfSeeded bool
}

// Summary is the verification log payload.
type Summary struct {
	Skipped     bool
	Threads     int
	Messages    int
	ByAccount   map[string]AccountSummary
	JobsDrained int
}

// AccountSummary counts ingested threads by status for one account.
type AccountSummary struct {
	Threads int
	New     int
	Open    int
	Done    int
	Spam    int
}

// DefaultDialer uses the same IMAP dialer as the application.
func DefaultDialer(_ context.Context, acct config.MailAccountConfig) (mail.IMAPClient, error) {
	return mail.DialIMAP(acct.IMAPHost, acct.IMAPPort, acct.IMAPUsername, acct.IMAPPassword, acct.IMAPImplicitTLS)
}

// Run applies the seed document. It never migrates: SchemaReady (or a goose version check)
// must already succeed.
func Run(ctx context.Context, opt Options) (Summary, error) {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Dial == nil {
		opt.Dial = DefaultDialer
	}
	if opt.JobTimeout <= 0 {
		opt.JobTimeout = 2 * time.Minute
	}
	if opt.SchemaReady == nil {
		opt.SchemaReady = func(ctx context.Context) error { return SchemaExists(ctx, opt.Pool) }
	}
	if err := opt.SchemaReady(ctx); err != nil {
		return Summary{}, err
	}
	if opt.File == nil {
		return Summary{}, errors.New("seed file is required")
	}

	q := opt.Queries
	auditLogger := audit.NewLogger(q, opt.Logger)
	users, err := ensureUsers(ctx, q, auditLogger, opt.File.Users)
	if err != nil {
		return Summary{}, err
	}
	if err := ensureSettings(ctx, q, auditLogger, opt.File.Settings, users); err != nil {
		return Summary{}, err
	}

	if already, err := alreadySeeded(ctx, q, opt.File); err != nil {
		return Summary{}, err
	} else if already {
		opt.Logger.InfoContext(ctx, "seed_already_applied", slog.String("probe", probeMessageID(opt.File)))
		summary, err := summarize(ctx, q, opt.File)
		summary.Skipped = true
		return summary, err
	}

	if err := appendAll(ctx, opt); err != nil {
		return Summary{}, err
	}
	if err := ingestAll(ctx, opt, auditLogger); err != nil {
		return Summary{}, err
	}
	if err := applyState(ctx, opt, users, auditLogger); err != nil {
		return Summary{}, err
	}
	drained, err := drainJobs(ctx, opt)
	if err != nil {
		return Summary{}, err
	}
	summary, err := summarize(ctx, q, opt.File)
	if err != nil {
		return Summary{}, err
	}
	summary.JobsDrained = drained
	opt.Logger.InfoContext(ctx, "seed_completed",
		slog.Int("threads", summary.Threads),
		slog.Int("messages", summary.Messages),
		slog.Int("jobs_drained", drained),
	)
	for id, acct := range summary.ByAccount {
		opt.Logger.InfoContext(ctx, "seed_account_summary",
			slog.String("account_id", id),
			slog.Int("threads", acct.Threads),
			slog.Int("new", acct.New),
			slog.Int("in_progress", acct.Open),
			slog.Int("completed", acct.Done),
			slog.Int("spam", acct.Spam),
		)
	}
	return summary, nil
}

func ensureUsers(ctx context.Context, q *sqlc.Queries, logger *audit.Logger, users []User) (map[string]sqlc.User, error) {
	out := make(map[string]sqlc.User, len(users))
	for _, u := range users {
		existing, err := q.GetUserByEmail(ctx, u.Email)
		if err == nil {
			out[u.Email] = existing
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("lookup user %s: %w", u.Email, err)
		}
		created, err := q.CreateUser(ctx, sqlc.CreateUserParams{
			Email:           u.Email,
			DisplayName:     u.DisplayName,
			Role:            "agent",
			IsActive:        true,
			AllowedAccounts: append([]string(nil), u.Accounts...),
		})
		if err != nil {
			return nil, fmt.Errorf("create user %s: %w", u.Email, err)
		}
		if err := logger.LogSystemEvent(ctx, audit.SystemEventParams{
			TargetUserID: &created.ID,
			EventType:    "user_precreated",
			NewValue:     created,
		}); err != nil {
			return nil, err
		}
		out[u.Email] = created
	}
	return out, nil
}

func ensureSettings(ctx context.Context, q *sqlc.Queries, logger *audit.Logger, s Settings, users map[string]sqlc.User) error {
	if s.Timezone != "" {
		current, err := q.GetSystemSettings(ctx)
		if err != nil {
			return fmt.Errorf("load system_settings: %w", err)
		}
		if current.DisplayTimezone == "" || current.DisplayTimezone == "UTC" {
			current.DisplayTimezone = s.Timezone
			if _, err := q.UpsertSystemSettings(ctx, current); err != nil {
				return fmt.Errorf("set display_timezone: %w", err)
			}
			if err := logger.LogSystemEvent(ctx, audit.SystemEventParams{
				EventType: "settings_updated",
				NewValue:  map[string]string{"display_timezone": s.Timezone, "source": "seed"},
			}); err != nil {
				return err
			}
		}
	}
	for accountID, email := range s.DefaultAssignees {
		user, ok := users[email]
		if !ok {
			return fmt.Errorf("default assignee %s was not created", email)
		}
		if _, err := q.UpsertAccountSettings(ctx, sqlc.AccountSettings{
			AccountID:         accountID,
			DefaultAssigneeID: &user.ID,
		}); err != nil {
			return fmt.Errorf("set default assignee for %s: %w", accountID, err)
		}
		if err := logger.LogSystemEvent(ctx, audit.SystemEventParams{
			EventType: "account_settings_updated",
			NewValue:  map[string]string{"account_id": accountID, "default_assignee": email, "source": "seed"},
		}); err != nil {
			return err
		}
	}
	return nil
}

func alreadySeeded(ctx context.Context, q *sqlc.Queries, f *File) (bool, error) {
	id := probeMessageID(f)
	if id == "" {
		return false, nil
	}
	account := f.Threads[0].Account
	_, err := q.GetEmailByMessageID(ctx, account, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe seed message %s: %w", id, err)
	}
	return true, nil
}

func probeMessageID(f *File) string {
	if f == nil || len(f.Threads) == 0 || len(f.Threads[0].Messages) == 0 {
		return ""
	}
	return MessageID(f.Threads[0].Account, f.Threads[0].Slug(), 1)
}

func appendAll(ctx context.Context, opt Options) error {
	byID := accountsByID(opt.Accounts)
	lastID := map[string]string{}
	for _, thread := range opt.File.Threads {
		acct := byID[thread.Account]
		reopen := ""
		if thread.Reopens != "" {
			reopen = lastID[thread.Reopens]
		}
		built, err := BuildMessages(thread, acct, opt.File.BaseDir(), reopen)
		if err != nil {
			return err
		}
		client, err := opt.Dial(ctx, acct)
		if err != nil {
			return fmt.Errorf("dial imap for %s: %w", acct.ID, err)
		}
		for _, msg := range built {
			if err := client.AppendMessage(ctx, msg.Mailbox, msg.Raw); err != nil {
				_ = client.Close()
				return fmt.Errorf("append %s to %s: %w", msg.MessageID, msg.Mailbox, err)
			}
			lastID[thread.ID] = msg.MessageID
		}
		if err := client.Close(); err != nil {
			return fmt.Errorf("close imap for %s: %w", acct.ID, err)
		}
		opt.Logger.InfoContext(ctx, "seed_appended",
			slog.String("account_id", thread.Account),
			slog.String("thread", thread.ID),
			slog.Int("messages", len(built)),
		)
	}
	return nil
}

func ingestAll(ctx context.Context, opt Options, auditLogger *audit.Logger) error {
	assigner := assignment.NewEngine(0.5, assignment.NewDefaultAssigneeResolver(opt.Queries))
	matcher := threading.NewMatcher(opt.Queries, assigner, auditLogger)
	byID := accountsByID(opt.Accounts)
	for _, id := range opt.File.Accounts {
		acct := byID[id]
		dialer := func(ctx context.Context) (mail.IMAPClient, error) {
			return opt.Dial(ctx, acct)
		}
		worker := mail.NewIMAPSyncWorker(acct.ID, opt.Queries, matcher, opt.Attachments, dialer, time.Minute, opt.Logger).
			WithEndpoint(mail.IMAPEndpoint{Host: acct.IMAPHost, Port: acct.IMAPPort, Username: acct.IMAPUsername}).
			WithInitialSyncDays(0).
			WithOwnAddresses(acct.SMTPFromAddress, acct.IMAPUsername)
		n, err := worker.SyncOnce(ctx)
		if err != nil {
			return fmt.Errorf("sync account %s: %w", acct.ID, err)
		}
		opt.Logger.InfoContext(ctx, "seed_ingested", slog.String("account_id", acct.ID), slog.Int("ingested", n))
	}
	return nil
}

func applyState(ctx context.Context, opt Options, users map[string]sqlc.User, auditLogger *audit.Logger) error {
	wf := workflow.NewService(workflow.QueriesAdapter{Queries: opt.Queries}, auditLogger, nil)
	for _, thread := range opt.File.Threads {
		id, err := threadIDByProbe(ctx, opt.Queries, thread)
		if err != nil {
			return err
		}
		if thread.AssignedSource == AssignManual {
			user := users[thread.AssignedTo]
			if err := wf.Reassign(ctx, id, user.ID, nil); err != nil {
				return fmt.Errorf("assign thread %s: %w", thread.ID, err)
			}
		}
		row, err := opt.Queries.GetThreadByID(ctx, id)
		if err != nil {
			return fmt.Errorf("load thread %s: %w", thread.ID, err)
		}
		switch thread.Status {
		case StatusInProgress:
			if row.Status == sqlc.ThreadStatusNew {
				if err := wf.Transition(ctx, id, sqlc.ThreadStatusInProgress, nil); err != nil {
					return fmt.Errorf("transition %s to in_progress: %w", thread.ID, err)
				}
			}
		case StatusCompleted:
			switch row.Status {
			case sqlc.ThreadStatusNew:
				if err := wf.CompleteFromNew(ctx, id, nil); err != nil {
					return fmt.Errorf("complete thread %s: %w", thread.ID, err)
				}
			case sqlc.ThreadStatusInProgress:
				if err := wf.Transition(ctx, id, sqlc.ThreadStatusCompleted, nil); err != nil {
					return fmt.Errorf("complete thread %s: %w", thread.ID, err)
				}
			}
		case StatusSpam:
			if row.Status == sqlc.ThreadStatusInProgress || row.Status == sqlc.ThreadStatusNew {
				if err := wf.Transition(ctx, id, sqlc.ThreadStatusSpam, nil); err != nil {
					return fmt.Errorf("mark thread %s spam: %w", thread.ID, err)
				}
			}
		}
		if body := strings.TrimSpace(thread.Draft); body != "" && thread.Status != StatusCompleted && thread.Status != StatusSpam {
			if _, err := opt.Queries.AppendReplyDraft(ctx, row, body, "plain", "seed", false); err != nil {
				return fmt.Errorf("save draft on %s: %w", thread.ID, err)
			}
		}
	}
	return nil
}

func threadIDByProbe(ctx context.Context, q *sqlc.Queries, thread Thread) (uuid.UUID, error) {
	// The first inbound message creates the thread. Outbound-first threads are keyed by the
	// first inbound reply, which references the outbound Message-ID.
	probe := 1
	for i, msg := range thread.Messages {
		if msg.Direction == DirectionInbound || msg.Direction == "" {
			probe = i + 1
			break
		}
	}
	email, err := q.GetEmailByMessageID(ctx, thread.Account, MessageID(thread.Account, thread.Slug(), probe))
	if err != nil {
		return uuid.Nil, fmt.Errorf("find ingested thread %s: %w", thread.ID, err)
	}
	return email.ThreadID, nil
}

func drainJobs(ctx context.Context, opt Options) (int, error) {
	byID := accountsByID(opt.Accounts)
	endpoints := map[string]mail.IMAPEndpoint{}
	for id, acct := range byID {
		endpoints[id] = mail.IMAPEndpoint{Host: acct.IMAPHost, Port: acct.IMAPPort, Username: acct.IMAPUsername}
	}
	wake := make(chan struct{}, 1)
	worker := mail.NewIMAPActionWorker(opt.Queries, opt.Attachments, func(ctx context.Context, accountID string) (mail.IMAPClient, error) {
		acct, ok := byID[accountID]
		if !ok {
			return nil, fmt.Errorf("no IMAP account configured for %q", accountID)
		}
		return opt.Dial(ctx, acct)
	}, wake, opt.Logger).WithEndpoints(endpoints)

	deadline := time.Now().Add(opt.JobTimeout)
	total := 0
	for {
		n, err := worker.ProcessPending(ctx)
		total += n
		if err != nil {
			return total, fmt.Errorf("drain imap jobs: %w", err)
		}
		pending, err := countPendingJobs(ctx, opt.Pool)
		if err != nil {
			return total, err
		}
		if pending == 0 {
			return total, nil
		}
		if time.Now().After(deadline) {
			return total, fmt.Errorf("timed out draining imap jobs (%d still pending)", pending)
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func countPendingJobs(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	if pool == nil {
		return 0, errors.New("database pool is required to drain imap jobs")
	}
	var n int
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM imap_jobs WHERE status = 'pending' AND next_run_at <= NOW()`).Scan(&n)
	return n, err
}

func summarize(ctx context.Context, q *sqlc.Queries, f *File) (Summary, error) {
	summary := Summary{ByAccount: map[string]AccountSummary{}}
	for _, thread := range f.Threads {
		id, err := threadIDByProbe(ctx, q, thread)
		if err != nil {
			return Summary{}, err
		}
		row, err := q.GetThreadByID(ctx, id)
		if err != nil {
			return Summary{}, err
		}
		acct := summary.ByAccount[row.AccountID]
		acct.Threads++
		switch row.Status {
		case sqlc.ThreadStatusNew:
			acct.New++
		case sqlc.ThreadStatusInProgress:
			acct.Open++
		case sqlc.ThreadStatusCompleted:
			acct.Done++
		case sqlc.ThreadStatusSpam:
			acct.Spam++
		}
		summary.ByAccount[row.AccountID] = acct
		summary.Threads++
		emails, err := q.ListEmailsByThread(ctx, id)
		if err != nil {
			return Summary{}, err
		}
		summary.Messages += len(emails)
	}
	return summary, nil
}

func accountsByID(accounts []config.MailAccountConfig) map[string]config.MailAccountConfig {
	out := make(map[string]config.MailAccountConfig, len(accounts))
	for _, acct := range accounts {
		out[acct.ID] = acct
	}
	return out
}

// RequiredSchemaVersion is the newest goose migration the seed CLI depends on. Bump it
// whenever the seeding code starts relying on a new migration, so a half-migrated database
// is rejected instead of failing later with a confusing SQL error.
const RequiredSchemaVersion = 8

// ErrSchemaMissing reports that the application schema has not been created yet. It is
// transient: the seed CLI waits for miniCRM to finish its startup migrations.
var ErrSchemaMissing = errors.New("database schema is missing; run minicrm so migrations apply before seeding")

// SchemaRetryInterval is how long WaitForSchema sleeps between schema probes.
const SchemaRetryInterval = 500 * time.Millisecond

// WaitForSchema blocks until the application schema exists or ctx expires.
//
// Only a missing schema is retried: the compose seed service starts as soon as the miniCRM
// container has started, which is before its startup migrations have committed. A schema
// that exists but is too old, or a probe that fails outright, is a real configuration or
// ordering error and is returned immediately.
func WaitForSchema(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	return waitForSchema(ctx, func(ctx context.Context) error { return SchemaExists(ctx, pool) }, logger)
}

// waitForSchema is the retry loop behind WaitForSchema with an injectable probe.
func waitForSchema(ctx context.Context, probe func(context.Context) error, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	for attempt := 1; ; attempt++ {
		err := probe(ctx)
		if err == nil {
			if attempt > 1 {
				logger.InfoContext(ctx, "seed_schema_ready", slog.Int("attempts", attempt))
			}
			return nil
		}
		if !errors.Is(err, ErrSchemaMissing) {
			return err
		}
		logger.InfoContext(ctx, "seed_waiting_for_schema",
			slog.Int("attempt", attempt),
			slog.Duration("retry_in", SchemaRetryInterval),
		)
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for schema: %w (last error: %w)", ctx.Err(), err)
		case <-time.After(SchemaRetryInterval):
		}
	}
}

// SchemaExists reports whether goose has applied migrations. The seed CLI never migrates.
func SchemaExists(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("database pool is required")
	}
	var ready bool
	err := pool.QueryRow(ctx, `
		SELECT to_regclass('public.goose_db_version') IS NOT NULL
		   AND to_regclass('public.threads') IS NOT NULL
		   AND EXISTS (
		     SELECT 1 FROM information_schema.columns
		     WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'allowed_accounts'
		   )`).Scan(&ready)
	if err != nil {
		return fmt.Errorf("check schema: %w", err)
	}
	if !ready {
		return ErrSchemaMissing
	}
	var version int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version`).Scan(&version); err != nil {
		return fmt.Errorf("read goose version: %w", err)
	}
	if version < RequiredSchemaVersion {
		return fmt.Errorf("database schema version %d is older than %d; start minicrm to finish migrations before seeding", version, RequiredSchemaVersion)
	}
	return nil
}
