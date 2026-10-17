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

## 2. Layer 2: Opaque-Origin `<iframe sandbox>` Isolation (`internal/web/views/views.go`)

Sanitized email HTML is **never** injected directly into the main MiniCRM DOM. Instead, `/threads/{id}` embeds each message inside an isolated, `credentialless` `<iframe>` pointing to `/emails/{id}/body`:

```html
<iframe
  class="email-frame"
  src="/emails/{{.Email.ID}}/body"
  sandbox="allow-popups allow-popups-to-escape-sandbox"
  referrerpolicy="no-referrer"
  credentialless
  loading="lazy">
</iframe>
```

- **`allow-scripts` is omitted**: Even if an HTML sanitizer bypass ever existed, the browser engine refuses to execute JavaScript inside the frame.
- **`allow-same-origin` is omitted**: The frame runs in a unique opaque origin (`null`) and cannot access parent cookies, `localStorage`, or DOM nodes.
- **`allow-forms` and `allow-top-navigation` are omitted**: The email cannot submit forms or navigate the operator's top-level window.
- **`allow-popups allow-popups-to-escape-sandbox`**: Clicking an external `<a target="_blank" rel="noopener noreferrer">` link opens cleanly in a new browser tab without `window.opener` access.

---

## 3. Layer 3: HTTP Security Headers (`internal/web/security_headers.go`)

`SecurityHeadersMiddleware` applies strict HTTP headers to all application responses, and `SetIsolatedEmailBodyHeaders` applies an even tighter policy to `/emails/{id}/body`:

| HTTP Header | Main Application Routes (`/threads`, `/settings`) | Isolated Email Body Route (`/emails/{id}/body`) |
|---|---|---|
| `Content-Security-Policy` | `default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'; object-src 'none'; upgrade-insecure-requests` | `default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'self'; sandbox allow-popups allow-popups-to-escape-sandbox; form-action 'none'; base-uri 'none'; object-src 'none'` |
| `X-Content-Type-Options` | `nosniff` | `nosniff` |
| `X-Frame-Options` | `DENY` | `SAMEORIGIN` |
| `Referrer-Policy` | `no-referrer` | `no-referrer` |
| `Cross-Origin-Opener-Policy` | `same-origin` | `same-origin` |
| `Cross-Origin-Resource-Policy` | `same-origin` | `same-origin` |
| `Cross-Origin-Embedder-Policy` | `require-corp` | `require-corp` |
| `Permissions-Policy` | Disables `accelerometer`, `autoplay`, `camera`, `clipboard-read`, `clipboard-write`, `display-capture`, `fullscreen`, `geolocation`, `gyroscope`, `hid`, `idle-detection`, `magnetometer`, `microphone`, `midi`, `payment`, `picture-in-picture`, `publickey-credentials-get`, `screen-wake-lock`, `serial`, `usb`, `web-share`, `xr-spatial-tracking` | Same |
| `Strict-Transport-Security` | `max-age=63072000; includeSubDomains; preload` | `max-age=63072000; includeSubDomains; preload` |
| `Cache-Control` | `no-store, max-age=0` | `private, no-store, max-age=0` |

In addition, `/emails/{id}/body` embeds a `<meta http-equiv="Content-Security-Policy" ...>` tag inside the HTML `<head>` as a secondary enforcement layer.

---

## 4. Layer 4: Attachment Downloads, CSRF & Request Bounds

- **Attachment Download Hardening (`SetAttachmentDownloadHeaders`)**:
  - `/attachments/{id}/download` sets:
    - `Content-Disposition: attachment; filename*=UTF-8''<url-escaped-name>`
    - `Content-Security-Policy: default-src 'none'; sandbox`
    - `X-Content-Type-Options: nosniff`
    - `X-Download-Options: noopen`
  - Browsers are forced to save the file rather than rendering uploaded HTML, SVG, or scriptable documents inline in the CRM origin.
- **Cross-Site Request Forgery (CSRF) Protection**:
  - Enforced globally via Go's `http.NewCrossOriginProtection()` (`Sec-Fetch-Site` and `Origin` header validation) paired with `SameSite=Lax`, `HttpOnly`, `__Host-` prefixed `Secure` session cookies.
- **Request Body Size Bounds**:
  - Every `POST` handler wraps `r.Body` with `http.MaxBytesReader` (`64 KiB` for standard forms, `20 MiB` for multipart reply uploads) to prevent memory-exhaustion DoS attacks.
