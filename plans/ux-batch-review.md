# Plan — Batch UX review follow-up

Source: browser review of the current queue and thread view (1440×900 and 390×844, seeded Dev Admin inbox), plus the five user reports. This plan only covers those findings. It does not reopen draft persistence, image consent, reply formatting, or pagination.

Approved in Plannotator (`pn-0aa41c`) with one note: the per-row assign control stays a button, because a button takes less space than a select or a wrapped secondary control. That note is applied in workstream 1.

**Constraints (from `AGENTS.md`):** do not add a direct `new → completed` transition. Completing a new thread stays the audited composite `CompleteFromNew` (`new → in_progress → completed`, both rows in `thread_events`, one `move_to_archive` job on the owning account). Spam and assign stay account-scoped and revalidated per thread. No `updated_at` columns. Customer history must keep filtering by `allowed_accounts` and must not reveal inaccessible accounts.

## 1 — Give the queue its width back (P1)

**Problem.** At 1440px the selection column is 187px for a 13px checkbox, and every open row carries a wrapping 79×44 **Assign to me** button. Those two columns are about 289px. The Age column is last, so "Waiting 12d" is clipped. Rows are 87px tall. On a phone the bulk bar is 139px tall before the first card, and cards are 214px.

**Changes**

- `internal/web/views/views.go`, `internal/web/static/app.css`: keep the row checkbox, but remove the visible "Select all on page" label from the column header. The accessible name stays on the checkbox (`aria-label`). Size the column to the checkbox, not the label. Target a hit area of at least 24×24 without a 187px column.
- Keep per-row assign as a `<button>`, not a select and not an icon-only control. Shrink it to a compact button (`btn-small`, no wrap, no extra cell padding) so it stays shorter than the current 79×44 wrapped control. Show it for unassigned `new` and `in_progress` rows. Keep the accessible name ("Assign {subject} to me"). It must remain available to keyboard and touch users without relying on hover.
- Do not add a per-row assignee `<select>`. One bulk assignee control is enough. The current name ("Unassigned", or the assignee) stays as text.
- Let the subject and age share the recovered width. Age (wait badge plus date) must be fully visible at 1280px and 1440px without horizontal page scroll. Row height should fit two text lines plus the compact button, not a wrapped button.
- Mobile: keep **Assign to me** as a compact button on the card, not a second full-width bar. The bulk bar should collapse to one line until something is selected (see workstream 2).

**Tests:** web test that the list still posts to `/threads/{id}/assign` with `return_to`, and that completed and spam rows do not offer assign. Manual check at 1280, 1440, and 390 that the wait badge and date are fully visible.

## 2 — Disable bulk actions until a selection exists (P1)

**Problem.** With nothing selected, **Assign selected** and **Mark selected as spam** are enabled. Spam opens "Mark 0 selected conversation(s) on this page as spam?" and still offers **Mark as spam**. Assign skips the dialog and returns a raw `400`: `select an assignee and between 1 and 500 conversations`. The same bar is shown on the Spam tab, where assignment is not allowed.

**Changes**

- `internal/web/static/app.js`: while the selected count is 0, disable the assignee `<select>`, **Assign selected**, and **Mark selected as spam**. Set `aria-disabled` and do not open the confirm dialog for an empty selection. The live count remains the explanation: "Select conversations to assign or mark as spam."
- When a selection exists, enable only the actions that apply. **Mark selected as spam** applies to `new` and `in_progress` only. **Assign selected** applies to those same statuses. On the Spam and Completed tabs, do not render assign or spam bulk actions. A Spam tab may later grow a bulk "Not spam" action, but that is out of scope here.
- `internal/web/handlers_bulk.go`: keep the server checks. Replace the raw `http.Error` body with a redirect back to `return_to` and a flash ("Select at least one conversation." / "Choose an assignee."). A direct POST must not dump a plaintext error page.
- Confirming spam must include the count. If the count is zero, the client must not submit. The server still rejects an empty selection.

**Tests:** extend `internal/web/web_test.go` so an empty bulk assign and empty bulk spam redirect with a flash rather than a plaintext 400 body. A browser check that the confirm dialog does not open at zero selected, and that both buttons are not keyboard-activatable until a box is checked.

## 3 — Hide empty customer history (P2)

**Problem.** Every thread shows a collapsed **Previous conversations** disclosure. Opening it on a customer with no other threads shows "Other conversations from this email address" and then "No other conversations found." The heading claims conversations that are not there.

**Changes**

- `internal/web/views/views.go`: render the `<details class="previous-conversations">` block only when `PreviousThreads` is non-empty. Delete the empty-state helper.
- When the section exists, put the count in the summary: "1 earlier conversation" / "N earlier conversations". Keep the helper "Other conversations from this email address" inside the open section so the match rule stays honest. Do not imply a resolved customer identity.
- Leave the disclosure collapsed by default so the thread header stays short. History links stay `target="_blank" rel="noopener"`.

**Tests:** update `internal/web/web_test.go` so a thread with no other accessible conversations does not contain "Other conversations from this email address" or "No other conversations found." A thread with one accessible match still shows the summary count, the subject, and the helper. Inaccessible-account rows stay excluded.

## 4 — Lay out history as rows (P2)

**Problem.** A related thread renders as a default bullet: link, status badge, `2026-10-06 14:30`, and account badge run together in a `list-item` with no gap or columns. The "Links open in a new tab…" sentence is a second helper under the list.

**Changes**

- `internal/web/static/app.css` plus the history markup in `internal/web/views/views.go`: render each item as a row, not a bulleted list. Columns: subject (link), status badge, relative or formatted time, and account badge only when `ShowAccountUI` is on. Align badges so a second row scans as a table. Remove the bullet (`list-style: none`) and add a gap between subject and metadata.
- Use the same timestamp formatting as the inbox (`FormattedTime`, full timestamp as `title`). Do not invent a new date format.
- Move "Opens in a new tab so your reply stays here." to the link `title` (or a visually hidden note per link). Do not keep a paragraph under the list.
- Mobile: stack subject above the metadata; do not let the row overflow horizontally.

**Tests:** markup test that each history item contains the subject link, status label, and time, and that the new-tab warning is on the link rather than a trailing paragraph. Visual check with Bob Builder (one prior thread) and a customer with several.

## 5 — Complete a new thread from the queue (P2)

**Problem.** **Mark complete** exists only on the thread page, beside **Start working** and **Mark as spam**. The New queue has no complete action and no bulk complete. Clearing a newsletter or receipt still means open, confirm, and go back.

**Changes**

- Reuse `workflow.CompleteFromNew` for `new` threads and the existing complete transition for `in_progress`. Do not add a direct `new → completed` transition. One `move_to_archive` job per thread, on that thread's `account_id`. Both transitions recorded in `thread_events` for a new thread.
- Row action: a compact **Complete** button on `new` and `in_progress` rows, posting to the existing transition endpoint with `complete_from_new=1` when status is `new`, and `return_to` set to the current filtered list. One confirmation per submit, naming the archive move. Do not also require **Start working**.
- Bulk action: `POST /threads/bulk-complete`, same selection and revalidation pattern as `HandleBulkSpam`. Eligible statuses: `new` and `in_progress`. Per-thread success and failure summary. Redirect to `return_to` with a flash that says the archive moves were queued, not that IMAP has finished. Confirm once for the batch, including the count. Disable the button until a selection exists (workstream 2).
- Do not offer complete on spam or completed rows. A mixed selection reports the skipped rows in the summary rather than failing the whole batch.
- Keep the thread-page **Mark complete** action. This plan adds a queue path; it does not remove the detail path.

**Tests:** workflow test that bulk complete of a `new` thread writes both events and exactly one archive job; an `in_progress` thread writes one completion event and one job; spam, completed, and inaccessible threads are skipped and counted. Web test that the New list contains a complete control and that the redirect preserves status, assignee, account, and search. Empty selection redirects with a flash.

## Out of scope

- Changing the state machine's allowed direct transitions.
- Bulk "Not spam" / `restore_from_junk`.
- Per-row assignee dropdowns.
- Drafts, remote images, reply formatting, pagination, and mobile tab overflow.

## Verification

```bash
golangci-lint run ./...
go test -v -race ./...
```

Then re-check in the browser at 1440×900 and 390×844: age column fully visible, bulk buttons disabled at zero selected, Gina's thread has no history section, Bob's history is a row, and a new thread can be completed from the New tab without opening it.

**Commit style:** Conventional Commits, one commit per workstream, for example `fix(inbox): stop bulk actions with an empty selection`, `feat(inbox): complete conversations from the queue`.
