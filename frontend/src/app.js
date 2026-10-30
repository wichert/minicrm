/* MiniCRM progressive enhancement entry point.

   Everything here is optional polish: without JavaScript the app still works
   because every interaction is a plain form or link. The pieces are split by
   concern so the critical one — draft autosave with its conflict and unload
   guards — can be unit-tested in isolation (frontend/test/draft.test.js).

   The list swap itself is htmx's job (see inbox-refresh.js). */

import { setupDraftForms } from "./draft.js";
import { currentBulkForm, setupBulkForm, setupLongPress } from "./selection.js";
import { setupRowLinks, setupRowMenus } from "./rows.js";
import { setupTabStrips, setupTabOverflowResize } from "./tabs.js";
import { setupInboxAutoRefresh, setupPullToRefresh } from "./inbox-refresh.js";
import { setupEmailFrames } from "./frames.js";
import { setupSplitListScroll, setupSplitListInert } from "./split-pane.js";
import { setupKeyboardNav } from "./keys.js";
import { setupReplySheet } from "./reply.js";
import { setupAutoSubmit, setupConfirmDialog, setupFilterAutoSubmit, setupFlashDismiss, setupMessageToggle } from "./chrome.js";

document.addEventListener("DOMContentLoaded", () => {
  // Progressive enhancement flag: bulk-bar and selection-mode styling in app.css
  // is scoped under html.js so the no-JS experience stays as today.
  document.documentElement.classList.add("js");

  setupFilterAutoSubmit();
  setupAutoSubmit();
  setupTabStrips();
  setupTabOverflowResize();
  setupDraftForms();
  setupBulkForm();
  setupConfirmDialog(currentBulkForm);
  setupLongPress();
  setupFlashDismiss();
  setupEmailFrames();
  setupMessageToggle();
  setupReplySheet();
  setupRowLinks();
  setupRowMenus();
  const refresh = setupInboxAutoRefresh();
  setupPullToRefresh(refresh);
  setupSplitListScroll();
  setupSplitListInert();
  setupKeyboardNav();

  if ("serviceWorker" in navigator) {
    window.addEventListener("load", () => {
      navigator.serviceWorker.register("/sw.js").catch(() => {
        // Ignore registration errors in restricted or non-HTTPS dev contexts.
      });
    });
  }
});
