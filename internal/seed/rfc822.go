package seed

import (
	"bytes"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wichert/minicrm/internal/config"
)

// BuiltMessage is one RFC 822 payload and the mailbox it should be APPENDed to.
type BuiltMessage struct {
	Mailbox   string
	MessageID string
	Raw       []byte
}

// BuildMessages renders every YAML message as RFC 822.
//
// Message-IDs are deterministic (<seed-<account>-<slug>-<n>@minicrm.dev>). Later messages
// reference every earlier Message-ID in the thread. When reopens is set, the first message
// also references the completed thread's last message so threading creates a Reopen & Merge
// candidate.
func BuildMessages(thread Thread, acct config.MailAccountConfig, baseDir string, reopenLastID string) ([]BuiltMessage, error) {
	fromAddr := acct.SMTPFromAddress
	if fromAddr == "" {
		fromAddr = acct.IMAPUsername
	}
	fromName := acct.SMTPFromName
	if fromName == "" {
		fromName = acct.Name
	}

	ids := make([]string, len(thread.Messages))
	for i := range thread.Messages {
		ids[i] = MessageID(thread.Account, thread.Slug(), i+1)
	}

	out := make([]BuiltMessage, 0, len(thread.Messages))
	for i, msg := range thread.Messages {
		var refs []string
		if i == 0 && reopenLastID != "" {
			refs = append(refs, reopenLastID)
		}
		refs = append(refs, ids[:i]...)

		from := msg.From
		name := msg.FromName
		mailbox := "INBOX"
		toAddr := fromAddr
		toName := fromName
		if msg.Direction == DirectionOutbound {
			mailbox = "Sent"
			from = fromAddr
			name = fromName
			toAddr = thread.Customer
			toName = thread.CustomerName
		} else if from == "" {
			from = thread.Customer
			name = thread.CustomerName
		}

		raw, err := renderRFC822(renderInput{
			From:        from,
			FromName:    name,
			To:          toAddr,
			ToName:      toName,
			Subject:     thread.Subject,
			Date:        msg.Date.UTC(),
			MessageID:   ids[i],
			References:  refs,
			Body:        msg.Body,
			HTML:        msg.HTML,
			Attachments: msg.Attach,
			BaseDir:     baseDir,
		})
		if err != nil {
			return nil, fmt.Errorf("thread %s message %d: %w", thread.ID, i+1, err)
		}
		out = append(out, BuiltMessage{Mailbox: mailbox, MessageID: ids[i], Raw: raw})
	}
	return out, nil
}

type renderInput struct {
	From, FromName string
	To, ToName     string
	Subject        string
	Date           time.Time
	MessageID      string
	References     []string
	Body           string
	HTML           bool
	Attachments    []Attachment
	BaseDir        string
}

func renderRFC822(in renderInput) ([]byte, error) {
	var buf bytes.Buffer
	writeHeader := func(k, v string) {
		fmt.Fprintf(&buf, "%s: %s\r\n", k, mime.QEncoding.Encode("utf-8", v))
	}
	// Address headers must stay plain (no RFC 2047 on the addr-spec).
	fmt.Fprintf(&buf, "From: %s\r\n", formatAddress(in.FromName, in.From))
	fmt.Fprintf(&buf, "To: %s\r\n", formatAddress(in.ToName, in.To))
	writeHeader("Subject", in.Subject)
	fmt.Fprintf(&buf, "Date: %s\r\n", in.Date.Format(time.RFC1123Z))
	fmt.Fprintf(&buf, "Message-ID: <%s>\r\n", in.MessageID)
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")
	if len(in.References) > 0 {
		fmt.Fprintf(&buf, "In-Reply-To: <%s>\r\n", in.References[len(in.References)-1])
		var b strings.Builder
		for i, id := range in.References {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteByte('<')
			b.WriteString(id)
			b.WriteByte('>')
		}
		fmt.Fprintf(&buf, "References: %s\r\n", b.String())
	}

	if len(in.Attachments) == 0 && !in.HTML {
		fmt.Fprintf(&buf, "Content-Type: text/plain; charset=utf-8\r\n")
		fmt.Fprintf(&buf, "Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		if err := writeQP(&buf, normalizeNewlines(in.Body)); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	if len(in.Attachments) == 0 {
		boundary := "minicrm-alt-" + sanitizeToken(in.MessageID)
		fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
		if err := writeAlternative(&buf, boundary, in.Body); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	boundary := "minicrm-mixed-" + sanitizeToken(in.MessageID)
	fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)
	if err := writeTextPart(&buf, boundary, in.Body, in.HTML); err != nil {
		return nil, err
	}
	for _, a := range in.Attachments {
		path := a.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.BaseDir, path)
		}
		// #nosec G304 -- path was confined to baseDir by Validate before render
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read attachment %s: %w", path, err)
		}
		fmt.Fprintf(&buf, "--%s\r\n", boundary)
		fmt.Fprintf(&buf, "Content-Type: %s; name=%q\r\n", a.ContentType, a.Filename)
		fmt.Fprintf(&buf, "Content-Disposition: attachment; filename=%q\r\n", a.Filename)
		fmt.Fprintf(&buf, "Content-Transfer-Encoding: base64\r\n\r\n")
		writeBase64(&buf, data)
	}
	fmt.Fprintf(&buf, "--%s--\r\n", boundary)
	return buf.Bytes(), nil
}

func writeTextPart(buf *bytes.Buffer, boundary, body string, html bool) error {
	fmt.Fprintf(buf, "--%s\r\n", boundary)
	if !html {
		fmt.Fprintf(buf, "Content-Type: text/plain; charset=utf-8\r\n")
		fmt.Fprintf(buf, "Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		return writeQP(buf, normalizeNewlines(body))
	}
	alt := boundary + "-alt"
	fmt.Fprintf(buf, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", alt)
	if err := writeAlternative(buf, alt, body); err != nil {
		return err
	}
	return nil
}

func writeAlternative(buf *bytes.Buffer, boundary, body string) error {
	fmt.Fprintf(buf, "--%s\r\n", boundary)
	fmt.Fprintf(buf, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(buf, "Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	if err := writeQP(buf, normalizeNewlines(stripTags(body))); err != nil {
		return err
	}
	fmt.Fprintf(buf, "--%s\r\n", boundary)
	fmt.Fprintf(buf, "Content-Type: text/html; charset=utf-8\r\n")
	fmt.Fprintf(buf, "Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	htmlBody := body
	if !strings.Contains(strings.ToLower(body), "<html") {
		htmlBody = "<html><body>" + strings.ReplaceAll(normalizeNewlines(body), "\n", "<br>\n") + "</body></html>"
	}
	if err := writeQP(buf, htmlBody); err != nil {
		return err
	}
	fmt.Fprintf(buf, "--%s--\r\n", boundary)
	return nil
}

func writeQP(buf *bytes.Buffer, s string) error {
	w := quotedprintable.NewWriter(buf)
	if _, err := w.Write([]byte(s)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	buf.WriteString("\r\n")
	return nil
}

func writeBase64(buf *bytes.Buffer, data []byte) {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	col := 0
	for i := 0; i < len(data); i += 3 {
		var n uint32
		remain := len(data) - i
		switch {
		case remain >= 3:
			n = uint32(data[i])<<16 | uint32(data[i+1])<<8 | uint32(data[i+2])
			buf.WriteByte(table[(n>>18)&63])
			buf.WriteByte(table[(n>>12)&63])
			buf.WriteByte(table[(n>>6)&63])
			buf.WriteByte(table[n&63])
		case remain == 2:
			n = uint32(data[i])<<16 | uint32(data[i+1])<<8
			buf.WriteByte(table[(n>>18)&63])
			buf.WriteByte(table[(n>>12)&63])
			buf.WriteByte(table[(n>>6)&63])
			buf.WriteByte('=')
		default:
			n = uint32(data[i]) << 16
			buf.WriteByte(table[(n>>18)&63])
			buf.WriteByte(table[(n>>12)&63])
			buf.WriteByte('=')
			buf.WriteByte('=')
		}
		col += 4
		if col == 76 {
			buf.WriteString("\r\n")
			col = 0
		}
	}
	if col != 0 {
		buf.WriteString("\r\n")
	}
}

func formatAddress(name, email string) string {
	email = strings.TrimSpace(email)
	name = strings.TrimSpace(name)
	if name == "" {
		return email
	}
	return mime.QEncoding.Encode("utf-8", name) + " <" + email + ">"
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	return s + "\n"
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sanitizeToken(s string) string {
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, s)
	return strings.Trim(s, "-")
}

// HeaderValue returns a decoded header from a generated message. Test helper exported for the package tests.
func HeaderValue(raw []byte, name string) string {
	for _, line := range bytes.Split(raw, []byte("\r\n")) {
		k, v, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			break
		}
		if textproto.CanonicalMIMEHeaderKey(string(k)) == textproto.CanonicalMIMEHeaderKey(name) {
			dec, err := new(mime.WordDecoder).DecodeHeader(strings.TrimSpace(string(v)))
			if err != nil {
				return strings.TrimSpace(string(v))
			}
			return dec
		}
	}
	return ""
}
