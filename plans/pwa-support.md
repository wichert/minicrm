# Progressive Web App (PWA) Support Plan

## Context

MiniCRM is a server-rendered Go web application with progressive-enhancement JavaScript (`internal/web/static/app.js`), embedded static assets (`internal/web/static/embed.go`), strict defense-in-depth HTTP security headers (`internal/web/security_headers.go`), and OIDC + PostgreSQL session authentication (`internal/auth/`).

While MiniCRM already uses clean semantic HTML, embedded assets, and responsive breakpoints, it **cannot currently be installed or used comfortably as a Progressive Web App (PWA)** due to:

1. **Missing Web App Manifest & Icons**: No `/manifest.webmanifest` endpoint (with per-instance configurable `name`, `short_name`, and `description`), no `192×192` / `512×512` / maskable PNG icons, no `<link rel="manifest">`, no `<meta name="theme-color">`, and no Apple touch icon metadata.
2. **CSP Blocks Manifest & Service Worker**: `mainCSPBase` (`internal/web/security_headers.go:12`) sets `default-src 'none'` without `manifest-src 'self'` or `worker-src 'self'`.
3. **Blanket `Cache-Control: no-store, max-age=0` on Static Assets**: `SecurityHeadersMiddleware` sets `no-store` on every response, including `/static/*`, preventing static asset caching.
4. **No Service Worker / Offline Fallback**: No root-scoped `/sw.js` to precache static shell assets or render a clean offline fallback page when network connectivity drops.
5. **Authentication & Session Friction in `display: standalone`**:
   - Hardcoded `2h` idle timeout and `12h` lifetime (`internal/auth/middleware.go:31-33`) force frequent external OIDC redirects in standalone windows.
   - `RequireAuth` drops deep links (always redirecting to `/threads` after `/auth/callback`).
   - Expanding a lazy-loaded email `<iframe src="/emails/{id}/body" loading="lazy">` after session expiry redirects to `/auth/login` → external OIDC IdP, which is blocked by `frame-src 'self'` and leaves a broken error frame inside the message card.
6. **Standalone & Mobile UI Issues**:
   - `.navbar` has `margin-top: 0.75rem` (`internal/web/static/app.css:989`), leaving a light-gray strip between the OS status bar and the dark `#14142b` navigation bar.
   - Missing `viewport-fit=cover` and `env(safe-area-inset-*)` padding for notched devices and home indicators.
   - Four conflicting `.threads-table` `min-width` rules in `app.css` (lines `435`, `938`, `1480`, `1498`) end with `@media (max-width: 900px) { .threads-table { min-width: 46rem; } }`, forcing horizontal table scrolling on phones.
   - Status filter `.tabs` wrap into two uneven rows on mobile instead of scrolling horizontally.
   - Form controls inherit `15px` (`< 16px`) font size, triggering forced viewport zoom on focus in iOS Safari / standalone WebKit.
   - Standalone windows lack a browser reload button and lose inbox filter query parameters when clicking `"← Back to threads"`.

---

## Non-Negotiable Security & Architectural Invariants Preserved

1. **Zero Caching of Sensitive or Account-Scoped Data**:
   - HTML pages (`/threads*`, `/settings*`), isolated email bodies (`/emails/{id}/body`), and attachments (`/attachments/{id}/view`, `/attachments/{id}/download`) **must** keep `Cache-Control: no-store, max-age=0` (or `private, no-store, max-age=0`) and **must never** be stored by the Service Worker. This prevents cross-user or cross-account leakage on shared devices.
2. **Untrusted Email HTML Isolation Preserved**:
   - `<iframe class="email-frame" sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer">` and `EmailBodyCSP` remain unchanged. No `allow-scripts` or `allow-top-navigation` will be added to email frames.
3. **Read-Only Offline Mode (No Offline Queued Mutations)**:
   - Offline mode shows an explicit offline notice with a retry action. Workflow transitions (`internal/workflow/service.go`), assignments, and SMTP replies are **not** queued offline, preserving strict state-machine validation and CSRF protection (`http.NewCrossOriginProtection()`).

---

## Implementation Plan

### Phase 1: Security Headers, Static Caching & PWA Routes

**Files**: `internal/web/security_headers.go`, `internal/web/router.go`, `internal/web/static/embed.go`

1. **Update `mainCSPBase` (`internal/web/security_headers.go`)**:
   - Add `manifest-src 'self'; worker-src 'self'` to `mainCSPBase`:
     ```go
     const (
         mainCSPBase = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
             "font-src 'self'; connect-src 'self'; frame-src 'self'; manifest-src 'self'; worker-src 'self'; " +
             "frame-ancestors 'none'; form-action 'self'; base-uri 'none'; object-src 'none'"
     ```
   - Leave `EmailBodyCSP`, `AttachmentCSP`, `AttachmentInlineCSP`, and `AttachmentPDFCSP` unchanged.
2. **Scoped `Cache-Control` Headers**:
   - Keep `Cache-Control: no-store, max-age=0` as the default in `SecurityHeadersMiddleware` for all dynamic routes.
   - Add a small wrapper for `/static/*` that sets `Cache-Control: public, max-age=3600, must-revalidate`.
   - Serve `/manifest.webmanifest` (`Content-Type: application/manifest+json; charset=utf-8`) and `/sw.js` (`Content-Type: application/javascript; charset=utf-8`, `Service-Worker-Allowed: /`) with `Cache-Control: no-cache` so browsers always validate updates.
3. **Public Top-Level PWA Routes (`internal/web/router.go`, `internal/web/static/embed.go`)**:
   - Update `//go:embed` in `internal/web/static/embed.go` to embed `*.css *.js *.svg *.png *.html`.
   - Mount `GET /manifest.webmanifest` (`srv.HandleManifest`) and `GET /sw.js` **outside** `authed.Group` in `NewRouter` so browser manifest fetches and background Service Worker update checks never redirect to `/auth/login`.

---

### Phase 2: Per-Instance Configurable Web App Manifest, Icons & HTML Head Metadata

**Files**: `internal/config/config.go`, `cmd/minicrm/main.go`, `internal/web/handlers_threads.go`, `internal/web/static/*.png`, `internal/web/views/views.go`

1. **Per-Instance Branding Configuration (`internal/config/config.go`)**:
   - Add instance-level PWA branding fields to `Config` so each deployment can customize how the installed app appears on home screens, app switchers, and install prompts:
     - `AppName string` (`env:"MINICRM_APP_NAME" envDefault:"MiniCRM"`)
     - `AppShortName string` (`env:"MINICRM_APP_SHORT_NAME"` — defaults to `AppName` when empty)
     - `AppDescription string` (`env:"MINICRM_APP_DESCRIPTION" envDefault:"Lightweight multi-account email CRM"`)
   - Pass these values into `web.Server` (`web.AppBranding{Name, ShortName, Description}`) at startup in `cmd/minicrm/main.go`.
2. **Dynamic `/manifest.webmanifest` Handler (`internal/web/handlers_threads.go` or `internal/web/manifest.go`)**:
   - Serve `GET /manifest.webmanifest` dynamically from `Server.HandleManifest` using `encoding/json` and the configured `AppBranding` (requires no database query and works unauthenticated):
     ```json
     {
       "id": "/threads",
       "name": "<MINICRM_APP_NAME>",
       "short_name": "<MINICRM_APP_SHORT_NAME>",
       "description": "<MINICRM_APP_DESCRIPTION>",
       "start_url": "/threads",
       "scope": "/",
       "display": "standalone",
       "background_color": "#f5f6fa",
       "theme_color": "#14142b",
       "icons": [
         {
           "src": "/static/icon-192.png",
           "sizes": "192x192",
           "type": "image/png",
           "purpose": "any"
         },
         {
           "src": "/static/icon-512.png",
           "sizes": "512x512",
           "type": "image/png",
           "purpose": "any"
         },
         {
           "src": "/static/icon-maskable-512.png",
           "sizes": "512x512",
           "type": "image/png",
           "purpose": "maskable"
         }
       ]
     }
     ```
3. **Generate App Icons (`internal/web/static/`)**:
   - Generate crisp PNG icons matching `favicon.svg` (`#6366f1` rounded square / `#14142b` maskable background with white envelope mark):
     - `icon-192.png` (`192×192`)
     - `icon-512.png` (`512×512`)
     - `icon-maskable-512.png` (`512×512` with 20% maskable safe-zone inset)
     - `apple-touch-icon.png` (`180×180`)
4. **Update `layoutHeader` (`internal/web/views/views.go:696`)**:
   - Update `<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">`.
   - Use the configured instance `AppName` / `AppShortName` in `<title>` and `<meta name="apple-mobile-web-app-title">`:
     ```html
     <meta name="theme-color" content="#14142b">
     <meta name="apple-mobile-web-app-capable" content="yes">
     <meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
     <meta name="apple-mobile-web-app-title" content="{{if .AppShortName}}{{.AppShortName}}{{else}}MiniCRM{{end}}">
     <link rel="manifest" href="/manifest.webmanifest">
     <link rel="apple-touch-icon" href="/static/apple-touch-icon.png">
     ```

---

### Phase 3: Authentication, OAuth2 Refresh Tokens & Session Hardening for Standalone Mode

**Files**: `internal/config/config.go`, `internal/auth/middleware.go`, `internal/auth/oidc.go`, `cmd/minicrm/main.go`, `internal/web/handlers_threads.go`, `internal/web/static/app.js`

1. **How MiniCRM Sessions Relate to OAuth2 Refresh Tokens (Automatic Alignment & Encrypted Storage)**:
   - **Current behavior**: In `OIDCProvider.ExchangeAndVerify` (`internal/auth/oidc.go:129`), MiniCRM exchanges the authorization code at the IdP's `/token` endpoint, verifies the `id_token`, and **immediately discards the `oauth2.Token` (including any `refresh_token`)**. After `/auth/callback`, session duration is governed purely by MiniCRM's PostgreSQL-backed SCS session (`sm.Lifetime = 12h`, `sm.IdleTimeout = 2h`), completely decoupled from the OIDC provider's token lifetimes.
   - **Planned improvement — Automatic Alignment to Refresh Token Lifetime**:
     1. **Extract Refresh Token & Its Expiry in `ExchangeAndVerify` (and on Refresh)**:
        - Extend `VerifiedIdentity` (or return a `TokenBundle`) from `ExchangeAndVerify` and a new `RefreshAndVerify(ctx, refreshToken)` method on `IdentityVerifier` with:
          - `RefreshToken string`
          - `IDTokenExpiry time.Time` (from `idToken.Expiry` or `token.Expiry`)
          - `RefreshTokenExpiry time.Time`, determined automatically in priority order:
            1. `refresh_token_expires_in` or `refresh_expires_in` from the OAuth2 `/token` response (`token.Extra(...)`).
            2. The `exp` claim of the `refresh_token` if the IdP issues structured JWT refresh tokens (e.g. Kanidm, Keycloak, Auth0).
            3. Fallback to `time.Now().Add(cfg.SessionLifetime)` (`MINICRM_SESSION_LIFETIME`, default `168h` / 7 days) when the IdP issues an opaque refresh token without an explicit expiry field.
     2. **Automatically Align SCS Session Deadline to the Refresh Token**:
        - When a `refresh_token` is present at `/auth/callback` (and whenever a refresh rotates the token), call `s.sessions.SetDeadline(ctx, refreshTokenExpiry)` so the SCS session row in PostgreSQL and the browser's `__Host-minicrm_session` cookie `Max-Age`/`Expires` automatically match the lifetime of the refresh token.
        - Remove the hardcoded 2-hour `IdleTimeout` when a refresh token manages the session (default `MINICRM_SESSION_IDLE_TIMEOUT` to `0` / disabled, while still allowing operators to set an explicit idle timeout via env var if desired), because in `scs` a non-zero `IdleTimeout` caps both database row expiry and cookie `Max-Age` to `min(deadline, now + IdleTimeout)`.
        - When the IdP does **not** return a `refresh_token` (e.g. `cmd/dev-oidc` or an IdP without `offline_access`), align the session deadline to `max(idToken.Expiry, now + cfg.SessionLifetime)`.
     3. **Encrypt `refresh_token` at Rest in Session Storage (`internal/auth/token_crypto.go`)**:
        - Because a `refresh_token` is a bearer credential capable of minting new tokens at the IdP, **never store it in plaintext in PostgreSQL (`sessions.data`)**.
        - Encrypt the `refresh_token` before calling `s.sessions.Put(ctx, SessionKeyRefreshToken, ciphertext)` using **AES-256-GCM** (`crypto/aes` + `crypto/cipher.NewGCM`) with:
          - A 32-byte key derived via `crypto/hkdf.Key(sha256.New, secret, nil, "minicrm-session-refresh-token-v1", 32)` from `MINICRM_SESSION_ENCRYPTION_KEY` (falling back to `MINICRM_OIDC_CLIENT_SECRET` via HKDF when `MINICRM_SESSION_ENCRYPTION_KEY` is unset, so the encryption key lives only in server process memory and never in PostgreSQL).
          - A fresh 12-byte cryptographic random nonce (`crypto/rand`) per encryption.
          - The authenticated user's UUID (`user.ID[:]`) passed as **AEAD Additional Authenticated Data (AAD)** so an encrypted refresh token is cryptographically bound to that specific user and cannot be transplanted across session rows.
        - Additionally enable `sm.HashTokenInStore = true` on `scs.SessionManager` so session tokens themselves are stored as SHA-256 hashes in `sessions.token` rather than raw cookie values.
     4. **Server-Side Background Token Refresh in `RequireAuth` with Concurrency Deduplication (`singleflight`)**:
        - **Why concurrency protection is critical**: Loading `/threads/{id}` triggers parallel browser requests for the page, `<iframe src="/emails/{id}/body">`, and inline `<img src="/attachments/{id}/view">` thumbnails. If `now >= IDTokenExpiry` when those parallel requests arrive, multiple goroutines would otherwise read the same `refresh_token` and call the IdP concurrently. On IdPs that enforce **refresh token rotation** (Kanidm, Keycloak, Okta, Auth0), a second concurrent request with the same old `refresh_token` is rejected with `invalid_grant` (or triggers token-reuse revocation), and concurrent `scs` commits could overwrite the newly rotated token in PostgreSQL with the stale one.
        - **Singleflight + short-TTL rotation cache (`golang.org/x/sync/singleflight`)**:
          - Protect refresh calls in `auth.Service` with a `singleflight.Group` keyed by `sha256(refreshToken)` (plus a mutex-guarded short-TTL `30s` map of `sha256(oldRefreshToken) -> refreshResult` to cover requests that loaded the old session snapshot from PostgreSQL just before the winning goroutine committed the rotated token).
          - Only **one** request performs the outbound `verifier.RefreshAndVerify(ctx, refreshToken)` call (using a detached context `context.WithoutCancel(ctx)` with a bounded `10s` timeout so one canceled sub-resource request doesn't abort the refresh for sibling requests).
          - All concurrent requests sharing that refresh apply the resulting encrypted `refresh_token`, `IDTokenExpiry`, and `s.sessions.SetDeadline(ctx, newRefreshExpiry)` to their own `scs` request context so whichever request commits last writes the newest rotated token.
          - On success: if a fresh `id_token` is included, update `users.allowed_accounts` and `display_name` in PostgreSQL (keeping Invariant #5 synchronized without interrupting the standalone PWA).
          - On failure (expired or revoked refresh token / `invalid_grant`): immediately destroy the SCS session (`s.sessions.Destroy(ctx)`) and redirect to `/auth/login` (or return `401` for email iframes).
2. **Preserve Deep-Link `return_to` Across OIDC Login (`internal/auth/middleware.go`, `internal/auth/oidc.go`)**:
   - Add `SessionKeyReturnTo = "auth_return_to"`.
   - In `RequireAuth`, when an unauthenticated `GET` request arrives for a normal page (excluding `/emails/*/body`, `/attachments/*`, `/auth/*`), store its sanitized relative URI (`r.URL.RequestURI()`, verified via `isSafeRelativeReturnURL`) in the session before redirecting to `/auth/login`.
   - In `HandleCallback`, pop `SessionKeyReturnTo` and redirect to it if valid; otherwise fall back to `/threads`.
3. **Graceful Session-Expiry Handling for Lazy-Loaded Email Frames (`internal/auth/middleware.go`, `internal/web/static/app.js`)**:
   - **Why a "Reload the page" message is not enough**: Neither iOS standalone PWAs nor Android WebAPKs show a browser toolbar or reload button (and pull-to-refresh is absent on iOS standalone and ineffective inside an iframe on Android). Telling a user to "reload the page" without an interactive button strands mobile PWA users.
   - In `RequireAuth`, if an unauthenticated request has `Sec-Fetch-Dest: iframe` or path `/emails/{id}/body`, return `401 Unauthorized` (with `SetIsolatedEmailBodyHeaders`) containing `<body data-session-expired="true">` instead of a `303` redirect to `/auth/login` (which would redirect to the cross-origin OIDC issuer and be blocked by `frame-src 'self'`).
   - In `internal/web/static/app.js`, inside `observeEmailFrame(frame)` when `doc.body && doc.body.dataset.sessionExpired === "true"`:
     - Immediately replace the `<iframe class="email-frame">` in the **parent** DOM with a styled card containing an explicit tap target: *"Your session has expired."* + **`<a class="btn btn-primary" href="/auth/login">Sign in again</a>`** (in the parent document, so it navigates the top-level window without needing to weaken the iframe `sandbox` attribute), and automatically trigger `window.location.reload()` so active sessions immediately start the top-level `/auth/login` flow (with `return_to` pointing back to the current thread).

---

### Phase 4: Standalone & Mobile UI Design Improvements

**Files**: `internal/web/static/app.css`, `internal/web/views/views.go`, `internal/web/handlers_threads.go`

1. **Fix Standalone Header & Safe-Area Insets (`internal/web/static/app.css`)**:
   - Remove `margin-top: 0.75rem;` from `.navbar` (`app.css:989`) so the `#14142b` navbar sits flush with the top of the window and matches `theme-color`.
   - Add safe-area inset support:
     ```css
     .navbar {
       padding-top: max(0.7rem, env(safe-area-inset-top));
       padding-left: max(1.5rem, env(safe-area-inset-left));
       padding-right: max(1.5rem, env(safe-area-inset-right));
     }
     .container {
       padding-left: max(1.5rem, env(safe-area-inset-left));
       padding-right: max(1.5rem, env(safe-area-inset-right));
       padding-bottom: calc(3rem + env(safe-area-inset-bottom));
     }
     ```
   - On `@media (max-width: 768px)`, hide `.navbar-user-email` and keep `.navbar` compact instead of wrapping `.navbar-user` onto a tall second row.
2. **Responsive Inbox List on Mobile (`< 768px`) (`internal/web/static/app.css`)**:
   - Remove the conflicting `.threads-table` `min-width` overrides (`app.css:435, 938, 1480, 1498`).
   - Below `768px`, style `.threads-table` as a stacked card list using CSS Grid on `tr.thread-row` (preserving the single HTML template and desktop table):
     - Hide `thead` (`display: none`).
     - Lay out `tr.thread-row` as a compact card with Customer + Wait/Age on the top line, Subject + count pill + snippet in the middle, and Status + Account + Assignee metadata on the bottom line.
     - Preserve the `3px` indigo left accent bar on `.thread-row-new`.
3. **Single-Row Scrollable Status Tabs on Mobile (`internal/web/static/app.css`)**:
   - Below `768px`, set `.tabs` to `display: flex; flex-wrap: nowrap; overflow-x: auto; max-width: 100%; scrollbar-width: none; -webkit-overflow-scrolling: touch;` and `.tab { flex: none; white-space: nowrap; }` so the 6 status filters form a clean horizontal pill strip.
4. **Standalone Navigation & Refresh Ergonomics (`internal/web/views/views.go`, `internal/web/handlers_threads.go`)**:
   - Neither iOS standalone PWAs nor Android WebAPKs provide a persistent browser reload button. Add an explicit **Refresh** action button in the `/threads` `.page-heading` (`<a class="btn btn-secondary btn-small" href="/threads?status={{.StatusFilter}}&amp;assignee={{.AssigneeFilter}}&amp;account={{.AccountFilter}}">Refresh</a>`) and in the `/threads/{id}` thread header so standalone users on iOS, Android, and desktop can always refresh threads with a single tap.
   - In `HandleViewThread` (`internal/web/handlers_threads.go`) and `ThreadDetailViewData` (`internal/web/views/views.go`), populate `BackURL` from a same-origin `/threads?...` `Referer` header (defaulting to `"/threads"`) so clicking **"← Back to threads"** returns to the operator's filtered inbox view.
5. **Touch Ergonomics & iOS Zoom Prevention (`internal/web/static/app.css`)**:
   - Under `@media (max-width: 768px), (pointer: coarse)`, set `font-size: 1rem` (`16px`) on `input`, `select`, and `textarea` to prevent iOS auto-zoom on focus, and ensure interactive controls (`.btn`, `.tab`, `select`) meet a minimum `2.5rem` (`40px`) touch height.

---

### Phase 5: Minimal Security-Safe Service Worker & Offline Fallback

**Files**: `internal/web/static/sw.js`, `internal/web/static/offline.html`, `internal/web/static/app.js`

1. **Offline Fallback Page (`internal/web/static/offline.html`)**:
   - Self-contained static HTML page styled consistently with MiniCRM's error card (`--bg: #f5f6fa`, `--ink: #14142b` header, card saying *"You're offline — MiniCRM requires a network connection to view and reply to threads"* with a `<a class="btn btn-primary" href="/threads">Try again</a>`).
2. **Service Worker (`internal/web/static/sw.js`)**:
   - Cache name versioned (e.g. `minicrm-shell-v1`).
   - `install`: precache only static public assets (`/static/app.css`, `/static/app.js`, `/static/favicon.svg`, `/static/icon-192.png`, `/static/icon-512.png`, `/static/offline.html`).
   - `activate`: delete older `minicrm-shell-*` caches and `clients.claim()`.
   - `fetch`:
     - Immediately ignore (`return`) if `request.method !== "GET"`.
     - Immediately ignore if pathname starts with `/auth/`, `/emails/`, or `/attachments/` (never intercept OIDC redirects, isolated email frames, or attachments).
     - For `/static/*`: network-first with cache fallback (or stale-while-revalidate) so static assets work offline while picking up updates promptly.
     - For navigation requests (`request.mode === "navigate"`): network-only (`fetch(event.request)`), falling back to the cached `/static/offline.html` **only** when `fetch` rejects due to a network error. Never store navigation HTML responses in any cache.
3. **Register Service Worker (`internal/web/static/app.js`)**:
   - At the end of `app.js`, register `/sw.js` on `window` `load` if `"serviceWorker" in navigator`.

---

## Verification & Testing Plan

1. **Unit & Integration Tests**:
   - `internal/web/web_test.go`:
     - Verify `GET /manifest.webmanifest` works unauthenticated, returns `200`, `Content-Type: application/manifest+json; charset=utf-8`, `Cache-Control: no-cache`, and reflects custom `AppBranding` (`name`, `short_name`, `description`) and icons.
     - Verify `GET /sw.js` works unauthenticated, returns `200`, `Content-Type: application/javascript`, and `Cache-Control: no-cache`.
     - Verify `MainCSP` and `MainCSPInsecure` include `manifest-src 'self'; worker-src 'self'`.
     - Verify `/static/app.css` receives `Cache-Control: public, max-age=3600, must-revalidate` while `/threads` and `/emails/{id}/body` retain `no-store`.
     - Verify unauthenticated `GET /emails/{id}/body` with `Sec-Fetch-Dest: iframe` returns `401 Unauthorized` with `data-session-expired="true"` instead of a `303` redirect.
     - Verify `BackURL` in `/threads/{id}` preserves valid `/threads?...` `Referer` query parameters and rejects cross-origin or non-`/threads` referers.
   - `internal/auth/auth_test.go`:
     - Verify `RequireAuth` → `HandleLogin` → `HandleCallback` preserves `return_to` for deep links (e.g. `/threads/<uuid>?foo=bar`) and rejects open-redirect values (`//evil.example`, `https://evil.example`).
     - Verify session deadline (`sessions.Deadline`) automatically aligns with the `refresh_token` expiry (both via `refresh_token_expires_in` and JWT `exp` claim).
     - Verify AES-256-GCM encryption/decryption of stored `refresh_token`s, confirming that raw session bytes in the store never contain the plaintext `refresh_token` and that tampering or mismatched user-UUID AAD fails decryption cleanly.
     - Verify background OAuth2 `refresh_token` renewal in `RequireAuth` refreshes expired tokens transparently, updates `allowed_accounts` when refreshed ID token groups change, and destroys the session when the refresh token is rejected (`invalid_grant`).
     - Verify concurrent requests arriving with an expired `IDTokenExpiry` (e.g. 10 parallel goroutines with a strict single-use rotating refresh token verifier) trigger **exactly one** `RefreshAndVerify` call and all succeed without `invalid_grant` or stale-token overwrites.
   - `internal/config/config_test.go`:
     - Verify `MINICRM_APP_NAME`, `MINICRM_APP_SHORT_NAME`, `MINICRM_APP_DESCRIPTION`, `MINICRM_SESSION_LIFETIME`, `MINICRM_SESSION_IDLE_TIMEOUT`, and `MINICRM_SESSION_ENCRYPTION_KEY` defaults and validation.
2. **Mobile Viewport Browser Review (`375×812` & `390×844` phone, `768×1024` tablet)**:
   - Run MiniCRM locally with `cmd/dev-oidc` and open it in a real browser (and automated Playwright / headless Chromium mobile device emulation at `375×812` and `390×844`) to visually inspect and verify:
     - **Inbox (`/threads`)**: Flush top `#14142b` navbar with compact single-row mobile layout, single-row horizontally scrollable status tabs, stacked CSS Grid thread cards (no horizontal page overflow: `document.documentElement.scrollWidth <= document.documentElement.clientWidth`), filter dropdowns, and the in-app **Refresh** button.
     - **Thread detail (`/threads/{id}`)**: Header badges and customer info wrapping cleanly, full-width mobile action buttons and assignee selector, collapsible email message cards with properly sized `<iframe class="email-frame">`, attachment cards, reply form (verifying computed `font-size >= 16px` on inputs/selects/textarea), **Refresh** button, and filter-preserving **"← Back to threads"** link.
     - **Settings (`/settings`)**: Responsive single-column form grid and contained table scrolling inside `.table-wrap`.
     - **Offline fallback (`/static/offline.html`)**: Clean mobile card layout and working **"Try again"** button.
3. **Documentation Updates**:
   - Update `docs/security.md` (CSP table and static vs. dynamic `Cache-Control` note), `docs/authentication.md` (session lifetime, idle timeout, and OAuth2 refresh token behavior), and `README.md` (new `MINICRM_APP_*` and `MINICRM_SESSION_*` env vars).
4. **Required Verification Commands**:
   ```bash
   golangci-lint run ./...
   go test -v -race ./...
   ```
