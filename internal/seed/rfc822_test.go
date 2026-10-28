package seed

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wichert/minicrm/internal/mail"
)

func TestBuildMessagesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pdf := []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")
	if err := os.WriteFile(filepath.Join(dir, "receipt.pdf"), pdf, 0o644); err != nil {
		t.Fatal(err)
	}
	thread := Thread{
		ID:           "login",
		Account:      "support",
		Customer:     "alice@customer.example",
		CustomerName: "Alice Example",
		Subject:      "Cannot log in",
		Messages: []Message{
			{
				Direction: DirectionInbound,
				Date:      time.Date(2026, 10, 5, 9, 15, 0, 0, time.UTC),
				Body:      "Hi support team",
				Attach: []Attachment{{
					Filename: "receipt.pdf", ContentType: "application/pdf", File: "receipt.pdf",
				}},
			},
			{
				Direction: DirectionOutbound,
				Date:      time.Date(2026, 10, 5, 11, 40, 0, 0, time.UTC),
				Body:      "Hi Alice",
			},
			{
				Date: time.Date(2026, 10, 5, 12, 10, 0, 0, time.UTC),
				HTML: true,
				Body: "<p>Thanks</p>",
			},
		},
	}
	built, err := BuildMessages(thread, testAccounts()[0], dir, "seed-support-older-1@minicrm.dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 3 {
		t.Fatalf("len = %d", len(built))
	}
	if built[0].Mailbox != "INBOX" || built[1].Mailbox != "Sent" || built[2].Mailbox != "INBOX" {
		t.Fatalf("mailboxes = %s %s %s", built[0].Mailbox, built[1].Mailbox, built[2].Mailbox)
	}
	if built[0].MessageID != "seed-support-login-1@minicrm.dev" {
		t.Fatal(built[0].MessageID)
	}

	first, err := mail.ParseRFC5322(bytes.NewReader(built[0].Raw))
	if err != nil {
		t.Fatal(err)
	}
	if first.MessageID != built[0].MessageID {
		t.Fatalf("parsed id %q", first.MessageID)
	}
	if first.InReplyTo != "seed-support-older-1@minicrm.dev" {
		t.Fatalf("in-reply-to %q", first.InReplyTo)
	}
	if len(first.Attachments) != 1 || first.Attachments[0].Filename != "receipt.pdf" {
		t.Fatalf("attachments %+v", first.Attachments)
	}
	if !bytes.HasPrefix(first.Attachments[0].Data, []byte("%PDF-")) {
		t.Fatalf("attachment bytes %q", first.Attachments[0].Data[:8])
	}
	if !strings.Contains(first.BodyText, "Hi support team") {
		t.Fatalf("body %q", first.BodyText)
	}

	reply, err := mail.ParseRFC5322(bytes.NewReader(built[1].Raw))
	if err != nil {
		t.Fatal(err)
	}
	if reply.FromAddress != "support@example.com" {
		t.Fatalf("from %q", reply.FromAddress)
	}
	if reply.InReplyTo != built[0].MessageID {
		t.Fatalf("in-reply-to %q", reply.InReplyTo)
	}

	html, err := mail.ParseRFC5322(bytes.NewReader(built[2].Raw))
	if err != nil {
		t.Fatalf("parse html message:\n%s\nerr: %v", built[2].Raw, err)
	}
	if html.BodyHTMLSanitized == "" {
		t.Fatal("expected sanitized html")
	}
	if len(html.ReferencesIDs) < 2 {
		t.Fatalf("references %+v", html.ReferencesIDs)
	}
}
