# MiniCRM UX & Visual Redesign Plan

## Context & Problem Statement

MiniCRM's current web interface is a functional database scaffold, but it creates daily friction for support and sales operators across the three core stages of email triage:

1. **Queue Triage (`/threads`)**:
   - **Completed & Spam clutter**: Defaulting to `All statuses` mixes finished (`Completed`) and junk (`Spam`) threads into the active work queue.
   - **Invisible SLA / wait duration**: Raw timestamps (`2026-10-06 16:30:00 CEST`) make it hard to tell how long a `New` thread has been waiting for attention, and sorting strictly by `last_message_at DESC` pushes the oldest untouched `New` inquiries to the bottom of the list.
   - **Missing message blurb**: Only the subject line is shown, forcing operators to open each thread just to preview what the latest email says.
   - **Awkward column wrapping & link styling**: Long customer email addresses break mid-word (`alice@customer.exam` / `ple`), and subjects look like default 1990s blue underlined links rather than interactive queue rows.

2. **Conversation Reading & Action Ergonomics (`/threads/{id}`)**:
   - **Chronology & buried latest message**: In multi-message threads, older emails occupy the entire viewport while the most recent message and the **Send reply** composer are pushed far below the fold.
   - **Nested scrollbars**: Every email is trapped in a fixed `170px`-tall `<iframe>` with a resize grip and a manual `Expand` button—wasting vertical space on 1-line replies and forcing annoying scroll-inside-scroll on longer messages.
   - **Misaligned & duplicated Assignee controls**: The thread header card displays `Assignee` twice (once as read-only metadata and again below as a stacked `<label>Assignee <select>...</label> + [Assign]` form), breaking horizontal baseline alignment with the `Start working` / `Mark complete` / `Assign to me` action buttons.

3. **Visual Identity & Cross-Screen Polish**:
   - **No clear visual identity**: Generic system styling, unbranded top navigation, and minimal distinction between inbound customer emails and outbound team replies make MiniCRM feel like an unstyled prototype.
   - **Timezone repetition**: Repeating `(Europe/Amsterdam)` in section headers and `CEST` plus `:SS` seconds on every single row across `/threads`, `/threads/{id}`, and `/settings` adds visual noise.Timezone configuration belongs exclusively in `/settings` (with full exact timestamps available via hover tooltips).

---

## Non-Negotiable Architectural & Security Guardrails

All UX improvements must preserve MiniCRM's core invariants:
- **Sandboxed Email Isolation**: Email bodies must continue to be sanitized via `mail.SanitizeEmailHTML` and served from `/emails/{id}/body` inside `<iframe sandbox="allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer" credentialless>` with `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; ...`. Auto-sizing the iframe must be performed by the parent page (`app.js`) reading `iframe.contentDocument.documentElement.scrollHeight` on same-origin load/resize—never by executing scripts inside the email iframe.
- **Strict Workflow State Machine**: Only `new -> in_progress`, `new -> spam`, `in_progress -> completed`, and explicit `ReopenAndMerge` transitions are allowed.
- **No `updated_at` Columns & UTC Storage**: Any summary metadata needed for the inbox view must be derived via SQL query (`LATERAL` join on `emails` for latest snippet/direction/count and `MIN(sent_at)` for thread age) without adding `updated_at` columns.
- **Multi-Account Isolation**: Single-account users continue to have all `Account` filters/badges hidden (`ShowAccountUI == false`).

---

## Redesign Pillars

### Pillar 1: Visual Identity — "Crisp Editorial Service Desk"

Give MiniCRM a recognizable, high-craft visual identity suited for focused daily triage:

1. **Brand Header & Navigation**:
   - **Deep Ink Top Bar (`#0f172a`)**: Replace the washed-out white header with a grounded dark slate top bar featuring a custom geometric envelope/spark logomark next to **MiniCRM**, high-contrast pill navigation (`Threads`, `Settings`), and a compact operator badge (`Dev Admin` + subtle role/email + `Log out`).
2. **Color System & Semantic Tokens (`internal/web/static/app.css`)**:
   - **Canvas & Surfaces**: Warm alabaster canvas (`#f8fafc`) with crisp white cards (`#ffffff`), subtle slate borders (`#e2e8f0`), and refined elevation.
   - **Signature Accent**: Deep cobalt/indigo primary (`#1e40af` / `#2563eb`) paired with purposeful semantic status tokens:
     - `New`: Electric blue (`#dbeafe` bg, `#1e40af` text, `#3b82f6` dot/accent)
     - `In progress`: Warm amber (`#fef3c7` bg, `#92400e` text, `#f59e0b` dot)
     - `Completed`: Emerald (`#dcfce7` bg, `#166534` text)
     - `Spam`: Muted rose (`#fee2e2` bg, `#991b1b` text)
   - **Urgency / Wait-Time Escalation Tokens**:
     - Fresh (`< 4h`): Neutral slate pill
     - Aging (`4h – 24h`): Warm amber badge (`#fffbeb` bg, `#b45309` border/text)
     - Overdue (`> 24h`): High-visibility coral/red badge (`#fef2f2` bg, `#dc2626` text)
3. **Typography & Tabular Hierarchy**:
   - Clean humanist UI stack (`"Inter", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif`) with tight heading tracking (`-0.02em`) and `font-variant-numeric: tabular-nums` on all timestamps, counts, and metrics so columns align cleanly.

---

### Pillar 2: Triage-First Inbox (`/threads`)

Transform `/threads` from a raw database table into an actionable support queue:

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│ [■] MiniCRM   Threads   Settings                     Dev Admin (admin@...) Log out│
├──────────────────────────────────────────────────────────────────────────────────┤
│ Threads                                                       Showing 2 open     │
│                                                                                  │
│ [ Open (2) ] [ New (1) ] [ In progress (1) ] [ Completed ] [ Spam ]   [All ▾]   │
│                                                    Assignee: [Anyone ▾] Acct:[▾] │
├──────────────────────────────────────────────────────────────────────────────────┤
│ THREAD & LATEST MESSAGE          CUSTOMER          STATUS       ASSIGNEE  AGE    │
│ ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ │
│ ● Question about invoice INV...  Bob Builder       [New]        Dev Admin ⏳ 14d │
│   "Hello, I received invoice..." bob@customer...                          Oct 6  │
│ ──────────────────────────────────────────────────────────────────────────────── │
│   Cannot log in to my account [3] Alice Example    [In progress] Unassigned 9h   │
│   "Never mind, it works now..."  alice@customer...                        Oct 19 │
└──────────────────────────────────────────────────────────────────────────────────┘
```

1. **Default to Open Queue (`status = "open"` -> `new` + `in_progress`)**:
   - When visiting `/threads` with no `?status=` query parameter, default `StatusFilter` to `"open"` (`status IN ('new', 'in_progress')`).
   - Completed and Spam threads are immediately excluded from the main open list, while remaining 1 click away via segmented status tabs.
   - Support status filter values:
     - `open` (default): `new` + `in_progress`
     - `new`: `new` only
     - `in_progress`: `in_progress` only
     - `completed`: `completed` only
     - `spam`: `spam` only
     - `all`: all statuses
2. **Compact Filter & Segmented Tab Bar**:
   - Replace the bulky 3-column white card with a single horizontal toolbar:
     - **Left**: Segmented status tabs (`Open`, `New`, `In progress`, `Completed`, `Spam`, `All`) with badge counts for active open states.
     - **Right**: Compact inline `<select>` controls for `Assignee` (`Anyone`, `Assigned to me`, `Unassigned`, specific users) and `Account` (when `ShowAccountUI` is true).
3. **Subject + Latest Email Blurb + Message Count**:
   - Enrich `ListThreads` (via a `LATERAL` subquery on `emails`) to return for each thread:
     - `MessageCount int` (total messages in thread)
     - `LatestSnippet string` (first ~140 chars of the latest email's `body_text`, whitespace-normalized, or stripped fallback from `body_html_sanitized`)
     - `LatestDirection string` (`"inbound"` or `"outbound"`)
     - `OpenedAt time.Time` (timestamp of the first message or thread creation)
   - In the **Thread** column:
     - **Line 1**: Bold subject link (no default underline) + message count pill (`3` when `MessageCount > 1`) + account badge (if multi-account).
     - **Line 2**: Muted 1-line truncated blurb (`text-overflow: ellipsis`). If `LatestDirection == "outbound"`, prefix with `You: `; if empty body, show `(No message text)`.
   - Make the entire table row clickable (`data-row-href="/threads/{id}"`) while preserving standard `<a href>` keyboard/middle-click behavior.
4. **Attention / Wait-Time Visibility for `New` Threads**:
   - For `New` threads (which have not received agent attention yet), display a prominent **Waiting badge** showing how long the email has been waiting since it arrived (e.g., `Waiting 14d`, `Waiting 6h`, `Just now`), color-coded by urgency (`<4h` neutral, `4h–24h` amber, `>24h` red/coral), plus a left accent border on the row.
   - Order the default `Open` queue so `New` threads appear with clear urgency or allow sorting where untouched `New` items and recent activity are immediately scannable.
   - Fix the **Customer** column width and truncation (`max-width` + `text-overflow: ellipsis` on `.customer-email`) so addresses never wrap mid-word.

---

### Pillar 3: Redesigned Thread Detail & Conversation View (`/threads/{id}`)

Solve the header alignment, chronology, nested scrollbars, and collapsing in one cohesive layout:

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│ ← Back to threads                                                                │
│ ┌──────────────────────────────────────────────────────────────────────────────┐ │
│ │ Cannot log in to my account                                                  │ │
│ │ [In progress] · [Customer Support] · Alice Example <alice@...> · Waiting 14d │ │
│ │ ──────────────────────────────────────────────────────────────────────────── │ │
│ │ [✓ Mark complete]                        Assignee: [ Dev Admin ▾ ] [Take]    │ │
│ └──────────────────────────────────────────────────────────────────────────────┘ │
│                                                                                  │
│ Messages (3)                                                  [Expand all (3)]   │
│ ┌──────────────────────────────────────────────────────────────────────────────┐ │
│ │ ▸ [Customer] Alice Example — "Hi support team, I have been unable..." Oct 5  │ │
│ ├──────────────────────────────────────────────────────────────────────────────┤ │
│ │ ▸ [Customer] Alice Example — "Never mind, it works now. I had Caps..." Oct 5 │ │
│ └──────────────────────────────────────────────────────────────────────────────┘ │
│ ┌──────────────────────────────────────────────────────────────────────────────┐ │
│ │ ▾ [Customer] Alice Example <alice@customer.example>            Oct 19, 23:05 │ │
│ │ ──────────────────────────────────────────────────────────────────────────── │ │
│ │ (Full auto-sized email body — NO inner scrollbar, NO resize handle)          │ │
│ └──────────────────────────────────────────────────────────────────────────────┘ │
│                                                                                  │
│ ┌──────────────────────────────────────────────────────────────────────────────┐ │
│ │ Reply to Alice Example <alice@customer.example>      From: Customer Support  │ │
│ │ ┌──────────────────────────────────────────────────────────────────────────┐ │ │
│ │ │ Write your reply...                                                      │ │ │
│ │ └──────────────────────────────────────────────────────────────────────────┘ │ │
│ │ [📎 Attach file]                                              [Send reply]   │ │
│ └──────────────────────────────────────────────────────────────────────────────┘ │
│                                                                                  │
│ ▸ Activity (6 events)                                                            │
└──────────────────────────────────────────────────────────────────────────────────┘
```

1. **Unified, Aligned Thread Header & Action Toolbar**:
   - Add a `← Back to threads` breadcrumb link above the header card.
   - **Remove the duplicate Assignee display**: Do not show `ASSIGNEE` as static text in the metadata list *and* again as a stacked form below.
   - **Single-row Action Toolbar with matching 36px control heights**:
     - **Left group (Workflow Transitions)**: Primary state action (`Start working`, `Mark complete`) and secondary danger action (`Mark as spam`).
     - **Right group (Ownership)**: Inline horizontal `Assignee` control (`<span class="toolbar-label">Assignee</span> <select name="assignee_id" data-auto-submit>...</select>`). Selecting an assignee auto-submits the form via `app.js` (with a `<noscript>` fallback button). When unassigned, display a compact `Assign to me` button on the exact same baseline and height (`36px`).
   - Remove the redundant top-right `Reply` button once older messages are collapsed (or make it a subtle `↓ Jump to reply` action that focuses `#reply-body`).

2. **Eliminate Nested Scrollbars (Seamless Auto-Sizing Email Iframes)**:
   - Keep the security-critical `<iframe class="email-frame" sandbox="allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer" credentialless>` isolation intact.
   - In `app.css`: Remove `height: 170px`, `resize: vertical`, and inner scrollbars (`overflow: hidden` once sized).
   - In `app.js`: Attach a `load` listener (and `ResizeObserver` on the iframe's `contentDocument.body`) that sets `iframe.style.height = doc.documentElement.scrollHeight + "px"`.
   - Remove the manual `Expand` / `Collapse` iframe height button (`.message-actions [data-frame-expand]`).
   - **Graceful Empty Body State**: In `isolatedEmailBodyHTML` (`views.go`), if both `SanitizedHTML` and ` strings.TrimSpace(PlainText)` are empty, render a compact muted italic notice `<p class="empty-body">(No message content)</p>` so empty emails render as a clean 1-line note instead of a blank white box.

3. **Collapse Older Messages by Default; Keep Latest Message Open**:
   - In a thread with `N` messages:
     - **Messages `1 .. N-1` (older messages)** render in a **collapsed `<details class="message-card">` (or collapsible `<article>`) state** by default, showing a single-line summary bar:
       - Chevron indicator (`▸` / `▾`)
       - Direction badge (`Customer` vs `Team reply`)
       - Sender name
       - 1-line body snippet (`"Hi support team, I have been unable to log in..."`)
       - Attachment indicator icon (`📎 1`) if attachments exist
       - Concise timestamp (`Oct 5, 11:15`)
     - **Message `N` (the most recent message)** renders **expanded (`open`) by default** right above the **Send reply** composer.
     - Add an **`Expand all` / `Collapse older`** toggle button in the `Messages (N)` section header so operators can open the full history with a single click.
     - Visually distinguish **Customer** messages (clean white surface with slate border) from **Team replies** (subtle indigo/slate-tinted header & accent border) so conversation direction is effortless to scan.

4. **Streamlined Reply Composer & Collapsible Activity Log**:
   - Combine the `From` / `To` header into a compact single-line bar at the top of the Reply card.
   - Style the file attachment control cleanly alongside the primary `Send reply` action on a single footer row of the reply card.
   - Render the **Activity** audit trail inside a collapsible section (`<details class="card activity-card">` open by default on Completed/Spam threads, or compactly styled) and deduplicate consecutive identical status-change lines in presentation if needed.

---

### Pillar 4: Concise Timestamps & Single-Place Timezone Display

1. **Remove Timezone Repetition Across All Views**:
   - `/threads` table header: Change `LAST MESSAGE (EUROPE/AMSTERDAM)` -> `AGE / LAST ACTIVITY` (or `UPDATED`).
   - `/threads/{id}` activity heading: Change `Activity (Europe/Amsterdam)` -> `Activity`.
   - Timestamp strings everywhere: Strip the trailing `CEST` / `UTC` timezone abbreviation and `:SS` seconds.
   - Keep `Display timezone (IANA)` visible and editable solely in `/settings`.
2. **Human-Centric Timestamp Formatting (`views.FormatTimestamp` & `views.FormatRelativeAge`)**:
   - Provide two complementary formatters in `internal/web/views/views.go`:
     - **Concise display timestamp** (`FormatTimestamp`): `2026-10-19 23:05` (or `Oct 19, 23:05` for current year; `2026-10-06 16:30`), without seconds or timezone suffix.
     - **Full tooltip timestamp** (`FormatTimestampFull`): `2026-10-19 23:05:00 CEST (Europe/Amsterdam)` placed in the `<time datetime="..." title="...">` attribute for audit precision on hover.
     - **Relative wait duration** (`FormatWaitDuration(now, since)`): Computes human-readable elapsed duration (`14d`, `5h`, `25m`, `<1m`) and urgency tier (`fresh`, `warning`, `urgent`) for `New` threads awaiting attention.

---

### Pillar 5: Settings Page Information Hierarchy (`/settings`)

1. **Separate Routine Routing from Low-Level IMAP Configuration**:
   - Group **Account Routing & Display** (per-account Default Assignee dropdowns in a clean grid, Display Timezone, Subject Match Window) at the top of the settings card.
   - Place **IMAP Folder Mapping** (`INBOX`, `Archive`, `Junk`, `Sent`) in a clearly labeled secondary subsection (`IMAP folder mapping`) with helper text.
2. **Coalesce / Cap Recent Activity Table**:
   - Display concise timestamps without repeated timezone suffixes and keep the activity table scannable with a bounded height or clean compact rows.

---

## Files to Modify

1. **`internal/db/sqlc/models.go` & `internal/db/sqlc/queries.go`**:
   - Extend `ListThreadsFilter` to support `StatusGroup "open"` (`status IN ('new', 'in_progress')`) in addition to single status filters.
   - Enrich `Thread` (or `ThreadListItem` / `ListThreads` scan) with `MessageCount`, `LatestSnippet`, `LatestDirection`, and `OpenedAt` via a single SQL query using `LEFT JOIN LATERAL` on `emails` (no N+1 queries, no schema migration required).
2. **`internal/web/handlers_threads.go`**:
   - Default `StatusFilter` to `"open"` when `?status=` is not provided (`?status=all` shows all statuses).
   - Populate `ThreadSummaryItem` with `LatestSnippet`, `LatestDirection`, `MessageCount`, `WaitDuration`, `UrgencyClass`, and concise/full timestamps.
   - Populate `EmailItemView` on `HandleViewThread` with `Snippet` (for collapsed headers) and `IsLatest bool` (so the most recent email defaults to expanded).
3. **`internal/web/views/views.go`**:
   - Update `FormatTimestamp` (drop seconds and timezone abbreviation), add `FormatTimestampFull` (for `<time title="...">`) and `FormatWaitDuration` / urgency helpers.
   - Redesign `layoutHeader` (brand mark + dark slate navbar), `inboxBody` & `inboxTablePartial` (status tabs, compact filters, 2-line subject+blurb, wait-time badges, non-wrapping customer email), `threadDetailBody` (breadcrumb, unified aligned action bar with inline auto-submit Assignee, collapsible `<details>` message cards with latest expanded, seamless auto-sized iframes, empty-body placeholder), and `settingsBody`.
4. **`internal/web/static/app.css`**:
   - Implement the new visual design system, segmented tabs, table row hover/click states, urgency badges, unified 36px toolbar controls, collapsible message headers, customer vs. team message styling, and seamless iframe rules.
5. **`internal/web/static/app.js`**:
   - Auto-resize `.email-frame` iframes to their `contentDocument.documentElement.scrollHeight` on load, window resize, and `<details>` toggle.
   - Support `[data-auto-submit]` on the inline Assignee `<select>`.
   - Support `Expand all` / `Collapse older` toggle for thread messages and clickable inbox rows (`[data-row-href]`).
6. **`internal/web/web_test.go` & `e2e/auth_e2e_test.go`**:
   - Update and add tests verifying default `open` inbox filtering, `?status=all` / `?status=completed` filtering, latest email snippet rendering, wait-time badge rendering, timezone-free visible timestamps, and collapsible message markup.

---

## Implementation Steps

- [ ] **Step 1: Enrich Thread List Query & Open-Queue Filter (`internal/db/sqlc/`)**
  - Update `ListThreadsFilter` and `ListThreads` in `internal/db/sqlc/queries.go` to support filtering by open statuses (`status IN ('new', 'in_progress')`) and retrieve `message_count`, `latest_snippet`, `latest_direction`, and `opened_at` via a `LATERAL` subquery on `emails`.
- [ ] **Step 2: Timestamp, Wait-Time & Snippet Formatting (`internal/web/views/`, `internal/web/handlers_threads.go`)**
  - Update `FormatTimestamp` to omit timezone suffixes and seconds (`2006-01-02 15:04`), add `FormatTimestampFull` for tooltips, and add wait-time badge calculation (`FormatWaitAge`) for `New` threads.
  - Default `/threads` to the `open` status view when no `status` param is passed, while supporting `all`, `new`, `in_progress`, `completed`, and `spam`.
- [ ] **Step 3: Redesign Inbox View & Thread Detail Templates (`internal/web/views/views.go`)**
  - Implement segmented status tabs + compact filter bar, 2-line Subject + Latest Blurb cell, and SLA wait-time indicator in `inboxBody` / `inboxTablePartial`.
  - Redesign `threadDetailBody`: add `← Back to threads`, unify the header action bar and inline Assignee selector, wrap messages in collapsible cards with only the most recent message open by default, add `Expand all`, remove manual iframe `Expand` buttons, and render a clean placeholder for empty email bodies.
  - Remove `(Europe/Amsterdam)` from table/section headers across `/threads`, `/threads/{id}`, and `/settings`.
- [ ] **Step 4: Visual Identity, Seamless Iframes & Progressive Enhancement (`internal/web/static/app.css`, `internal/web/static/app.js`)**
  - Apply the new color palette, dark ink top navigation with brand icon, refined badges, customer vs. team message styling, and aligned 36px toolbar controls in `app.css`.
  - Add automatic iframe height sizing (`scrollHeight`), `Expand all` / `Collapse older` message toggling, inline assignee auto-submit, and clickable table rows in `app.js`.
- [ ] **Step 5: Verification (`golangci-lint`, unit/integration/E2E tests, and live browser inspection)**
  - Update tests in `internal/web/web_test.go` and `e2e/` for the new default open filter and timestamp format.
  - Run `golangci-lint run ./...` and `go test -v -race ./...`, and visually verify every screen in Chrome.
