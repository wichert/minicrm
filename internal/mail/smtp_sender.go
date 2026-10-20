package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/threading"
	gomail "github.com/wneessen/go-mail"
)

// OutgoingAttachmentInput represents an uploaded file attachment for an outbound reply.
type OutgoingAttachmentInput struct {
	Filename    string
	ContentType string
	Data        []byte
}

// ComposeReplyParams describes an operator's outbound reply to a thread.
type ComposeReplyParams struct {
	ThreadID     uuid.UUID
	SenderUserID uuid.UUID
	BodyText     string
	Attachments  []OutgoingAttachmentInput
}

// SMTPTransport abstracts SMTP message delivery using username + password authentication.
type SMTPTransport interface {
	Send(ctx context.Context, msg *gomail.Msg) error
}

// GoMailTransport implements SMTPTransport using github.com/wneessen/go-mail.
type GoMailTransport struct {
	host     string
	port     int
	username string
	password string
	tlsMode  string
}

// NewGoMailTransport creates an SMTP transport configured with basic username + password auth.
func NewGoMailTransport(host string, port int, username, password, tlsMode string) *GoMailTransport {
	return &GoMailTransport{
		host:     host,
		port:     port,
		username: username,
		password: password,
		tlsMode:  tlsMode,
	}
}

// Send delivers msg through the configured SMTP server.
func (t *GoMailTransport) Send(ctx context.Context, msg *gomail.Msg) error {
	opts := []gomail.Option{
		gomail.WithPort(t.port),
		gomail.WithSMTPAuth(gomail.SMTPAuthPlain),
		gomail.WithUsername(t.username),
		gomail.WithPassword(t.password),
	}
	switch t.tlsMode {
	case "implicit":
		opts = append(opts, gomail.WithSSLPort(false))
		opts = append(opts, gomail.WithSSL())
	case "none":
		opts = append(opts, gomail.WithTLSPortPolicy(gomail.NoTLS))
	default:
		opts = append(opts, gomail.WithTLSPortPolicy(gomail.TLSMandatory))
	}
	client, err := gomail.NewClient(t.host, opts...)
	if err != nil {
		return fmt.Errorf("create smtp client for server %s:%d (user %q): %w", t.host, t.port, t.username, err)
	}
	if err := client.DialAndSendWithContext(ctx, msg); err != nil {
		return fmt.Errorf("smtp send via server %s:%d (user %q): %w", t.host, t.port, t.username, err)
	}
	return nil
}

// OutboxStore defines the persistence operations required for composing and sending outbound replies.
type OutboxStore interface {
	GetThreadByID(ctx context.Context, id uuid.UUID) (sqlc.Thread, error)
	UpdateThreadStatus(ctx context.Context, id uuid.UUID, status sqlc.ThreadStatus) error
	UpdateThreadAssignee(ctx context.Context, id, assigneeID uuid.UUID, source string) error
	TouchThreadLastMessageAt(ctx context.Context, id uuid.UUID, ts time.Time) error
	ListEmailsByThread(ctx context.Context, threadID uuid.UUID) ([]sqlc.Email, error)
	CreateEmail(ctx context.Context, e sqlc.Email) (sqlc.Email, error)
	GetEmailByID(ctx context.Context, id uuid.UUID) (sqlc.Email, error)
	CreateAttachment(ctx context.Context, a sqlc.Attachment) (sqlc.Attachment, error)
	ListAttachmentsByEmail(ctx context.Context, emailID uuid.UUID) ([]sqlc.Attachment, error)
	EnqueueOutboxEmail(ctx context.Context, emailID, senderUserID uuid.UUID) (sqlc.OutboxEmail, error)
	ClaimNextOutboxEmail(ctx context.Context) (sqlc.OutboxEmail, error)
	CompleteOutboxEmail(ctx context.Context, id uuid.UUID) error
	FailOutboxEmail(ctx context.Context, id uuid.UUID, attempts int, errMsg string, nextRun time.Time) error
	GetSystemSettings(ctx context.Context) (sqlc.SystemSettings, error)
	EnqueueIMAPJob(ctx context.Context, job sqlc.IMAPJob) (sqlc.IMAPJob, error)
}

// AccountSenderConfig holds the outbound SMTP identity of a single mail account.
type AccountSenderConfig struct {
	AccountID   string
	FromAddress string
	FromName    string
	Transport   SMTPTransport
	// SMTPServer (host:port) and SMTPUsername are used only to identify the account's
	// SMTP login in log records; they are optional.
	SMTPServer   string
	SMTPUsername string
}

// OutboxWorker composes outbound replies, sends them via SMTP, and queues IMAP Sent folder APPENDs.
type OutboxWorker struct {
	store         OutboxStore
	attachments   *storage.Store
	senders       map[string]AccountSenderConfig
	audit         *audit.Logger
	mailDomain    string
	outboxWakeCh  chan struct{}
	imapJobWakeCh chan<- struct{}
	logger        *slog.Logger
}

// NewOutboxWorker constructs an OutboxWorker with per-account SMTP sender configurations.
func NewOutboxWorker(
	store OutboxStore,
	attachments *storage.Store,
	senders []AccountSenderConfig,
	auditLogger *audit.Logger,
	mailDomain string,
	imapJobWakeCh chan<- struct{},
	logger *slog.Logger,
) *OutboxWorker {
	if mailDomain == "" {
		mailDomain = "minicrm.local"
	}
	if logger == nil {
		logger = slog.Default()
	}
	senderMap := make(map[string]AccountSenderConfig, len(senders))
	for _, cfg := range senders {
		senderMap[cfg.AccountID] = cfg
	}
	return &OutboxWorker{
		store:         store,
		attachments:   attachments,
		senders:       senderMap,
		audit:         auditLogger,
		mailDomain:    mailDomain,
		outboxWakeCh:  make(chan struct{}, 8),
		imapJobWakeCh: imapJobWakeCh,
		logger:        logger,
	}
}

// senderFor returns the SMTP sender configuration for a mail account.
func (w *OutboxWorker) senderFor(accountID string) (AccountSenderConfig, error) {
	cfg, ok := w.senders[accountID]
	if !ok {
		return AccountSenderConfig{}, fmt.Errorf("no SMTP sender configured for account %q", accountID)
	}
	return cfg, nil
}

// ComposeAndQueueReply creates an outbound email linked to the thread's latest message and queues it for SMTP delivery.
func (w *OutboxWorker) ComposeAndQueueReply(ctx context.Context, p ComposeReplyParams) (sqlc.Email, error) {
	if strings.TrimSpace(p.BodyText) == "" {
		return sqlc.Email{}, errors.New("reply body must not be empty")
	}
	thread, err := w.store.GetThreadByID(ctx, p.ThreadID)
	if err != nil {
		return sqlc.Email{}, fmt.Errorf("load thread: %w", err)
	}
	// Terminal threads cannot receive outbound mail. Completed threads must be reopened
	// through Reopen & Merge first; spam threads are never replied to.
	if thread.Status == sqlc.ThreadStatusSpam {
		return sqlc.Email{}, errors.New("replies are disabled on spam threads")
	}
	if thread.Status == sqlc.ThreadStatusCompleted {
		return sqlc.Email{}, errors.New("replies are disabled on completed threads")
	}
	sender, err := w.senderFor(thread.AccountID)
	if err != nil {
		return sqlc.Email{}, err
	}
	existingEmails, err := w.store.ListEmailsByThread(ctx, p.ThreadID)
	if err != nil {
		return sqlc.Email{}, fmt.Errorf("list thread emails: %w", err)
	}

	var (
		parentID      *uuid.UUID
		inReplyTo     *string
		referencesIDs []string
	)
	if len(existingEmails) > 0 {
		last := existingEmails[len(existingEmails)-1]
		parentID = &last.ID
		parentMsgID := last.MessageID
		inReplyTo = &parentMsgID
		referencesIDs = append(referencesIDs, last.ReferencesIDs...)
		referencesIDs = append(referencesIDs, parentMsgID)
		if len(referencesIDs) > 20 {
			referencesIDs = referencesIDs[len(referencesIDs)-20:]
		}
	}

	subject := thread.SubjectDisplay
	normSubject, hadPrefix := threading.NormalizeSubject(subject)
	if !hadPrefix {
		subject = "Re: " + subject
	}

	now := time.Now().UTC()
	msgID := fmt.Sprintf("%s@%s", uuid.NewString(), w.mailDomain)
	emailRec, err := w.store.CreateEmail(ctx, sqlc.Email{
		ThreadID:          thread.ID,
		AccountID:         thread.AccountID,
		ParentEmailID:     parentID,
		Direction:         "outbound",
		MessageID:         msgID,
		InReplyTo:         inReplyTo,
		ReferencesIDs:     referencesIDs,
		FromAddress:       sender.FromAddress,
		FromName:          sender.FromName,
		ToAddresses:       []sqlc.Address{{Name: thread.CustomerName, Email: thread.CustomerEmail}},
		CCAddresses:       []sqlc.Address{},
		Subject:           subject,
		SubjectNormalized: normSubject,
		BodyText:          p.BodyText,
		RawHeaders:        map[string]string{},
		SentAt:            now,
		ReceivedAt:        now,
	})
	if err != nil {
		return sqlc.Email{}, fmt.Errorf("insert outbound email: %w", err)
	}

	for _, att := range p.Attachments {
		if len(att.Data) == 0 {
			continue
		}
		blob, err := w.attachments.Save(ctx, bytes.NewReader(att.Data))
		if err != nil {
			return sqlc.Email{}, fmt.Errorf("save outbound attachment: %w", err)
		}
		ct := att.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		if _, err := w.store.CreateAttachment(ctx, sqlc.Attachment{
			EmailID:        emailRec.ID,
			Filename:       att.Filename,
			ContentType:    ct,
			SizeBytes:      blob.SizeBytes,
			SHA256Hex:      blob.SHA256Hex,
			StorageRelPath: blob.StorageRelPath,
		}); err != nil {
			return sqlc.Email{}, fmt.Errorf("insert outbound attachment row: %w", err)
		}
	}

	if _, err := w.store.EnqueueOutboxEmail(ctx, emailRec.ID, p.SenderUserID); err != nil {
		return sqlc.Email{}, fmt.Errorf("enqueue outbox email: %w", err)
	}
	if err := w.store.TouchThreadLastMessageAt(ctx, thread.ID, now); err != nil {
		return sqlc.Email{}, fmt.Errorf("touch thread last_message_at: %w", err)
	}

	if err := w.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
		ThreadID:    thread.ID,
		ActorUserID: &p.SenderUserID,
		EventType:   "reply_sent",
		NewValue:    map[string]string{"email_id": emailRec.ID.String(), "message_id": msgID},
	}); err != nil {
		return sqlc.Email{}, err
	}

	// Automatically assign an unassigned thread to the replying operator.
	if thread.AssigneeID == nil {
		if err := w.store.UpdateThreadAssignee(ctx, thread.ID, p.SenderUserID, "reply"); err != nil {
			return sqlc.Email{}, fmt.Errorf("assign thread to replying operator: %w", err)
		}
		if err := w.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
			ThreadID:    thread.ID,
			ActorUserID: &p.SenderUserID,
			EventType:   "assigned",
			OldValue:    map[string]any{"assignee_id": nil},
			NewValue:    map[string]string{"assignee_id": p.SenderUserID.String(), "source": "reply"},
		}); err != nil {
			return sqlc.Email{}, err
		}
	}

	// Automatically transition new -> in_progress when first reply is sent.
	if thread.Status == sqlc.ThreadStatusNew {
		if err := w.store.UpdateThreadStatus(ctx, thread.ID, sqlc.ThreadStatusInProgress); err != nil {
			return sqlc.Email{}, fmt.Errorf("auto-transition thread to in_progress: %w", err)
		}
		if err := w.audit.LogThreadEvent(ctx, audit.ThreadEventParams{
			ThreadID:    thread.ID,
			ActorUserID: &p.SenderUserID,
			EventType:   "status_changed",
			OldValue:    map[string]string{"status": string(sqlc.ThreadStatusNew)},
			NewValue:    map[string]string{"status": string(sqlc.ThreadStatusInProgress)},
		}); err != nil {
			return sqlc.Email{}, err
		}
	}

	select {
	case w.outboxWakeCh <- struct{}{}:
	default:
	}
	return emailRec, nil
}

// Run processes pending outbox emails until ctx is canceled.
func (w *OutboxWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		if _, err := w.ProcessPending(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.ErrorContext(ctx, "outbox_worker_error", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-w.outboxWakeCh:
		case <-ticker.C:
		}
	}
}

// ProcessPending sends all currently runnable outbox emails via SMTP and enqueues Sent folder APPEND jobs.
func (w *OutboxWorker) ProcessPending(ctx context.Context) (int, error) {
	sentCount := 0
	for {
		item, err := w.store.ClaimNextOutboxEmail(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return sentCount, nil
		}
		if err != nil {
			return sentCount, fmt.Errorf("claim outbox email: %w", err)
		}
		if accountID, err := w.sendOne(ctx, item); err != nil {
			backoff := time.Duration(1<<min(item.Attempts, 8)) * time.Second
			_ = w.store.FailOutboxEmail(ctx, item.ID, item.Attempts, err.Error(), time.Now().UTC().Add(backoff))
			attrs := []any{
				slog.String("outbox_id", item.ID.String()),
				slog.String("email_id", item.EmailID.String()),
				slog.String("account_id", accountID),
			}
			if cfg, ok := w.senders[accountID]; ok && cfg.SMTPServer != "" {
				attrs = append(attrs, slog.String("smtp_server", cfg.SMTPServer), slog.String("smtp_username", cfg.SMTPUsername))
			}
			attrs = append(attrs, slog.String("error", err.Error()))
			w.logger.WarnContext(ctx, "smtp_send_failed", attrs...)
			continue
		}
		sentCount++
	}
}

// sendOne delivers a single outbox email and reports the owning account ID (empty if the
// email could not be loaded) so failures can be attributed to an account in logs.
func (w *OutboxWorker) sendOne(ctx context.Context, item sqlc.OutboxEmail) (string, error) {
	accountID := ""
	err := w.sendOneInner(ctx, item, &accountID)
	return accountID, err
}

func (w *OutboxWorker) sendOneInner(ctx context.Context, item sqlc.OutboxEmail, accountID *string) error {
	emailRec, err := w.store.GetEmailByID(ctx, item.EmailID)
	if err != nil {
		return fmt.Errorf("load email %s: %w", item.EmailID, err)
	}
	*accountID = emailRec.AccountID
	atts, err := w.store.ListAttachmentsByEmail(ctx, emailRec.ID)
	if err != nil {
		return fmt.Errorf("load attachments for email %s: %w", emailRec.ID, err)
	}

	sender, err := w.senderFor(emailRec.AccountID)
	if err != nil {
		return err
	}

	msg, err := w.buildGoMailMessage(ctx, emailRec, atts)
	if err != nil {
		return err
	}

	var rawBuf bytes.Buffer
	if _, err := msg.WriteTo(&rawBuf); err != nil {
		return fmt.Errorf("serialize rfc5322 message: %w", err)
	}

	if err := sender.Transport.Send(ctx, msg); err != nil {
		return err
	}
	if err := w.store.CompleteOutboxEmail(ctx, item.ID); err != nil {
		return fmt.Errorf("complete outbox email: %w", err)
	}

	// Save serialized RFC 822 copy and enqueue IMAP APPEND to Sent folder.
	rawBlob, err := w.attachments.Save(ctx, bytes.NewReader(rawBuf.Bytes()))
	if err != nil {
		return fmt.Errorf("save raw rfc822 for sent append: %w", err)
	}
	sentMailbox := "Sent"
	if settings, err := w.store.GetSystemSettings(ctx); err == nil && settings.IMAPSentMailbox != "" {
		sentMailbox = settings.IMAPSentMailbox
	}
	relPath := rawBlob.StorageRelPath
	if _, err := w.store.EnqueueIMAPJob(ctx, sqlc.IMAPJob{
		AccountID:     emailRec.AccountID,
		JobType:       "append_to_sent",
		EmailID:       emailRec.ID,
		TargetMailbox: sentMailbox,
		RawRFC822Path: &relPath,
		MaxAttempts:   10,
	}); err != nil {
		return fmt.Errorf("enqueue append_to_sent job: %w", err)
	}
	if w.imapJobWakeCh != nil {
		select {
		case w.imapJobWakeCh <- struct{}{}:
		default:
		}
	}

	w.logger.InfoContext(ctx, "data_change",
		slog.String("event_category", "data_change"),
		slog.String("operation", "update"),
		slog.String("resource_type", "outbound_email"),
		slog.String("account_id", emailRec.AccountID),
		slog.String("resource_id", emailRec.ID.String()),
		slog.String("status", "sent"),
	)
	return nil
}

func (w *OutboxWorker) buildGoMailMessage(ctx context.Context, emailRec sqlc.Email, atts []sqlc.Attachment) (*gomail.Msg, error) {
	m := gomail.NewMsg()
	if emailRec.FromName != "" {
		if err := m.FromFormat(emailRec.FromName, emailRec.FromAddress); err != nil {
			return nil, fmt.Errorf("set from: %w", err)
		}
	} else if err := m.From(emailRec.FromAddress); err != nil {
		return nil, fmt.Errorf("set from: %w", err)
	}

	for _, to := range emailRec.ToAddresses {
		if err := m.AddTo(to.Email); err != nil {
			return nil, fmt.Errorf("add to %q: %w", to.Email, err)
		}
	}
	m.Subject(emailRec.Subject)
	m.SetDateWithValue(emailRec.SentAt)
	m.SetMessageIDWithValue(emailRec.MessageID)

	if emailRec.InReplyTo != nil && *emailRec.InReplyTo != "" {
		m.SetGenHeader(gomail.HeaderInReplyTo, "<"+*emailRec.InReplyTo+">")
	}
	if len(emailRec.ReferencesIDs) > 0 {
		formatted := make([]string, 0, len(emailRec.ReferencesIDs))
		for _, ref := range emailRec.ReferencesIDs {
			formatted = append(formatted, "<"+ref+">")
		}
		m.SetGenHeader(gomail.HeaderReferences, strings.Join(formatted, " "))
	}
	m.SetBodyString(gomail.TypeTextPlain, emailRec.BodyText)

	for _, att := range atts {
		f, err := w.attachments.Open(ctx, att.StorageRelPath)
		if err != nil {
			return nil, fmt.Errorf("open attachment %s: %w", att.Filename, err)
		}
		data, readErr := io.ReadAll(f)
		_ = f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read attachment %s: %w", att.Filename, readErr)
		}
		if err := m.AttachReader(att.Filename, bytes.NewReader(data)); err != nil {
			return nil, fmt.Errorf("attach %s: %w", att.Filename, err)
		}
	}
	return m, nil
}
