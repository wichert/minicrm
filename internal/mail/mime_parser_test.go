package mail_test

import (
	"strings"
	"testing"

	"github.com/wichert/minicrm/internal/mail"
)

func TestSanitizeEmailHTML(t *testing.T) {
	malicious := `<div>
		<p onclick="evil()">Hello</p>
		<script>alert(1)</script>
		<iframe src="https://evil.example/frame"></iframe>
		<a href="javascript:alert(2)">Bad Link</a>
		<a href="data:text/html,<script>alert(4)</script>">Bad Data Link</a>
		<a href="https://example.com/docs">Safe Link</a>
		<img src="cid:logo@acme" alt="Acme logo" width="64" height="64">
		<img src="https://tracker.example/pixel.gif" onerror="alert(3)" alt="pixel">
	</div>`

	got := mail.SanitizeEmailHTML(malicious)
	if strings.Contains(got, "<script") || strings.Contains(got, "onclick") || strings.Contains(got, "onerror") || strings.Contains(got, "<iframe") || strings.Contains(got, "javascript:") || strings.Contains(got, "data:text/html") {
		t.Fatalf("sanitizer failed to strip dangerous HTML: %s", got)
	}
	if !strings.Contains(got, `src="cid:logo@acme"`) {
		t.Fatalf("expected cid: image src to be preserved: %s", got)
	}
	if !strings.Contains(got, `data-blocked-src="https://tracker.example/pixel.gif"`) {
		t.Fatalf("expected remote image src to be rewritten to data-blocked-src: %s", got)
	}
	if !strings.Contains(got, `target="_blank"`) || !strings.Contains(got, `noopener`) {
		t.Fatalf("expected safe external link to have target=_blank and noopener: %s", got)
	}
}

func TestParseRFC5322MultipartWithAttachment(t *testing.T) {
	raw := "From: Alice <alice@client.com>\r\n" +
		"To: Support <support@company.com>\r\n" +
		"Subject: Re: Order #99\r\n" +
		"Message-ID: <msg-99@client.com>\r\n" +
		"In-Reply-To: <msg-98@company.com>\r\n" +
		"References: <msg-97@client.com> <msg-98@company.com>\r\n" +
		"Date: Sun, 12 Apr 2026 12:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"bound1\"\r\n\r\n" +
		"--bound1\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Plain text body here.\r\n" +
		"--bound1\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>HTML body <script>bad()</script></p>\r\n" +
		"--bound1\r\n" +
		"Content-Type: text/plain; name=\"note.txt\"\r\n" +
		"Content-Disposition: attachment; filename=\"note.txt\"\r\n\r\n" +
		"attachment content\r\n" +
		"--bound1--\r\n"

	parsed, err := mail.ParseRFC5322(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	if parsed.MessageID != "msg-99@client.com" || parsed.InReplyTo != "msg-98@company.com" {
		t.Fatalf("unexpected message IDs: %+v", parsed)
	}
	if len(parsed.ReferencesIDs) != 2 || parsed.SubjectNormalized != "Order #99" {
		t.Fatalf("unexpected references/subject: %+v", parsed)
	}
	if parsed.BodyText != "Plain text body here." || strings.Contains(parsed.BodyHTMLSanitized, "<script") {
		t.Fatalf("unexpected bodies: text=%q html=%q", parsed.BodyText, parsed.BodyHTMLSanitized)
	}
	if len(parsed.Attachments) != 1 || parsed.Attachments[0].Filename != "note.txt" {
		t.Fatalf("expected 1 attachment note.txt, got %+v", parsed.Attachments)
	}
}
