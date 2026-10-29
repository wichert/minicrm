# Plan 4 — Content & Mobile: Remote Images, Reply Formatting, Mobile Tabs (P2/P3)

Source: `docs/ux-review-user-stories.md`, Workstreams 7, 8 & 9.

**Constraints (from `AGENTS.md`):** untrusted HTML never rendered directly — always `mail.SanitizeEmailHTML` + isolated sandboxed `<iframe>` + `script-src 'none'`; keep `SecurityHeadersMiddleware`; outbound mail stored in PostgreSQL and APPENDed to `Sent` via the owning account.

**Dependency:** Workstream 8 extends the draft storage introduced in Plan 1 (`plans/ux-draft-persistence.md`); coordinate if both ship. Workstreams 7 and 9 are independent.

## Workstream 7 — Blocked remote images: notice + consent to load (P2)

**Problem:** blocked remote images render as broken icons with no explanation or load option (`internal/mail/mime_parser.go`, `internal/web/views/views.go`).

**Changes**
- Sanitizer (`internal/mail/mime_parser.go`): rewrite remote `<img src>` to a placeholder carrying the original URL in a data attribute; render an accessible notice: "Remote images blocked for your privacy" with **Load images for this message** and a note that the sender may learn the message was opened.
- Loading route: server-side image proxy (`/images/proxy?...`) — strictly allow-listed: SSRF protection (no private/loopback/link-local targets), redirects capped/disallowed to private ranges, size cap, content-type allow-list (image/*), timeout, no caching of arbitrary content; response served only to users with access to the owning email/account.
- Keep sanitizer, sandbox, CSP (`script-src 'none'`, no `allow-scripts`) and account checks fully intact; per-message consent only (no global opt-out).

**Tests:** SSRF unit tests (private IPs, redirects), size/type rejection, access control, sanitizer placeholder round-trip.

## Workstream 8 — Reply formatting: bold/italic with text fallback (P2)

**Problem:** replies are plain text only (`text/plain` in `internal/mail/smtp_sender.go`).

**Changes**
- Composer: **reuse an existing editor rather than building one** — integrate a small, maintained rich-text library (candidates: TipTap or a minimal ProseMirror setup) configured to only bold/italic, keyboard shortcuts Cmd/Ctrl+B/I, visible formatting state, and a plain-text fallback toggle. No new heavyweight dependency beyond that single editor package; evaluate bundle size and accessibility before committing.
- Send as `multipart/alternative`: `text/plain` part (existing content) + restricted HTML part generated server-side from the editor's limited node set (whitelist b/i/br/p/a only; escape everything else) — no raw user HTML.
- Sent copies stored/APPENDed as today and continue through existing sanitization/isolation when rendered.
- Draft storage (Plan 1) extended to carry formatting state.

**Tests:** html-generation whitelist tests (XSS attempts reduced to text), multipart structure, rendering of received copies through sanitizer.

## Workstream 9 — Mobile status-tab overflow affordance (P3)

**Changes**
- Add scroll fade/arrow affordance on the horizontal status strip; ensure keyboard focus scrolls the active tab into view (`scrollIntoView` on focus, no touch-target shrink).
- Validate on real devices + keyboard/screen-reader before finalizing (per review's caveat).

**Tests:** manual + e2e check at 390 px; focus-visible test.

**Verification:** `golangci-lint run ./... && go test -race ./...`; update `docs/security.md` for the image proxy and any sanitization changes.

**Commit style:** Conventional Commits, e.g. `feat(mail): remote image consent with proxy`, `feat(compose): bold/italic formatting`.