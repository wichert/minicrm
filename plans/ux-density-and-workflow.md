# Plan — UX Density, Navigation & Workflow Fit (UX Review #3)

**Status:** Approved for hand-off (reviewed in Plannotator, 2026-10-28) — not yet implemented
**Date:** 2026-10-28
**Author:** UX review (browser-verified against the running dev stack)
**Companion evidence:** `docs/ux-review-3/` (screenshots, listed in [Appendix A](#appendix-a-evidence))
**Baseline reviews:** [`docs/ux-review.md`](../docs/ux-review.md), [`docs/ux-review-2.md`](../docs/ux-review-2.md) (all prior findings closed)

---

## 1. Goal & design principles

The app is functionally complete and visually consistent. This round is about **content density and workflow fit**: the current screens spend too much of the viewport on chrome, and the navigation model does not match how the tool is actually used.

### Implementation guidelines for this plan

- **Do not write tests that assert the presence of a CSS rule or a specific style value.** Rendering is verified visually (screenshots at the listed viewports, checked by a human). Automated visual/geometry assertions are **out of scope** — no pixel, layout or `getComputedStyle` assertions, and no screenshot-diff baselines. Tests cover *behaviour and data* (queries, handlers, workflow transitions, undo/confirm flows), not appearance.
- Keep behaviour progressive: the app must work without JS (`<noscript>` submit buttons exist), and every navigation target stays a real `<a href>`.
- Prefer template + CSS over new JavaScript; when JS is needed, keep it in `internal/web/static/app.js` next to the existing progressive-enhancement code.

Design principles applied (from the brief):

1. **Maximise space for content.** Navigation and filters are available when needed, but must not permanently occupy large areas. A thread list's job is to show threads.
2. **Hiding unimportant information on small screens is fine.** Mobile should show fewer fields more legibly rather than all fields cramped.
3. **Use common interaction patterns** to reduce the learning curve (email-client master/detail, contextual selection bars, sticky reply composer, `j/k` keyboard navigation).
4. **Optimise for the two real usage modes:**
   - **Triage new messages** — a stream of `new` threads; act fast (open → start working → reply → complete).
   - **Act on old messages with no recent attention** — currently *not representable in the UI at all* (see F-1): the list is sorted by last activity, so old-but-quiet threads sink, and the age badge is suppressed outside `new`.

---

## 2. Measurements (baseline)

Captured headless Chromium, session `Dev Admin <admin@example.com>`, seeded dev data (43 open threads).

| Metric | Desktop 1920 | Desktop 1440 | Desktop 1024 | Mobile 390 |
|---|---|---|---|---|
| Viewport height before the first thread row | ~470 px | **370 px** (41% of 900) | ~300 px | **~900 px** (full screen, 0 threads visible) |
| Threads fully visible on first screen | 5 | 5 | 4 | **0** (the 3rd card is cut off mid-row; ~2.5 cards are partially painted) |
| Row height | ~110 px | ~110 px | ~110 px | ~190 px |
| Horizontal space used by the list | 1011 px of 1920 (53%) | 1011 px of 1440 | ~1011 px (overflow: `.actions-cell` clipped) | full width |
| Controls rendered above the list | 6 blocks | 6 blocks | 6 blocks | 6 blocks (≈ 620 px of chrome) |
| Bulk-action bar when nothing is selected | ~90 px of disabled controls | same | same | ~120 px of disabled controls |

"6 blocks" = dark navbar → `<h1>Threads</h1>` + Refresh → 6-tab strip → filter form (search, assignee, account) → bulk-action bar + instruction sentence → "Showing 43 threads" → table header.

Additional observations:

- At **1024 px** the table overflows its container: the `ACTIONS` header and the `⋯` triggers are clipped (`.container { max-width: 1120px }` + non-collapsing `customer-cell { width: 12rem }`). See `docs/ux-review-3/inbox-desktop-1024-selected.png`.
- At **1920 px** the list occupies the middle 53% of the screen; ~900 px is unused gutters.
- On **mobile**, the tab strip shows `Open / New / In progress / Completed` and silently hides `Spam` / `All`; the overflow fade only appears *after* the user has already dragged the strip.
- Rows carry three text lines each: `subject`/`snippet`, `name`/`email`, `waiting badge`/`absolute date`.

---

## 3. Findings

Severity: **P1** = blocks a stated usage mode or costs a large share of the viewport; **P2** = significant friction; **P3** = polish.

### F-1 (P1) — "Old messages with no recent attention" has no view, and the age badge is missing for `in_progress`

**Two defects, one root cause.**

**(a) No first-class view.** The tabs are *status*-based (`Open / New / In progress / Completed / Spam / All`). Nothing surfaces "in flight for a long time" — the list is ordered by `last_message_at DESC`, so an old thread that received a quick internal reply recently sorts to the top while a 3-week-old thread with no activity sinks out of view. A user working mode 2 must scan the `In progress` tab and reason about two timestamps per row.

**(b) The `Waiting` badge is computed only for `new`.** In `internal/web/handlers_threads.go:501-508`:

```go
waitSince := th.OpenedAt
if waitSince.IsZero() {
    waitSince = th.LastMessageAt
}
waitLabel, waitUrgency := "", ""
if th.Status == sqlc.ThreadStatusNew {
    waitLabel, waitUrgency = views.FormatWaitDuration(waitSince, now)
}
```

`internal/web/views/views.go:227-250` (`FormatWaitDuration`) then thresholds at `<1m / <1h / <4h / <24h / Nd`.

Consequence: an `in_progress` thread that has been open for three weeks renders **"Just now"** (the relative time of the latest message) and is visually indistinguishable from a thread handled a minute ago. In the seeded data, `Quote for the northwind rollout` shows "Just now" while `Renewal options after year one` shows "Waiting 20d" — purely because of status, not because of how long the thread has been open.

**Age of the open thread is the right prioritisation signal, regardless of who spoke last.** We cannot reliably infer "waiting on us" vs "waiting on the customer": the last word being ours does not mean the next action is not ours (e.g. *"I'll look into this and get back to you"* — the follow-up is ours, and the customer is in fact waiting). The product goal is to close open threads as fast as possible, so a thread's age should drive priority uniformly. `threads.opened_at` (=`MIN(emails.sent_at)`) is the correct basis for that; direction is only useful as *secondary context* ("customer replied 2d ago", "you replied 3d ago"), never as a gate that suppresses the age badge.

### F-2 (P1) — Chrome consumes 41% (desktop) to 100% (mobile) of the first screen

`internal/web/views/views.go:851-918` renders, above the list: a heading row (`<h1>Threads</h1>` + `Refresh`), the tab strip, the entire filter form (a labelled search input **plus a separate `Search` button**, assignee select, account select), the bulk form with the instructional copy *"Select conversations to assign, complete, or mark as spam."*, and the results count.

The `<h1>` duplicates the active navbar item. The bulk form is rendered in an all-disabled state that cannot be used until a row is selected. The results-count sentence sits on its own line.

### F-3 (P2) — Three-line rows halve list density

Per row (`inboxTablePartial`, `views.go:786-845`): subject + `Draft` + count pill on line 1, snippet on line 2, and in the customer cell name **and** email stacked (`views.go:800-808`); the age cell stacks the badge **and** the absolute timestamp (`views.go:822-827`). On mobile the card grows to five visual lines (`app.css:1938-2010`).

Nothing here is wrong information; it is simply all shown at the same visual weight, so the eye has no entry point and each row costs ~110 px instead of ~56 px.

### F-4 (P2) — Bulk actions are permanent chrome instead of a contextual mode

`views.go:897-912`: the bulk bar is always present. `app.css:1203-1204` hides `.bulk-actions-controls` only once JS sets `data-has-selection="false"`, but the wrapper, the mobile `Select all on page` checkbox, the instruction text, and the disabled `Assign selected / Complete selected / Mark selected as spam` buttons remain — ~90 px desktop, ~120 px mobile.

On mobile every card also carries a permanent checkbox (`td.select-cell`, `app.css:2003-2008`), which is the single largest source of clutter in the mobile list and shrinks the tappable card area.

### F-5 (P2) — Desktop ≥1280 px wastes space and loses list context (no master/detail)

Opening a thread navigates away from the list (`views.go:922`, full-page detail). There is no split view, so the dominant desktop workflow ("scan list → open → act → next") costs a full page load and a `← Back to threads` round-trip each cycle. At the same time 47% of a 1920 px viewport is empty gutter, and `.container { max-width: 1120px }` (`app.css:191-196`) caps the list while the detail page uses the same narrow column for reading email.

### F-6 (P2) — Mobile thread detail puts ~450 px of controls before the conversation

`views.go:922-1044` (`.card.thread-header-card` → `.actions-bar` at 982 → `.section-heading` at 1041): three full-width stacked buttons (`Start working`, `Mark complete`, `Mark as spam`), then the assignee select, then the `(auto-assigned)` note, then `Messages (2)`. In the screenshot (`docs/ux-review-3/thread-mobile-390.png`) the first message body starts below the fold on a 390×844 device. The reply composer is far further down — after reading a long thread it is many screens away, with no sticky affordance.

Also on the detail page: the From/To routing is rendered as a two-row `<dl class="reply-context">` (`views.go:1133-1136`) that repeats what the user already knows, and there is no keyboard send shortcut (no `Cmd/Ctrl+Enter` handler in `internal/web/static/app.js`).

### F-7 (P2) — Settings is unusable at 390 px

Team members and Recent activity are `<table>`s with 5 and 4 columns (`views.go` settings body). At 390 px, names wrap to two lines ("Dev / Admin"), "Account access" wraps to three lines, and the page grows to ~2300 px on desktop / ~3300 px on mobile with no internal navigation. See `docs/ux-review-3/settings-mobile-390.png`.

### F-8 (P3) — Inbox action menu is a subset of the detail page

The row `⋯` menu (`views.go:828-845`) offers only `Assign to me`, `Complete`, `Mark as spam`. It omits `Start working` and assigning to another user, so the inbox cannot perform every transition the detail page can. Consequence: two different, incomplete mental models of the same state machine.

### F-9 (P3) — Pagination and sorting are implicit

Only `ORDER BY last_message_at DESC` is available (`internal/db/sqlc/queries.go:378-435`). Paging is a bare `Older threads` link plus the prose `; more results available` in the count (`views.go:914-918`). There is no way to sort by "longest waiting" — the exact ordering mode 2 needs.

### F-10 (P3) — Keyboard navigation absent

`internal/web/static/app.js` has no list-navigation key handling (`j/k`, `Enter`, `x`, `/`). For an operator working a queue this is the largest remaining gap versus a mail client, and it is cheap: the row actions already exist as real `<form>`s.

---

## 4. Proposed improvements

Each workstream is independently shippable and independently verifiable. Recommended order is W1 → W2 → W3 → W4 → W5 → W6 (W2/W3 are pure template+CSS and can land together; W6+W7 are last).

Constraints that apply to every workstream (from `AGENTS.md`):

- No `updated_at` columns by default; all changes are audited via `audit_logs` / `thread_events`. The maintainer has confirmed this constraint **may be relaxed if a functional need arises** — functionality must not be built by reading audit data. Concretely for this plan: `opened_at` is already derivable from `emails` and needs no new column, but if a future workstream (e.g. W7 sorting/pagination at scale) is materially simpler with a denormalised column such as `threads.opened_at` or `threads.last_activity_direction`, propose it rather than reconstructing behaviour from the audit trail.
- Do not change the workflow state machine: `new → in_progress`, `new → spam`, `in_progress → completed`, `in_progress → spam`; `spam → in_progress` only via `RecoverSpam`; reopening only via `ReopenAndMerge`.
- Email HTML only ever inside the existing sandboxed iframe; keep `SecurityHeadersMiddleware`, `http.NewCrossOriginProtection()`, `MaxBytesReader`.
- Every thread/email/attachment access keeps its `HasAccountAccess` check; every new query keeps the `account_id` / `AllowedAccounts` filter.
- Timestamps stay UTC `TIMESTAMPTZ`, formatted with `display_timezone` (`views.FormatTimestamp`).

---

### W1 (P1) — Correct thread-age metric + a "Needs attention" view

**Why first:** it is the smallest change that makes the second stated usage mode possible, and it fixes a metric that is currently *misleading* (not merely missing). It needs no schema change and no new aggregates in its minimum form.

**Primary metric: thread age, not direction.** Replace the `status == new` gate in `handlers_threads.go:501-508` with one derivation used by both the inbox and the detail page:

| Status | `WaitLabel` | Source |
|---|---|---|
| `new` | `Waiting <age>` | `now - opened_at` (unchanged behaviour) |
| `in_progress` | `Open <age>` | `now - opened_at` — the badge now applies to in-progress threads too |
| `completed` / `spam` | — | plain relative time of last message (unchanged) |

`opened_at` (= `MIN(emails.sent_at)`, already returned by `ListThreads` as `COALESCE(stats.opened_at, t.last_message_at)`) is the right basis: the objective is to close open threads quickly, so a thread's age is the priority signal **regardless of who sent the last message**. Do not attempt to classify "waiting on us" vs "waiting on the customer": the last message being ours does not mean the next action is not ours (e.g. *"I'll look into this and get back to you"* — the customer is in fact still waiting), and a heuristic that hides the age badge in that case would deprioritise exactly the threads that need chasing.

**Tiers must reflect that customers do not expect an instant answer.** Replace the current minute/hour tiers (`<1m / <1h / <4h / <24h / Nd`, `views.go:227-250`) with three buckets:

| Bucket | Age | Copy | Class |
|---|---|---|---|
| Today | `opened_at` is today (calendar day in `display_timezone`) | `Today` | `wait-fresh` (neutral/muted) |
| This week | 1–7 days | `3d` / `This week` | `wait-warning` (amber) |
| Overdue | > 7 days | `12d` | `wait-urgent` (red) |

No sub-day granularity: an hour-old thread is "Today", not noise that draws attention. Bucket boundaries are computed in the user's `display_timezone`, consistent with `views.FormatTimestamp`. Keep the three existing CSS classes so no new visual language is introduced. Put the derivation in `internal/web/views/views.go` next to `FormatWaitDuration` so it is unit-testable without HTTP. Use one wording for both statuses — the current "Waiting" reads oddly on an in-progress thread, so prefer "Open 12d" (or keep "Waiting" everywhere if copy churn is undesirable; decide once and use it in inbox + detail).

**Secondary context (optional, additive only).** Direction is still useful as *informational* text, never as a gate. Add two lateral aggregates to `ListThreads` (`internal/db/sqlc/queries.go:378`) with no schema change:

- `last_inbound_at` — `MAX(e.sent_at) WHERE e.direction = 'inbound'`
- `last_outbound_at` — `MAX(e.sent_at) WHERE e.direction = 'outbound'`

Use them for two non-blocking hints: a `Customer replied 2d ago` line in the row's `title`/metadata, and (optional) a small `↑` marker meaning "the latest message is inbound". Consider a partial index `emails (thread_id, direction, sent_at DESC)` if the lateral cost shows up in `EXPLAIN ANALYZE` on realistic volume. **If the aggregates are skipped, W1 still works** — age alone delivers the feature. Do not let them change sort order, filtering, or badge presence.

**"Needs attention" view.** Add a new `status=attention` filter value (UI label **Needs attention**) meaning *open threads older than the threshold*:

```
status IN ('new', 'in_progress')
AND opened_at < now() - interval '<attention_age_days> days'
```

Using `opened_at` (not `last_message_at`) is deliberate: a thread that keeps receiving internal replies but never closes is still unattended. Make the threshold a `system_settings` value (default 2 days; mirrored in `internal/web/handlers_settings.go` like `Subject match window (days)`), not a hidden constant. Implement as an explicit SQL branch in `ListThreadsFilter` (e.g. `NeedsAttention bool` + `AttentionAge time.Duration`), **not** by post-filtering in the handler — the list is paged and post-filtering breaks the cursor.

Route handling must also accept `status=attention` without a 500 (the previous review found malformed `status` values returning 500; reuse the same validation path).

**Tab set** — ordered by how often the queue worker needs it, not by workflow state:

1. `Needs attention`, 2. `New`, 3. `Mine` — always visible, each with a **live count** pill. These are the three views an operator works from.

**`In progress` is removed as a primary tab.** In-progress threads are already present in all three primary views (`Needs attention` = old open threads, `New` = unanswered, `Mine` = my open queue), so a dedicated tab adds chrome without adding reach.

Everything else moves behind a compact `More ▾` overflow, where it costs no permanent horizontal space but stays reachable:

- `In progress` — kept in the overflow for the "show me only what is in flight" case
- `Completed`, `Spam` — audit and mis-spam recovery only; these have no bulk controls and show the absolute date instead of an age badge
- `All`

`Mine` = open threads assigned to me (`assignee=me`, open only). Keeping it as a tab lets us drop the assignee `<select>` for single-account users (see W2). Counts make the strip self-describing and let W2 remove the "Showing N threads" line. `Completed` / `Spam` render without the age badge and without bulk controls, as today.

**Pull to refresh + auto-refresh.** Three visible tabs fit at 390 px with room for the `More ▾` trigger (that is the point of demoting the rest). Auto-refresh must not disrupt work: pause while the user has a selection active, while a row menu is open, or while the document is hidden; re-fetch the current list fragment and preserve scroll position and focus.

Counting: one aggregate query per request scoped to `AllowedAccounts` (+ account filter), returning counts for the three primary tabs (attention, new, mine) and the overflow entries; do not run one query per tab.

**Acceptance**

- Every `new` and `in_progress` row shows an age badge derived from `opened_at`, using the Today / This week / Overdue buckets (no minute-level tiers); a thread last touched by us 3 weeks ago no longer shows "Just now".
- In-progress rows are prioritised identically to new rows of the same age (no direction-based suppression of the badge).
- An hour-old thread renders `Today` (no urgency colour), a 3-day-old thread `This week`, an 8-day-old thread `12d` in the urgent style.
- `Needs attention` returns exactly the open threads older than the configured threshold; changing the threshold in Settings takes effect on the next request.
- Counts in tabs match the rows each tab returns.
- No N+1 queries (assert via `pgx.QueryTracer` output in a test or manual run).

---

### W2 (P1/P2) — Reclaim the first screen (chrome, filters, rows)

Pure template + CSS work; no backend behaviour change. Target: **first thread row at ≤ 170 px on desktop**, **≤ 180 px on mobile**, **≥ 8 threads visible at 1440×900**.

1. **Remove the page heading block and the Refresh button entirely.** Drop `<h1>Threads</h1>` and `Refresh` (`views.go:852-855`); the navbar already indicates the section, so keep `<h1 class="sr-only">Threads</h1>` for a11y. Do **not** replace it with an icon button. Instead:
   - **Mobile:** native **pull-to-refresh** by turning the list container into a scroll container (`overscroll-behavior-y: contain`) and triggering the existing `?refresh` path on the `touchstart/touchmove` pull gesture; if the browser's own reload is acceptable on PWA, prefer that and add nothing (evaluate both, pick one and document it). This is the pattern phone users already expect.
   - **All sizes:** **auto-refresh** the list fragment on an interval (e.g. 60 s), paused while the tab is hidden (`document.visibilityState`), while text is selected, while a row menu or confirm dialog is open, or while an inline edit is pending. A subtle `Updated 2m ago` muted timestamp replaces the button and doubles as the manual affordance (click to refresh).
   - Keep revalidate-on-focus (back/forward cache) as the zero-cost fallback; no-JS users get a normal page reload.
2. **Collapse the filter form into one compact row.** `search` field with an icon and an inline `×` clear, assignee and account as compact selects, no separate `Search` button (submit on `Enter`, keep `hx-get`-style auto-submit already used by the selects via `data-hx-get`; keep a `<noscript>` submit). Hide the assignee select when the user has one team member and the account select when they have one account (`ShowAccountUI` already gates accounts; add the equivalent for assignee).
3. **Make the filter row sticky** under the navbar (`position: sticky; top: 0` on a wrapper, matching navbar height) with a hairline bottom border. Rows are 56–110 px; keeping filters reachable at zero scroll cost is what makes a dense list usable. Verify it does not break the mobile tab strip's horizontal scroll or the confirm `<dialog>`.
4. **Move "Showing N threads" into the tab strip / results area.** Prefer the per-tab counts from W1. If counts are not yet available, render `43 threads` right-aligned on the filter row as muted text.
5. **Two-line rows.**
   - Subject and snippet on **one line** at ≥ 900 px: `Subject — snippet` with the snippet truncated and muted; keep the current stacked layout below 900 px.
   - Single timestamp: show `WaitLabel` when present, else relative time; move the absolute timestamp to `title` (already present via `FullTime`). Show the absolute date only in `Completed` / `Spam` tabs.
   - Merge customer, account and assignee into one muted metadata line: `Lena Berg · Sales · Alex Agent`. Hide the customer email in the list entirely (it is on the detail page); keep it in `title`.
   - The badge uses the three W1 buckets only (`Today` muted, `This week` amber, `12d` red) — no minute/hour variants.
   - Typography hierarchy: subject 600 weight at body size, snippet muted, metadata 12–13 px muted. Status pill keeps its current size and colour.
6. **Widen the container for the list** to `max-width: min(1400px, 100%)` (or a `clamp()`); keep the current 1120 px column for the detail page's message reading width. Add a container query or a `min-width: 1280px` breakpoint for the one-line row layout.
7. **Fix 1024 px overflow** (`docs/ux-review-3/inbox-desktop-1024-selected.png`): `.customer-cell` fixed `width: 12rem` (`app.css:1398-1400`) must become `max-width` with `text-overflow: ellipsis`, and `.actions-cell` must not be allowed to clip. Add a regression test in `e2e/` asserting `document.documentElement.scrollWidth === clientWidth` at 1024, 1280 and 1440.

Expected result: ~56 px rows, ~150 px of chrome → 8–10 threads at 1440×900.

**Acceptance**

- No `Refresh` button exists on the list. On a phone a pull gesture refreshes (or the native PWA reload does); on desktop the muted `Updated 2m ago` affordance refreshes and auto-refresh keeps it current without stealing focus or resetting scroll.
- The three primary tabs (`Needs attention`, `New`, `Mine`) fit at 390 px without horizontal scrolling; `In progress`, `Completed`, `Spam` and `All` are reachable from a `More ▾` menu, and each still returns correct results.
- At 1440×900 the first row is visible above 200 px from the top of the content area.
- Every row is exactly two text lines at ≥ 900 px and informative at 390 px.
- No horizontal overflow at 1024/1280/1440/1920 (measured, not eyeballed).
- Sticky filter row stays legible over rows and does not overlap the bulk bar (W3).
- Auto-refresh never interrupts: no re-render while a row menu, confirm dialog or active selection is present, and `document.visibilityState === "hidden"` pauses the timer.
- All previously visible information is still reachable (hover title for absolute time and customer email, detail page for the rest).

---

### W3 (P2) — Contextual bulk actions + mobile selection mode

1. **Render the bulk bar only while selection is non-empty — CSS-driven.** Keep the markup and `display: none` / `visibility: hidden` the whole `#bulk-spam-form` until the container carries `[data-has-selection="true"]`; the existing JS already sets that attribute, so this is a no-new-JS change. **Animation is done in CSS** (transition/`@keyframes` on transform + opacity, wrapped in `@media (prefers-reduced-motion: reduce)`), not by mounting DOM in JS. Do not add a JS-rendered variant.
2. **Desktop:** floating bar pinned bottom-centre (`position: fixed`), showing `N selected` + `Assign ▾` / `Complete` / `Spam` / `Clear`. Keep the existing form actions (`/threads/bulk-assign`, `/threads/bulk-complete`, `/threads/bulk-spam`) and the existing `data-confirm` dialogs — do not change the endpoints.
3. **Mobile:** fixed bottom sheet above `env(safe-area-inset-bottom)`. Enter selection mode by **long-press on a card** or via a `Select` item in the row menu; hide per-card checkboxes (`td.select-cell`) outside selection mode. `Select all on page` lives inside the bar.
4. **Remove the instructional sentence** (`views.go:900`). The controls now only appear when they are actionable, so the instruction is redundant; keep an `aria-live` announcement of the selection count (already present via `data-selected-count`).
5. Ensure the bar never covers the last row: add bottom padding to the list equal to the bar height while active.

**Acceptance**

- With nothing selected, zero vertical space is spent on bulk controls on any viewport.
- Selecting one row reveals the bar on desktop (fixed, bottom-centre) and presents a bottom sheet on mobile.
- Bulk actions still work end-to-end for assign/complete/spam, including confirmation dialogs and the `return_to` behaviour.
- Checkboxes are absent from mobile cards until selection mode is entered; long-press and the row menu both enter it.
- Screen readers announce selection changes; the bar is dismissible.
- The reveal is animated in CSS only (transform/opacity) and respects `prefers-reduced-motion`; no DOM is created or destroyed by JS to show the bar.

---

### W4 (P2) — Master–detail on wide viewports

Targets desktop ≥ 1280 px. The split↔single switch is **CSS-triggered** at that breakpoint, not a server-side viewport hint: the server always renders the same document, and CSS decides which pane is visible. A resize in either direction therefore switches layout immediately, with no reload, no JS, and no state loss.

1. **Layout:** two panes — list `clamp(340px, 32vw, 460px)` + detail pane filling the rest. List pane keeps all W1/W2/W3 behaviour (tabs, filters, selection) but in a single column with the two-line row content.
2. **Routing without breaking links:** keep `/threads/{id}` as the canonical detail route (deep links, bookmarks, e2e tests) — **no new query parameter for layout**. Rows keep their real `<a href="/threads/{id}">`, so the no-JS path is unchanged and no client-side routing is introduced. `HandleViewThread` additionally renders the inbox list beside the thread in the same document (see 2a). The server never decides the layout; it only decides whether the list markup is worth emitting at all.
   1. **What the server renders:** the detail document contains a `<div class="split-layout" data-split-layout>` wrapper holding `[data-split-list]` (the same `inboxTablePartial` + toolbar/filters markup the inbox uses, already shared) and `[data-split-detail]` (the existing `threadDetailBody`). Above 1280 px CSS lays them out side by side and shows both; at or below 1280 px `[data-split-list]` is hidden (`display: none`, plus `inert` per 2c) and the detail pane is the full-width single-pane page it is today. The single media query, not a request-time hint, is the entire mechanism.
   2. **Avoiding the cost on every detail load:** the list is a rendering-cost optimisation only, never a layout switch — so it is emitted by default, and a bare `?nolist=1` lets a caller skip it. That escape hatch exists for the e2e single-pane assertion and for links followed from a context known to be narrow; it must never be set automatically by JS, and omitting the list must leave an otherwise identical single-pane page. Keep the rendered rows to the current page size; do not paginate the pane server-side beyond `NextURL`.
   3. **Hidden-pane hygiene:** the collapsed list stays in the DOM but must not be reachable when hidden — set `inert` (or `aria-hidden` + `tabindex="-1"` fallback) on `[data-split-list]` below the breakpoint via a `matchMedia("(min-width: 1280px)")` listener in app.js, toggled on `change` so an in-place resize updates it too. Keyboard and screen-reader users must never tab into an invisible list; `display: none` alone is not sufficient because the pane could otherwise remain in the tab order during a transition.
3. **Detail pane:** reuse `threadDetailBody` unchanged except for container width; add a `Back to threads` that is only shown in single-pane mode (CSS-driven: hidden ≥1280 px).
4. **Preserve list state** (filters, scroll position) when navigating between threads: because the list is re-rendered per detail load, keep the filter query string in the split-pane links (append the current inbox query to `/threads/{id}` via a `return_to`-style parameter already supported by `BackURL`), and restore the list scroll offset from `sessionStorage` keyed on that query string. Opening a thread must not reset the pane's scroll position.
5. **Resize correctness is a hard acceptance criterion,** not a deferred nice-to-have: resizing from 1440 → 1024 → 1440 in one page session must show split → single → split without a reload, without losing the entered reply draft, and without re-running list fetches. No `orientationchange`/`resize` JS is needed for the layout itself; only the `inert` toggling and scroll restore use JS.
6. **Deferred-until-measured:** if the double-render cost or code complexity is judged too high on review, ship a narrower first step — a persistent list column at ≥ 1600 px only — and treat full split view as a follow-up. State the chosen option in the PR description. The CSS-triggered switch requirement is unchanged by that fallback.

**Acceptance**

- At 1440 and 1920 the list and the open thread are visible simultaneously; opening a second thread does not lose the list.
- Below 1280 the behaviour is byte-for-byte the current single-pane flow.
- **Resizing across 1280 px in either direction swaps split and single layouts in place — no reload, no navigation, draft preserved, and the hidden pane is not focusable.**
- Deep links and `← Back to threads` still work; `BackURL`/`resolveInboxBackURL` semantics unchanged.
- No regression in the Reopen & Merge banner, draft restore, attachment rendering or iframe isolation.
- The split document still renders correctly with JavaScript disabled (both panes in the DOM, CSS alone choosing which is visible; the hidden pane is simply unreachable).

---

### W5 (P2) — Mobile thread detail: fewer controls before content, reply always reachable

1. **Compact action row.** Primary `Start working` (or `Mark complete` when `in_progress`) as a full-width button; `Complete` and `Spam` as icon+label secondary/danger buttons on the same row; assignee becomes an inline chip/select on that row. Removes ~250 px above `Messages`.
2. **Collapse the routing block** (`<dl class="reply-context">`, `views.go:1133-1136`) to one line `Sales → Lena Berg` with a chevron that expands the full From/To detail. Keep the full detail in the DOM for screen readers or as an expanded `<details>`.
3. **Sticky reply affordance.** A fixed bottom `Reply` bar on mobile that opens the existing composer in a full-height sheet (or scrolls to and focuses it, expanding to full height). Do not build a second composer — reuse the existing form, `Save draft` / `Discard draft` / attachment input, and draft-persistence behaviour.
4. **Keyboard/gesture send:** `Cmd/Ctrl+Enter` sends when the composer has focus and the message is non-empty; keep the explicit `Send reply` button. Confirm before send only if that matches existing behaviour (currently `data-confirm` is used for transitions, not for send — keep it that way).
5. Hide the redundant `<email>` under the sender name on mobile; it is in the summary line already.

**Acceptance**

- On 390×844 the first message body is visible without scrolling for a 2-message thread.
- The reply composer is reachable in one tap from anywhere in the thread; sending still stores the email, queues `append_to_sent`, and assigns the thread per existing rules.
- Drafts survive sheet open/close and page reload as today.
- `Cmd/Ctrl+Enter` sends exactly once; no double-submit.
- No change to iframe sandbox attributes, sanitization or remote-image blocking.

---

### W6 (P2) — Settings: card layout on mobile, sectioned page

1. Below 768 px, render **Team members** and **Recent activity** as stacked cards instead of tables: line 1 name + role badge + status, line 2 muted email, line 3 account-access chips. Implement as a CSS-only transformation of the existing `<table>` (as already done for the inbox at `app.css:1938`), or as a parallel mobile markup block — prefer CSS-only to keep one source of truth.
2. Truncate **Recent activity** to the latest 5 on mobile and latest 10 on desktop, with a `View full activity` link to a dedicated admin route (new `GET /settings/activity`, paginated, admin-only, audited read via the existing tracer). If a new route is out of scope, keep the current list but paginate it in place.
3. Add a **sticky in-page section nav** (`Routing & display` / `Folders` / `Team` / `Activity`) so the ~2300 px page is navigable. Anchor links only; no new state.
4. Do not change any field semantics, `system_settings` keys, or audit events.

**Acceptance**

- At 390 px no settings content wraps to more than three lines per card, and "Account access" is readable as chips.
- The activity list is bounded in height; full history remains reachable.
- All existing settings still save, and each save still produces the same audit event.
- No horizontal overflow at 390 px.

---

### W7 (P3) — Inbox parity, sorting, pagination, keyboard

Small, independent items; can be split across contributors.

1. **Row menu parity** (`views.go:828-845`): add `Start working` (for `new`) and `Assign to…` (a nested list of users, same endpoint `/threads/{id}/assign`), so the menu mirrors the detail page's action bar. Respect the same guardrails (no assignment to users without account access; no transitions from `completed`/`spam`).
2. **Explicit sorting:** `Newest` (default) / `Longest waiting` (time since the last email received on or sent to the thread). `Oldest open` was implemented in W7 and then retired in favour of `Longest waiting`, which reflects actual last activity rather than the first message's timestamp. All three are derived from existing columns (`last_message_at`, `opened_at`), so they need an index-aware `ORDER BY` variant on `ListThreadsFilter` (e.g. `Sort string` with a closed allow-list — never interpolate user input). Cursor pagination must be extended per sort key; if that is too invasive, cap the sorted views at the existing 100-row limit and show the truncation notice.
3. **Pagination affordance:** replace the `Older threads` button + `; more results available` prose (`views.go:914-918`) with a clear footer row: `43 threads · Load older` (button) and a `Showing 1–100 of 240` style summary. Keep the existing `before`/`id` cursor query parameters.
4. **Keyboard navigation** in `internal/web/static/app.js` (list page only, disabled while focus is in an input/textarea/select):
   - `j` / `k` — move the active row; `Enter` / `o` — open
   - `x` — toggle selection (participates in W3's bar)
   - `c` — complete (existing confirm dialog), `s` — spam
   - `/` — focus search, `Esc` — clear/blur
   - Visible focus treatment on the active row (visual check); expose state via `aria-selected`/`aria-current`.
5. **Hover quick actions on desktop:** ghost icon buttons for `Complete` and `Spam` in the actions cell on row hover; keep the `⋯` menu for the full set. Same endpoints and confirmations.

**Acceptance**

- The row menu offers every action the detail page offers for the same status.
- Each sort order returns a stable, fully paginated sequence with no duplicates or gaps (test with the cursor).
- `Load older` is an obvious control; the count is unambiguous.
- Keyboard shortcuts work with a screen reader, do not fire while typing in the composer, and do not bypass confirmation dialogs.
- Hover actions are keyboard-reachable (not hover-only) and do not shift row height.

---

## 5. Sequencing, dependencies, risk

| Order | Workstream | Depends on | Risk | Size |
|---|---|---|---|---|
| 1 | W1 thread-age metric + Needs attention | — | Low (age from existing `opened_at`; the new status value touches query, handler validation and tabs, plus the paging cursor) | S–M |
| 2 | W2 chrome + rows | W1 (counts, tiers) | Low–Medium (template/CSS; pull-to-refresh + auto-refresh need care with scroll/focus and confirm dialogs) | M |
| 3 | W3 contextual bulk bar | W2 sticky filter row | Low–Medium (fixed positioning vs. sticky, confirm dialogs) | S |
| 4 | W5 mobile detail + sticky reply | — (independent) | Medium (composer reuse, mobile viewport units) | M |
| 5 | W4 master–detail ≥1280 | W1–W3 (list rendering final) | **High** (dual rendering, hidden-pane `inert`/focus, draft/state preservation across a CSS-triggered switch — no server round-trip to change layout) | L |
| 6 | W6 settings | — | Low | S |
| 7 | W7 parity/sort/pagination/keyboard | W1 (description of age), W3 (selection) | Low–Medium (cursor-per-sort) | M |

Notes for the implementer:

- **Do W1 before W2's count pills and tier swap**, otherwise the strip will show counts for a tab that does not exist and the badge will still use minute-level tiers.
- Keep every change **progressive**: the app must keep working without JS (the `noscript` submit buttons exist for a reason), and every link must remain a real `<a href>` so W4 can be introduced without rewriting navigation.
- Do not move rows' click handling off `tr[data-row-href]` (`app.js:374-386`) without a keyboard alternative; if rows become JS-routed, add `tabindex` + `role` and `Enter` handling in the same change.
- **W4 layout is chosen by CSS, never by the server or by a resize listener.** One document serves both widths; the breakpoint alone decides split vs single. Server-side or cookie-based viewport hints are explicitly rejected — they cannot follow a resize without a reload, which the acceptance criteria forbid. JS is only allowed to keep the hidden pane out of the tab order (`inert`) and to restore the list scroll offset.
- Screenshot-diff risk: the existing e2e suite asserts on some labels ("Showing N threads", `Older threads`, "Select conversations to assign…"). Grep `e2e/` before deleting copy, and update assertions in the same PR.

---

## 6. Verification (every workstream)

```bash
golangci-lint run ./...
go test -v -race ./...
```

Plus, per workstream:

- **W1:** unit tests for the three badge cases (`new`, `in_progress`, `completed`/`spam`) including a zero `opened_at` fallback; boundary tests for the tiers (today, 1d/7d edge, 8d) across a non-UTC `display_timezone` and across midnight; a query test asserting counts match rows and that `Needs attention` respects `AllowedAccounts`; a test that `status=attention` cannot leak other accounts' threads.
- **W2/W3/W6:** functional e2e only — no-JS render parity, every `More ▾` entry returns the correct thread set, the bulk endpoints still work, and auto-refresh skips while a dialog/selection is active. **Layout, density and overflow are verified visually** by a human at 390 / 768 / 1024 / 1280 / 1440 / 1920 px against the acceptance criteria; do not add CSS-presence or geometry assertions.
- **W4:** e2e for deep-link → detail with list present at 1440, single-pane at 1024 (`?nolist=1` path), back-link behaviour, draft restore across pane switches, and a resize test that sets the viewport to 1440, types into the reply draft, resizes to 1024 and back to 1440, then asserts the list is visible again and the draft text is intact. Assert on `visibility`/`offsetParent`-based "is this pane shown", never on computed pixel geometry.
- **W5:** e2e for composer reachability, send → DB row + `append_to_sent` job, draft persistence, `Cmd/Ctrl+Enter` single submit.
- **W7:** cursor-pagination tests per sort order; keyboard e2e (`j`/`k`/`x`/`c`/`s`/`/`); row-menu parity test comparing action labels to the detail page's for each status.

Manual pass required (cannot be fully automated): iOS Safari bottom-sheet behaviour, safe-area insets, sticky filter row over the on-screen keyboard, and screen-reader announcement of the selection bar.

---

## Appendix A — Evidence

Screenshots in [`docs/ux-review-3/`](../docs/ux-review-3/):

| File | Shows |
|---|---|
| `inbox-desktop-1920.png` | 47% of the viewport unused; 6 chrome blocks above the list |
| `inbox-desktop-1440.png` | baseline: first row at 370 px, 5 threads visible, three-line rows |
| `inbox-desktop-1024-selected.png` | `ACTIONS` column and `⋯` trigger clipped (F-2/§2 overflow) |
| `inbox-mobile-390.png` | zero threads above the fold; tab strip clipped after `Completed` (fixed by the 4-tab set + overflow in W1) |
| `inbox-mobile-selected.png` | bulk bar consuming ~120 px with one row selected; per-card checkbox |
| `inbox-mobile-rowmenu.png` | `⋯` popover: only Complete / Mark as spam (F-8) |
| `thread-desktop.png` | detail layout, action bar, message list, composer, Activity |
| `thread-mobile-390.png` | ~450 px of stacked controls before `Messages (2)` (F-6) |
| `settings-desktop.png` | 5-column team table, unbounded activity table (F-7) |
| `settings-mobile-390.png` | wrapped names/short cells at 390 px (F-7) |

## Appendix B — Files this plan touches

| File | Workstreams |
|---|---|
| `internal/db/sqlc/queries.go` (`ListThreads`, `ListThreadsFilter`, `ListCustomerHistory`) | W1, W7 |
| `internal/web/handlers_threads.go` (wait derivation ~501-508, status validation, counts) | W1, W2, W7 |
| `internal/web/views/views.go` (`inboxBody` 851-919, `inboxTablePartial` 770-847, `threadDetailBody` 921-1184 incl. `actions-bar` 982, `Messages` 1041, reply card 1130-1168, `settingsBody` 1195-1344, `FormatWaitDuration` 227-250) | W1–W7 |
| `internal/web/static/app.css` (`.navbar` 117/1021, `.container` 191/1030, `.toolbar` 1135, `.tab-strip` 1144, `.bulk-actions` 1203, `.threads-table` 1326/1745, mobile block 1783-2010) | W2–W6 |
| `internal/web/static/app.js` (`data-hx-get` 3, `data-auto-submit` 349, `tr[data-row-href]` 374, keydown 407, `data-has-selection` handling, refresh/auto-refresh) | W1 (auto-refresh), W2, W3, W5, W7 |
| `internal/web/handlers_settings.go` (+ optional new activity route) | W1 (threshold), W6 |
| `e2e/` | W2–W7 assertions |
