package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/storage"
)

// ActionJobStore defines the persistence operations required by IMAPActionWorker.
type ActionJobStore interface {
	ClaimNextIMAPJob(ctx context.Context) (sqlc.IMAPJob, error)
	CompleteIMAPJob(ctx context.Context, jobID, emailID uuid.UUID, newMailbox string) error
	FailIMAPJob(ctx context.Context, jobID uuid.UUID, attempts, maxAttempts int, errMsg string, nextRun time.Time) error
}

// AccountDialer returns an IMAP client for the given mail account ID.
type AccountDialer func(ctx context.Context, accountID string) (IMAPClient, error)

// IMAPActionWorker executes queued IMAP folder moves (Archive, Junk) and Sent folder APPENDs.
type IMAPActionWorker struct {
	store       ActionJobStore
	attachments *storage.Store
	dialer      AccountDialer
	endpoints   map[string]IMAPEndpoint
	wakeCh      <-chan struct{}
	logger      *slog.Logger
}

// NewIMAPActionWorker constructs an IMAPActionWorker.
func NewIMAPActionWorker(
	store ActionJobStore,
	attachments *storage.Store,
	dialer AccountDialer,
	wakeCh <-chan struct{},
	logger *slog.Logger,
) *IMAPActionWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &IMAPActionWorker{
		store:       store,
		attachments: attachments,
		dialer:      dialer,
		wakeCh:      wakeCh,
		logger:      logger,
	}
}

// WithEndpoints records the IMAP server and username per account ID for log records.
func (w *IMAPActionWorker) WithEndpoints(endpoints map[string]IMAPEndpoint) *IMAPActionWorker {
	w.endpoints = endpoints
	return w
}

// Run processes IMAP jobs until ctx is canceled.
func (w *IMAPActionWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		_, err := w.ProcessPending(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			w.logger.ErrorContext(ctx, "imap_action_worker_error", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wakeCh:
		case <-ticker.C:
		}
	}
}

// ProcessPending drains all currently runnable pending IMAP jobs.
func (w *IMAPActionWorker) ProcessPending(ctx context.Context) (int, error) {
	processed := 0
	for {
		store := w.store
		var tx pgx.Tx
		var job sqlc.IMAPJob
		var err error
		if transactional, ok := w.store.(interface {
			BeginIMAPJob(context.Context) (sqlc.IMAPJob, *sqlc.Queries, pgx.Tx, error)
		}); ok {
			var bound *sqlc.Queries
			job, bound, tx, err = transactional.BeginIMAPJob(ctx)
			if err == nil {
				store = bound
			}
		} else {
			job, err = store.ClaimNextIMAPJob(ctx)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return processed, nil
		}
		if err != nil {
			return processed, fmt.Errorf("claim next imap job: %w", err)
		}
		jobCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		executeErr := w.executeJob(jobCtx, store, job)
		cancel()
		if err := executeErr; err != nil {
			backoff := time.Duration(1<<min(job.Attempts, 8)) * time.Second
			nextRun := time.Now().UTC().Add(backoff)
			if failErr := store.FailIMAPJob(ctx, job.ID, job.Attempts, job.MaxAttempts, err.Error(), nextRun); failErr != nil {
				if tx != nil {
					_ = tx.Rollback(context.Background())
				}
				return processed, fmt.Errorf("reschedule imap job: %w", failErr)
			}
			if tx != nil {
				if commitErr := tx.Commit(ctx); commitErr != nil {
					return processed, fmt.Errorf("commit imap job failure: %w", commitErr)
				}
			}
			attrs := []any{
				slog.String("job_id", job.ID.String()),
				slog.String("job_type", job.JobType),
				slog.String("account_id", job.AccountID),
			}
			attrs = append(attrs, w.endpoints[job.AccountID].LogAttrs()...)
			attrs = append(attrs, slog.Int("attempts", job.Attempts), slog.String("error", err.Error()))
			w.logger.WarnContext(ctx, "imap_job_failed", attrs...)
			continue
		}
		if tx != nil {
			if err := tx.Commit(ctx); err != nil {
				return processed, fmt.Errorf("commit imap job: %w", err)
			}
		}
		processed++
	}
}

func (w *IMAPActionWorker) executeJob(ctx context.Context, store ActionJobStore, job sqlc.IMAPJob) error {
	if job.AccountID == "" {
		return fmt.Errorf("job %s has no account id", job.ID)
	}
	client, err := w.dialer(ctx, job.AccountID)
	if err != nil {
		return fmt.Errorf("dial imap for job %s (account %s): %w", job.ID, job.AccountID, err)
	}
	stopClose := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer func() { stopClose(); _ = client.Close() }()

	switch job.JobType {
	case "move_to_archive", "move_to_junk", "restore_from_junk":
		located, ok := store.(interface {
			RestoreIMAPEmail(context.Context, string, uuid.UUID) (sqlc.Email, error)
		})
		if !ok {
			return errors.New("IMAP store does not support locating messages")
		}
		em, err := located.RestoreIMAPEmail(ctx, job.AccountID, job.EmailID)
		if err != nil {
			return err
		}
		if em.IMAPMailbox == nil {
			return store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox)
		}
		if strings.EqualFold(*em.IMAPMailbox, job.TargetMailbox) {
			return store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox)
		}
		source, exists, err := client.ResolveMailbox(ctx, *em.IMAPMailbox)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("source folder unavailable")
		}
		// Resolve by exact Message-ID even with a saved UID: after a lost MOVE
		// response the original UID can no longer establish the message's location.
		matches, err := client.FindExactMessageID(ctx, source, em.MessageID)
		if err != nil {
			return err
		}
		if len(matches) > 1 {
			return errors.New("cannot safely move message: ambiguous source")
		}
		if len(matches) == 0 {
			target, found, err := client.ResolveMailbox(ctx, job.TargetMailbox)
			if err != nil {
				return err
			}
			if found {
				dest, err := client.FindExactMessageID(ctx, target, em.MessageID)
				if err != nil {
					return err
				}
				if len(dest) > 1 {
					return errors.New("cannot safely reconcile message: ambiguous destination")
				}
				if len(dest) == 1 {
					return store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox)
				}
			}
			// A message deleted externally remains a benign no-op for ordinary moves.
			if job.JobType != "restore_from_junk" && em.IMAPUID != nil {
				return store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox)
			}
			return errors.New("cannot safely move message: no exact match in source or destination")
		}
		if err := client.MoveMessage(ctx, source, matches[0].UIDValidity, matches[0].UID, job.TargetMailbox); err != nil {
			return err
		}
		if err := store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox); err != nil {
			return err
		}
		w.logger.InfoContext(ctx, "data_change", slog.String("event_category", "data_change"), slog.String("operation", "update"), slog.String("resource_type", "imap_message_folder"), slog.String("account_id", job.AccountID), slog.String("email_id", job.EmailID.String()), slog.String("job_type", job.JobType), slog.String("target_mailbox", job.TargetMailbox))
		return nil

	case "append_to_sent":
		if job.RawRFC822Path == nil || *job.RawRFC822Path == "" {
			return store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox)
		}
		f, err := w.attachments.Open(ctx, *job.RawRFC822Path)
		if err != nil {
			return fmt.Errorf("open raw rfc822 for sent append: %w", err)
		}
		rawBytes, readErr := io.ReadAll(f)
		_ = f.Close()
		if readErr != nil {
			return fmt.Errorf("read raw rfc822: %w", readErr)
		}
		if err := client.AppendMessage(ctx, job.TargetMailbox, rawBytes); err != nil {
			return err
		}
		if err := store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox); err != nil {
			return err
		}
		w.logger.InfoContext(ctx, "data_change",
			slog.String("event_category", "data_change"),
			slog.String("operation", "create"),
			slog.String("resource_type", "imap_sent_copy"),
			slog.String("account_id", job.AccountID),
			slog.String("email_id", job.EmailID.String()),
			slog.String("target_mailbox", job.TargetMailbox),
		)
		return nil

	default:
		return fmt.Errorf("unknown imap job_type %q", job.JobType)
	}
}
