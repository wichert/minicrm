package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/threading"
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
		processed++
	}
}

func (w *IMAPActionWorker) executeJob(ctx context.Context, job sqlc.IMAPJob) error {
	if job.AccountID == "" {
		return fmt.Errorf("job %s has no account id", job.ID)
	}
	client, err := w.dialer(ctx, job.AccountID)
	if err != nil {
		return fmt.Errorf("dial imap for job %s (account %s): %w", job.ID, job.AccountID, err)
	}
	defer func() { _ = client.Close() }()

	switch job.JobType {
	case "move_to_archive", "move_to_junk":
		if job.SourceMailbox == nil {
			return w.store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox)
		}
		source, uid := *job.SourceMailbox, job.IMAPUID
		uidValidity := job.IMAPUIDValidity
		if store, ok := w.store.(interface {
			RestoreIMAPEmail(context.Context, string, uuid.UUID) (sqlc.Email, error)
		}); ok {
			// A preceding recovery/move changes the UID. Resolve the current location
			// only after that job has completed, rather than using a stale snapshot.
			em, err := store.RestoreIMAPEmail(ctx, job.AccountID, job.EmailID)
			if err != nil {
				return err
			}
			if em.IMAPMailbox == nil || strings.EqualFold(*em.IMAPMailbox, job.TargetMailbox) {
				return w.store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox)
			}
			source = *em.IMAPMailbox
			if em.IMAPUID != nil {
				uid, uidValidity = em.IMAPUID, em.IMAPUIDValidity
			} else {
				actual, exists, err := client.ResolveMailbox(ctx, source)
				if err != nil {
					return err
				}
				if !exists {
					return errors.New("source folder unavailable")
				}
				matches, err := client.SearchByMessageID(ctx, actual, em.MessageID, 0)
				if err != nil {
					return err
				}
				var exact []RawIMAPMessage
				for _, candidate := range matches {
					parsed, err := mail.ReadMessage(strings.NewReader(string(candidate.RawRFC822)))
					if err == nil && threading.NormalizeMessageID(parsed.Header.Get("Message-ID")) == em.MessageID {
						exact = append(exact, candidate)
					}
				}
				if len(exact) != 1 {
					return fmt.Errorf("cannot safely move message: found %d exact matches in source", len(exact))
				}
				source, uid, uidValidity = actual, &exact[0].UID, &exact[0].UIDValidity
			}
		} else if uid == nil {
			return errors.New("IMAP store does not support locating moved messages")
		}
		var uidVal int64
		if uidValidity != nil {
			uidVal = *uidValidity
		}
		err := client.MoveMessage(ctx, source, uidVal, *uid, job.TargetMailbox)
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
			slog.String("account_id", job.AccountID),
			slog.String("email_id", job.EmailID.String()),
			slog.String("job_type", job.JobType),
			slog.String("target_mailbox", job.TargetMailbox),
		)
		return nil

	case "restore_from_junk":
		store, ok := w.store.(interface {
			RestoreIMAPEmail(context.Context, string, uuid.UUID) (sqlc.Email, error)
		})
		if !ok {
			return errors.New("IMAP store does not support recovery")
		}
		em, err := store.RestoreIMAPEmail(ctx, job.AccountID, job.EmailID)
		if err != nil {
			return err
		}
		if job.SourceMailbox == nil || em.MessageID == "" {
			return errors.New("recovery requires a source folder and message ID")
		}
		actual, exists, err := client.ResolveMailbox(ctx, *job.SourceMailbox)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("junk folder unavailable")
		}
		matches, err := client.SearchByMessageID(ctx, actual, em.MessageID, 0)
		if err != nil {
			return err
		}
		var exact []RawIMAPMessage
		for _, candidate := range matches {
			parsed, err := mail.ReadMessage(strings.NewReader(string(candidate.RawRFC822)))
			if err == nil && threading.NormalizeMessageID(parsed.Header.Get("Message-ID")) == em.MessageID {
				exact = append(exact, candidate)
			}
		}
		if len(exact) == 0 && em.IMAPMailbox != nil && strings.EqualFold(*em.IMAPMailbox, job.TargetMailbox) {
			return w.store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox)
		}
		if len(exact) != 1 {
			return fmt.Errorf("cannot safely restore message: found %d exact matches in junk", len(exact))
		}
		if err := client.MoveMessage(ctx, actual, exact[0].UIDValidity, exact[0].UID, job.TargetMailbox); err != nil {
			return err
		}
		return w.store.CompleteIMAPJob(ctx, job.ID, job.EmailID, job.TargetMailbox)

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
			slog.String("account_id", job.AccountID),
			slog.String("email_id", job.EmailID.String()),
			slog.String("target_mailbox", job.TargetMailbox),
		)
		return nil

	default:
		return fmt.Errorf("unknown imap job_type %q", job.JobType)
	}
}
