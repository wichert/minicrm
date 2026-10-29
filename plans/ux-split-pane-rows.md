# Plan — Readable split-view list rows (two-line: subject + customer / preamble)

**Status:** Proposal for review
**Date:** 2026-10-29
**Scope:** W4 split view only (`≥1280px` detail page, left list pane). The full inbox at `/threads` and the `<1280px` single-pane pages are **not** changed by this plan.
**Predecessor:** [`ux-density-and-workflow.md`](./ux-density-and-workflow.md) §W4; current pane code landed in PR #29 (commits `77bb38d`, `2fcaf63`, `2161eaf`).

---

## 1. Problem

The split-view list pane is not useful. Both the subject and the preamble are truncated to near-nothing, so the pane cannot be scanned — you have to open threads to find out what they are, which defeats the purpose of a master-detail view.

The pane currently renders **one** row line inside `clamp(360px, 34vw, 520px)`:

```html
<!-- internal/web/views/views.go:922-925 -->
<span class="thread-line">
  <a class="thread-link">Subject</a> <span class="badge">Draft</span> <span class="count-pill">3</span>
  <span class="thread-snippet">— latest message text…</span>
</span>
<span class="thread-meta">Customer · Account · Assignee · 3d</span>
```

`internal/web/static/app.css:1592-1630` makes the subject `flex: 0 1 auto` (it shrinks) and the snippet `flex: 1 1 auto` with `white-space: nowrap` + `text-overflow: ellipsis` (one line, hard cut). The pane override at `app.css:2671-2682` sets `flex: 0 1 auto` on the link and keeps both on one line, so subject and preamble **compete for the same row** and both lose.

### Width budget (measured, 15px base font)

| Pane width | Cell content width¹ | Subject @600wt (~8px/char) | Snippet @0.86rem (~6.4px/char) |
|---|---|---|---|
| 360 px | ≈ 315 px | ≈ 39 ch — shared with snippet | ≈ 49 ch — shared with subject |
| 520 px | ≈ 475 px | ≈ 59 ch — shared with snippet | ≈ 74 ch — shared with subject |

¹ After `padding-left: 0.55rem`, `padding-right: 0.35rem`, the `1.9rem` actions cell and borders (`app.css:2661-2695`).

The subject is a *label* and the snippet is *content*. Sharing one line means a long subject ("Re: Invoice #4412 — delivery address change") reduces the preamble to "— Hi, we st…". Both strings become unscannable at exactly the moment the user is triaging.

---

## 2. Goal & principles

1. **Two fixed rows per thread, one purpose each.**
   - **Row 1 — identity:** subject + customer name.
   - **Row 2 — content:** the message preamble on its own full-width line.
2. **Give the preamble a real budget** — two clamped lines instead of one ellipsized fragment, using the full cell width because it no longer shares a line.
3. **Drop what the pane cannot afford, keep what triage needs.** The pane is a *queue*, not the record: status and account are implied or already visible; assignee and absolute date belong in the detail. The one attention signal that must survive is the **age**, because "act on old messages that have not had recent attention" is a core usage mode (plan §2.4).
4. **No new chrome.** This is a row-layout change, not another toolbar.
5. **The full inbox is untouched.** Its `≥1280px` one-line row (`app.css:2032-2050`) is right for a wide table with status/age columns; only `.split-list` opts in to the new layout.

---

## 3. Proposed row

```
┌──────────────────────────────────────────────────────┐
│ Invoice #4412 — delivery address change       Ann Lee│   ← row 1: subject (2-line clamp) + customer
│ Hi, we moved offices last week and the invoice…      │   ← row 2: preamble (2-line clamp)
│                                              ·  12d  │   ← age, right-aligned, muted
├──────────────────────────────────────────────────────┤
```

Markup shape (inside the existing `<td class="thread-cell">`, `PaneMode` branch only):

```html
<div class="pane-row">
  <div class="pane-row-top">
    <a class="thread-link" href="…">Subject</a>
    <span class="badge">Draft</span><span class="count-pill">3</span>
    <span class="pane-customer">Ann Lee</span>
  </div>
  <p class="pane-row-snippet">Hi, we moved offices last week and the invoice…</p>
  <span class="pane-age wait-overdue">12d</span>   <!-- absolutely positioned or flex-end of row 2 -->
</div>
```

### Layout rules (pane only, `@media (min-width: 1280px)` under `.split-list`)

| Element | Rule | Why |
|---|---|---|
| `.pane-row-top` | `display: flex; align-items: baseline; gap: .4rem` | subject flexes, customer holds its share |
| `.thread-link` | `flex: 1 1 auto; min-width: 0;` `display: -webkit-box; -webkit-line-clamp: 2; line-clamp: 2; white-space: normal` | the subject gets **two lines when it needs them** instead of being cut at ~28 chars |
| `.pane-customer` | `flex: 0 1 auto; max-width: 40%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--muted); font-size: .8rem` | identity on the right; never collapses below ~11 chars while the subject has room |
| `.pane-row-snippet` | `display: -webkit-box; -webkit-line-clamp: 2; line-clamp: 2; overflow: hidden; white-space: normal; font-size: .86rem; color: var(--muted); margin: .1rem 0 0; padding-right: 2.2rem` | the preamble finally gets ~49 chars/line × 2 at the narrowest pane instead of ~24 total |
| age | keep `.wait-badge` but render a **compact** variant: label only (`12d`, `Today`, `2d`), no "Overdue ·" prefix; position at the end of row 2 (`position: absolute; right: .35rem; bottom: .15rem`) | one attention signal survives without stealing a column |
| `.pane-row` | `position: relative` | anchor for the age |

Delete the pane's snippet `::before { content: "— " }` (`app.css:1620-1623`) and the `You:` prefix stays (`snippet-you`), since the preamble now has its own line and reads as a sentence.

### What the pane drops (and where it lives instead)

| Field | Today | Proposal | Rationale |
|---|---|---|---|
| Assignee | in `.thread-meta`, pane | **dropped in pane** | visible in the detail header; "Mine" tab already filters by it |
| Account | in `.thread-meta`, pane | **dropped in pane** | only rendered when `ShowAccountUI`; single-account users never saw it, multi-account users can filter by it above the split |
| Absolute date | `title` only | kept in `title` on the age badge | hover reveals it; detail header shows full timestamps |
| Message count / Draft | inline on row 1 | **kept** | cheap, high-signal |

---

## 4. Row height & density trade-off

| | Row height | Rows in an 800px pane viewport |
|---|---|---|
| Current (one line + meta) | ≈ 56 px | ≈ 14 |
| Proposed (2-line subject cap + 2-line snippet) | ≈ 96 px worst case, ≈ 74 px typical (1-line subject) | ≈ 8–10 |

**This is a deliberate trade: fewer, readable rows beat more, unreadable ones.** The pane is `position: sticky; max-height: calc(100vh - 2rem); overflow-y: auto` (`app.css:2648-2655`), so scrolling is already expected there, and the detail pane beside it keeps its height.

**Alternative considered and rejected:** keep one line but widen the pane to 40 vw. Rejected because the subject/snippet conflict is structural, not a width problem — at 640px the two still split one line, and a wider pane eats the reading column that the whole split view exists to protect.

**Tunable if 8 rows feels too sparse:** clamp the subject to **1** line and the snippet to **2** (≈ 78px rows, ≈ 11 visible) — one CSS value in the table above. Ship with 2+2 and adjust from a screenshot, not from guesswork.

---

## 5. Workstream

Single workstream, small and self-contained. Branch `feat/ux-split-pane-rows` off `feat/ux-w7-parity` (top of the merge stack), PR stacked on #31.

### W-rows-1 — Two-line pane rows (template + CSS)

1. **`internal/web/views/views.go`** — in `inboxTablePartial`'s `<td class="thread-cell">`, add a `{{if $.PaneMode}}` branch that emits the three-part `.pane-row` structure above; the existing one-line/`thread-meta` markup stays as the `{{else}}` for the full inbox and stays byte-identical.
   - Reuse `ThreadSummaryItem` fields as-is: `Thread.SubjectDisplay`, `CustomerName`/`CustomerEmail` (fallback "Unknown customer"), `LatestSnippet`, `LatestDirection`, `WaitLabel`, `WaitUrgency`, `OpenedFullTime`, `MessageCount`, `Thread.HasDraft`.
   - **No handler changes**, no new query columns, no `updated_at` (AGENTS.md invariant 3).
2. **`internal/web/views/views.go`** — add a compact age helper if `WaitLabel` cannot be shortened in CSS: `ThreadAgeCompact(status, since, now, tz)` returning `Today` / `Nd` with the same bucketing as `ThreadAgeBadge` so the two never disagree. Prefer reusing `WaitLabel` and shortening via a distinct element only if it already yields a short form; do not fork the bucketing logic.
3. **`internal/web/static/app.css`** — inside the existing `@media (min-width: 1280px)` block, replace the `.split-list .thread-rowline` / `.thread-line` / `.thread-link` / `.thread-meta` overrides (`app.css:2665-2697`) with the `.pane-row*` rules from §3. Keep the `.split-list .threads-table` cell-padding rule and the `aria-current` selected-row rule untouched.
4. **Accessibility:** row 1's link keeps `title="{{.Thread.SubjectDisplay}}"`; the customer span gets `title` with the customer email (today's `.thread-meta` already does this). The age badge keeps `title="Opened {{.OpenedFullTime}}"`. No `aria-hidden` on information — the pane must not lose anything to a screen reader that the full inbox has.

### W-rows-2 — Tests (behaviour, not appearance)

Per the plan-wide guideline in `ux-density-and-workflow.md` §1, **do not assert CSS values or pixel geometry**. Assert the rendered markup:

- `internal/web/web_split_detail_test.go`
  - Pane rows render `.pane-row` with a subject link, a customer span and a `pane-row-snippet` element — and the snippet is a **separate element from the subject link**, not a sibling inside the same line container.
  - The full inbox (`/threads`) still renders the one-line markup and does **not** emit `.pane-row` — the two-row layout is pane-only.
  - A thread with no body text still renders `(No message text)` in `pane-row-snippet`.
  - An outbound latest message still renders `You:` before the snippet.
  - A customer with no name falls back to the email, then to "Unknown customer".
  - Selected row keeps `aria-current="true"` and `thread-row-current`.
- `internal/web/views` unit test for the compact age helper (if added): `Today`, `1–7d`, `>7d` buckets, matching `ThreadAgeBadge`'s boundaries in `display_timezone`.

### W-rows-3 — Visual verification (human, screenshots)

Capture at **1280×800, 1440×900, 1920×1080** with the seeded dev stack (`docker compose up`; session `Dev Admin <admin@example.com>`, 43 open threads): Needs-attention tab, a filtered list, and a row with a very long subject. Check: preamble is legible without hovering, subject is not cut mid-word, the selected row still reads as selected, and the pane scrolls cleanly under the sticky header.

---

## 6. Out of scope

- The full inbox row layout at `/threads` (W2/W7 intentionally made it one line at `≥1280px`).
- The mobile card layout (`≤768px`, `app.css:2379-2410`) — already multi-line and correct there.
- Any change to filters, tabs, bulk actions, sorting, pagination or keyboard navigation.
- A third sort key (deferred in W7, PR #31).

---

## 7. Risks

| Risk | Mitigation |
|---|---|
| Two clamped lines make rows tall enough that 1280px viewports only show 6–7 threads | 2+2 is a starting value; dropping the subject clamp to 1 line is a one-value change. Decide from the W-rows-3 screenshots. |
| `-webkit-line-clamp` needs `display: -webkit-box`, which conflicts with the flex row | Clamp applies to `.thread-link` (a block-level box inside `.pane-row-top`), not to the flex container — same pattern already used successfully for phone cards (`app.css:2388-2395`). |
| Dropping assignee from the pane hides triage-relevant info | Assignee is in the detail header one click away, and the "Mine" tab is the filter for it; the alternative (a third row) costs 20px × every row. |
| Compact age badge changes wording users rely on | Bucketing logic stays in `ThreadAgeBadge`; only the rendered length in the pane changes. `title` keeps the full text. |
| Pane and inbox rows drift apart over time | Both stay in one template with an explicit `{{if $.PaneMode}}` switch, guarded by the W-rows-2 test that the inbox does not emit `.pane-row`. |

---

## 8. Files touched

| File | Change |
|---|---|
| `internal/web/views/views.go` | `inboxTablePartial`: `PaneMode` two-row markup; optional compact-age helper |
| `internal/web/static/app.css` | Replace split-pane row overrides in the `≥1280px` block (~35 lines) |
| `internal/web/web_split_detail_test.go` | ~6 render assertions |
| `internal/web/views/*_test.go` | compact-age bucket test (if the helper is added) |

No handler, schema, migration, workflow, IMAP, audit or JS changes.
