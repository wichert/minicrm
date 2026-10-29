# Plan: Fix Mobile Thread Card Layout (`@media (max-width: 768px)`)

## Findings from Browser Inspection

Inspecting the mobile thread list in headless Chrome (`@media (max-width: 768px)`) revealed the exact layout measurements causing the broken mobile cards:

1. **Status, Account, Assignee, and Age cells collapse to `4px` wide (`width: 1%`)**:
   - On desktop, `.status-cell`, `.account-cell`, `.assignee-cell`, and `.age-cell` use `width: 1%` so table columns shrink-wrap their contents.
   - On mobile (`@media (max-width: 768px)`), `tr.thread-row` switches to `display: flex; flex-wrap: wrap`, and those cells use `flex: 0 0 auto` (`flex-basis: auto`), which falls back to `width: 1%` (`~4px` wide on a mobile viewport):
     - `TD.status-cell`: `left=32 right=36 width=4 scrollWidth=89`
     - `TD.account-cell`: `left=46 right=50 width=4 scrollWidth=133` — starts only `14px` to the right of `status-cell`, so the `"Account"` badge renders directly on top of `"New"` / `"In progress"`.
     - `TD.age-cell`: `left=449 right=453 width=4 scrollWidth=86` — its `Waiting 3d` badge overflows to the right (`scrollWidth=518` on a `451px` row) and gets clipped by `.threads-table { overflow: hidden }`.
     - `TD.assignee-cell`: `width=4 scrollWidth=88` — overflows to the right into the `actions-cell` (`⋯` button).

2. **Customer email and 4-row wrapping waste vertical and horizontal space**:
   - Showing `.customer-email` on a second line inside `.customer-cell` adds height to every card and competes with the subject and snippet.
   - When `CustomerName` is empty, `.customer-name` currently renders `"Unknown customer"` and puts the actual address in `.customer-email`; if `.customer-email` is hidden on mobile without falling back to the email address in `.customer-name`, rows without a display name would only say `"Unknown customer"`.
   - `td.actions-cell` (`order: 7`) sits at the bottom right after `td.assignee-cell` (`order: 6`), crowding the bottom metadata row. Moving `td.actions-cell` to the top-right header row alongside Checkbox + Customer + Age gives the card a clean 3-band layout.
   - `.threads-table, .threads-table tbody { overflow: hidden }` clips the `<details class="row-menu">` dropdown when opened on the last row of the list.

---

## Proposed Changes

### 1. Mobile CSS overrides in `internal/web/static/app.css` (`@media (max-width: 768px)`)

- **Reset desktop table widths on mobile flex items**:
  - In `.threads-table tr.thread-row td`, set `width: auto; max-width: none;` so no cell inherits `width: 1%` or `width: 12rem` as its flex basis.
- **Prevent clipping of row action menus**:
  - Remove `overflow: hidden` from `.threads-table, .threads-table tbody` on mobile so the `⋯` popup menu can extend outside the bottom card.
- **Structure each mobile card into 3 clean horizontal bands**:
  1. **Band 1 — Header (`Checkbox` · `Customer` · `Age` · `Actions ⋯`)**:
     - `td.select-cell`: `order: 0; flex: 0 0 auto; width: auto;`
     - `td.customer-cell`: `order: 1; flex: 1 1 0%; width: auto; max-width: none; min-width: 0;`
     - `.threads-table td.customer-cell .customer-email`: `display: none;` (hide email address on small screens to save space).
     - `td.age-cell`: `order: 2; flex: 0 0 auto; width: auto; margin-left: auto; text-align: right; font-size: 0.82rem;` (natural width, never clipped).
     - `td.actions-cell`: `order: 3; flex: 0 0 auto; width: auto; padding: 0;` (compact `⋯` trigger at top right).
  2. **Band 2 — Thread content (`Subject` + `Draft`/count badges + `Snippet`)**:
     - `td.thread-cell`: `order: 4; flex: 1 1 100%; width: 100%; max-width: 100%; min-width: 0; overflow: hidden; padding: 0.1rem 0;`
  3. **Band 3 — Metadata footer (`Status` · `Account` · `Assignee`)**:
     - `td.status-cell`: `order: 5; flex: 0 0 auto; width: auto;`
     - `td.account-cell`: `order: 6; flex: 0 0 auto; width: auto;`
     - `td.assignee-cell`: `order: 7; flex: 0 1 auto; width: auto; max-width: 55%; margin-left: auto; overflow: hidden; text-overflow: ellipsis; font-size: 0.82rem; color: var(--muted);`

### 2. Customer Name Fallback in `internal/web/views/views.go`

- In `inboxTablePartial`:
  - When `.Thread.CustomerName` is non-empty, render `.customer-name` with `{{.Thread.CustomerName}}` and `.customer-email` with `{{.Thread.CustomerEmail}}` (which is visible on desktop and hidden on mobile).
  - When `.Thread.CustomerName` is empty, render `{{if .Thread.CustomerEmail}}{{.Thread.CustomerEmail}}{{else}}Unknown customer{{end}}` inside `.customer-name` (with `title="{{.Thread.CustomerEmail}}"`), so hiding `.customer-email` on mobile still identifies the sender by email address instead of showing `"Unknown customer"`.

### 3. Verification

- Render the mobile view in headless Chrome to verify zero overlap, no clipped age badge, and clean 3-band card alignment.
- Run `golangci-lint run ./...` and `go test -v -race ./...`.
