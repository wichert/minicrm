package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"sort"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/threading"
)

// ErrMessageNotFound indicates an IMAP UID no longer exists in the source mailbox.
var ErrMessageNotFound = errors.New("imap message uid not found in mailbox")

// RawIMAPMessage holds a raw RFC 5322 message and its UID fetched from an IMAP mailbox.
type RawIMAPMessage struct {
	UID         int64
	UIDValidity int64
	RawRFC822   []byte
}

// IMAPClient abstracts IMAP operations for syncing INBOX, moving messages to Junk/Archive, and appending Sent.
type IMAPClient interface {
	SelectMailbox(ctx context.Context, mailbox string) (uidValidity int64, err error)
	FetchMessagesSinceUID(ctx context.Context, mailbox string, afterUID int64) (uidValidity int64, msgs []RawIMAPMessage, err error)
	MoveMessage(ctx context.Context, sourceMailbox string, uidValidity, uid int64, targetMailbox string) error
	AppendMessage(ctx context.Context, targetMailbox string, rawRFC822 []byte) error
	Close() error
}

// GoIMAPClient implements IMAPClient using github.com/emersion/go-imap/v2/imapclient.
type GoIMAPClient struct {
	client *imapclient.Client
}

// DialIMAP connects and authenticates to the IMAP server using basic username + password.
func DialIMAP(host string, port int, username, password string, implicitTLS bool) (*GoIMAPClient, error) {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	var (
		c   *imapclient.Client
		err error
	)
	tlsCfg := &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}
	if implicitTLS {
		c, err = imapclient.DialTLS(addr, &imapclient.Options{TLSConfig: tlsCfg})
	} else {
		c, err = imapclient.DialStartTLS(addr, &imapclient.Options{TLSConfig: tlsCfg})
	}
	if err != nil {
		return nil, fmt.Errorf("dial imap %s: %w", addr, err)
	}
	if err := c.Login(username, password).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap login: %w", err)
	}
	return &GoIMAPClient{client: c}, nil
}

// SelectMailbox selects the given mailbox and returns its UIDVALIDITY.
func (g *GoIMAPClient) SelectMailbox(_ context.Context, mailbox string) (int64, error) {
	data, err := g.client.Select(mailbox, nil).Wait()
	if err != nil {
		return 0, fmt.Errorf("imap select %q: %w", mailbox, err)
	}
	return int64(data.UIDValidity), nil
}

// FetchMessagesSinceUID fetches all messages in mailbox with UID > afterUID using BODY.PEEK[].
func (g *GoIMAPClient) FetchMessagesSinceUID(ctx context.Context, mailbox string, afterUID int64) (int64, []RawIMAPMessage, error) {
	uidVal, err := g.SelectMailbox(ctx, mailbox)
	if err != nil {
		return 0, nil, err
	}

	startUID, err := toIMAPUID(afterUID + 1)
	if err != nil {
		return 0, nil, err
	}
	var uidSet imap.UIDSet
	uidSet.AddRange(startUID, 0) // <afterUID+1>:*
	bodySection := &imap.FetchItemBodySection{Peek: true}
	fetchOpts := &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{bodySection},
	}

	fetchCmd := g.client.Fetch(uidSet, fetchOpts)
	var out []RawIMAPMessage
	for {
		msg := fetchCmd.Next()
		if msg == nil {
			break
		}
		var (
			uid     imap.UID
			rawBody []byte
		)
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			switch data := item.(type) {
			case imapclient.FetchItemDataUID:
				uid = data.UID
			case imapclient.FetchItemDataBodySection:
				rawBody, _ = io.ReadAll(data.Literal)
			}
		}
		if int64(uid) > afterUID && len(rawBody) > 0 {
			out = append(out, RawIMAPMessage{
				UID:         int64(uid),
				UIDValidity: uidVal,
				RawRFC822:   rawBody,
			})
		}
	}
	if err := fetchCmd.Close(); err != nil {
		return uidVal, nil, fmt.Errorf("imap fetch close: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return uidVal, out, nil
}

// MoveMessage moves a message by UID from sourceMailbox to targetMailbox (creating targetMailbox if needed).
func (g *GoIMAPClient) MoveMessage(ctx context.Context, sourceMailbox string, uidValidity, uid int64, targetMailbox string) error {
	_ = g.client.Create(targetMailbox, nil).Wait()
	curValidity, err := g.SelectMailbox(ctx, sourceMailbox)
	if err != nil {
		return err
	}
	if uidValidity > 0 && curValidity != uidValidity {
		// UIDVALIDITY changed; UID is no longer valid, treat as benign no-op.
		return ErrMessageNotFound
	}
	imapUID, err := toIMAPUID(uid)
	if err != nil {
		return err
	}
	var uidSet imap.UIDSet
	uidSet.AddNum(imapUID)
	if _, err := g.client.Move(uidSet, targetMailbox).Wait(); err != nil {
		return fmt.Errorf("imap move uid %d to %q: %w", uid, targetMailbox, err)
	}
	return nil
}

// AppendMessage appends a raw RFC 822 message to targetMailbox with the \Seen flag.
func (g *GoIMAPClient) AppendMessage(_ context.Context, targetMailbox string, rawRFC822 []byte) error {
	_ = g.client.Create(targetMailbox, nil).Wait()
	appendCmd := g.client.Append(targetMailbox, int64(len(rawRFC822)), &imap.AppendOptions{
		Flags: []imap.Flag{imap.FlagSeen},
		Time:  time.Now().UTC(),
	})
	if _, err := appendCmd.Write(rawRFC822); err != nil {
		_ = appendCmd.Close()
		return fmt.Errorf("imap append write: %w", err)
	}
	if err := appendCmd.Close(); err != nil {
		return fmt.Errorf("imap append close: %w", err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		return fmt.Errorf("imap append wait: %w", err)
	}
	return nil
}

// Close closes the underlying IMAP connection.
func (g *GoIMAPClient) Close() error {
	return g.client.Close()
}

func toIMAPUID(v int64) (imap.UID, error) {
	if v <= 0 || v > math.MaxUint32 {
		return 0, fmt.Errorf("imap uid %d out of uint32 range", v)
	}
	return imap.UID(v), nil
}

// SyncStore defines the persistence operations needed by IMAPSyncWorker.
type SyncStore interface {
	GetSystemSettings(ctx context.Context) (sqlc.SystemSettings, error)
	GetIMAPSyncState(ctx context.Context, mailbox string) (sqlc.IMAPSyncState, error)
	UpsertIMAPSyncState(ctx context.Context, s sqlc.IMAPSyncState) error
	GetEmailByMessageID(ctx context.Context, msgID string) (sqlc.Email, error)
	CreateEmail(ctx context.Context, e sqlc.Email) (sqlc.Email, error)
	CreateAttachment(ctx context.Context, a sqlc.Attachment) (sqlc.Attachment, error)
}

// IMAPSyncWorker pulls incoming emails from the IMAP INBOX and ingests them into PostgreSQL + disk.
type IMAPSyncWorker struct {
	store       SyncStore
	matcher     *threading.Matcher
	attachments *storage.Store
	dialer      func(ctx context.Context) (IMAPClient, error)
	pollEvery   time.Duration
	logger      *slog.Logger
}

// NewIMAPSyncWorker creates a new IMAPSyncWorker.
func NewIMAPSyncWorker(
	store SyncStore,
	matcher *threading.Matcher,
	attachments *storage.Store,
	dialer func(ctx context.Context) (IMAPClient, error),
	pollEvery time.Duration,
	logger *slog.Logger,
) *IMAPSyncWorker {
	if pollEvery <= 0 {
		pollEvery = 2 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &IMAPSyncWorker{
		store:       store,
		matcher:     matcher,
		attachments: attachments,
		dialer:      dialer,
		pollEvery:   pollEvery,
		logger:      logger,
	}
}

// Run starts the periodic IMAP sync loop until ctx is canceled.
func (w *IMAPSyncWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollEvery)
	defer ticker.Stop()

	for {
		if _, err := w.SyncOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.ErrorContext(ctx, "imap_sync_error", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SyncOnce performs a single synchronization pass against the configured IMAP INBOX.
func (w *IMAPSyncWorker) SyncOnce(ctx context.Context) (int, error) {
	settings, err := w.store.GetSystemSettings(ctx)
	if err != nil {
		return 0, fmt.Errorf("load system_settings: %w", err)
	}
	inbox := settings.IMAPInboxMailbox
	if inbox == "" {
		inbox = "INBOX"
	}

	client, err := w.dialer(ctx)
	if err != nil {
		return 0, fmt.Errorf("dial imap: %w", err)
	}
	defer func() { _ = client.Close() }()

	syncState, err := w.store.GetIMAPSyncState(ctx, inbox)
	if errors.Is(err, pgx.ErrNoRows) {
		syncState = sqlc.IMAPSyncState{Mailbox: inbox, UIDValidity: 0, LastSyncedUID: 0}
	} else if err != nil {
		return 0, fmt.Errorf("get imap_sync_state: %w", err)
	}

	uidVal, err := client.SelectMailbox(ctx, inbox)
	if err != nil {
		return 0, err
	}
	afterUID := syncState.LastSyncedUID
	if syncState.UIDValidity != 0 && syncState.UIDValidity != uidVal {
		// UIDVALIDITY changed: reset watermark and rely on Message-ID deduplication.
		afterUID = 0
	}

	_, msgs, err := client.FetchMessagesSinceUID(ctx, inbox, afterUID)
	if err != nil {
		return 0, err
	}

	ingested := 0
	highestUID := afterUID
	for _, rawMsg := range msgs {
		if err := w.ingestOne(ctx, inbox, uidVal, rawMsg); err != nil {
			return ingested, fmt.Errorf("ingest uid %d: %w", rawMsg.UID, err)
		}
		if rawMsg.UID > highestUID {
			highestUID = rawMsg.UID
		}
		if err := w.store.UpsertIMAPSyncState(ctx, sqlc.IMAPSyncState{
			Mailbox:       inbox,
			UIDValidity:   uidVal,
			LastSyncedUID: highestUID,
		}); err != nil {
			return ingested, fmt.Errorf("update imap_sync_state: %w", err)
		}
		ingested++
	}
	return ingested, nil
}

func (w *IMAPSyncWorker) ingestOne(ctx context.Context, mailbox string, uidValidity int64, rawMsg RawIMAPMessage) error {
	parsed, err := ParseRFC5322(bytes.NewReader(rawMsg.RawRFC822))
	if err != nil {
		return fmt.Errorf("parse rfc5322: %w", err)
	}

	// Deduplicate by Message-ID
	if _, err := w.store.GetEmailByMessageID(ctx, parsed.MessageID); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check existing message_id: %w", err)
	}

	toList := make([]string, 0, len(parsed.ToAddresses))
	for _, a := range parsed.ToAddresses {
		toList = append(toList, a.Email)
	}

	resolution, err := w.matcher.ResolveThread(ctx, threading.InboundMessageMetadata{
		MessageID:     parsed.MessageID,
		InReplyTo:     parsed.InReplyTo,
		ReferencesIDs: parsed.ReferencesIDs,
		FromAddress:   parsed.FromAddress,
		FromName:      parsed.FromName,
		ToAddresses:   toList,
		Subject:       parsed.Subject,
		BodyText:      parsed.BodyText,
		SentAt:        parsed.SentAt,
	})
	if err != nil {
		return fmt.Errorf("resolve thread: %w", err)
	}

	var inReplyToPtr *string
	if parsed.InReplyTo != "" {
		inReplyToPtr = &parsed.InReplyTo
	}
	uid := rawMsg.UID
	emailRec, err := w.store.CreateEmail(ctx, sqlc.Email{
		ThreadID:          resolution.Thread.ID,
		ParentEmailID:     resolution.ParentEmailID,
		Direction:         "inbound",
		MessageID:         parsed.MessageID,
		InReplyTo:         inReplyToPtr,
		ReferencesIDs:     parsed.ReferencesIDs,
		IMAPMailbox:       &mailbox,
		IMAPUIDValidity:   &uidValidity,
		IMAPUID:           &uid,
		FromAddress:       parsed.FromAddress,
		FromName:          parsed.FromName,
		ToAddresses:       parsed.ToAddresses,
		CCAddresses:       parsed.CCAddresses,
		Subject:           parsed.Subject,
		SubjectNormalized: parsed.SubjectNormalized,
		BodyText:          parsed.BodyText,
		BodyHTMLSanitized: parsed.BodyHTMLSanitized,
		RawHeaders:        parsed.RawHeaders,
		SentAt:            parsed.SentAt,
		ReceivedAt:        time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("create email: %w", err)
	}

	for _, att := range parsed.Attachments {
		blob, err := w.attachments.Save(ctx, bytes.NewReader(att.Data))
		if err != nil {
			return fmt.Errorf("save attachment %q: %w", att.Filename, err)
		}
		if _, err := w.store.CreateAttachment(ctx, sqlc.Attachment{
			EmailID:        emailRec.ID,
			Filename:       att.Filename,
			ContentType:    att.ContentType,
			SizeBytes:      blob.SizeBytes,
			SHA256Hex:      blob.SHA256Hex,
			StorageRelPath: blob.StorageRelPath,
			ContentID:      att.ContentID,
			IsInline:       att.IsInline,
		}); err != nil {
			return fmt.Errorf("insert attachment record: %w", err)
		}
	}

	w.logger.InfoContext(ctx, "data_change",
		slog.String("event_category", "data_change"),
		slog.String("operation", "create"),
		slog.String("resource_type", "inbound_email"),
		slog.String("resource_id", emailRec.ID.String()),
		slog.String("thread_id", resolution.Thread.ID.String()),
		slog.String("message_id", parsed.MessageID),
	)
	return nil
}
