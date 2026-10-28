package seed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wichert/minicrm/internal/config"
)

func testAccounts() []config.MailAccountConfig {
	return []config.MailAccountConfig{
		{ID: "support", Name: "Customer Support", SMTPFromAddress: "support@example.com", SMTPFromName: "Customer Support", IMAPUsername: "support@example.com"},
		{ID: "sales", Name: "Sales", SMTPFromAddress: "sales@example.com", SMTPFromName: "Sales", IMAPUsername: "sales@example.com"},
	}
}

func baseYAML() string {
	return `
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
    messages:
      - date: 2026-10-05T09:15:00Z
        body: Help
`
}

func TestParseAcceptsValidDocument(t *testing.T) {
	f, err := Parse([]byte(baseYAML()), t.TempDir(), testAccounts())
	if err != nil {
		t.Fatal(err)
	}
	if f.Threads[0].Status != StatusNew {
		t.Fatalf("status = %q", f.Threads[0].Status)
	}
	if f.Threads[0].Messages[0].Direction != DirectionInbound {
		t.Fatalf("direction = %q", f.Threads[0].Messages[0].Direction)
	}
}

func TestParseRejectsUnknownAccount(t *testing.T) {
	raw := strings.Replace(baseYAML(), "account: support", "account: billing", 1)
	_, err := Parse([]byte(raw), t.TempDir(), testAccounts())
	if err == nil || !strings.Contains(err.Error(), "billing") {
		t.Fatalf("got %v", err)
	}
}

func TestParseRejectsNonIncreasingDates(t *testing.T) {
	raw := strings.Replace(baseYAML(), "body: Help", "body: Help\n      - date: 2026-10-05T09:15:00Z\n        body: Again", 1)
	_, err := Parse([]byte(raw), t.TempDir(), testAccounts())
	if err == nil || !strings.Contains(err.Error(), "strictly after") {
		t.Fatalf("got %v", err)
	}
}

func TestParseRejectsAssigneeWithoutAccess(t *testing.T) {
	raw := strings.Replace(baseYAML(), "account: support\n    customer:", "account: sales\n    assigned_to: sam@example.com\n    customer:", 1)
	_, err := Parse([]byte(raw), t.TempDir(), testAccounts())
	if err == nil || !strings.Contains(err.Error(), "no access") {
		t.Fatalf("got %v", err)
	}
}

func TestParseRejectsMissingAttachment(t *testing.T) {
	raw := strings.Replace(baseYAML(), "body: Help", "body: Help\n        attach:\n          - filename: receipt.pdf\n            content_type: application/pdf\n            file: missing.pdf", 1)
	_, err := Parse([]byte(raw), t.TempDir(), testAccounts())
	if err == nil || !strings.Contains(err.Error(), "missing.pdf") {
		t.Fatalf("got %v", err)
	}
}

func TestParseAcceptsAttachmentOnDisk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "receipt.pdf"), []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(baseYAML(), "body: Help", "body: Help\n        attach:\n          - filename: receipt.pdf\n            content_type: application/pdf\n            file: receipt.pdf", 1)
	if _, err := Parse([]byte(raw), dir, testAccounts()); err != nil {
		t.Fatal(err)
	}
}

func TestMessageIDIsDeterministic(t *testing.T) {
	got := MessageID("support", "login", 2)
	if got != "seed-support-login-2@minicrm.dev" {
		t.Fatal(got)
	}
	_ = time.UTC
}
