# Security Architecture: Untrusted HTML & HTTP Controls

Displaying incoming emails in a web-based CRM is a high-risk attack surface for Cross-Site Scripting (XSS), Cross-Site Request Forgery (CSRF), UI redressing (clickjacking), CSS data exfiltration, and tracking-pixel privacy leaks. MiniCRM enforces **four independent layers of defense-in-depth**.

---

## 1. Layer 1: Strict Server-Side HTML Sanitization (`internal/mail/mime_parser.go`)

When an email is ingested, `mail.SanitizeEmailHTML` sanitizes the raw HTML part using a strict `github.com/microcosm-cc/bluemonday` allowlist policy before storing `body_html_sanitized`:

- **Allowed Elements**:
  - Typography & structure: `p`, `br`, `div`, `span`, `blockquote`, `pre`, `code`, `h1`–`h6`, `hr`, `b`, `i`, `u`, `strong`, `em`
  - Lists & tables: `ul`, `ol`, `li`, `table`, `thead`, `tbody`, `tfoot`, `tr`, `th`, `td`
  - Links & inline images: `a`, `img`
- **Forbidden & Stripped**:
  - `<script>`, `<style>`, `<iframe>`, `<frame>`, `<object>`, `<embed>`, `<applet>`, `<form>`, `<input>`, `<button>`, `<svg>`, `<math>`, `<base>`, `<meta>`, `<link>`, and all inline `on*` event attributes (`onclick`, `onerror`, `onload`, etc.).
- **Link Hardening**:
  - Only `http://`, `https://`, and `mailto:` schemes are permitted on `<a href>` (`javascript:`, `data:`, and `vbscript:` URLs are stripped).
  - External links are automatically rewritten with `target="_blank"` and `rel="nofollow noopener noreferrer"`.
- **Tracking Pixel & Remote Image Blocking**:
  - Remote `http://` and `https://` `<img src="...">` attributes are rewritten to `data-blocked-src="..."` prior to sanitization so opening an email never triggers external tracking requests.

---

## 2. Layer 2: `<iframe sandbox>` Script Isolation (`internal/web/views/views.go`)

Sanitized email HTML is **never** injected directly into the main MiniCRM DOM. Instead, `/threads/{id}` embeds each message inside an isolated `<iframe sandbox>` pointing to `/emails/{id}/body`. The iframe deliberately does **not** use `credentialless`: that attribute strips the `minicrm_session` cookie, so `RequireAuth` would redirect the frame to the OIDC issuer and Chromium would block it under `frame-src 'self'`.

```html
<iframe
  class="email-frame"
  src="/emails/{{.Email.ID}}/body"
  sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"
  referrerpolicy="no-referrer"
  loading="lazy">
</iframe>
```

- **`allow-scripts` is omitted** (and `Content-Security-Policy` sets `default-src 'none'`): Even if an HTML sanitizer bypass ever existed, the browser engine refuses to execute JavaScript inside the frame.
- **`allow-same-origin` without `allow-scripts`**: Because script execution is disabled inside the embedded browsing context, the inert email DOM cannot run scripts or access parent state, while the parent page (`static/app.js`) can observe `contentDocument.body` via `ResizeObserver` to size every email frame to its exact rendered height across Safari, Chrome, and Firefox (alongside `<meta name="responsive-embedded-sizing">` and `frame-sizing: content-block-size`).
- **`allow-forms` and `allow-top-navigation` are omitted**: The email cannot submit forms or navigate the operator's top-level window.
- **`allow-popups allow-popups-to-escape-sandbox`**: Clicking an external `<a target="_blank" rel="noopener noreferrer">` link opens cleanly in a new browser tab without `window.opener` access.

---

## 3. Layer 3: HTTP Security Headers (`internal/web/security_headers.go`)

`SecurityHeadersMiddleware` applies strict HTTP headers to all application responses, and `SetIsolatedEmailBodyHeaders` applies an even tighter policy to `/emails/{id}/body`:

| HTTP Header | Main Application Routes (`/threads`, `/settings`) | Isolated Email Body Route (`/emails/{id}/body`) |
|---|---|---|
| `Content-Security-Policy` | `default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-src 'self'; manifest-src 'self'; worker-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'; object-src 'none'` plus `upgrade-insecure-requests` when served over HTTPS | `default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'self'; sandbox allow-same-origin allow-popups allow-popups-to-escape-sandbox; form-action 'none'; base-uri 'none'; object-src 'none'` |
| `X-Content-Type-Options` | `nosniff` | `nosniff` |
| `X-Frame-Options` | `DENY` | `SAMEORIGIN` |
| `Referrer-Policy` | `no-referrer` | `no-referrer` |
| `Cross-Origin-Opener-Policy` | `same-origin` | `same-origin` |
| `Cross-Origin-Resource-Policy` | `same-origin` | `same-origin` |
| `Cross-Origin-Embedder-Policy` | `require-corp` | `require-corp` |
| `Permissions-Policy` | Disables `accelerometer`, `autoplay`, `camera`, `clipboard-read`, `clipboard-write`, `display-capture`, `fullscreen`, `geolocation`, `gyroscope`, `hid`, `idle-detection`, `magnetometer`, `microphone`, `midi`, `payment`, `picture-in-picture`, `publickey-credentials-get`, `screen-wake-lock`, `serial`, `usb`, `web-share`, `xr-spatial-tracking` | Same |

`upgrade-insecure-requests` is only emitted for requests that reached MiniCRM over HTTPS (either a direct TLS connection or `X-Forwarded-Proto: https` from a TLS-terminating reverse proxy). Sending it over plain HTTP would make browsers upgrade same-origin asset URLs such as `/static/app.css` to `https://<host>:8080/...`, which breaks local development stacks that serve HTTP only.
| `Strict-Transport-Security` | `max-age=63072000; includeSubDomains; preload` | `max-age=63072000; includeSubDomains; preload` |
| `Cache-Control` | `no-store, max-age=0` (dynamic/authenticated routes); `public, max-age=3600, must-revalidate` (`/static/*`); `no-cache` (`/manifest.webmanifest`, `/sw.js`) | `private, no-store, max-age=0` |

In addition, `/emails/{id}/body` embeds a `<meta http-equiv="Content-Security-Policy" ...>` tag inside the HTML `<head>` as a secondary enforcement layer. The root-scoped PWA Service Worker (`/sw.js`) precaches only public static shell assets (`/static/*`) and never intercepts or caches `/auth/*`, `/emails/*`, `/attachments/*`, or authenticated HTML responses.

---

## 4. Layer 4: Attachment Downloads, CSRF & Request Bounds

- **Account-Scoped Attachment Authorization**:
  - `/attachments/{id}/view` and `/attachments/{id}/download` resolve `attachment -> email -> account_id` and require the signed-in user to have access to that mail account (`sqlc.User.HasAccountAccess`), returning `404 Not Found` otherwise. This holds even when the same content-addressed blob is shared by attachments of several accounts.
  - `/emails/{id}/body` performs the same account-access check before rendering the isolated email body.
- **Attachment Viewing & Download Hardening (`SetAttachmentInlineHeaders`, `SetAttachmentDownloadHeaders`)**:
  - `/attachments/{id}/download` sets:
    - `Content-Disposition: attachment; filename*=UTF-8''<url-escaped-name>`
    - `Content-Security-Policy: default-src 'none'; sandbox`
    - `X-Content-Type-Options: nosniff`
    - `X-Download-Options: noopen`
  - `/attachments/{id}/view` only serves an explicit allowlist of non-scriptable media types (`image/png`, `image/jpeg`, `image/gif`, `image/webp`, `application/pdf`, and `text/plain; charset=utf-8`) with `Content-Disposition: inline`, `X-Content-Type-Options: nosniff`, `Cross-Origin-Resource-Policy: same-origin`, and strict CSP (`AttachmentInlineCSP` / `AttachmentPDFCSP`). Any non-allowlisted type (such as HTML or SVG) automatically falls back to `SetAttachmentDownloadHeaders` so browsers never execute uploaded documents in the CRM origin.
- **Cross-Site Request Forgery (CSRF) Protection**:
  - Enforced globally via Go's `http.NewCrossOriginProtection()` (`Sec-Fetch-Site` and `Origin` header validation) paired with `SameSite=Lax`, `HttpOnly`, `__Host-` prefixed `Secure` session cookies.
- **Request Body Size Bounds**:
  - Every `POST` handler wraps `r.Body` with `http.MaxBytesReader` (`64 KiB` for standard forms, `20 MiB` for multipart reply uploads) to prevent memory-exhaustion DoS attacks.
