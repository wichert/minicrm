package web

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func TestPublicImageAddress(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "192.0.2.1", "::1", "fc00::1", "::ffff:127.0.0.1", "64:ff9b::a00:1"} {
		if publicImageAddress(netip.MustParseAddr(ip)) {
			t.Errorf("allowed %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicImageAddress(netip.MustParseAddr(ip)) {
			t.Errorf("blocked %s", ip)
		}
	}
}

func TestProxyRejectsRedirectToPrivateAddress(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	err := imageHTTPClient.CheckRedirect(req, []*http.Request{req})
	if !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect was not blocked: %v", err)
	}
}

func TestBlockedImageURLs(t *testing.T) {
	urls := blockedImageURLs(`<img alt="x" data-blocked-src="https://x.example/a?a=1&amp;b=2"><img src="cid:x"><img data-blocked-src="https://y.example/">`)
	if len(urls) != 2 || urls[0] != "https://x.example/a?a=1&b=2" || urls[1] != "https://y.example/" {
		t.Fatalf("unexpected URLs: %v", urls)
	}
	for _, u := range []string{"file:///etc/passwd", "http://user:password@example.com", "http://example.com:8080/"} {
		if validImageURL(u) {
			t.Errorf("allowed %s", u)
		}
	}
}

type imageRoundTripper func(*http.Request) (*http.Response, error)

func (f imageRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRemoteImageResponseLimitsAndType(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nhello")
	cases := []struct {
		name   string
		status int
		body   []byte
		ok     bool
	}{
		{"png", 200, png, true}, {"html", 200, []byte("<html>not image</html>"), false},
		{"svg", 200, []byte("<svg></svg>"), false}, {"too large", 200, bytes.Repeat([]byte{'a'}, maxRemoteImageBytes+1), false},
		{"redirect", 302, png, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: imageRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(bytes.NewReader(tc.body)), ContentLength: -1, Header: make(http.Header)}, nil
			})}
			req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
			data, typ, err := fetchRemoteImage(client, req)
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.ok && (typ != "image/png" || !strings.HasPrefix(string(data), "\x89PNG")) {
				t.Fatal("unexpected image")
			}
		})
	}
}
