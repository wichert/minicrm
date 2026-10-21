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
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/google/uuid"
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
	// ListUIDsSince returns, in ascending order, the UIDs in mailbox that are greater than afterUID.
	// When since is non-zero only messages received on or after that date are returned.
	ListUIDsSince(ctx context.Context, mailbox string, afterUID int64, since time.Time) (uids []int64, err error)
	// FetchMessagesByUID downloads the full messages for the given UIDs of the currently selected or given mailbox.
	FetchMessagesByUID(ctx context.Context, mailbox string, uids []int64) ([]RawIMAPMessage, error)
	// ResolveMailbox maps a configured folder name onto the server's real mailbox name without
	// creating it, and reports whether the mailbox exists.
	ResolveMailbox(ctx context.Context, name string) (actual string, exists bool, err error)
	// SearchByMessageID returns the messages in mailbox whose Message-ID header contains messageID
	// (normalized, without angle brackets), at most limit of them.
	SearchByMessageID(ctx context.Context, mailbox, messageID string, limit int) ([]RawIMAPMessage, error)
	MoveMessage(ctx context.Context, sourceMailbox string, uidValidity, uid int64, targetMailbox string) error
	AppendMessage(ctx context.Context, targetMailbox string, rawRFC822 []byte) error
	Close() error
}

// GoIMAPClient implements IMAPClient using github.com/emersion/go-imap/v2/imapclient.
type GoIMAPClient struct {
	client *imapclient.Client
}

// IMAPEndpoint identifies the IMAP server and login used for a mail account; it is attached to
// log records so failures can be traced to a specific account and server.
type IMAPEndpoint struct {
	Host     string
	Port     int
	Username string
}

// Addr returns host:port.
func (e IMAPEndpoint) Addr() string {
	return net.JoinHostPort(e.Host, fmt.Sprintf("%d", e.Port))
}

// LogAttrs returns structured log attributes describing the endpoint (never the password).
func (e IMAPEndpoint) LogAttrs() []any {
	if e.Host == "" {
		return nil
	}
	return []any{slog.String("imap_server", e.Addr()), slog.String("imap_username", e.Username)}
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
		return nil, fmt.Errorf("dial imap server %s (user %q): %w", addr, username, err)
	}
	if err := c.Login(username, password).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap login to server %s as user %q: %w", addr, username, err)
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

// ListUIDsSince returns the ascending UIDs in mailbox with UID > afterUID, optionally limited to
// messages received on or after since (IMAP SEARCH SINCE; only the date is significant).
func (g *GoIMAPClient) ListUIDsSince(ctx context.Context, mailbox string, afterUID int64, since time.Time) ([]int64, error) {
	if _, err := g.SelectMailbox(ctx, mailbox); err != nil {
		return nil, err
	}
	criteria := &imap.SearchCriteria{}
	if afterUID > 0 {
		startUID, err := toIMAPUID(afterUID + 1)
		if err != nil {
			return nil, err
		}
		var set imap.UIDSet
		set.AddRange(startUID, 0) // <afterUID+1>:*
		criteria.UID = []imap.UIDSet{set}
	}
	if !since.IsZero() {
		criteria.Since = since
	}
	data, err := g.client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap uid search in %q: %w", mailbox, err)
	}
	var out []int64
	for _, uid := range data.AllUIDs() {
		// "N:*" always matches the highest UID even when it is below N, so filter explicitly.
		if int64(uid) > afterUID {
			out = append(out, int64(uid))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// FetchMessagesByUID downloads the complete messages (BODY.PEEK[]) for uids from mailbox.
// A read error on any literal fails the whole call so truncated messages are never ingested.
func (g *GoIMAPClient) FetchMessagesByUID(ctx context.Context, mailbox string, uids []int64) ([]RawIMAPMessage, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	uidVal, err := g.SelectMailbox(ctx, mailbox)
	if err != nil {
		return nil, err
	}
	var uidSet imap.UIDSet
	for _, u := range uids {
		iu, err := toIMAPUID(u)
		if err != nil {
			return nil, err
		}
		uidSet.AddNum(iu)
	}
	fetchOpts := &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
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
			readErr error
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
				rawBody, readErr = io.ReadAll(data.Literal)
			}
		}
		if readErr != nil {
			_ = fetchCmd.Close()
			return nil, fmt.Errorf("read imap message uid %d: %w", uid, readErr)
		}
		out = append(out, RawIMAPMessage{UID: int64(uid), UIDValidity: uidVal, RawRFC822: rawBody})
	}
	if err := fetchCmd.Close(); err != nil {
		return nil, fmt.Errorf("imap fetch close: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// ensureMailbox maps a configured folder name (e.g. "Junk") onto the server's real mailbox name
// and creates the folder when it does not exist yet. Servers that keep personal folders below
// INBOX (namespace prefix "INBOX." or "INBOX/") reject top-level names such as "Junk", so the
// personal namespace prefix is applied when needed.
func (g *GoIMAPClient) ensureMailbox(name string) (string, error) {
	target, exists, err := g.resolveMailbox(name)
	if err != nil {
		return "", err
	}
	if exists {
		return target, nil
	}
	if err := g.client.Create(target, nil).Wait(); err != nil {
		return "", fmt.Errorf("create imap mailbox %q (configured as %q): %w", target, name, err)
	}
	return target, nil
}

// ResolveMailbox maps a configured folder name onto the server's real mailbox name without creating it.
func (g *GoIMAPClient) ResolveMailbox(_ context.Context, name string) (string, bool, error) {
	return g.resolveMailbox(name)
}

func (g *GoIMAPClient) resolveMailbox(name string) (string, bool, error) {
	listed, err := g.client.List("", "*", nil).Collect()
	if err != nil {
		return "", false, fmt.Errorf("imap list mailboxes: %w", err)
	}
	existing := make([]string, 0, len(listed))
	var delim rune
	for _, l := range listed {
		existing = append(existing, l.Mailbox)
		if delim == 0 && l.Delim != 0 {
			delim = l.Delim
		}
	}
	var personalPrefix string
	if g.client.Caps().Has(imap.CapNamespace) {
		if ns, err := g.client.Namespace().Wait(); err == nil && len(ns.Personal) > 0 {
			personalPrefix = ns.Personal[0].Prefix
			if ns.Personal[0].Delim != 0 {
				delim = ns.Personal[0].Delim
			}
		}
	}
	target, exists := resolveMailboxName(name, existing, delim, personalPrefix)
	return target, exists, nil
}

// SearchByMessageID finds messages by Message-ID header (IMAP SEARCH HEADER) and downloads them.
func (g *GoIMAPClient) SearchByMessageID(ctx context.Context, mailbox, messageID string, limit int) ([]RawIMAPMessage, error) {
	if messageID == "" {
		return nil, nil
	}
	if _, err := g.SelectMailbox(ctx, mailbox); err != nil {
		return nil, err
	}
	data, err := g.client.UIDSearch(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: messageID}},
	}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap search message-id in %q: %w", mailbox, err)
	}
	var uids []int64
	for _, u := range data.AllUIDs() {
		uids = append(uids, int64(u))
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
	if limit > 0 && len(uids) > limit {
		uids = uids[:limit]
	}
	return g.FetchMessagesByUID(ctx, mailbox, uids)
}

// resolveMailboxName returns the real server-side name for the configured folder name and
// whether that mailbox already exists. personalPrefix is the NAMESPACE personal prefix (may
// be empty); when unknown, an "INBOX<delim>" hierarchy among the existing mailboxes is used.
func resolveMailboxName(name string, existing []string, delim rune, personalPrefix string) (string, bool) {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX", true
	}
	find := func(candidate string) (string, bool) {
		for _, e := range existing {
			if strings.EqualFold(e, candidate) {
				return e, true
			}
		}
		return "", false
	}
	if e, ok := find(name); ok {
		return e, true
	}
	prefix := personalPrefix
	if prefix == "" && delim != 0 {
		inboxPrefix := "INBOX" + string(delim)
		for _, e := range existing {
			if strings.HasPrefix(strings.ToUpper(e), inboxPrefix) {
				prefix = inboxPrefix
				break
			}
		}
	}
	if prefix == "" || strings.HasPrefix(strings.ToUpper(name), strings.ToUpper(prefix)) {
		return name, false
	}
	candidate := prefix + name
	if e, ok := find(candidate); ok {
		return e, true
	}
	return candidate, false
}

// MoveMessage moves a message by UID from sourceMailbox to targetMailbox (creating targetMailbox if needed).
func (g *GoIMAPClient) MoveMessage(ctx context.Context, sourceMailbox string, uidValidity, uid int64, targetMailbox string) error {
	targetMailbox, err := g.ensureMailbox(targetMailbox)
	if err != nil {
		return err
	}
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
	targetMailbox, err := g.ensureMailbox(targetMailbox)
	if err != nil {
		return err
	}
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
	GetIMAPSyncState(ctx context.Context, accountID, mailbox string) (sqlc.IMAPSyncState, error)
	UpsertIMAPSyncState(ctx context.Context, s sqlc.IMAPSyncState) error
	GetEmailByMessageID(ctx context.Context, accountID, msgID string) (sqlc.Email, error)
	CreateEmail(ctx context.Context, e sqlc.Email) (sqlc.Email, error)
	CreateAttachment(ctx context.Context, a sqlc.Attachment) (sqlc.Attachment, error)
	SetEmailIMAPLocation(ctx context.Context, id uuid.UUID, mailbox string, uidValidity, uid int64) (bool, error)
}

// ThreadTransitioner changes a thread's workflow status (implemented by workflow.Service).
type ThreadTransitioner interface {
	Transition(ctx context.Context, threadID uuid.UUID, targetStatus sqlc.ThreadStatus, actorID *uuid.UUID) error
}

// IMAPSyncWorker pulls incoming emails from the IMAP INBOX and ingests them into PostgreSQL + disk.
type IMAPSyncWorker struct {
	accountID string
	endpoint  IMAPEndpoint
	// initialSyncDays limits the very first sync of an account to messages received within
	// the last N days; 0 downloads the complete mailbox history.
	initialSyncDays int
	batchSize       int
	transitioner    ThreadTransitioner
	ownAddresses    []string
	now             func() time.Time
	store           SyncStore
	matcher         *threading.Matcher
	attachments     *storage.Store
	dialer          func(ctx context.Context) (IMAPClient, error)
	pollEvery       time.Duration
	logger          *slog.Logger
}

// NewIMAPSyncWorker creates a new IMAPSyncWorker.
func NewIMAPSyncWorker(
	accountID string,
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
		accountID:   accountID,
		store:       store,
		matcher:     matcher,
		attachments: attachments,
		dialer:      dialer,
		pollEvery:   pollEvery,
		logger:      logger,

		initialSyncDays: DefaultInitialSyncDays,
		batchSize:       defaultSyncBatchSize,
		now:             time.Now,
	}
}

// DefaultInitialSyncDays is how far back a brand-new account is synchronized by default.
const DefaultInitialSyncDays = 60

const defaultSyncBatchSize = 20

// WithTransitioner sets the workflow service used to move a "new" thread to "in_progress" when a
// reply sent from another mail client is found in the Sent folder.
func (w *IMAPSyncWorker) WithTransitioner(t ThreadTransitioner) *IMAPSyncWorker {
	w.transitioner = t
	return w
}

// WithOwnAddresses lists the account's own addresses so they are never mistaken for the customer.
func (w *IMAPSyncWorker) WithOwnAddresses(addrs ...string) *IMAPSyncWorker {
	w.ownAddresses = addrs
	return w
}

// WithInitialSyncDays sets how many days of history the first sync downloads (0 = everything).
func (w *IMAPSyncWorker) WithInitialSyncDays(days int) *IMAPSyncWorker {
	if days < 0 {
		days = 0
	}
	w.initialSyncDays = days
	return w
}

// WithEndpoint records the IMAP server and username so log records identify them.
func (w *IMAPSyncWorker) WithEndpoint(e IMAPEndpoint) *IMAPSyncWorker {
	w.endpoint = e
	return w
}

// Run starts the periodic IMAP sync loop until ctx is canceled.
func (w *IMAPSyncWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollEvery)
	defer ticker.Stop()

	for {
		if _, err := w.SyncOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.ErrorContext(ctx, "imap_sync_error", w.attrs(slog.String("error", err.Error()))...)
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
	sentName := settings.IMAPSentMailbox
	if sentName == "" {
		sentName = "Sent"
	}

	client, err := w.dialer(ctx)
	if err != nil {
		return 0, fmt.Errorf("dial imap: %w", err)
	}
	defer func() { _ = client.Close() }()

	sess := &syncSession{client: client}
	// The Sent folder is only read, never created here (MiniCRM creates it on its first append).
	if actual, exists, err := client.ResolveMailbox(ctx, sentName); err != nil {
		w.logger.WarnContext(ctx, "imap_sent_folder_unavailable", w.attrs(slog.String("error", err.Error()))...)
	} else if exists {
		sess.sentMailbox = actual
	}

	// The INBOX is always synchronized first: replies found in Sent are linked to threads created
	// from the customer's incoming mail, which therefore has to be ingested before them.
	ingested, err := w.syncFolder(ctx, client, inbox, func(ctx context.Context, uidVal int64, raw RawIMAPMessage) error {
		return w.ingestOne(ctx, sess, inbox, uidVal, raw)
	})
	if err != nil {
		return ingested, err
	}
	if sess.sentMailbox == "" {
		return ingested, nil
	}
	if _, err := w.syncFolder(ctx, client, sess.sentMailbox, func(ctx context.Context, uidVal int64, raw RawIMAPMessage) error {
		sess.sentUIDValidity = uidVal
		return w.ingestSentOne(ctx, sess.sentMailbox, uidVal, raw)
	}); err != nil {
		return ingested, fmt.Errorf("sync sent folder %q: %w", sess.sentMailbox, err)
	}
	return ingested, nil
}

// syncSession carries per-pass state shared by the folder ingestors.
type syncSession struct {
	client          IMAPClient
	sentMailbox     string // resolved Sent folder name; empty when the folder does not exist
	sentUIDValidity int64
}

// syncFolder downloads and ingests new messages of one folder in batches, persisting the watermark
// after every batch. It returns the number of messages ingested.
func (w *IMAPSyncWorker) syncFolder(ctx context.Context, client IMAPClient, folder string, ingest func(ctx context.Context, uidVal int64, raw RawIMAPMessage) error) (int, error) {
	syncState, err := w.store.GetIMAPSyncState(ctx, w.accountID, folder)
	if errors.Is(err, pgx.ErrNoRows) {
		syncState = sqlc.IMAPSyncState{AccountID: w.accountID, Mailbox: folder, UIDValidity: 0, LastSyncedUID: 0}
	} else if err != nil {
		return 0, fmt.Errorf("get imap_sync_state: %w", err)
	}

	uidVal, err := client.SelectMailbox(ctx, folder)
	if err != nil {
		return 0, err
	}
	afterUID := syncState.LastSyncedUID
	if syncState.UIDValidity != 0 && syncState.UIDValidity != uidVal {
		// UIDVALIDITY changed: reset watermark and rely on Message-ID deduplication.
		afterUID = 0
	}

	// A brand-new account (no watermark) only pulls recent history unless a full backfill is configured.
	var since time.Time
	if afterUID == 0 && w.initialSyncDays > 0 {
		since = w.now().UTC().AddDate(0, 0, -w.initialSyncDays)
	}

	uids, err := client.ListUIDsSince(ctx, folder, afterUID, since)
	if err != nil {
		return 0, err
	}
	if afterUID == 0 && len(uids) > 0 {
		w.logger.InfoContext(ctx, "imap_initial_sync_started", w.attrs(
			slog.String("mailbox", folder),
			slog.Int("messages", len(uids)),
			slog.Int("initial_sync_days", w.initialSyncDays),
		)...)
	}

	ingested, skipped := 0, 0
	highestUID := afterUID
	for start := 0; start < len(uids); start += w.batchSize {
		if err := ctx.Err(); err != nil {
			return ingested, err
		}
		end := min(start+w.batchSize, len(uids))
		msgs, err := client.FetchMessagesByUID(ctx, folder, uids[start:end])
		if err != nil {
			return ingested, err
		}
		for _, rawMsg := range msgs {
			if len(rawMsg.RawRFC822) == 0 {
				skipped++
				w.logger.WarnContext(ctx, "imap_message_skipped", w.attrs(
					slog.String("mailbox", folder),
					slog.Int64("uid", rawMsg.UID), slog.String("reason", "empty message body"))...)
			} else if err := ingest(ctx, uidVal, rawMsg); err != nil {
				var parseErr *parseMessageError
				if !errors.As(err, &parseErr) {
					return ingested, fmt.Errorf("ingest uid %d: %w", rawMsg.UID, err)
				}
				// A message that cannot be parsed will never succeed; skip it so it cannot block the mailbox.
				skipped++
				w.logger.WarnContext(ctx, "imap_message_skipped", w.attrs(
					slog.String("mailbox", folder),
					slog.Int64("uid", rawMsg.UID), slog.String("reason", err.Error()))...)
			} else {
				ingested++
			}
			if rawMsg.UID > highestUID {
				highestUID = rawMsg.UID
			}
		}
		// Persist progress after every batch so a failure never re-downloads completed work.
		if err := w.store.UpsertIMAPSyncState(ctx, sqlc.IMAPSyncState{
			AccountID:     w.accountID,
			Mailbox:       folder,
			UIDValidity:   uidVal,
			LastSyncedUID: highestUID,
		}); err != nil {
			return ingested, fmt.Errorf("update imap_sync_state: %w", err)
		}
	}
	if ingested > 0 || skipped > 0 {
		w.logger.InfoContext(ctx, "imap_sync_completed", w.attrs(
			slog.String("mailbox", folder),
			slog.Int("ingested", ingested), slog.Int("skipped", skipped), slog.Int64("last_uid", highestUID))...)
	}
	return ingested, nil
}

// attrs returns log attributes identifying this worker's account and IMAP server, plus extra.
func (w *IMAPSyncWorker) attrs(extra ...any) []any {
	out := append([]any{slog.String("account_id", w.accountID)}, w.endpoint.LogAttrs()...)
	return append(out, extra...)
}

// parseMessageError marks a message that is permanently unparseable (as opposed to a transient failure).
type parseMessageError struct{ err error }

func (e *parseMessageError) Error() string { return "parse rfc5322: " + e.err.Error() }
func (e *parseMessageError) Unwrap() error { return e.err }

func (w *IMAPSyncWorker) ingestOne(ctx context.Context, sess *syncSession, mailbox string, uidValidity int64, rawMsg RawIMAPMessage) error {
	parsed, err := ParseRFC5322(bytes.NewReader(rawMsg.RawRFC822))
	if err != nil {
		return &parseMessageError{err: err}
	}

	// Deduplicate by Message-ID within this account, so an email delivered to several
	// accounts is ingested once per account.
	if _, err := w.store.GetEmailByMessageID(ctx, w.accountID, parsed.MessageID); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check existing message_id: %w", err)
	}

	toList := make([]string, 0, len(parsed.ToAddresses))
	for _, a := range parsed.ToAddresses {
		toList = append(toList, a.Email)
	}
	inbound := threading.InboundMessageMetadata{
		AccountID:     w.accountID,
		MessageID:     parsed.MessageID,
		InReplyTo:     parsed.InReplyTo,
		ReferencesIDs: parsed.ReferencesIDs,
		FromAddress:   parsed.FromAddress,
		FromName:      parsed.FromName,
		ToAddresses:   toList,
		Subject:       parsed.Subject,
		BodyText:      parsed.BodyText,
		SentAt:        parsed.SentAt,
	}

	// A customer replying to a message that was first sent from outside MiniCRM: pull the original
	// outbound mail from the Sent folder so the new thread contains the whole exchange.
	if sess != nil && sess.sentMailbox != "" {
		if err := w.importOutboundParent(ctx, sess, inbound); err != nil {
			// Best effort: the customer's mail must still be ingested.
			w.logger.WarnContext(ctx, "imap_outbound_parent_lookup_failed", w.attrs(
				slog.String("message_id", parsed.MessageID), slog.String("error", err.Error()))...)
		}
	}

	resolution, err := w.matcher.ResolveThread(ctx, inbound)
	if err != nil {
		return fmt.Errorf("resolve thread: %w", err)
	}

	emailRec, err := w.storeParsedEmail(ctx, parsed, resolution.Thread.ID, resolution.ParentEmailID, "inbound", mailbox, uidValidity, rawMsg.UID)
	if err != nil {
		return err
	}

	w.logger.InfoContext(ctx, "data_change",
		slog.String("event_category", "data_change"),
		slog.String("operation", "create"),
		slog.String("resource_type", "inbound_email"),
		slog.String("account_id", w.accountID),
		slog.String("resource_id", emailRec.ID.String()),
		slog.String("thread_id", resolution.Thread.ID.String()),
		slog.String("message_id", parsed.MessageID),
	)
	return nil
}

// storeParsedEmail persists a parsed message and its attachments into the given thread.
func (w *IMAPSyncWorker) storeParsedEmail(ctx context.Context, parsed *ParsedEmail, threadID uuid.UUID, parentID *uuid.UUID, direction, mailbox string, uidValidity, uid int64) (sqlc.Email, error) {
	var inReplyToPtr *string
	if parsed.InReplyTo != "" {
		inReplyToPtr = &parsed.InReplyTo
	}
	emailRec, err := w.store.CreateEmail(ctx, sqlc.Email{
		ThreadID:          threadID,
		AccountID:         w.accountID,
		ParentEmailID:     parentID,
		Direction:         direction,
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
		return sqlc.Email{}, fmt.Errorf("create email: %w", err)
	}
	for _, att := range parsed.Attachments {
		blob, err := w.attachments.Save(ctx, bytes.NewReader(att.Data))
		if err != nil {
			return sqlc.Email{}, fmt.Errorf("save attachment %q: %w", att.Filename, err)
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
			return sqlc.Email{}, fmt.Errorf("insert attachment record: %w", err)
		}
	}
	return emailRec, nil
}

// outboundMetadata converts a parsed message from the Sent folder into matcher metadata.
func (w *IMAPSyncWorker) outboundMetadata(parsed *ParsedEmail) threading.OutboundMessageMetadata {
	recipients := append(append([]sqlc.Address{}, parsed.ToAddresses...), parsed.CCAddresses...)
	own := append([]string{}, w.ownAddresses...)
	if w.endpoint.Username != "" {
		own = append(own, w.endpoint.Username)
	}
	return threading.OutboundMessageMetadata{
		AccountID:     w.accountID,
		MessageID:     parsed.MessageID,
		InReplyTo:     parsed.InReplyTo,
		ReferencesIDs: parsed.ReferencesIDs,
		FromAddress:   parsed.FromAddress,
		Recipients:    recipients,
		Subject:       parsed.Subject,
		BodyText:      parsed.BodyText,
		SentAt:        parsed.SentAt,
		OwnAddresses:  own,
	}
}

// maxOutboundParentLookups caps the Sent-folder searches done for one incoming message.
const maxOutboundParentLookups = 3

// importOutboundParent looks in the Sent folder for the message a customer is replying to when no
// existing thread matches, and ingests it as the first (outbound) message of a new thread.
func (w *IMAPSyncWorker) importOutboundParent(ctx context.Context, sess *syncSession, inbound threading.InboundMessageMetadata) error {
	candidates := threading.CandidateMessageIDs(inbound.InReplyTo, inbound.ReferencesIDs)
	if len(candidates) == 0 {
		return nil
	}
	known, err := w.matcher.HasExistingThread(ctx, inbound)
	if err != nil || known {
		return err
	}
	if len(candidates) > maxOutboundParentLookups {
		candidates = candidates[:maxOutboundParentLookups]
	}
	for _, id := range candidates {
		found, err := sess.client.SearchByMessageID(ctx, sess.sentMailbox, id, 1)
		if err != nil {
			return err
		}
		for _, raw := range found {
			parsed, err := ParseRFC5322(bytes.NewReader(raw.RawRFC822))
			if err != nil || parsed.MessageID != id {
				continue
			}
			if _, err := w.store.GetEmailByMessageID(ctx, w.accountID, parsed.MessageID); err == nil {
				continue
			}
			meta := w.outboundMetadata(parsed)
			// Only treat it as the parent when the replying customer was one of its recipients.
			addressed := false
			for _, r := range meta.Recipients {
				if strings.EqualFold(r.Email, inbound.FromAddress) {
					addressed = true
				}
			}
			if !addressed {
				continue
			}
			thread, ok, err := w.matcher.CreateThreadFromOutbound(ctx, meta)
			if err != nil || !ok {
				return err
			}
			uidVal := sess.sentUIDValidity
			if raw.UIDValidity != 0 {
				uidVal = raw.UIDValidity
			}
			if _, err := w.storeParsedEmail(ctx, parsed, thread.ID, nil, "outbound", sess.sentMailbox, uidVal, raw.UID); err != nil {
				return err
			}
			w.logger.InfoContext(ctx, "data_change", w.attrs(
				slog.String("event_category", "data_change"),
				slog.String("operation", "create"),
				slog.String("resource_type", "outbound_email"),
				slog.String("source", "imap_sent_on_demand"),
				slog.String("thread_id", thread.ID.String()),
				slog.String("message_id", parsed.MessageID),
			)...)
			return nil
		}
	}
	return nil
}

// ingestSentOne ingests one message from the Sent folder. It only ever attaches to an existing
// thread: a message nobody replied to (or that matches no thread) is ignored.
func (w *IMAPSyncWorker) ingestSentOne(ctx context.Context, mailbox string, uidValidity int64, rawMsg RawIMAPMessage) error {
	parsed, err := ParseRFC5322(bytes.NewReader(rawMsg.RawRFC822))
	if err != nil {
		return &parseMessageError{err: err}
	}

	// MiniCRM's own replies are already stored under the same Message-ID (that is how duplicates are
	// avoided); just record where the copy lives on the server.
	if existing, err := w.store.GetEmailByMessageID(ctx, w.accountID, parsed.MessageID); err == nil {
		if existing.IMAPUID == nil {
			changed, err := w.store.SetEmailIMAPLocation(ctx, existing.ID, mailbox, uidValidity, rawMsg.UID)
			if err != nil {
				return fmt.Errorf("backfill imap location: %w", err)
			}
			if changed {
				w.logger.InfoContext(ctx, "data_change", w.attrs(
					slog.String("event_category", "data_change"),
					slog.String("operation", "update"),
					slog.String("resource_type", "email_imap_location"),
					slog.String("resource_id", existing.ID.String()),
					slog.String("mailbox", mailbox),
				)...)
			}
		}
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check existing message_id: %w", err)
	}

	res, err := w.matcher.ResolveOutboundThread(ctx, w.outboundMetadata(parsed))
	if err != nil {
		return fmt.Errorf("resolve outbound thread: %w", err)
	}
	if !res.Found {
		w.logger.DebugContext(ctx, "imap_sent_message_ignored", w.attrs(
			slog.String("message_id", parsed.MessageID), slog.String("reason", "no matching thread"))...)
		return nil
	}

	emailRec, err := w.storeParsedEmail(ctx, parsed, res.Thread.ID, res.ParentEmailID, "outbound", mailbox, uidValidity, rawMsg.UID)
	if err != nil {
		return err
	}
	// Somebody answered: a "new" thread is now being worked on (this also clears the waiting badge).
	if res.Thread.Status == sqlc.ThreadStatusNew && w.transitioner != nil {
		if err := w.transitioner.Transition(ctx, res.Thread.ID, sqlc.ThreadStatusInProgress, nil); err != nil {
			return fmt.Errorf("move thread to in_progress: %w", err)
		}
	}
	w.logger.InfoContext(ctx, "data_change", w.attrs(
		slog.String("event_category", "data_change"),
		slog.String("operation", "create"),
		slog.String("resource_type", "outbound_email"),
		slog.String("source", "imap_sent"),
		slog.String("resource_id", emailRec.ID.String()),
		slog.String("thread_id", res.Thread.ID.String()),
		slog.String("message_id", parsed.MessageID),
	)...)
	return nil
}
