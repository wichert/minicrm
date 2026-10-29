# MiniCRM UX review

## Executive summary

MiniCRM presents a clean, readable inbox and makes the account, customer, status and assignee visible. The biggest gaps are in the everyday work around the message: users cannot act on several threads together, quickly complete a new thread, find a customer's other conversations, or save a reply for later. Spam handling also interrupts triage with a confirmation and leaves the user on the completed action rather than returning to the queue.

**Recommended order:** (1) prevent draft loss and reduce triage friction, (2) make customer history discoverable and the queue navigable beyond its first 100 results, and (3) improve remote-image and reply-formatting support without weakening the existing security model.

## Scope and method

- Heuristic review of the supplied user stories and feedback; this is expert assessment, not a usability study with actual users.
- Opened the seeded development stack in headless Chrome at **1440 × 900** and **390 × 844**. Inspected the inbox and thread detail at both sizes; exercised spam, the first step of completion, and draft navigation in the desktop browser. The mobile viewport is an emulation, not a physical-device or assistive-technology test.
- Inspected the corresponding templates, handlers, workflow and mail code to check capabilities not fully exercised. The development fixture had one operator and ten open threads; it did not establish performance at scale or validate a multi-person handoff. No outbound reply was sent.
- To inspect a blocked image, added a synthetic already-sanitized remote-image placeholder to a development-only email. This demonstrated the rendered placeholder, not the full ingestion path for arbitrary HTML. No production data was used.

**Priority:** P1 = blocks a common task or risks lost work; P2 = substantial friction or missing context; P3 = worthwhile polish. Priorities reflect judgement, not measured usage frequency.

## Findings

### P1 — Triage and spam are one thread at a time

**Evidence.** Neither desktop table nor mobile cards offer selection, bulk actions or row-level assignment. A user must open a new thread, choose **Mark as spam**, confirm **Move to Junk**, and then use **Back to threads**. In the browser the success message, “Thread marked as Spam.”, appeared on the spam thread itself, not the list. Spam disables replies and reassignment, and the current state machine offers no normal transition out of spam. `internal/web/views/views.go` and `internal/web/handlers_threads.go` corroborate the actions and redirect.

**Impact.** Repeated navigation and confirmation make a spam-heavy queue expensive to clear; an accidental classification is difficult to correct. The user's request to return to the list with feedback is justified, but removing all safeguards without a recovery path would be risky.

**Recommendation.** Add accessible multi-select and a bulk **Mark as spam** action, with a selected count, explicit scope and a review/confirmation for the batch. Return to the same filtered list with a persistent success or partial-failure summary; do not imply the IMAP move has completed merely because it was queued. For individual actions, reduce confirmation friction only once a safe, explicit recovery mechanism is designed. Recovery requires a deliberate workflow and IMAP lifecycle change, not just an “Undo” label. Revalidate selection, status and account access on the server and report per-thread failures. Consider a list-level action or next-thread navigation for single-message triage.

### P1 — A new thread cannot be completed directly

**Evidence.** A new thread offers **Start working** and **Mark as spam**, but not completion. In the browser, **Mark complete** appeared only after **Start working**. The existing workflow only permits `new → in_progress → completed`; completion additionally asks for confirmation (`internal/web/views/views.go`, `internal/workflow/service.go`).

**Impact.** A newsletter or test message requires a misleading intermediate step before it can be cleared. Calling that step “Start working” does not match the user's intent.

**Recommendation.** Offer **Mark complete** from a new thread as one user action, while preserving the existing state-machine invariant: implement an intentional, audited composite operation that performs the permitted transitions safely and enqueues the normal archive job. Return to the prior queue with an accurate result. Review the necessity of a per-item confirmation alongside recovery options; do not bypass the workflow or IMAP lifecycle.

### P1 — Unsent replies are lost on navigation

**Evidence.** Entered text in the reply box, navigated back to the inbox and reopened the same thread: the textarea was empty. The interface offers no save-draft control or draft indicator. It can preserve entered text when a reply submission fails or becomes disallowed, but that is not durable draft storage (`internal/web/views/views.go`, `internal/web/handlers_threads.go`).

**Impact.** Research, switching threads, refreshing or leaving the page can destroy work; a user has no reliable way to resume an investigation.

**Recommendation.** Provide explicit **Save draft** plus autosave, visible **Saving / Saved / Could not save** feedback and a draft indicator in the inbox. Restore drafts across navigation and sessions; make ownership and visibility to other authorized staff explicit, and handle concurrent editing rather than silently overwriting. Keep drafts separate from sent mail, assignment and state transitions. Warn before leaving only when unsaved changes remain; account for attachments, failures and retention in the design. Test reload, expired sessions, multiple operators and send/discard paths.

### P2 — Customer history is not discoverable from a conversation

**Evidence.** The seeded inbox contained two separate threads for Bob Builder, including an invoice question and a receipt. The detail header displays his name and email as text, but provides no link to his other requests; the inbox has status, assignee and account filters but no customer search (`internal/web/views/views.go`). Existing message expansion and the special link to a completed thread when a follow-up reopens it are useful, but do not cover unrelated past requests.

**Impact.** Answering accurately requires manual scanning or external tools, especially for older/completed conversations.

**Recommendation.** Add a **Previous conversations** entry from the customer header, with subject, status, date and account, and a searchable customer/email view. Show results only for accounts the current user can access; do not reveal counts or metadata from inaccessible accounts. Clarify matching by email address and allow for aliases and shared addresses rather than implying a perfect customer identity. Keep the current reply and draft intact while consulting history.

### P2 — Assignment requires leaving the queue and its effects are not always clear

**Evidence.** The inbox shows assignee and supports an unassigned filter, but assigning another person requires opening each thread and choosing from its detail dropdown. The review fixture had only Dev Admin, so a real cross-user handoff was not tested. The new-thread reply helper says sending “assigns this thread to you”; the implementation assigns the sender only when the thread is currently unassigned (`internal/mail/smtp_sender.go`).

**Impact.** Front-desk routing becomes a repeated open/assign/back cycle. The unconditional helper can make a user believe an already-assigned conversation will change owner when it will not.

**Recommendation.** Allow account-aware assignment from the queue, including an optional bulk action and clear feedback on partial failures. Preserve assignment when appropriate and state the actual consequence before sending: “If unassigned, this thread will be assigned to you.” Make the distinction between assignment and starting work clear. Enforce existing account-access restrictions for each eligible assignee.

### P2 — The queue can hide older work

**Evidence.** The list is capped at 100 results and displays “first 100; narrow the filters to see more” when the cap is reached; there is no pagination or search in the inbox (`internal/web/handlers_threads.go`, `internal/web/views/views.go`). The cap was verified in source, not with a 100-plus-thread browser fixture.

**Impact.** Filtering by the few available fields may still leave older work unreachable, especially in completed or spam views. A results count is useful but does not give users a way to reach the remaining results.This

**Recommendation.** Add stable pagination or a reliable load-more pattern, total/remaining-result context where practical, and search by customer and subject. Preserve filters and position when returning from a thread. Test large queues and updates occurring during triage.

### P2 — Blocked remote images look broken and offer no explanation

**Evidence.** The synthetic blocked remote image appeared as a broken-image icon and alt text inside the message, with no explanation or load option. The sanitizer rewrites remote image sources and the isolated message frame's CSP blocks external image loading (`internal/mail/mime_parser.go`, `internal/web/views/views.go`). Inline attachments are a separate, supported case.

**Impact.** Brand-heavy messages can be hard to read, and users cannot tell whether content is missing intentionally or due to a failure. The user's desire for an option is reasonable, but automatically loading remote images would expose viewing activity to senders and weaken a deliberate privacy protection.

**Recommendation.** Keep remote images blocked by default. Show an accessible, per-message notice such as “Remote images blocked for your privacy” and an explicit **Load images for this message** control, explaining that the sender may learn the message was opened. First investigate a security-reviewed image proxy, with strict SSRF, redirect, size, type and timeout controls; a direct browser load still discloses the recipient's IP and viewing activity. Preserve sanitization, sandboxing, account authorization and restrictive CSP; do not globally relax them or enable scripts. Provide a clear placeholder even when the user chooses not to load images.

### P2 — Replies are plain text only

**Evidence.** The browser shows a plain textarea without formatting controls; the sender constructs `text/plain` mail (`internal/web/views/views.go`, `internal/mail/smtp_sender.go`).

**Impact.** Users cannot add the requested emphasis when explaining steps or important details.

**Recommendation.** Support a small, accessible formatting set—bold and italic first—with keyboard shortcuts, visible formatting state and a plain-text fallback. If sending HTML, generate a safe, restricted `multipart/alternative` message and ensure received/rendered copies continue through existing isolation and sanitization. Avoid promising Markdown formatting unless it is actually converted and previewed. Integrate formatting with drafts and test pasted content and email-client compatibility.

### P3 — Mobile status tabs are easy to miss

**Evidence.** At 390 px the inbox becomes legible cards and fits the viewport without document-level horizontal overflow. However, the horizontal status strip initially shows Open through Completed, with Spam and All off to the right and little indication that the strip scrolls. Thread actions become large, full-width targets; this is helpful, though the stack pushes message content and composer farther down.

**Impact.** Users may not discover Spam or All and must scroll more for frequent actions on long conversations.

**Recommendation.** Make overflow discoverable with a fade/scroll affordance or an accessible status selector; ensure keyboard focus scrolls the selected tab into view. Consider a compact action layout or a context-aware reply shortcut without reducing touch-target size. Validate on real devices and with keyboard and screen-reader users before selecting a design.

## What works well

- The desktop inbox is scannable: customer, snippet, account, status, assignment and waiting age are visible together. Mobile cards retain the key fields rather than squeezing a desktop table.
- The detail view distinguishes customer messages from team replies, collapses older messages, shows sender/recipient context and provides attachment previews and downloads.
- Account and assignee filters, **Assign to me**, visible success notices, and a back-to-threads control are useful foundations. Account-scoped access and blocking remote images by default are important safety properties to retain.

## Suggested validation

1. Test front-desk users on a realistic mixed queue: assign five messages, mark several as spam, and complete a no-action message. Measure time, navigation count, errors, recovery and whether the original filter is retained.
2. Test drafting across navigation, reload, a second device and two operators; deliberately induce save failure and confirm no silent loss or overwrite.
3. Ask users to find an earlier unrelated request by the same customer, including a customer with threads in multiple accounts; verify restricted users cannot discover inaccessible history.
4. Test image consent and formatted replies with real email clients, privacy/security review, mobile devices, keyboard-only navigation and a screen reader. Include an inbox over 100 results.
