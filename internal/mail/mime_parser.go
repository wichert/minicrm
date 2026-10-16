// Package mail implements MIME parsing, HTML sanitization, IMAP synchronization/actions, and SMTP sending.
package mail

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	_ "github.com/emersion/go-message/charset" // Register standard MIME charsets
	gomail "github.com/emersion/go-message/mail"
	"github.com/microcosm-cc/bluemonday"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/threading"
)

// ParsedAttachment holds an extracted MIME attachment and its raw payload.
type ParsedAttachment struct {
	Filename    string
	ContentType string
	ContentID   string
	IsInline    bool
	Data        []byte
}

// ParsedEmail holds the structured fields extracted from an RFC 5322 message.
type ParsedEmail struct {
	MessageID         string
	InReplyTo         string
	ReferencesIDs     []string
	FromAddress       string
	FromName          string
	ToAddresses       []sqlc.Address
	CCAddresses       []sqlc.Address
	Subject           string
	SubjectNormalized string
	BodyText          string
	BodyHTMLSanitized string
	RawHeaders        map[string]string
	SentAt            time.Time
	Attachments       []ParsedAttachment
}

var (
	remoteImgSrcRe = regexp.MustCompile(`(?i)(<img\b[^>]*?)\bsrc=(["']https?://[^"']+["'])`)
	htmlPolicy     = buildStrictEmailHTMLPolicy()
)

func buildStrictEmailHTMLPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements(
		"p", "br", "div", "span", "blockquote", "pre", "code",
		"ul", "ol", "li",
		"table", "thead", "tbody", "tfoot", "tr", "th", "td",
		"b", "i", "u", "strong", "em", "h1", "h2", "h3", "h4", "h5", "h6", "hr",
	)
	p.AllowAttrs("href").OnElements("a")
	p.AllowURLSchemes("http", "https", "mailto")
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.RequireNoFollowOnLinks(true)

	// Allow inline cid: or data: images; remote http(s) images are rewritten to data-blocked-src first.
	p.AllowAttrs("alt", "title", "data-blocked-src").OnElements("img")
	p.AllowAttrs("src").Matching(regexp.MustCompile(`(?i)^(cid:|data:image/(png|jpeg|gif|webp);base64,)`)).OnElements("img")
	return p
}

// SanitizeEmailHTML applies strict allowlist HTML sanitization and blocks remote tracking images.
func SanitizeEmailHTML(rawHTML string) string {
	if strings.TrimSpace(rawHTML) == "" {
		return ""
	}
	rewritten := remoteImgSrcRe.ReplaceAllString(rawHTML, `${1}data-blocked-src=${2}`)
	return htmlPolicy.Sanitize(rewritten)
}

// ParseRFC5322 parses a raw RFC 5322 MIME message into a ParsedEmail.
func ParseRFC5322(r io.Reader) (*ParsedEmail, error) {
	mr, err := gomail.CreateReader(r)
	if err != nil {
		return nil, fmt.Errorf("create mail reader: %w", err)
	}

	hdr := mr.Header
	rawHeaders := make(map[string]string)
	fields := hdr.Fields()
	for fields.Next() {
		rawHeaders[strings.ToLower(fields.Key())] = fields.Value()
	}

	subject, err := hdr.Subject()
	if err != nil {
		subject = hdr.Get("Subject")
	}
	normSubject, _ := threading.NormalizeSubject(subject)

	sentAt, err := hdr.Date()
	if err != nil || sentAt.IsZero() {
		sentAt = time.Now().UTC()
	} else {
		sentAt = sentAt.UTC()
	}

	fromList, _ := hdr.AddressList("From")
	var fromAddr, fromName string
	if len(fromList) > 0 {
		fromAddr = strings.ToLower(strings.TrimSpace(fromList[0].Address))
		fromName = strings.TrimSpace(fromList[0].Name)
	}

	toList, _ := hdr.AddressList("To")
	ccList, _ := hdr.AddressList("Cc")

	msgID := threading.NormalizeMessageID(hdr.Get("Message-Id"))
	if msgID == "" {
		msgID = synthesizeMessageID(rawHeaders, sentAt)
	}

	inReplyTo := ""
	if list := threading.ParseMessageIDList(hdr.Get("In-Reply-To")); len(list) > 0 {
		inReplyTo = list[0]
	}
	refs := threading.ParseMessageIDList(hdr.Get("References"))

	var textParts []string
	var htmlParts []string
	var attachments []ParsedAttachment

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read mime part: %w", err)
		}
		bodyBytes, err := io.ReadAll(io.LimitReader(part.Body, 25<<20)) // 25 MiB per part cap
		if err != nil {
			return nil, fmt.Errorf("read mime part body: %w", err)
		}

		switch h := part.Header.(type) {
		case *gomail.InlineHeader:
			ct, ctParams, _ := h.ContentType()
			cid := threading.NormalizeMessageID(h.Get("Content-Id"))
			switch strings.ToLower(ct) {
			case "text/plain":
				textParts = append(textParts, string(bodyBytes))
			case "text/html":
				htmlParts = append(htmlParts, string(bodyBytes))
			default:
				if cid != "" {
					filename := ctParams["name"]
					if filename == "" {
						filename = cid
					}
					attachments = append(attachments, ParsedAttachment{
						Filename:    filename,
						ContentType: ct,
						ContentID:   cid,
						IsInline:    true,
						Data:        bodyBytes,
					})
				}
			}
		case *gomail.AttachmentHeader:
			filename, _ := h.Filename()
			if filename == "" {
				filename = "attachment.bin"
			}
			ct, _, _ := h.ContentType()
			if ct == "" {
				ct = "application/octet-stream"
			}
			cid := threading.NormalizeMessageID(h.Get("Content-Id"))
			attachments = append(attachments, ParsedAttachment{
				Filename:    filename,
				ContentType: ct,
				ContentID:   cid,
				IsInline:    false,
				Data:        bodyBytes,
			})
		}
	}

	rawHTML := strings.Join(htmlParts, "\n")
	return &ParsedEmail{
		MessageID:         msgID,
		InReplyTo:         inReplyTo,
		ReferencesIDs:     refs,
		FromAddress:       fromAddr,
		FromName:          fromName,
		ToAddresses:       convertAddresses(toList),
		CCAddresses:       convertAddresses(ccList),
		Subject:           subject,
		SubjectNormalized: normSubject,
		BodyText:          strings.TrimSpace(strings.Join(textParts, "\n")),
		BodyHTMLSanitized: SanitizeEmailHTML(rawHTML),
		RawHeaders:        rawHeaders,
		SentAt:            sentAt,
		Attachments:       attachments,
	}, nil
}

func convertAddresses(in []*gomail.Address) []sqlc.Address {
	out := make([]sqlc.Address, 0, len(in))
	for _, a := range in {
		if a == nil || strings.TrimSpace(a.Address) == "" {
			continue
		}
		out = append(out, sqlc.Address{
			Name:  strings.TrimSpace(a.Name),
			Email: strings.ToLower(strings.TrimSpace(a.Address)),
		})
	}
	return out
}

func synthesizeMessageID(headers map[string]string, sentAt time.Time) string {
	var buf bytes.Buffer
	buf.WriteString(sentAt.UTC().Format(time.RFC3339Nano))
	buf.WriteString("|")
	buf.WriteString(headers["from"])
	buf.WriteString("|")
	buf.WriteString(headers["subject"])
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:16]) + "@synthetic.minicrm"
}
