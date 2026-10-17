package mail_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/mail"
	"github.com/wichert/minicrm/internal/storage"
	gomail "github.com/wneessen/go-mail"
)

type fakeSMTPTransport struct {
	sentRaw []string
}

func (f *fakeSMTPTransport) Send(_ context.Context, msg *gomail.Msg) error {
	var buf bytes.Buffer
	_, _ = msg.WriteTo(&buf)
	f.sentRaw = append(f.sentRaw, buf.String())
	return nil
}

func (m *memCRMStore) GetEmailByID(_ context.Context, id uuid.UUID) (sqlc.Email, error) {
	em, ok := m.emails[id]
	if !ok {
		return sqlc.Email{}, pgx.ErrNoRows
	}
	return em, nil
}

func (m *memCRMStore) ListAttachmentsByEmail(_ context.Context, emailID uuid.UUID) ([]sqlc.Attachment, error) {
	var out []sqlc.Attachment
	for _, a := range m.attachments {
		if a.EmailID == emailID {
			out = append(out, a)
		}
	}
	return out, nil
}

type memOutboxStore struct {
	*memCRMStore
	outbox []sqlc.OutboxEmail
}

func (s *memOutboxStore) EnqueueOutboxEmail(_ context.Context, emailID, senderUserID uuid.UUID) (sqlc.OutboxEmail, error) {
	item := sqlc.OutboxEmail{
		ID:           uuid.New(),
		EmailID:      emailID,
		SenderUserID: senderUserID,
		Status:       "pending",
		EnqueuedAt:   time.Now().UTC(),
	}
	s.outbox = append(s.outbox, item)
	return item, nil
}

func (s *memOutboxStore) ClaimNextOutboxEmail(_ context.Context) (sqlc.OutboxEmail, error) {
	for i := range s.outbox {
		if s.outbox[i].Status == "pending" {
			s.outbox[i].Status = "sending"
			s.outbox[i].Attempts++
			return s.outbox[i], nil
		}
	}
	return sqlc.OutboxEmail{}, pgx.ErrNoRows
}

func (s *memOutboxStore) CompleteOutboxEmail(_ context.Context, id uuid.UUID) error {
	for i := range s.outbox {
		if s.outbox[i].ID == id {
			s.outbox[i].Status = "sent"
		}
	}
	return nil
}

func (s *memOutboxStore) FailOutboxEmail(_ context.Context, id uuid.UUID, _ int, errMsg string, _ time.Time) error {
	for i := range s.outbox {
		if s.outbox[i].ID == id {
			s.outbox[i].Status = "failed"
			s.outbox[i].LastError = &errMsg
		}
	}
	return nil
}

func TestComposeReplySMTPSendAndIMAPSentAppend(t *testing.T) {
	baseStore := newMemCRMStore()
	outboxStore := &memOutboxStore{memCRMStore: baseStore}
	attStore, err := storage.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	auditLogger := audit.NewLogger(baseStore, nil)
	smtpTransport := &fakeSMTPTransport{}
	imapSrv := newFakeIMAPServer()

	outboxWorker := mail.NewOutboxWorker(
		outboxStore,
		attStore,
		[]mail.AccountSenderConfig{{
			AccountID:   "support",
			FromAddress: "support@company.com",
			FromName:    "MiniCRM Support",
			Transport:   smtpTransport,
		}},
		auditLogger,
		"company.com",
		nil,
		nil,
	)
	actionWorker := mail.NewIMAPActionWorker(
		baseStore,
		attStore,
		func(_ context.Context, _ string) (mail.IMAPClient, error) { return imapSrv, nil },
		nil,
		nil,
	)

	ctx := context.Background()
	assignee := baseStore.defaultUser.ID
	thread, _ := baseStore.CreateThread(ctx, sqlc.Thread{
		AccountID:         "support",
		SubjectNormalized: "Billing Help",
		SubjectDisplay:    "Billing Help",
		Status:            sqlc.ThreadStatusNew,
		AssigneeID:        &assignee,
		CustomerEmail:     "customer@client.com",
		CustomerName:      "Customer",
		LastMessageAt:     time.Now().UTC(),
	})
	_, _ = baseStore.CreateEmail(ctx, sqlc.Email{
		ThreadID:      thread.ID,
		AccountID:     "support",
		Direction:     "inbound",
		MessageID:     "orig-1@client.com",
		ReferencesIDs: []string{"root-0@client.com"},
		FromAddress:   "customer@client.com",
		Subject:       "Billing Help",
		SentAt:        time.Now().UTC(),
	})

	replyEmail, err := outboxWorker.ComposeAndQueueReply(ctx, mail.ComposeReplyParams{
		ThreadID:     thread.ID,
		SenderUserID: baseStore.defaultUser.ID,
		BodyText:     "We have updated your invoice.",
		Attachments: []mail.OutgoingAttachmentInput{{
			Filename:    "invoice.txt",
			ContentType: "text/plain",
			Data:        []byte("invoice #100"),
		}},
	})
	if err != nil {
		t.Fatalf("ComposeAndQueueReply: %v", err)
	}
	if replyEmail.InReplyTo == nil || *replyEmail.InReplyTo != "orig-1@client.com" {
		t.Fatalf("expected InReplyTo orig-1@client.com, got %v", replyEmail.InReplyTo)
	}
	if baseStore.threads[thread.ID].Status != sqlc.ThreadStatusInProgress {
		t.Fatalf("expected thread auto-transitioned to in_progress, got %s", baseStore.threads[thread.ID].Status)
	}

	sentCount, err := outboxWorker.ProcessPending(ctx)
	if err != nil || sentCount != 1 {
		t.Fatalf("OutboxWorker.ProcessPending: sent=%d err=%v", sentCount, err)
	}
	if len(smtpTransport.sentRaw) != 1 || !strings.Contains(smtpTransport.sentRaw[0], "In-Reply-To: <orig-1@client.com>") {
		t.Fatalf("expected In-Reply-To header in SMTP message, got: %v", smtpTransport.sentRaw)
	}

	appendedJobs, err := actionWorker.ProcessPending(ctx)
	if err != nil || appendedJobs != 1 {
		t.Fatalf("IMAPActionWorker.ProcessPending: count=%d err=%v", appendedJobs, err)
	}
	if len(imapSrv.appended["Sent"]) != 1 {
		t.Fatalf("expected 1 message appended to IMAP Sent folder, got %d", len(imapSrv.appended["Sent"]))
	}
}
