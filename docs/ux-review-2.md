# MiniCRM UX & Visual Design Review #2 (Post-Remediation)

**Date:** 2026-10-19
**Reviewer:** UX & Product Design Expert (browser-verified against the live Podman stack + source inspection)
**Baseline:** [`docs/ux-review.md`](./ux-review.md) — findings `B1, B2, E1–E9, V1–V5, C1–C3, A1`

## Executive verdict

**The remediation closes the previous review.** Both deployment blockers are gone: email bodies render in Chromium (no `credentialless` iframe, no console/CSP errors on any page I loaded), and the UI now has a coherent design system — tokens, semantic status pills, button hierarchy, tables, focus rings, flash notices, inline validation, and styled 400/404/422/500 pages. Every baseline finding is fixed or is a documented, defensible trade-off. What remains is genuine polish, not foundation: a **500 on a malformed `?status=` query**, a **mobile-clipped empty state** hidden behind the table's horizontal scroll, **duplicate inline error rendering**, a few **small copy/consistency rough edges**, and a **latent multipart-parsing gap** in the settings handlers. No regressions were observed in the primary journeys.

**Overall rating: 4.0 / 5 in Chromium as deployed** (up from 1.5/5), and **4.0 / 5 for the underlying interaction structure** (up from 2.5/5). This is now a credible daily-use tool; the remaining Now/Next items are about robustness and mobile edge states, not usability.

---

## Scope and method

**Browser-verified (live stack, headless Google Chrome 155 via the DevTools Protocol, screenshots + computed styles + console/network capture).**
- Session: browser OIDC login as `Dev Admin <admin@example.com>` (both accounts).
- Desktop `1280×900` and mobile `375×812` (mobile emulation flag on).
- Screens: inbox (all statuses, filters, empty states), thread detail (`new`, `in_progress`, `completed`, `spam`, and a real **follow-up** thread with `reopens_thread_id`), reply composer, `/settings`, styled `400`/`404`/`422`/`500` pages, `/emails/{id}/body` iframe rendering, attachment download headers.
- Interactions: filter auto-submit via `change`, `?assignee=me|unassigned|<uuid>|bogus`, transition + assign + spam-cancel/complete-accept through the new `data-confirm` dialogs, Assign-to-me, reply send (verified in Mailpit), whitespace-only reply (422), reply to a completed thread (422 + "Draft not sent"), Reopen & Merge execution (temp thread deleted, original reopened), flash dismissal, iframe Expand/Collapse, resize affordance, keyboard Tab focus ring, `noscript` fallback, `prefers-color-scheme: dark`, horizontal-overflow measurement (`documentElement.scrollWidth`).
- Cross-checks: `curl` with the session cookie (status codes + header inspection), `psql` for resulting DB state, `doveadm save` to inject a follow-up email into the IMAP INBOX so the Reopen & Merge banner could be exercised with real data.

**Source-reviewed:** `internal/web/views/views.go`, `internal/web/static/app.css`, `internal/web/static/app.js`, `internal/web/security_headers.go`, `internal/web/handlers_threads.go`, `internal/web/handlers_settings.go`, `internal/web/router.go`.

**Not verified / limits**
- **High-volume inbox and the 100-row cap:** the dev stack contains only 4–5 threads, so the `Limit: 100` truncation notice ("first 100; narrow the filters…") and long-list performance were not rendered. Source-verified only (`handlers_threads.go:35,143`, `views.go` `Truncated`).
- **Single-account installations:** only the multi-account Dev Admin was exercised; `ShowAccountUI` suppression is source-verified, not browser-verified.
- **Real SMTP/IMAP failure paths:** only the queue path (success + Mailpit delivery) was exercised; the "SMTP failed" error copy is source-only.
- **Drag-resizing the iframe by hand:** `resize: vertical` computes correctly and the handle renders; I confirmed the Expand toggle rather than simulating a drag gesture.
- Timezone validation was confirmed with `curl` (browser `FormData` POSTs are affected by finding N4, see below).

---

## Baseline scorecard

| # | Finding (v1) | Status | Runtime evidence |
|---|---|---|---|
| **B1** | Email iframes show Chrome broken-frame icon (`credentialless` strips session) | **Fixed** | All `.email-frame` elements now have `credentialless: false`; message text ("Hello sales team, …") renders in every frame; Network capture shows zero failed `/emails/*/body` requests; console clean. |
| **B2** | CSP `style-src 'self'` blocks inline styles | **Fixed** | No inline `style=` attributes remain; zero CSP violation entries on any page. Actions bar computes `display:flex` (buttons horizontal), reply textarea is full-width, navbar is one row. |
| **E1** | Bare-text error pages destroy input; silent successes | **Fixed** | `404` → styled card ("Thread not found", "Back to threads"); `400` invalid thread/id; `422` re-renders the thread page with inline errors; `500` styled. Flash banners verified for reply, transition, assignment, settings, user creation, reopen-merge; whitespace-only reply shows inline error and keeps the page. |
| **E2** | Unstyled/stacked workflow buttons, missing confirmation, dev jargon | **Fixed** | `Start working` / `Mark complete` primary blue, `Mark as spam` danger; `(new -> in-progress)`, `(Move to Junk)`, `Move to Archive`, `Send Reply via SMTP` gone. Native confirm dialogs fire for both destructive transitions; **Cancel keeps status `New`**, **OK completes the thread**. |
| **E3** | Terminal threads still accept replies/reassignment; replies leave status stuck | **Fixed** | Spam thread: no reply composer, no assignee form, helper "This thread is marked as spam. Replies and assignment are disabled." Completed thread: reply composer hidden, helper explains follow-up behaviour; forged reply POST returns `422` with "Draft not sent". Server blocks both (`handlers_threads.go:489-503`). |
| **E4** | Reply composer lacks From/To, side effects, jump link | **Fixed** | Reply card shows `From Sales <sales@example.com>` / `To Dave Prospect <…>`; `new` thread shows "Sending a reply assigns this thread to you and moves it to In progress."; header `Reply` link scrolls to `#reply` (verified `scrollY:1192`, reply top `91px`). |
| **E5** | Contradictory assignee controls | **Fixed** | Unassigned threads show a single select whose selected option is the **disabled** `-- Unassigned --` placeholder plus one `Assign to me` button; no phantom preselected user. |
| **E6** | Reopen banner leaks UUID, under-explains merge | **Fixed** | Live follow-up thread: "This email replies to the completed thread *Cannot log in to my account*, completed 2026-10-19 20:30:39 UTC by Dev Admin." No UUID in DOM (`uuid visible: null`); explains "this temporary thread is removed" and offers "Or keep this email as a separate new thread…". Merging worked: URL → original thread, flash, status `In progress`, temp thread row count `0`. |
| **E7** | No assignee filter, redundant Filter button, silent 100-row cap | **Fixed** | Assignee filter `Anyone / Assigned to me / Unassigned / <user>`; URL reflects selection; counts verified (`me` → 3, `unassigned` → 1). `Filter` button now wrapped in `<noscript>` (visible only with JS disabled). Results line "Showing N threads" with truncation suffix. |
| **E8** | Settings loads users/audit but renders neither | **Fixed** | `/settings` renders **Mailbox settings**, **Team members** (name/email/role/account access/Active pill), **Add team member**, **Recent activity** (36 rows, humanised labels, actor, target). |
| **E9** | Fixed 220px iframes waste space / trap scroll | **Fixed (trade-off accepted)** | 170px frame + native `resize: vertical` handle + `Expand`→`Collapse` toggle (170→640px verified). Scripted auto-height is impossible because the iframe sandbox omits `allow-scripts`; the toggle+resize combination is an acceptable substitute. |
| **V1** | Browser-default tables/buttons/inputs | **Fixed** | `body` 15px system-ui; buttons have real padding/radius/weight; tables `border-collapse: collapse`, `9.6px 11.2px` cells, uppercase 12.5px headers, row dividers + hover; links tokenised. |
| **V2** | No status colour differentiation | **Fixed** | `New` blue (`rgb(219,234,254)`/`rgb(30,64,175)`), `In progress` amber, `Completed` green, `Spam` red; `.badge-account` softened to slate. All four visible in one inbox screenshot. |
| **V3** | Mobile layout breaks / header clipped | **Fixed** | At 375px `documentElement.scrollWidth === 375` on inbox, detail and settings; table scrolls inside `.table-wrap` (`overflow-x:auto`, `clientWidth 343 / scrollWidth 756`); navbar wraps to two tidy rows; settings form grid collapses to one column. Residual polish noted as N2. |
| **V4** | Inbound/outbound messages identical; raw bytes | **Fixed** | `Customer` vs `Team reply` badges; outbound has blue left border + `#fbfdff` tint; sizes humanised (`43 B`). |
| **V5** | Pipe-separated run-on metadata | **Fixed** | Nav and header use flex/gap lists and labelled metadata; no literal `|` separators remain. |
| **C1** | Status naming drift | **Fixed** | Single `FormatStatus` (`New`, `In progress`, `Completed`, `Spam`) used in filters, pills, buttons, flashes and timeline sentences. |
| **C2** | Raw enums/IDs leak | **Fixed** | `(reply)`/`(default)` → `assigned via reply`/`auto-assigned`; timeline reads "Dev Admin changed status from New to In progress."; account slugs replaced by display names. Residual copy nit in N6. |
| **C3** | Generic `<title>`, heading drift | **Fixed** | Titles are `Threads — MiniCRM`, `Settings — MiniCRM`, `Pricing for 50 seats — MiniCRM`, `Thread not found — MiniCRM`; h1 now "Threads"; "Pre-Create User" → "Add team member" / "Create user". |
| **A1** | A11y gaps | **Fixed (minor residuals)** | Reply textarea/attachment have `<label for>`; iframes have descriptive `title`; `<nav aria-label="Main">` + `aria-current="page"`; all `<th scope="col">`; `:focus-visible` 3px blue ring verified on Tab; flash `role="status"`, errors `role="alert"`. Residuals: no skip link, dialog is native `confirm()` (see N7). |

*(No baseline finding regressed.)*

---

## Strengths

- **A real design system, applied consistently.** One token block (`app.css:1-19`), one button vocabulary (`.btn-primary/secondary/danger/ghost/small`), shared badges, cards, fields, table and timeline patterns across inbox, detail and settings — a large jump from the prototype.
- **Feedback loops are complete.** Flash banners on every mutation, inline field errors that preserve context, PRG redirects, and a genuinely thoughtful **"Draft not sent"** card for terminal-thread replies (verified content and preserved draft in the 422 HTML).
- **Terminal-state guardrails are layered.** Spam hides reply/assign controls *and* rejects forged POSTs; completed threads reject replies server-side and explain the follow-up model; destructive transitions confirm with explanatory copy ("…move its messages to the Archive folder?").
- **Human-readable history.** `FormatThreadEventSummary` and `FormatAuditEvent` turn raw enums into sentences with actor names, and audit/thread tables are finally rendered — this is the feature that makes the CRM legible.
- **Security-conscious rendering without UX cost.** Sanitised bodies inside a sandboxed, titled iframe with its own CSP; email content renders perfectly in Chrome and no console errors appear on any page.
- **Progressive enhancement done right.** Filters work without JS via a `<noscript>` Filter button, auto-submit with JS, iframes still render and remain resizable with JS disabled, flash dismissal and expansion are pure enhancements.
- **Responsive containment.** `overflow-x` is scoped to tables/iframes so pages never overflow at 375px, and the expanded frame is capped at 720px.

---

## New findings

### Ease of use

**N1. A malformed `status` filter value returns a styled 500 page (Medium).**
- *Observed:* `/threads?status=bogus` → HTTP 500, "Inbox unavailable — We could not load the conversation threads." Same for any non-enum string. The raw query value is cast to `sqlc.ThreadStatus` and sent to PostgreSQL, which rejects it (`handlers_threads.go:167-172`).
- *Impact:* A stale bookmark, hand-edited URL or a future filter value name change surfaces an error page instead of the inbox; it also makes `status` a trivially triggerable 500.
- *Fix:* Whitelist against the four known statuses and ignore unknown values (fall back to "All statuses"), exactly as the code already does for an invalid `assignee` UUID.

**N2. The empty inbox state is clipped on mobile (Medium-Low).**
- *Observed:* At 375px, `?status=spam&account=sales` shows "No threads match the…" cut off at the viewport edge; the colspan cell sits inside `.threads-table { min-width: 32rem }` inside the horizontal scroll container, so the message is only fully readable after scrolling the table sideways (screenshot `empty-375`).
- *Impact:* The one moment the user needs a clear "nothing here, adjust filters" message, it looks broken.
- *Fix:* When `len .Threads == 0`, render the empty message as a card *outside* `.table-wrap` (or set the empty cell to `white-space: normal; min-width: auto` and hide the table when empty).

**N3. Reply validation errors render twice (Low).**
- *Observed:* A whitespace-only reply (`422`) renders the same `<li>` in two `.form-errors` blocks — one in `.thread-header-card`, one inside `.reply-card` (verified DOM: parents `card thread-header-card` and `card reply-card`).
- *Impact:* Noisy, and on scroll the operator can see the same error twice with different surrounding context.
- *Fix:* Render the header-level error block only for transition/assign errors, or dedupe when the only error is a reply error (which already renders next to the field).

**N4. Multipart submissions to settings endpoints are silently ignored (Low; latent).**
- *Observed:* `parseBoundedForm` calls `r.ParseForm()`, which does **not** populate fields for `multipart/form-data`. A browser `FormData` POST to `/settings` with `display_timezone=Europe/Paris` returned a success redirect and **saved `UTC`** (confirmed in DB + audit `new_value`); `POST /settings/users` reported "email and display_name are required" for a form that supplied both. The shipped UI submits urlencoded (works), so users are unaffected today.
- *Impact:* Any future enctype change, API client, or test harness silently gets defaults instead of an error — the dangerous failure mode is silent data loss, not a visible bug.
- *Fix:* In settings handlers, call `r.ParseMultipartForm(...)` when the content type is multipart (as `HandleReplyThread` already does), or reject non-urlencoded bodies with a 400.

**N5. No favicon; every page logs a 404 (Low).**
- *Observed:* `GET /favicon.ico` → 404 on all pages; it is the only console entry on clean pages.
- *Fix:* Ship a small `/static/favicon.svg` + `<link rel="icon">`, or add an explicit 204 route.

### Visual design

**N6. Small copy details undercut the otherwise polished history (Low).**
- *Observed:* (a) Timeline shows "Dev Admin assigned this thread to Dev Admin." when actor == assignee; (b) after Reopen & Merge the merged timeline contains two "The customer opened this thread." entries (both threads' events are concatenated) with no indication of the merge boundary.
- *Impact:* Reads like a data glitch in an otherwise trustworthy audit trail.
- *Fix:* Use "…assigned this thread to themselves" when `actor == assignee`; prefix the merged follow-up's events (e.g., a "Follow-up thread" subheading) or suppress the duplicate "opened" line.

**N7. Muted text on page background sits just below AA (Low).**
- *Observed:* `--muted` `#64748b` computes 4.76:1 on white cards (passes) but ~4.47:1 on `--bg` `#f6f8fb`, used for `.results-count` and other muted text on the page surface.
- *Impact:* Marginal legibility for small 12.5–13.4px secondary text, especially for low-vision users.
- *Fix:* Darken `--muted` one step (e.g. `#5b6b7d`) or only use it on card surfaces.

**N8. User-status pills reuse workflow-status classes (Low).**
- *Observed:* "Active" uses `.badge-completed` (green) and "Inactive" uses `.badge-spam` (red) in `views.go` settings table.
- *Impact:* Any future tuning of workflow colours silently changes the meaning of user status, and the two concepts are now visually coupled in code.
- *Fix:* Add neutral `.badge-active` / `.badge-inactive` (or semantic aliases) rather than borrowing status classes.

### Consistency & accessibility

**N9. Completed threads can still be reassigned (Low).**
- *Observed:* The completed thread renders the `Assignee` select + `Assign` and an `Assign to me` button next to "This thread is completed…", while spam threads explicitly block assignment server-side (`handlers_threads.go:427-431`) and hide the controls. Assignment was verified to succeed on a completed thread.
- *Impact:* Mild inconsistency in what "terminal" means; also contradicts the helper message that frames the thread as closed.
- *Fix:* Either hide/disable assignment on completed threads to match spam, or add one line to the helper explaining that assignment is still allowed for follow-up ownership.

**N10. Native `confirm()` is functional but not part of the design system (Low).**
- *Observed:* `Mark as spam` / `Mark complete` use `window.confirm()` (`app.js:10-16`). It is keyboard-accessible and announced by screen readers, but it is browser-chrome styled, blocks the event loop, and cannot be themed; it also cannot explain consequences as richly as the surrounding UI does.
- *Impact:* Acceptable today; a visual/copy mismatch on the most consequential actions.
- *Fix (Later):* Replace with an in-page `<dialog>` confirmation that repeats the consequence sentence and an explicit "Move to Junk/Archive" label.

**N11. No dark theme; forced light rendering under `prefers-color-scheme: dark` (Later/optional).**
- *Observed:* Emulating dark mode leaves `body` at `rgb(246,248,251)`; there is no `color-scheme` meta and no dark tokens.
- *Impact:* Not broken, but users on dark-mode OSes get a bright surface; the app also doesn't opt out explicitly, so form controls may render dark-on-light inconsistently in some browsers.
- *Fix:* Add `<meta name="color-scheme" content="light">` now; a `prefers-color-scheme` token set is a reasonable Later item.

---

## Prioritized action plan

**Now (small, high-confidence)**
1. **N1** — whitelist the `status` query value; unknown → "All statuses" (prevents a user-triggerable 500).
2. **N2** — move the empty-state message outside the horizontally scrolling table.
3. **N3** — render reply errors once (reply card), not in both the header card and the reply card.
4. **N5** — add a favicon/link to remove the only console noise on every page.

**Next**
5. **N4** — parse (or reject) multipart bodies in `/settings` and `/settings/users`; add a regression test that posts multipart and expects a 400 or the submitted values.
6. **N6** — "assigned this thread to themselves"; distinguish merged follow-up history.
7. **N9** — align completed-thread assignment with the terminal-state story.
8. **N7/N8** — contrast bump for `--muted` on `--bg`; dedicated active/inactive badge classes.

**Later**
9. **N10** — replace native `confirm()` with a styled `<dialog>`.
10. **High-volume inbox** — verify the 100-row truncation path and consider pagination/"load more" (source exists, runtime unverified).
11. Optional dark theme (`color-scheme` meta now, tokens later) and triage keyboard shortcuts (`j/k`, `r`).

---

## Appendix: screens and states inspected in Chrome

| Screen / state | Trigger | Viewport(s) | Key observation |
|---|---|---|---|
| Inbox (4 statuses) | `/threads` | 1280×900 | Semantic pills for New/In progress/Completed/Spam; two-line customer cell; clean table; no overflow. |
| Inbox filters | `?status=completed`, `?assignee=me`, `?assignee=unassigned`, `?assignee=<unknown uuid>`, `?status=spam&account=sales`, `?status=bogus` | 1280×900 | Counts correct (1/3/1/0/0); unknown assignee falls back to "Anyone"; **`?status=bogus` → 500** (N1). |
| Empty state | `?status=spam&account=sales` | 375×812 | "Showing 0 threads"; message clipped by table scroll (N2). |
| Thread detail `new` (assigned, Sales) | `/threads/692840bc-…` | 1280×900, 375×812 | From/To reply context; helper about auto-assign; `Reply` jump link; Expand→Collapse 170→640px; attachment `43 B`. |
| Thread detail `in_progress` (after reply) | same thread | both | Reply sent → flash "Reply queued for delivery.", status pill `In progress`, `Mark complete` primary. |
| Thread detail `completed` | `/threads/b015a435-…` | both | Reply composer absent; helper explains follow-up; assignment still available (N9); "Draft not sent" card verified from 422 HTML. |
| Thread detail `spam` | `/threads/f8007c83-…` | 1280×900 | Reply and assignment controls absent; helper states they are disabled; forged reply → 422. |
| Follow-up thread + Reopen & Merge | `/threads/c4415b7f-…` (real IMAP follow-up) | 1280×900 | Banner without UUIDs, completion date/actor, consequence copy, secondary option; merge → original thread reopened `In progress`, temp thread deleted. |
| Confirm dialogs | `Mark as spam` (cancel), `Mark complete` (accept) | 1280×900 | Explanatory messages; cancel preserves `New`; accept completes and flashes. |
| Settings | `/settings` | 1280×900, 375×812 | Mailbox settings, Team members, Add team member (grid + checkboxes), Recent activity (36 rows). |
| Validation | whitespace reply, invalid timezone (`curl`), duplicate user | 1280×900 | Duplicate inline error (N3); timezone 400 preserves input; errors `role="alert"`. |
| Error pages | 400 `/threads/not-a-uuid`, 404 random UUID, 422, 500 | 1280×900 | All styled with status code, heading, message, "Back to threads"; per-page titles. |
| Email body iframes | `/emails/{id}/body` | both | Rendered content in Chrome; console clean; no `credentialless`; sandbox + `title` present. |
| Keyboard | 4× Tab on inbox | 1280×900 | Visible 3px blue `:focus-visible` ring on `Log out`. |
| No-JS | scripts disabled | 1280×900 | `<noscript>` Filter button visible; inbox and iframes still render; CSS resize still available. |
| Dark-mode emulation | `prefers-color-scheme: dark` | 1280×900 | Stays light; no broken colours; no `color-scheme` meta (N11). |
| Attachment download | `/attachments/{id}/download` | — | `Content-Disposition: attachment`, `CSP: default-src 'none'; sandbox`, `nosniff`. |
