package mail_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wichert/minicrm/internal/mail"
)

// TestSeedFixturesParseWithAttachments makes sure every seeded dev-mailbox message is
// ingestible and that the PDF/image fixtures keep their attachments.
func TestSeedFixturesParseWithAttachments(t *testing.T) {
	want := map[string][]string{
		"support@example.com/05-error-screenshots.eml": {"image/png:1", "image/jpeg:1"},
		"support@example.com/06-payment-receipt.eml":   {"application/pdf:1"},
		"support@example.com/07-signed-contract.eml":   {"application/pdf:1", "image/png:1"},
		"sales@example.com/03-purchase-order.eml":      {"application/pdf:1", "image/jpeg:1"},
		"sales@example.com/04-brand-assets.eml":        {"application/pdf:1", "image/png:1"},
	}
	files, err := filepath.Glob("../../docker/mail/seed/*/*.eml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no seed fixtures found: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := mail.ParseRFC5322(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: parse: %v", f, err)
		}
		key := filepath.Base(filepath.Dir(f)) + "/" + filepath.Base(f)
		counts := map[string]int{}
		for _, a := range parsed.Attachments {
			if len(a.Data) == 0 {
				t.Errorf("%s: empty attachment %q", key, a.Filename)
			}
			counts[a.ContentType]++
			switch a.ContentType {
			case "application/pdf":
				if !strings.HasPrefix(string(a.Data), "%PDF-") {
					t.Errorf("%s: %q is not a PDF", key, a.Filename)
				}
			case "image/png":
				if !strings.HasPrefix(string(a.Data), "\x89PNG") {
					t.Errorf("%s: %q is not a PNG", key, a.Filename)
				}
			case "image/jpeg":
				if !strings.HasPrefix(string(a.Data), "\xff\xd8") {
					t.Errorf("%s: %q is not a JPEG", key, a.Filename)
				}
			}
		}
		for _, w := range want[key] {
			parts := strings.Split(w, ":")
			if parts[1] == "1" && counts[parts[0]] == 0 {
				t.Errorf("%s: expected a %s attachment, got %v", key, parts[0], counts)
			}
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Errorf("missing seed fixtures: %v", want)
	}
}
