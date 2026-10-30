/* Thread rows: click-anywhere navigation and the per-row menu. Both are re-bound
   after a list swap, so setup is callable; the document-level menu handlers are
   bound once because they resolve open menus at event time. */

export function setupRowLinks(root = document) {
  root.querySelectorAll("tr[data-row-href]").forEach((row) => {
    row.addEventListener("click", (event) => {
      if (event.target.closest("a, button, input, select, label, summary, details")) {
        return;
      }
      if (window.getSelection && String(window.getSelection())) {
        return;
      }
      // Navigate through the row's own anchor instead of assigning a string read
      // from the DOM: the destination stays a URL the browser resolved from the
      // rendered markup, and no script-visible string is ever treated as a URL.
      const link = row.querySelector("a.thread-link[href]");
      if (link) {
        link.click();
      }
    });
  });
}

let documentReady = false;

export function setupRowMenus(root = document) {
  root.querySelectorAll("details[data-row-menu]").forEach((menu) => {
    if (menu.dataset.rowMenuReady === "true") {
      return;
    }
    menu.dataset.rowMenuReady = "true";
    menu.addEventListener("toggle", () => {
      if (!menu.open) {
        return;
      }
      document.querySelectorAll("details[data-row-menu][open]").forEach((other) => {
        if (other !== menu) {
          other.open = false;
        }
      });
    });
  });
  if (documentReady) {
    return;
  }
  documentReady = true;
  document.addEventListener("click", (event) => {
    document.querySelectorAll("details[data-row-menu][open]").forEach((menu) => {
      if (!menu.contains(event.target)) {
        menu.open = false;
      }
    });
  });
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape") {
      return;
    }
    document.querySelectorAll("details[data-row-menu][open]").forEach((menu) => {
      const summary = menu.querySelector("summary");
      menu.open = false;
      if (summary) {
        summary.focus();
      }
    });
  });
}
