package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// IMAPActionWorker executes queued IMAP folder moves (Archive, Junk) and Sent folder APPENDs.
type IMAPActionWorker struct {
	store       ActionJobStore
	attachments *storage.Store
	dialer      func(ctx context.Context) (IMAPClient, error)
	wakeCh      <-chan struct{}
	logger      *slog.Logger
}

// NewIMAPActionWorker constructs an IMAPActionWorker.
func NewIMAPActionWorker(
	store ActionJobStore,
	attachments *storage.Store,
	dialer func(ctx context.Context) (IMAPClient, error),
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
		job, err := w.store.ClaimNextIMAPJob(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return processed, nil
		}
		if err != nil {
			return processed, fmt.Errorf("claim next imap job: %w", err)
		}
		if err := w.executeJob(ctx, job); err != nil {
			backoff := time.Duration(1<<min(job.Attempts, 8)) * time.Second
			nextRun := time.Now().UTC().Add(backoff)
			_ = w.store.FailIMAPJob(ctx, job.ID, job.Attempts, job.MaxAttempts, err.Error(), nextRun)
			w.logger.WarnContext(ctx, "imap_job_failed",
				slog.String("job_id", job.ID.String()),
				slog.String("job_type", job.JobType),
				slog.Int("attempts", job.Attempts),
				slog.String("error", err.Error()),
			)
			continue
		}
		processed++
	}
}

func (w *IMAPActionWorker) executeJob(ctx context.Context, job sqlc.IMAPJob) error {
	client, err := w.dialer(ctx)
	if err != nil {
		return fmt.Errorf("dial imap for job %s: %w", job.ID, err)
	}
	defer func() { _ = client.Close() }()

	switch job.JobType {
	case "move_to_archive", "move_to_junk":
		if job.SourceMailbox == nil || job.IMAPUID == nil {
			// Already moved or missing UID; complete as benign no-op.
			return w.store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox)
		}
		var uidVal int64
		if job.IMAPUIDValidity != nil {
			uidVal = *job.IMAPUIDValidity
		}
		err := client.MoveMessage(ctx, *job.SourceMailbox, uidVal, *job.IMAPUID, job.TargetMailbox)
		if err != nil && !errors.Is(err, ErrMessageNotFound) {
			return err
		}
		if err := w.store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox); err != nil {
			return err
		}
		w.logger.InfoContext(ctx, "data_change",
			slog.String("event_category", "data_change"),
			slog.String("operation", "update"),
			slog.String("resource_type", "imap_message_folder"),
			slog.String("email_id", job.EmailID.String()),
			slog.String("job_type", job.JobType),
			slog.String("target_mailbox", job.TargetMailbox),
		)
		return nil

	case "append_to_sent":
		if job.RawRFC822Path == nil || *job.RawRFC822Path == "" {
			return w.store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox)
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
		if err := w.store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox); err != nil {
			return err
		}
		w.logger.InfoContext(ctx, "data_change",
			slog.String("event_category", "data_change"),
			slog.String("operation", "create"),
			slog.String("resource_type", "imap_sent_copy"),
			slog.String("email_id", job.EmailID.String()),
			slog.String("target_mailbox", job.TargetMailbox),
		)
		return nil

	default:
		return fmt.Errorf("unknown imap job_type %q", job.JobType)
	}
}
