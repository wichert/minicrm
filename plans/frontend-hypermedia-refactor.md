# Plan — Frontend Hypermedia Refactor: adopt htmx, modularize app.js, embed templates

**Status:** Partially complete — WS-C (template embed) and WS-A (htmx swap) landed; WS-B/WS-D pending
**Date:** 2026-10-30
**Scope:** Browser UI only (`internal/web/`, `frontend/`, `docs/`). No changes to workflow state machine, IMAP/SMTP workers, storage, auth, or security middleware — all invariants in `AGENTS.md` are untouched.
**Motivation:** The frontend is architecturally correct (SSR + progressive enhancement, no SPA) but currently hand-implements hypermedia swapping (`swapResults()` in `app.js`) while the codebase and `docs/architecture.md` claim htmx is in use. The `HX-Request` partial path exists but has no real client. This plan finishes that migration, modularizes the JS, and moves templates out of Go string literals.

---

## 1. Problem (current state, verified)

1. **Half-htmx with no htmx.** `handlers_threads.go:721` branches on `HX-Request: true`; `views.RenderInbox(..., partialOnly)` (`views.go:834-838`) renders `inboxPartTmpl` (table only). Nothing in the browser sends that header. The real auto-refresh is `swapResults()` (`app.js:~660-717`): full-page `GET`, `DOMParser`, then hand-copies three regions (`[data-inbox-results]`, `.results-count`, `.tabs`).
2. **Hand-rolled DOM lifecycle.** After every swap, JS must re-run `setupRowLinks()`, `setupRowMenus()`, `setupBulkForm()`, `setupTabStrips()`, `refreshBulkSelection()`, re-apply checked thread IDs + `data-selection-mode` + split-pane scroll, and guard re-binding with idempotency flags (`menu.dataset.rowMenuReady`, `tab.dataset.tabFocusBound`, `strip.dataset.tabStripReady`). ~200–300 lines exist only because there is no swap manager.
3. **Misleading naming/docs.** `data-hx-get` attributes (`views.go:1109,1122,1132`), `HX-Request` handling, and `docs/architecture.md` "Browser UI (SSR + HTMX)" all describe a library that is not shipped. `docs/ux-review.md:104` already flagged this confusion once.
4. **Templates are Go string constants.** 1,770 lines of markup in `views.go`, concatenated via `template.Must(Parse(a + b + c))` (`views.go:824-830`). No syntax highlighting, no editor tooling.
5. **`app.js` is one 1,246-line untested file.** `frontend/package.json` has only a `build` script. The draft-autosave module (revision/409-conflict/`beforeunload`), refresh pause logic, and keyboard nav have zero tests, while the markup contract is covered by `web_test.go` + 6 other web test files.

## 2. Goal & principles

- Adopt **htmx 2.x** as the swap manager; delete the hand-rolled swapping and re-bind system.
- htmx is a general hypermedia layer, not only a swap tool (`hx-post`/`hx-put` on any element, `hx-boost` for anchors/forms, `hx-push-url` history integration, `HX-Trigger` server-driven events, `hx-indicator`, SSE/WebSocket extensions). This plan deliberately adopts only declarative GET swaps plus OOB fragments, so every mutating interaction keeps its plain-form no-JS fallback.
- Keep every interaction working **without JavaScript** (plain `<form method="get/post">` everywhere; `<noscript>` Filter/Assign buttons stay).
- Keep custom JS for what is genuinely app-specific: draft autosave, refresh-pause gating (`isPaused()`), pull-to-refresh, keyboard nav, iframe resize/`inert`/scroll-restore, dialog confirm, Cmd/Ctrl+Enter send.
- No new build tooling beyond what exists (esbuild for the editor bundle). htmx ships as a vendored static file — no npm dependency.
- CSP stays `script-src 'self'` (htmx is attribute-driven; no `unsafe-inline`, no `unsafe-eval` for our usage).

## 3. Non-goals

- No SPA framework (Vue/React). No JSON API beyond the existing draft `GET/PUT`.
- No rewrite of the ProseMirror editor bridge (`frontend/editor.js` → `internal/web/static/editor.js`).
- No visual/CSS redesign.
- Keep the `<dialog data-confirm>` confirmation module (better UX than htmx's `hx-confirm`/`window.confirm`).
- No SSE/WebSocket extension and no `hx-boost` of POST forms in this iteration: the 60 s poll remains the refresh contract. SSE is a documented candidate follow-up if instant inbox updates are ever required.

---

## 4. Workstreams

### WS-A — Vendor htmx and wire the inbox list (the core swap)

**A1. Vendor the library.**
- Add `htmx.min.js` (htmx 2.x, ~14 KB gz) to `internal/web/static/`, referenced from `layoutHeader` after `app.js`'s `<script defer>` position is decided (htmx first, then modules). Verify `SecurityHeadersMiddleware` (`security_headers.go:13-15`) needs no change (`script-src 'self'` already allows it).
- Add a build/pin note in `docs/architecture.md` and hash-pin the download in `docs/release.md`-style verification (record the SHA-256 in the commit message or a `static/vendor/README.md`).

**A2. Server partials become the real contract.**
- Extend `inboxPartTmpl` composition (`views.go:825`) so `RenderInbox(partialOnly)` returns the table **plus out-of-band fragments** for `.results-count` and `.tabs` (wrapped in `hx-swap-oob="true"`), eliminating the JS-side region copying.
- Keep the existing `HX-Request` branch in `handlers_threads.go:721`; it becomes load-bearing instead of vestigial. Add tests asserting the partial response contains both the table and the OOB fragments (extend `web_test.go:1330`).

**A3. Client: declare the swap.**
- Replaced `swapResults()`/`refresh()`/`paintStatus()` polling loop with the region partial (implemented in `inbox_results.html`):
  ```html
  <div data-inbox-results id="inbox-results-region"
       hx-get="{{.PartialURL}}"
       hx-trigger="every 60s, refresh from:body"
       hx-target="this"
       hx-swap="outerHTML"
       hx-push-url="false">
  ```
  plus `hx-swap-oob="true"` on the count and tab-strip fragments in the same response.
- **Eval-free gating (deviation from the original sketch, and a CSP requirement):** trigger *filters* (`[cond]`) are compiled with `new Function` and are blocked by `script-src 'self'` (`security_headers.go:13-15`). The trigger is therefore unconditional (`every 60s, refresh from:body`) and the pause logic moved into a `htmx:beforeRequest` listener that calls `event.preventDefault()` when `isPaused()` returns true. `window.htmx` is never handed an expression to evaluate, so `htmx.config.allowEval` stays at its default `false`. Keep the "Updated Xm ago" label logic in JS, repainted on a 30 s interval as today.
- Pull-to-refresh triggers the same swap via `htmx.trigger(document.body, "refresh")`; the manual Refresh link becomes `hx-trigger="click"` on the anchor (keep the plain `href` for no-JS).
- Selection preservation across swaps: re-apply checked IDs via `htmx.on("htmx:afterSwap")` — one hook replaces `setupRowLinks/setupRowMenus/setupBulkForm/refreshBulkSelection` re-runs (htmx re-initializes new content automatically; delete the `dataset.*Ready` idempotency flags).
- Delete: `swapResults`, hand region-copying, per-swap re-setup calls, guard flags. Estimated −250 lines from `app.js`.

**A4. Filters and forms go declarative.**
- `data-hx-get` selects (`views.go:1109,1122,1132`) become real htmx: `hx-get="{{.Action}}" hx-target="#inbox-results" hx-include="closest form" hx-push-url="true"`. Keep `<noscript>` submit buttons.
- Transition/assign/reopen forms keep **plain POST + full navigation** (they already redirect-with-flash correctly; htmx adds nothing). Only the *list region* swaps.
- Keep the `data-confirm` `<dialog>` flow by leaving those forms as normal forms; do not convert them to htmx (documented decision, §2 principle 6).

**Implemented (landed):**
- `internal/web/static/htmx.min.js` is htmx **2.0.4** (50,917 bytes, SHA-256 `e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447`), pinned with provenance in `internal/web/static/vendor/README.md`; `sw.js` precaches it and bumps to `minicrm-shell-v3`.
- New templates `inbox_tabs.html` and `inbox_results.html` hold the shared tab strip, results count, bulk bar, table, footer and the swapped `inbox_results_region` / `inbox_results_fragment` definitions, so the inbox and the split pane render the list from one definition (`{{if .PaneMode}}` drops the bulk bar and prefixes ids with `split-`). `RenderInbox` lost its `partialOnly` parameter; `RenderInboxResults` renders the fragment (region + OOB count + OOB tabs).
- The split pane needed its own fragment endpoint (the pane's old `RefreshURL` was the *detail* document, so an htmx poll would have swapped a whole page into the `<aside>`): new `GET /threads/{id}/list` → `HandleThreadList`, rendering the pane region for `HX-Request` and `303`-redirecting plain navigations to the thread page. `InboxViewData` gained `PartialURL` (swap target) and `FilterURL` (filter form action) so the inbox and the pane each poll/filter their own endpoint.
- The redirect is built from the route uuid plus an allowlist of filter params (`inboxFilterQuery`) rather than the raw request URI, so `GET //other.example/...` cannot become an open redirect (gosec G710).
- Selection survives swaps through one `htmx:afterSwap` hook that re-applies checked ids, `data-selection-mode`, row links/menus, the bulk form and the tab strips; `htmx:beforeRequest` also captures the selection before the swap. `hx-get` on the filter selects is real now (plain form + `<noscript>` submit retained). Transition/assign/reopen/reply forms stay plain POSTs, and the `data-confirm` `<dialog>` flow is untouched.
- Tests: `internal/web/web_htmx_test.go` covers the fragment shape (region + OOB, no page chrome), progressive filter markup and script order, the pane endpoint (fragment, 303 redirect, 404 unknown thread, 404 inaccessible account), the pane polling its own endpoint, and the non-htmx redirect target.

**Acceptance:** 60 s auto-refresh, manual refresh, pull-to-refresh, filter changes, and pagination all swap only the list region; selection, scroll position, and open menu/dialog state survive swaps; all of it still works with JS disabled. Go tests cover partial rendering incl. OOB fragments.

### WS-B — Split app.js into ES modules and test the critical logic

**B1. Split** into `frontend/src/` modules bundled by the existing esbuild setup (`frontend/package.json` build extended to bundle `app.js` → `internal/web/static/app.js`):
- `src/inbox-refresh.js` — `isPaused()`, status label, htmx hooks (WS-A client glue)
- `src/bulk-selection.js` — bulk bar recount/confirm-text updates (kept: it is selection state, not swapping)
- `src/draft.js` — autosave, revision/409 conflict, `beforeunload` guard (unchanged logic)
- `src/frames.js` — iframe autoresize, `data-load-images`, `inert` toggle, split-pane scroll restore
- `src/keys.js` — j/k/x/c/s// keyboard nav
- `src/chrome.js` — tab strips, row menus, row links, flash dismiss, reply sheet, pull-to-refresh
- Public behavior must be unchanged; this is a mechanical split + htmx integration, not a rewrite.

**B2. Test the data-loss-critical module.** Add `vitest` to `frontend/` (test-only dev dependency) with jsdom tests for `draft.js`:
- 409 → conflicted state, submit blocked, message shown
- `beforeunload` prevented while dirty/saving, not while `sending`
- debounce/discard/revision-update flow
Stretch: one Playwright smoke in `e2e/` (the dev-oidc harness exists) asserting a swap preserves a checked row.

**Acceptance:** `npm run build` produces `app.js` + `editor.js`; `npm test` passes; no behavior change observed manually across inbox, split view, detail, settings.

### WS-C — Templates to `go:embed` files

**C1.** Move every template constant in `views.go` to `internal/web/views/templates/*.html` — one file per constant: `layout_header.html`, `layout_footer.html`, `inbox_table.html`, `inbox_filter_form.html`, `inbox_body.html`, `thread_detail.html`, `error_body.html`, `settings_body.html`, `settings_audit_table.html`, `activity_body.html`, `email_body.html` — embedded via `//go:embed templates/*.html`.
**C2.** Keep the concatenation composition (NOT `ParseFS`, which would make each file an associated template and break the page skeleton): `mustTemplates(name, files...)` in `views/templates.go` reads the named files, strips one trailing newline each, and calls `template.Must(template.New(name).Funcs(templateFuncs).Parse(...))`. Files whose constant was `{{define ...}}`-wrapped keep their `{{define}}`; plain layout files are concatenated in order. Rendered output must be byte-identical (verified). `views.go` keeps the data structs, helpers, and `Render*` functions; it lost the markup (1,771 → 902 lines).
**C3.** Existing template-rendering tests must pass unchanged, plus `views/templates_test.go` asserting every embedded file is referenced by a template set and that the partial sets define `inbox_table` / `inbox_filter_form` / `settings_audit_table`.

**Acceptance:** `golangci-lint run ./...` and `go test -race ./...` green; templates editable as `.html`.

### WS-D — Truth in documentation

**D1.** Update `docs/architecture.md`: browser UI = "SSR + htmx 2.x + progressive-enhancement ES modules"; refresh contract; list of what remains custom JS.
**D2.** Update `docs/security.md` if any CSP/`hx-` interaction needs mention (none expected; verify `script-src 'self'` suffices).
**D3.** Delete stale references: `data-hx-get` shim notes in `docs/ux-review.md` stay (historical record) but `architecture.md` must no longer claim HTMX while not shipping it.

---

## 5. Sequencing & dependencies

```
WS-C (embed templates)      ─┐  independent, do first (touches the same
                               │  files WS-A edits — avoids rebasing churn)
WS-A (htmx swap)            ─┤  depends on C for clean diffs
WS-B (module split + tests) ─┤  partially overlaps A's app.js edits; do after A lands
WS-D (docs)                 ─┘  last, reflects reality
```

Suggested commits (Conventional Commits, per `docs/release.md`):

1. `refactor(web): move templates to go:embed html files` (WS-C) — **landed** (e4a0a4c)
2. `feat(web): adopt htmx for inbox list swaps and filters` (WS-A) — **landed**
3. `refactor(web): split app.js into modules and unit-test draft autosave` (WS-B)
4. `docs: document htmx-based frontend architecture` (WS-D)

Each step leaves the app fully working and shippable.

## 6. Risks & mitigations

| Risk | Mitigation |
|---|---|
| htmx trigger conditions (`[canAutoRefresh()]`) need eval or globals | Register `canAutoRefresh` on `window` from the bundle; keep the complex pause logic in reviewed JS, not attribute soup |
| OOB fragment composition breaks if `.tabs`/`.results-count` markup drifts between full and partial templates | Both come from the same Go template definitions; add a test asserting the partial contains OOB markers |
| Draft module regressions during the split | Ported verbatim first (B1), then unit-tested (B2); no logic edits in the same commit |
| htmx swaps can destroy open `<details>` menus / dialogs mid-interaction | `isPaused()` gating preserved; `htmx:beforeSwap` hook cancels when paused |
| Scroll/selection restoration regressions in split pane | Playwright smoke + manual matrix: 1280 px split, <768 px mobile, no-JS |
| Vendored file drift/security | Pin version, record SHA-256, note update procedure in `static/vendor/README.md` |

## 7. Verification

Per AGENTS.md, before each commit:

```bash
golangci-lint run ./...
go test -v -race ./...
```

Plus: `cd frontend && npm run build && npm test`, and the e2e suite (`go test ./e2e/...` with dev-oidc) for the auth+swap smoke.
