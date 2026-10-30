/* Split-pane list: inertness below the 1280px breakpoint and scroll restoration.
   The pane is rendered server-side on every detail load and htmx swaps its
   contents, so both concerns read the pane node at call time. */

const splitListScrollKey = "minicrm:split-list-scroll:";

function splitListScrollStorageKey(win = window) {
  // Key on the filter query only; the thread id lives in the path, so switching
  // threads within the same queue keeps the same key.
  return splitListScrollKey + (win.location.search || "");
}

export function saveListScroll(doc = document, win = window) {
  const pane = doc.querySelector("[data-split-list]");
  if (!pane) {
    return;
  }
  try {
    win.sessionStorage.setItem(splitListScrollStorageKey(win), String(pane.scrollTop || 0));
  } catch {
    // Private mode or a full quota: scroll restore is a nicety, never fatal.
  }
}

export function restoreListScroll(doc = document, win = window) {
  const pane = doc.querySelector("[data-split-list]");
  if (!pane) {
    return;
  }
  // Staging rather than writing scrollTop directly: the pane may still be
  // display:none (narrow viewport), and a hidden element cannot be scrolled. The
  // staged value is consumed in setupSplitListScroll once the pane is visible.
  try {
    const saved = win.sessionStorage.getItem(splitListScrollStorageKey(win));
    if (saved !== null) {
      win.sessionStorage.removeItem(splitListScrollStorageKey(win));
      pane.setAttribute("data-restore-scroll", saved);
    }
  } catch {
    // Ignore storage failures.
  }
}

export function setupSplitListScroll(doc = document, win = window) {
  const pane = doc.querySelector("[data-split-list]");
  if (!pane) {
    return;
  }
  // Persist before leaving for a thread, and whenever the tab is hidden.
  pane.addEventListener("click", (event) => {
    if (event.target.closest("a[href]")) {
      saveListScroll(doc, win);
    }
  });
  win.addEventListener("pagehide", () => saveListScroll(doc, win));

  // Restore the remembered offset once the pane is actually visible. A load that
  // happens while the pane is hidden (narrow) leaves the flag set so a later
  // resize to wide — the matchMedia change branch — can consume it.
  function consumeRestoreFlag() {
    const pending = pane.getAttribute("data-restore-scroll");
    if (pending !== null && pane.offsetParent !== null) {
      pane.removeAttribute("data-restore-scroll");
      pane.scrollTop = Number(pending) || 0;
    }
  }
  if (win.matchMedia) {
    const scrollQuery = win.matchMedia("(min-width: 1280px)");
    if (scrollQuery.matches) {
      consumeRestoreFlag();
    } else {
      const onScrollQueryChange = (event) => {
        if (event.matches) {
          consumeRestoreFlag();
        }
      };
      if (typeof scrollQuery.addEventListener === "function") {
        scrollQuery.addEventListener("change", onScrollQueryChange);
      } else if (typeof scrollQuery.addListener === "function") {
        scrollQuery.addListener(onScrollQueryChange);
      }
    }
  }
}

/* The pane lives in the DOM on every thread detail page, but CSS only shows it at
   >=1280px. Below the breakpoint the hidden pane must not be reachable, so
   `inert` (or an aria-hidden/tabindex fallback) is toggled from a matchMedia
   listener: firing on change means an in-place resize across 1280px updates it
   without a reload. Pure attribute writes: CSP forbids inline styles/scripts. */
export function setupSplitListInert(doc = document, win = window) {
  const pane = doc.querySelector("[data-split-list]");
  if (!pane || !win.matchMedia) {
    return;
  }
  const query = win.matchMedia("(min-width: 1280px)");
  function apply() {
    if (query.matches) {
      if (pane.inert) {
        pane.inert = false;
      } else {
        pane.removeAttribute("inert");
        pane.removeAttribute("aria-hidden");
        pane.querySelectorAll("[data-inert-tabindex]").forEach((el) => {
          el.tabIndex = Number(el.getAttribute("data-inert-tabindex"));
          el.removeAttribute("data-inert-tabindex");
        });
      }
      pane.removeAttribute("data-inert-pane");
      // Same DOM node across a resize, so scrollTop survives natively here; only a
      // fresh document load needs its saved offset back.
      restoreListScroll(doc, win);
    } else {
      pane.setAttribute("data-inert-pane", "true");
      if ("inert" in pane) {
        pane.inert = true;
      } else {
        pane.setAttribute("aria-hidden", "true");
        pane.querySelectorAll("a[href], button, input, select, textarea, summary, [tabindex]").forEach((el) => {
          if (!el.hasAttribute("data-inert-tabindex")) {
            el.setAttribute("data-inert-tabindex", String(el.tabIndex));
            el.tabIndex = -1;
          }
        });
      }
      // The back link's visibility is handled entirely by CSS (hidden >=1280px).
    }
  }
  apply();
  if (typeof query.addEventListener === "function") {
    query.addEventListener("change", apply);
  } else if (typeof query.addListener === "function") {
    query.addListener(apply);
  }
}
