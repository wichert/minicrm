// Package web implements the MiniCRM HTTP router, security headers, handlers, and SSR views.
package web

import (
	"fmt"
	"net/http"
	"net/url"
)

// Strict HTTP security header constants.
const (
	MainCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"font-src 'self'; connect-src 'self'; frame-src 'self'; frame-ancestors 'none'; " +
		"form-action 'self'; base-uri 'none'; object-src 'none'; upgrade-insecure-requests"

	EmailBodyCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; " +
		"frame-ancestors 'self'; sandbox allow-popups allow-popups-to-escape-sandbox; " +
		"form-action 'none'; base-uri 'none'; object-src 'none'"

	AttachmentCSP = "default-src 'none'; sandbox"

	PermissionsPolicyValue = "accelerometer=(), autoplay=(), camera=(), clipboard-read=(), " +
		"clipboard-write=(), display-capture=(), fullscreen=(), geolocation=(), gyroscope=(), " +
		"hid=(), idle-detection=(), magnetometer=(), microphone=(), midi=(), payment=(), " +
		"picture-in-picture=(), publickey-credentials-get=(), screen-wake-lock=(), serial=(), " +
		"usb=(), web-share=(), xr-spatial-tracking=()"

	HSTSValue = "max-age=63072000; includeSubDomains; preload"
)

// SecurityHeadersMiddleware applies defense-in-depth HTTP security headers to every response.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", MainCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cross-Origin-Embedder-Policy", "require-corp")
		h.Set("Permissions-Policy", PermissionsPolicyValue)
		h.Set("Strict-Transport-Security", HSTSValue)
		h.Set("Cache-Control", "no-store, max-age=0")

		next.ServeHTTP(w, r)
	})
}

// SetIsolatedEmailBodyHeaders overrides CSP and framing headers for the sandboxed email body iframe endpoint.
func SetIsolatedEmailBodyHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", EmailBodyCSP)
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "private, no-store, max-age=0")
	h.Set("Content-Type", "text/html; charset=utf-8")
}

// SetAttachmentDownloadHeaders sets strict non-executing download headers for attachment responses.
func SetAttachmentDownloadHeaders(w http.ResponseWriter, filename, contentType string) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	escapedName := url.PathEscape(filename)
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", escapedName))
	h.Set("Content-Security-Policy", AttachmentCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Download-Options", "noopen")
	h.Set("Cache-Control", "private, no-store, max-age=0")
}
