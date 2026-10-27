package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/auth"
	"golang.org/x/net/html"
)

const maxRemoteImageBytes = 5 << 20

// blockedImageURLs returns image URLs only from sanitized message content, in DOM order.
func blockedImageURLs(body string) []string {
	z := html.NewTokenizer(strings.NewReader(body))
	var result []string
	for {
		typ := z.Next()
		if typ == html.ErrorToken {
			return result
		}
		if typ != html.StartTagToken && typ != html.SelfClosingTagToken {
			continue
		}
		tag := z.Token()
		if tag.Data != "img" {
			continue
		}
		for _, attr := range tag.Attr {
			if attr.Key == "data-blocked-src" {
				result = append(result, attr.Val)
				break
			}
		}
	}
}

var nonPublicPrefixes = func() []netip.Prefix {
	// IsGlobalUnicast alone includes reserved, documentation and shared-address space.
	blocks := []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "ff00::/8"}
	out := make([]netip.Prefix, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, netip.MustParsePrefix(block))
	}
	return out
}()

func publicImageAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

// imageDialContext pins the actual connection to a vetted DNS answer, with no proxy.
func imageDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("remote image host cannot be resolved")
	}
	for _, ip := range ips {
		if !publicImageAddress(ip) {
			return nil, errors.New("remote image host is not public")
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	var last error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

func validImageURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Opaque == "" && (u.Port() == "" || u.Port() == "80" || u.Port() == "443")
}

var imageHTTPClient = &http.Client{
	Timeout:       10 * time.Second,
	Transport:     &http.Transport{Proxy: nil, DialContext: imageDialContext, DisableKeepAlives: true, MaxResponseHeaderBytes: 32 << 10},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// HandleRemoteImage proxies one image already present in an authorized email.
func (s *Server) HandleRemoteImage(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	email, err := s.store.GetEmailByID(r.Context(), id)
	if err != nil || !user.HasAccountAccess(email.AccountID) {
		http.NotFound(w, r)
		return
	}
	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	urls := blockedImageURLs(email.BodyHTMLSanitized)
	if err != nil || index < 0 || index >= len(urls) || !validImageURL(urls[index]) {
		http.NotFound(w, r)
		return
	}
	s.audit.LogDataAccess(r.Context(), "remote_email_image", id.String())
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, urls[index], nil)
	if err != nil {
		http.Error(w, "image unavailable", http.StatusBadGateway)
		return
	}
	data, contentType, err := fetchRemoteImage(imageHTTPClient, req)
	if err != nil {
		http.Error(w, "image unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, no-store, max-age=0")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	_, _ = w.Write(data)
}

func fetchRemoteImage(client *http.Client, req *http.Request) ([]byte, string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxRemoteImageBytes {
		return nil, "", errors.New("invalid image response")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteImageBytes+1))
	if err != nil || len(data) > maxRemoteImageBytes {
		return nil, "", errors.New("image too large or unreadable")
	}
	contentType := http.DetectContentType(data)
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, "", errors.New("invalid image type")
	}
	return data, contentType, nil
}
