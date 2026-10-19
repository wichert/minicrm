# MiniCRM UX & Visual Design Review

**Date:** 2026-10-19
**Reviewer:** UX & Product Design Expert (browser-verified against live running stack + source inspection)

## Executive verdict

MiniCRM's backend architecture, multi-account scoping, and workflow state machine are well-structured, but **inspecting the live application in Google Chrome revealed two severe front-end blockers alongside foundational UX and visual-design gaps**:

1. **Email bodies cannot be read at all in Chromium browsers (Blocker):** Every message `<iframe>` renders Chrome's broken-frame ("sad document") icon. The `credentialless` attribute on `<iframe class="email-frame">` strips the `minicrm_session` cookie when fetching `/emails/{id}/body`, triggering a `303 -> /auth/login -> 302 -> OIDC issuer` redirect chain inside the iframe that is blocked by `Content-Security-Policy: frame-src 'self'`.
2. **All inline `style="..."` attributes are blocked by `Content-Security-Policy: style-src 'self'` (High):** Every page load logs CSP errors and strips inline layout rules. As a result, the reply `<textarea>` collapses to a tiny ~160px-wide box (`cols="20"`), the thread workflow buttons stack vertically edge-to-edge instead of forming a horizontal action bar, and the navbar `Logout` button drops onto a second line below the user's name.

Beyond those two runtime regressions, the UI is still an unstyled browser-default prototype (`app.css` is 71 lines). Native controls render in `13.33px Arial` with `2px outset` system bevels, statuses (`new`, `in_progress`, `completed`, `spam`) have no color coding, errors replace the screen with raw plain text (destroying unsent reply drafts), and terminal threads (`spam`, `completed`) still allow operators to send replies and reassign owners.

**Overall rating: 1.5 / 5 in Chromium as deployed (due to broken email iframes and CSP-blocked inline layout); 2.5 / 5 for the underlying interaction structure.** Fixing the two CSP/iframe bugs and applying a cohesive stylesheet and feedback pattern will immediately lift the product to a usable daily tool.

---

## Scope and method

- **Live browser verification:** Inspected the running Docker/Podman Compose stack (`http://localhost:8080` with `dev-oidc`, PostgreSQL, Dovecot IMAP, and Mailpit SMTP) using headless Google Chrome (`1280×900` @ 2x desktop and `375×812` @ 2x mobile viewports).
- **Flows & states exercised in browser:**
  - OIDC sign-in redirect and session establishment (`Dev Admin <admin@example.com>`).
  - Inbox (`/threads`) across all four workflow states (`new`, `in_progress`, `completed`, `spam`) and multi-account filtering (`Customer Support`, `Sales`).
  - Thread detail (`/threads/{id}`) for `new`, `in_progress`, `completed`, `spam`, and a follow-up thread displaying the **Completed Thread Found (Reopen & Merge)** banner.
  - Isolated email body rendering (`/emails/{id}/body`) with and without `credentialless`, for both sanitized HTML and plain-text emails.
  - Admin settings & user pre-creation (`/settings`).
  - Error states: `400` (`invalid IANA display_timezone`), `404` (`thread not found`), and `422` (`reply body must not be empty`).
  - DOM accessibility audit (`<label>`, `<iframe title>`, `<nav>`, `<th scope>`) and computed-style inspection.
- **Source reviewed:** `internal/web/views/views.go`, `internal/web/static/app.css`, `internal/web/static/htmx.min.js`, `internal/web/security_headers.go`, `internal/web/handlers_threads.go`, `internal/web/handlers_settings.go`, `internal/web/router.go`, `internal/auth/middleware.go`, and `internal/mail/smtp_sender.go`.

---

## Strengths

- **Clear single-card thread header context (`views.go:231-286`):** Subject, status, account, customer, assignee, and workflow actions are grouped together at the top of the thread detail page.
- **Progressive disclosure for multi-account UI (`views.go:178,216,235`):** Account columns, badges, and filters appear only when `ShowAccountUI` (`user.HasMultipleAccounts()`) is true, keeping single-account installations clean.
- **Guardrailed assignee options (`handlers_threads.go:239-243`, `views.go:354-358`):** Only active users with access to the thread's mail account are offered in assignment dropdowns.
- **Consistent timezone transparency (`views.go:183,332`):** Timestamps are converted to the configured IANA timezone, and the active timezone (`UTC`, etc.) is explicitly named in table headers and the audit timeline heading.
- **Distinct Reopen & Merge callout (`views.go:240-249`, `app.css:43-49`):** `.banner-reopen` has a dedicated blue surface (`#eff6ff` / `#93c5fd`) that visually separates the reopen prompt from surrounding cards.
- **Safe POST-Redirect-GET pattern (`handlers_threads.go:483`):** Successful mutations redirect with `303 See Other`, preventing duplicate form resubmission on browser refresh.

---

## Findings

### Critical runtime & layout bugs (browser-verified)

**B1. Email bodies fail to render in Chromium browsers due to `<iframe credentialless>` stripping the session cookie (Blocker).**
- *Browser evidence:* On every thread detail page in Chrome, every `<iframe class="email-frame" src="/emails/{id}/body" ... credentialless>` displays Chrome's gray broken-frame icon. Network trace shows:
  1. Chrome requests `GET /emails/{id}/body` from the `credentialless` iframe with `Cookie: (none)`.
  2. `RequireAuth` (`internal/auth/middleware.go:49-53`) sees no session cookie and returns `303 See Other -> /auth/login`.
  3. `/auth/login` redirects (`302`) to `http://dev-oidc.localhost:9000/authorize?...`.
  4. Chrome blocks the frame and logs: `Framing 'http://dev-oidc.localhost:9000/' violates the following Content Security Policy directive: "frame-src 'self'". The request has been blocked.`
- *Impact:* Operators using Chrome, Edge, Brave, or Arc cannot read any inbound or outbound emails. Removing `credentialless` in the DOM immediately allows the session cookie to accompany `GET /emails/{id}/body` and renders the email content cleanly.
- *Fix:* Remove `credentialless` from `<iframe class="email-frame">` in `views.go:300`. The iframe is already isolated in a unique opaque origin by `sandbox="allow-popups allow-popups-to-escape-sandbox"` (which omits `allow-same-origin` and `allow-scripts`) and locked down by `EmailBodyCSP` (`default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; ...`).

**B2. `Content-Security-Policy: style-src 'self'` blocks every inline `style="..."` attribute in `views.go` (High).**
- *Browser evidence:* Every page navigation logs multiple console errors: `Applying inline style violates the following Content Security Policy directive 'style-src 'self''`. All six inline `style=` attributes in `views.go` (`:159,245,251,320,322,325`) are ignored by the browser:
  - **Tiny reply box (`views.go:320`):** `<textarea name="body_text" rows="5" style="width:100%">` loses `width:100%` and renders at Chrome's default `cols="20"` (~160px wide inside a 1,060px card).
  - **Collapsed vertical button stack (`views.go:251`):** `<div style="display:flex; gap:1rem; align-items:center; flex-wrap:wrap;">` loses its flex layout, so `Start Progress`, `Mark as Spam`, `Take Thread`, and `Assignee` stack vertically as full-width block `<form>`s with `0px` vertical gap.
  - **Broken navbar alignment (`views.go:159`):** `<form method="post" action="/auth/logout" style="display:inline">` renders as a block element, pushing `Logout` onto a second line underneath `Dev Admin (admin@example.com)`.
  - **Missing vertical spacing (`views.go:245,322,325`):** `style="margin-top:0.5rem"` on the reopen form and reply controls is ignored.
- *Fix:* Keep `style-src 'self'` strict, remove all inline `style="..."` attributes from `views.go` (and `isolatedEmailBodyHTML`), and move those rules into `static/app.css` classes (plus a small stylesheet or `<style>` hash for `/emails/{id}/body` where `EmailBodyCSP` already permits `style-src 'unsafe-inline'`).

---

### Ease of use & workflow UX

**E1. Errors are bare plain-text pages that destroy user input, and successes give zero feedback (High).**
- *Browser evidence:* Submitting a whitespace-only reply (`POST /threads/{id}/reply`) replaces the entire application UI with a stark white page reading `reply body must not be empty` (`422`). Submitting an invalid timezone (`POST /settings`) renders `invalid IANA display_timezone` (`400`). Navigating to a missing thread renders `thread not found` (`404`). Conversely, clicking `Save Settings`, `Pre-Create User`, `Assign`, or `Send Reply via SMTP` performs a silent `303` redirect with no confirmation banner.
- *Impact:* If an SMTP error or validation error occurs during reply submission, the operator loses the reply they just typed and must use the browser Back button. On `/settings`, admins cannot tell whether changes were saved or whether a user was created.
- *Fix:* Store flash notices in the SCS session and render a dismissible banner at the top of `<main class="container">`. On 4xx form errors, re-render the page template with inline field errors and preserved form input. Provide a styled error template for `403`/`404`/`500`.

**E2. Workflow buttons are unstyled, stacked without hierarchy, leak internal state names, and lack confirmation on destructive moves (High).**
- *Browser evidence:* In `new` threads, `Start Progress (new -> in-progress)`, `Mark as Spam (Move to Junk)`, `Take Thread (assign to me)`, and `Assignee: [Dev Admin] [Assign]` sit in a tight vertical stack of identical gray `13.33px Arial` system buttons.
- *Impact:*
  - `Mark as Spam (Move to Junk)` is a terminal transition (`new -> spam`) that moves emails to IMAP `Junk` with no undo path in MiniCRM, yet it looks identical to `Start Progress` and sits 2px below it with no confirmation dialog.
  - `Complete Thread (Move to Archive)` (`in_progress -> completed`) moves emails out of `INBOX` to `Archive` without confirmation.
  - Labels expose developer syntax (`new -> in-progress`) and transport mechanics (`Move to Junk`, `Move to Archive`, `Send Reply via SMTP`).
- *Fix:* Style primary actions (`Start working`, `Mark complete`, `Send reply`) with `--primary`, secondary actions (`Assign to me`) as subtle outline buttons, and `Mark as spam` as a danger/ghost button with a confirmation prompt. Move IMAP folder details to helper text or tooltips.

**E3. Terminal threads (`spam` and `completed`) still allow replies and reassignment (High).**
- *Browser evidence:* Inspecting a `spam` thread (`/threads/f8007c83-...`) and a `completed` thread (`/threads/b015a435-...`) in Chrome shows that both `Take Thread` / `Assignee: [...] [Assign]` and the `Send Reply` card remain active. Worse, `ComposeAndQueueReply` (`internal/mail/smtp_sender.go:275-307`) only auto-transitions `new -> in_progress`; replying to a `spam` or `completed` thread sends an outbound email via SMTP and auto-assigns the thread (`Assignee: Dev Admin (reply)`) while leaving the thread status stuck in `spam` or `completed`!
- *Impact:* Operators can accidentally email a spammer or reply on a closed thread without reopening it, creating inconsistent CRM state (`status = spam` with outbound replies).
- *Fix:* Hide or disable the reply composer and assignment controls when `Thread.Status == "spam"`. On `completed` threads, either require reopening before replying or clearly state that sending a reply will reopen the thread to `in_progress` (and enforce that transition in the backend).

**E4. The reply composer sits below all messages, lacks recipient/sender context, and hides side effects (High).**
- *Browser evidence:* The `Send Reply` card is at the bottom of the page above `Audit Timeline`. It contains only a `<textarea>`, `<input type="file">`, and `Send Reply via SMTP`. It does not show `To: Customer Name <email>`, nor which mail account (`From: support@example.com` vs `sales@example.com`) will send the message.
- *Impact:* In multi-account setups, operators risk replying from the wrong brand/account expectation without realizing which mailbox owns the thread unless they scroll back to the top card. Additionally, replying to a `new` unassigned thread silently transitions it to `in_progress` and assigns it to the sender without warning.
- *Fix:* Display `From: {AccountName} <{AccountFromAddress}>` and `To: {CustomerName} <{CustomerEmail}>` at the top of the reply composer. Add a helper note when the thread is `new` ("Sending a reply will assign this thread to you and mark it In progress"). Add a "Reply" jump button in the top header card.

**E5. Overlapping and misleading assignee controls (Medium).**
- *Browser evidence:* On an unassigned thread, the metadata line reads `Assignee: Unassigned`, a `Take Thread (assign to me)` button appears, and directly underneath it the `Assignee:` `<select>` displays `Dev Admin (admin@example.com)` (the first active user in the list) as already selected next to an `Assign` button (`views.go:269-284`).
- *Impact:* Having `Assignee: Unassigned` in text while `Assignee: [Dev Admin (admin@example.com) v]` is preselected in the dropdown is visually contradictory. A single control with a `-- Unassigned --` placeholder option plus a quick `Assign to me` action is much clearer.

**E6. Reopen & Merge banner exposes raw UUIDs and under-explains the merge consequence (Medium).**
- *Browser evidence:* When `ReopensThread` is present (`views.go:240-249`), the banner reads:
  `Completed Thread Found: This email is a reply to already completed thread Cannot log in to my account (b015a435-f792-434b-a70f-98a063a00baa). [Reopen completed thread & merge this email into existing thread]`
- *Impact:* The 36-character UUID clutters the link (especially on mobile where it wraps across two lines). The banner does not explain what happens if the operator clicks the button (this temporary thread is deleted, its messages move into the original thread, and the original thread returns to `In progress`) versus if they ignore it and click `Start Progress` right below it.
- *Fix:* Drop the raw UUID from the link text, show when the original thread was completed and by whom, and present a clear two-choice callout: primary action **Reopen & merge into original thread** vs secondary explanation **Or keep as a separate new thread below**.

**E7. Inbox filtering lacks an Assignee ("Mine / Unassigned") filter, has a redundant submit button, and silently caps at 100 rows (Medium).**
- *Browser evidence:* Note on `htmx.min.js`: unlike the assumption in the earlier source-only review, `internal/web/static/htmx.min.js` is **not** the third-party HTMX library—it is a 10-line custom script that attaches a `change` listener to `[data-hx-get]` selects and calls `el.form.submit()`. Consequently:
  - Changing either `Status` or `Account` triggers a full-page GET navigation (`/threads?status=new&account=sales`), which **does** preserve both filter values and **does** update the browser URL.
  - However, the `Filter` submit button right next to the auto-submitting dropdowns is redundant when JavaScript is enabled.
  - Furthermore, `InboxViewData.AssigneeFilter` (`views.go:32`) and `InboxViewData.Users` (`views.go:33`) are populated in Go structs but never rendered in the filter bar (`views.go:206-227`) or wired into `ListThreads` (`handlers_threads.go:139-144`). Operators have no way to filter by **Assigned to me** or **Unassigned**—the two most critical views in daily triage.
  - Finally, `HandleListThreads` hardcodes `Limit: 100` (`handlers_threads.go:143`) with no count indicator or pagination.

**E8. Settings page fetches `Users` and `AuditLogs` on every load but renders neither (Medium).**
- *Browser evidence:* `/settings` renders only the `System Settings` form and the `Pre-Create User (Assignee)` form. Even though `HandleGetSettings` (`handlers_settings.go:26-27`) queries `s.store.ListUsers(ctx)` and `s.store.ListAuditLogs(ctx, 50)` and passes them in `SettingsViewData`, `settingsBody` (`views.go:341-412`) never displays existing users or system audit logs.
- *Impact:* After pre-creating a user, the admin is redirected back to `/settings` with an empty form, no confirmation flash, and no user directory table to verify the user's email, role, or account access.

**E9. Fixed 220px email iframes waste vertical space on short messages and trap long messages in nested scrollbars (Low-Medium).**
- *Browser evidence:* When `credentialless` is removed so email bodies render, short plain-text replies (e.g., 3 lines / ~80px tall in Thread 4) sit inside a fixed `220px`-tall `<iframe>` box with ~140px of empty white space below the text, while longer emails scroll inside the 220px frame.

---

### Visual design & responsive craft

**V1. Missing base styles leave tables, buttons, and inputs in 1990s browser defaults (High).**
- *Browser evidence (computed styles):*
  - `app.css` declares `--muted`, `--primary`, `--danger`, and `--success` (`:5-9`) and never uses any of them.
  - `<button>` and `<select>` compute to `font-family: Arial`, `font-size: 13.3333px`, `background-color: rgb(239, 239, 239)`, `border: 2px outset rgb(0, 0, 0)`, `padding: 1px 6px`, clashing with the `16px system-ui` body typography and clean `.card` borders.
  - `<table>`, `<th>`, and `<td>` have `padding: 1px`, `border: 0px none`, and `border-collapse: separate`. Columns are cramped, headers are centered while cells are left-aligned, there are no horizontal row dividers, and rows lack `:hover` states.
  - Links (`<a>`) use unstyled default `rgb(0, 0, 238)` with permanent underlines.

**V2. Status badges have zero color differentiation (High).**
- *Browser evidence:* Inspecting all four statuses simultaneously in the inbox (`new`, `in_progress`, `completed`, `spam`) confirms that `<span class="badge">` computes to `background-color: rgba(0, 0, 0, 0)` and `color: rgb(15, 23, 42)`. Meanwhile, `<span class="badge badge-account">` has a bright blue pill (`#e0ecff` background, `#1d4ed8` text, `#bfdbfe` border).
- *Impact:* The Account column is the loudest visual element on the screen, while workflow status—the primary triage signal—is visually invisible plain text.
- *Fix:* Give each status a distinct semantic pill style:
  - `New`: Blue (`background: #dbeafe; color: #1e40af; border: 1px solid #93c5fd`)
  - `In progress`: Amber (`background: #fef3c7; color: #92400e; border: 1px solid #fcd34d`)
  - `Completed`: Green (`background: #dcfce7; color: #166534; border: 1px solid #86efac`)
  - `Spam`: Slate/Red (`background: #fee2e2; color: #991b1b; border: 1px solid #fca5a5`)
  - Soften `.badge-account` to a neutral slate pill so it does not compete with `New`.

**V3. Mobile viewport (`375×812`) breaks the inbox layout and clips the navbar (Medium).**
- *Browser evidence:* On a `375px` mobile screen:
  - `#threads-table table` forces a `563px` minimum width (`documentElement.scrollWidth: 587px`) because email addresses (`<carol@customer.example>`) cannot wrap.
  - Scrolling right reveals that `header.navbar` and the filter `.card` stop at `375px`, leaving 212px of exposed gray background to the right of the header.
  - The `Subject` column is squeezed so narrow that links wrap one word per line (`Question / about / invoice / INV- / 2026- / 0042` — 6 lines tall for one subject).
  - On `/settings`, the inline `<label>` elements inside `Pre-Create User` wrap mid-label (`Email: [input] Display / Name: [input] Role: / [agent]`).
- *Fix:* Wrap `#threads-table` in a card with `overflow-x: auto` (or collapse table rows into stacked cards below `768px`), format the Customer column as a two-line stack (`CustomerName` in primary text, `CustomerEmail` in smaller `--muted` text below), and use CSS Grid / Flexbox for form fields on `/settings`.

**V4. Inbound and outbound messages look identical (Medium).**
- *Browser evidence:* On threads with both customer messages and agent replies (`/threads/f8007c83-...`), both cards use identical `<article class="card">` styling and print raw enum prefixes `inbound:` and `outbound:` (`views.go:292`).
- *Fix:* Distinguish message direction visually—for example, a subtle blue left border (`border-left: 3px solid var(--primary)`) and a `Team reply` badge on outbound messages versus a neutral header with `Customer` on inbound messages. Format attachment file sizes in human-readable `KB`/`MB` instead of raw `43 bytes` / `123456 bytes` (`views.go:308`).

**V5. Run-on pipe-separated metadata in navbar and thread header (Medium).**
- *Browser evidence:*Both the navbar (`MiniCRM | Threads | Settings`) and the thread header (`Status: new | Account: [Customer Support] | Customer: Bob Builder <bob@customer.example> | Assignee: Unassigned`) concatenate items with literal `|` characters inside a single line (`views.go:153-156, 233-238`). On mobile (`375px`), the navbar wraps `Settings` onto a second line with a trailing `|` on line 1 (`MiniCRM | Threads |`).
- *Fix:* Replace `|` text separators with flex/grid gaps and structured metadata items (`<dl>` or flex pills).

---

### Consistency & accessibility

**C1. Inconsistent status and role naming across the UI (Medium).**
- *Evidence:*
  - Status filter options (`views.go:209-213`): `All`, `new`, `in-progress` (hyphen), `completed`, `spam`.
  - Status badges (`views.go:192,234`): `new`, `in_progress` (underscore), `completed`, `spam`.
  - Transition buttons (`views.go:255,259,265`): `Start Progress (new -> in-progress)`, `Mark as Spam`, `Complete Thread`.
- *Fix:* Use a single `FormatStatus(sqlc.ThreadStatus)` view helper returning `New`, `In progress`, `Completed`, `Spam` across all filters, badges, buttons, and timeline entries.

**C2. Raw internal enum codes and database identifiers leak into the UI (Medium).**
- *Browser evidence:*
  - Thread header prints raw `AssignmentSource` in parentheses: `Assignee: Dev Admin (reply)` or `Assignee: Dev Admin (default)` (`views.go:237`).
  - Audit timeline prints raw `EventType` snake_case strings with no actor and no details: `2026-10-19 19:53:27 UTC — created`, `status_changed`, `reply_sent`, `assigned` (`views.go:335`), even though `thread_events` stores `actor_user_id`, `old_value`, and `new_value` JSON payloads.
  - Settings fieldsets print internal account slugs: `Account: Customer Support (support)` (`views.go:346,408`).
- *Fix:* Format `thread_events` into human-readable sentences including the actor and transition detail (e.g., *"Dev Admin changed status from New to In progress"*). Replace `(reply)` / `(default)` with subtle muted text (`assigned via reply`, `auto-assigned`).

**C3. Page `<title>`, headings, and terminology drift across screens (Low-Medium).**
- *Browser evidence:* Every page has `<title>MiniCRM</title>` (`views.go:146`), making browser tabs and history indistinguishable. The navbar link says `Threads` while the `<h1>` says `Conversation Threads`. The settings form heading `Pre-Create User (Assignee)` uses internal provisioning jargon.

**A1. Accessibility gaps verified by DOM audit (Medium).**
- *Browser evidence:*
  1. **Unlabeled reply textarea (`views.go:320`):** `<textarea name="body_text" placeholder="Write your reply...">` has no associated `<label>` or `aria-label`.
  2. **Untitled email iframes (`views.go:296`):** `<iframe class="email-frame">` has no `title` attribute (e.g., `title="Email message from {{.Email.FromAddress}}"`).
  3. **No `<nav>` landmark or active-link state (`views.go:150-162`):** Primary navigation is inside a plain `<div>` with no `<nav>` landmark or `aria-current="page"`.
  4. **Missing table header scope (`views.go:177-184`):** None of the 6 `<th>` elements in `#threads-table` specify `scope="col"`.
  5. **No custom `:focus-visible` ring (`app.css`):** Keyboard navigation relies entirely on inconsistent browser default outlines.

---

## Prioritized action plan

### Phase 1 — Immediate blockers & high-impact visual/UX fixes (1–2 days)
1. **Fix Chromium email rendering (`B1`):** Remove `credentialless` from `<iframe class="email-frame">` in `views.go:300` (keeping `sandbox="allow-popups allow-popups-to-escape-sandbox"` and `EmailBodyCSP`) so `/emails/{id}/body` receives the session cookie and emails actually render in Chrome/Edge/Brave/Arc.
2. **Eliminate CSP-blocked inline styles (`B2`):** Remove all `style="..."` attributes from `views.go` and move layout rules (`.navbar`, `.actions-bar`, `.reply-textarea`, `.form-row`) into `static/app.css`.
3. **Ship complete foundational CSS (`V1`, `V2`, `A1`):** Style `table`/`th`/`td`, `.btn` (`.btn-primary`, `.btn-secondary`, `.btn-danger`), `input`/`select`/`textarea` (`font: inherit`), semantic status badges (`.badge-new`, `.badge-in_progress`, `.badge-completed`, `.badge-spam`), and `:focus-visible` rings.
4. **Guard terminal threads & destructive actions (`E2`, `E3`):** Add confirmation before `Mark as spam` / `Mark complete`, simplify button labels, and disable/hide reply and assignment forms on `spam` threads (and handle `completed` thread replies cleanly).
5. **Clean up assignee & status vocabulary (`E5`, `C1`, `V4`):** Add `-- Unassigned --` to the assignee dropdown, add a `FormatStatus` helper (`New`, `In progress`, `Completed`, `Spam`), and replace raw `inbound:`/`outbound:` and byte counts with clear labels and `KB`/`MB` formatting.

### Phase 2 — Feedback loops, triage power, and admin completeness (3–5 days)
1. **Flash notifications & inline error handling (`E1`):** Add session flash messages on POST redirects (`Reply queued`, `Thread marked complete`, `Settings saved`, `User added`) and preserve form inputs on 4xx validation errors.
2. **Inbox triage improvements (`E7`, `V3`):** Wire up the existing `AssigneeFilter` (`Assigned to me`, `Unassigned`, specific agent), remove the redundant `Filter` button when JS is active (or use `<noscript>`), and show thread count / pagination.
3. **Reply composer context & ergonomics (`E4`, `E6`):** Show `From:` and `To:` headers on the reply card, add a top `Reply` jump button, note the auto-assign/auto-start behavior on `new` threads, and clarify the **Reopen & Merge** banner without raw UUIDs.
4. **Complete the Settings page (`E8`, `C2`):** Render the already-queried `Users` table and `AuditLogs` activity table on `/settings`, and format `thread_events` with actor names and human-readable change descriptions.

### Phase 3 — Polish & responsive refinement (Backlog)
1. **Responsive mobile layout (`V3`):** Wrap tables in horizontal scroll containers or collapse rows into cards under `768px`, and stack Customer name + email vertically.
2. **Auto-sizing email frames (`E9`):** Adjust iframe height to content (or provide an expand toggle) so short messages don't leave 140px of empty whitespace and long messages don't trap scroll.
3. **Per-page `<title>` and keyboard shortcuts (`C3`):** Include thread subject and section names in `<title>`, and add triage hotkeys (`j`/`k`, `r` to reply, `e` to complete).

---

## Appendix: Screens and states inspected in Chrome

| Screen / state | URL / Trigger | Viewport(s) | Key browser observation |
|---|---|---|---|
| Inbox — default & all 4 statuses | `/threads` | `1280×900`, `375×812` | Unstyled `1px`-padding table; `new`, `in_progress`, `completed`, `spam` have transparent badges; `Logout` wraps below user name due to CSP; table overflows `375px` mobile width by `212px`. |
| Inbox — filter interaction | `/threads?status=new&account=sales` | `1280×900` | Custom `htmx.min.js` calls `el.form.submit()` on select `change`, preserving both filters and updating URL; `Filter` button is redundant. |
| Thread detail (`new`, unassigned) | `/threads/692840bc-...`, `/threads/9f07e922-...` | `1280×900`, `375×812` | Email iframes blocked in Chrome (`credentialless` omits session cookie -> `303 /auth/login` -> CSP `frame-src 'self'` block); inline flex style blocked by CSP so action buttons stack vertically; `<textarea>` is ~160px wide because `style="width:100%"` is blocked by CSP; `Assignee` dropdown preselects `Dev Admin` even though thread is `Unassigned`. |
| Thread detail (`new` with Reopen & Merge banner) | `/threads/9f07e922-...` (`reopens_thread_id` set) | `1280×900`, `375×812` | `.banner-reopen` is visually distinct (`#eff6ff`), but link text includes a 36-char UUID and sits directly above `Start Progress` without explaining the difference between merging vs starting a new thread. |
| Thread detail (`in_progress`) | `/threads/692840bc-...` | `1280×900` | Shows `Complete Thread (Move to Archive)` in the same unstyled gray system button style. |
| Thread detail (`completed` & `spam`) | `/threads/b015a435-...`, `/threads/f8007c83-...` | `1280×900`, `375×812` | Still renders `Take Thread`, `Assignee` form, and `Send Reply via SMTP`. Sending a reply on a `spam` thread queues an SMTP email and assigns the thread (`Dev Admin (reply)`) while leaving status as `spam`. |
| Email body iframe (`credentialless` removed) | `/emails/{id}/body` | `1280×900` | Renders sanitized HTML and plain text cleanly once session cookie is sent; fixed `min-height: 220px` leaves ~140px blank space under short messages. |
| System Settings & Pre-Create User | `/settings` | `1280×900`, `375×812` | Unstyled inputs/fieldsets; `Users` and `AuditLogs` are loaded by the handler but never rendered; mobile labels wrap mid-phrase. |
| Error responses (`400`, `404`, `422`) | `POST /settings`, `GET /threads/0000...`, `POST /threads/{id}/reply` | `1280×900` | Bare plain-text browser responses (`invalid IANA display_timezone`, `thread not found`, `reply body must not be empty`); discards typed reply draft on 422. |
