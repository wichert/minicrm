/* Inbox list freshness.

   The swap itself belongs to htmx: the results region carries
   hx-trigger="every 60s, refresh from:body" and hx-swap="outerHTML", and the
   server answers with the region plus out-of-band updates for the results count
   and the status tabs. This module owns only what htmx cannot know:

     - isPaused(): when a swap would destroy work in progress,
     - preserving a bulk selection across the swap,
     - the "Updated 3m ago" label.

   Why not a trigger filter such as every 60s [canRefresh()]: htmx compiles
   trigger filters with new Function, and the CSP is script-src 'self' with no
   unsafe-eval, so such a filter would be blocked. Gating in htmx:beforeRequest
   is eval-free and identical in effect. */

import { resultsContainer, rerenderSelection, setupBulkForm } from "./selection.js";
import { setupRowLinks, setupRowMenus } from "./rows.js";
import { setupTabStrips } from "./tabs.js";

export function hasPendingInlineEdit(doc = document) {
  const active = doc.activeElement;
  if (!active) {
    return false;
  }
  const tag = active.tagName;
  if (tag === "TEXTAREA" || tag === "SELECT") {
    return true;
  }
  if (tag === "INPUT") {
    const type = (active.getAttribute("type") || "text").toLowerCase();
    if (type === "checkbox" || type === "radio" || type === "submit") {
      return false;
    }
    return String(active.value) !== String(active.defaultValue || "");
  }
  return Boolean(active.isContentEditable);
}

/* A swap must never destroy work in progress. Pause while the tab is hidden,
   text is selected, a row menu or confirm dialog is open, any thread checkbox is
   checked, focus is inside the results region, or an inline edit is dirty. */
export function isPaused(doc = document, win = window) {
  if (doc.visibilityState === "hidden") {
    return true;
  }
  if (win.getSelection && String(win.getSelection())) {
    return true;
  }
  if (doc.querySelector("details[data-row-menu][open]")) {
    return true;
  }
  if (doc.querySelector("dialog[open]")) {
    return true;
  }
  if (doc.querySelector("[data-select-thread]:checked")) {
    return true;
  }
  const results = resultsContainer();
  // Never yank the DOM out from under a focused row action or menu trigger.
  if (results && results.contains(doc.activeElement)) {
    return true;
  }
  return hasPendingInlineEdit(doc);
}

export function checkedThreadIDs(doc = document) {
  return Array.from(doc.querySelectorAll("[data-select-thread]:checked")).map((check) => check.value);
}

export function formatAgo(ms) {
  const seconds = Math.max(0, Math.round(ms / 1000));
  if (seconds < 45) {
    return "Updated just now";
  }
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) {
    return "Updated " + minutes + "m ago";
  }
  const hours = Math.round(minutes / 60);
  return "Updated " + hours + "h ago";
}

export function setupInboxAutoRefresh() {
  const results = resultsContainer();
  const statusEl = document.querySelector("[data-refresh-status]");
  const refreshLink = document.querySelector("[data-refresh-link]");
  const refresh = { trigger: null, repaint: null };
  if (!results) {
    return refresh;
  }
  let lastLoadedAt = Date.now();
  let selectionMode = null;
  let pendingSelection = null;

  // The split pane shows a quiet "Refresh" affordance and no age label: the list
  // refreshes itself and a ticking "Updated just now" beside a queue the user is
  // reading is noise, not information.
  const statusInSplitPane = Boolean(statusEl && statusEl.closest("[data-split-list]"));

  function paintStatus() {
    if (!statusEl || statusInSplitPane) {
      return;
    }
    statusEl.textContent = formatAgo(Date.now() - lastLoadedAt);
  }

  /* Preserving the selection across the swap is mandatory even though isPaused()
     normally prevents a swap while rows are checked. htmx replaces the region
     wholesale, so re-apply the checked ids that still exist afterwards, keep
     selection mode, then recount the bulk bar. */
  function restoreSelection() {
    const region = resultsContainer();
    if (!region) {
      return;
    }
    if (selectionMode) {
      region.setAttribute("data-selection-mode", selectionMode);
    }
    const selected = pendingSelection;
    pendingSelection = null;
    if (selected && selected.length) {
      const wanted = new Set(selected);
      region.querySelectorAll("[data-select-thread]").forEach((check) => {
        if (wanted.has(check.value)) {
          check.checked = true;
        }
      });
    }
    setupRowLinks(region);
    setupRowMenus(region);
    setupBulkForm(region);
    setupTabStrips(document.querySelector(".inbox-toolbar"));
    rerenderSelection();
  }

  // Explicit refresh (Refresh link, pageshow, pull-to-refresh) asks htmx to re-run
  // the region's own hx-get. Without JS the link is a plain anchor.
  function trigger(refreshTarget) {
    const region = refreshTarget || resultsContainer();
    if (!region || !window.htmx) {
      return false;
    }
    if (isPaused()) {
      return false;
    }
    window.htmx.trigger(region, "refresh");
    return true;
  }
  refresh.trigger = trigger;
  refresh.repaint = paintStatus;

  if (window.htmx) {
    // htmx has no way to know that a swap would destroy work in progress, so the
    // gate lives here. preventDefault() on the event cancels the request, which
    // is exactly the semantics a trigger filter would have had.
    document.body.addEventListener("htmx:beforeRequest", (event) => {
      const elt = event.detail && event.detail.elt;
      const region = resultsContainer();
      if (!region || !elt || !(elt === region || region.contains(elt))) {
        return;
      }
      if (isPaused()) {
        event.preventDefault();
        return;
      }
      // Capture the selection and selection mode before the region is replaced:
      // htmx swaps the element's subtree wholesale, so this is the only chance.
      pendingSelection = checkedThreadIDs();
      selectionMode = region.getAttribute("data-selection-mode");
    });
    document.body.addEventListener("htmx:afterSwap", (event) => {
      const region = resultsContainer();
      if (!region || !event.detail || event.detail.target !== region) {
        return;
      }
      lastLoadedAt = Date.now();
      paintStatus();
      restoreSelection();
    });
  }

  if (refreshLink) {
    refreshLink.addEventListener("click", (event) => {
      if (!window.htmx) {
        return; // no hypermedia layer: let the anchor navigate
      }
      event.preventDefault();
      trigger();
    });
  }

  paintStatus();
  // The 60s poll itself is htmx's (hx-trigger="every 60s" in the markup); this
  // only keeps the age label ticking.
  window.setInterval(paintStatus, 30000);
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") {
      paintStatus();
    }
  });
  // A restored back/forward cache entry revalidates the list once.
  window.addEventListener("pageshow", (event) => {
    if (event.persisted && !isPaused()) {
      trigger();
    }
  });
  return refresh;
}

/* Pull-to-refresh on touch devices, on top of the browser's own overscroll
   behaviour (which reloads the whole page and loses scroll position). Only
   engages at the very top of the document, ignores gestures that start in a
   paused context, and never fires while a refresh is already in flight. */
export function setupPullToRefresh(refresh) {
  if (!("ontouchstart" in window) || !resultsContainer()) {
    return;
  }
  // The pull feedback uses its own indicator, not [data-refresh-status]: the
  // results-status line is hidden below 768px, so the gesture would otherwise be
  // invisible on a phone.
  const indicator = document.querySelector("[data-pull-indicator]");
  let startY = null;
  let pulled = 0;
  const threshold = 70;
  let hideTimer = null;

  function showPull(label) {
    if (!indicator) {
      return;
    }
    clearTimeout(hideTimer);
    if (!label) {
      delete indicator.dataset.active;
      indicator.textContent = "";
      return;
    }
    indicator.dataset.active = "true";
    indicator.textContent = label;
  }

  document.addEventListener(
    "touchstart",
    (event) => {
      if (window.scrollY > 0 || event.touches.length !== 1 || event.target.closest("dialog, details[data-row-menu]")) {
        startY = null;
        return;
      }
      startY = event.touches[0].clientY;
      pulled = 0;
    },
    { passive: true }
  );

  document.addEventListener(
    "touchmove",
    (event) => {
      if (startY === null || event.touches.length !== 1) {
        return;
      }
      pulled = event.touches[0].clientY - startY;
      if (pulled > 0 && window.scrollY === 0) {
        showPull(pulled >= threshold ? "Release to refresh" : "Pull to refresh");
      }
    },
    { passive: true }
  );

  document.addEventListener(
    "touchend",
    () => {
      const shouldRefresh = startY !== null && pulled >= threshold && window.scrollY === 0;
      startY = null;
      pulled = 0;
      if (shouldRefresh) {
        showPull("Refreshing…");
        if (refresh.trigger) {
          refresh.trigger();
        }
        hideTimer = setTimeout(() => {
          showPull("");
        }, 1200);
      } else {
        showPull("");
        if (refresh.repaint) {
          refresh.repaint();
        }
      }
    },
    { passive: true }
  );
}
