/* Keyboard navigation: j/k move the active row, Enter/o open it, x toggles
   selection, c completes, s marks spam, / focuses search, Escape clears the
   active row. Bound once at document level; the active row is tracked via a class
   re-resolved after every fragment swap, so new rows keep working. */

let kbActiveRow = null;

function kbRows() {
  return Array.prototype.slice.call(document.querySelectorAll("tr[data-row-href]"));
}

function kbSetActive(row) {
  if (kbActiveRow) {
    kbActiveRow.classList.remove("kb-active");
  }
  kbActiveRow = row || null;
  if (kbActiveRow) {
    kbActiveRow.classList.add("kb-active");
    if (typeof kbActiveRow.scrollIntoView === "function") {
      kbActiveRow.scrollIntoView({ block: "nearest" });
    }
  }
}

export function setupKeyboardNav() {
  document.addEventListener("keydown", (event) => {
    if (event.defaultPrevented || event.altKey || event.ctrlKey || event.metaKey) {
      return;
    }
    const target = event.target;
    // Esc inside the search field clears and blurs it instead of only clearing
    // the active row.
    if (event.key === "Escape") {
      const searchField = target.closest(".filter-inline input[type=search]");
      if (searchField) {
        event.preventDefault();
        searchField.value = "";
        searchField.blur();
        return;
      }
    }
    // Focused links, buttons and summaries keep their own Enter/Space semantics:
    // never hijack activation keys away from interactive targets (e.g. the "Load
    // older" link or a row-menu trigger).
    if (target.closest("input, textarea, select, [contenteditable], dialog, details[data-row-menu], a, button, summary, [href]")) {
      return;
    }
    const rows = kbRows();
    if (!rows.length) {
      return;
    }
    if (event.key === "/") {
      const search = document.querySelector(".filter-inline input[type=search]");
      if (search) {
        event.preventDefault();
        search.focus();
      }
      return;
    }
    if (event.key === "Escape") {
      if (kbActiveRow) {
        kbSetActive(null);
      }
      return;
    }
    const index = kbActiveRow ? rows.indexOf(kbActiveRow) : -1;
    if (event.key === "j" || event.key === "k") {
      event.preventDefault();
      let next = event.key === "j" ? Math.min(index + 1, rows.length - 1) : Math.max(index - 1, 0);
      if (index < 0 && event.key === "k") {
        next = rows.length - 1;
      }
      kbSetActive(rows[next]);
      return;
    }
    if (!kbActiveRow) {
      return;
    }
    if (event.key === "Enter" || event.key === "o") {
      event.preventDefault();
      const link = kbActiveRow.querySelector("a.thread-link[href]");
      if (link) {
        link.click();
      }
      return;
    }
    if (event.key === "x") {
      event.preventDefault();
      const check = kbActiveRow.querySelector("input[data-select-thread]");
      if (check && !check.disabled) {
        check.checked = !check.checked;
        check.dispatchEvent(new Event("change", { bubbles: true }));
      }
      return;
    }
    if (event.key === "c" || event.key === "s") {
      const status = event.key === "c" ? "completed" : "spam";
      // Prefer the matching quick-action form; the menu forms are equivalent.
      const transitionForms = kbActiveRow.querySelectorAll('form[action$="/transition"]');
      let chosen = null;
      transitionForms.forEach((f) => {
        const input = f.querySelector('input[name="status"]');
        if (input && input.value === status && (!chosen || f.closest(".row-quick-actions"))) {
          chosen = f;
        }
      });
      if (chosen) {
        event.preventDefault();
        const btn = chosen.querySelector("button[type=submit]");
        if (btn) {
          btn.click();
        } else {
          // requestSubmit runs the submit event pipeline (confirm dialogs,
          // validation); a raw submit() call would bypass them.
          chosen.requestSubmit();
        }
      }
    }
  });

  // Re-resolve the active row after fragment swaps: re-match by thread id so the
  // highlight survives auto-refresh swaps.
  const resultsEl = document.querySelector("[data-inbox-results]");
  if (resultsEl && typeof MutationObserver === "function") {
    new MutationObserver(() => {
      if (!kbActiveRow || !kbActiveRow.isConnected) {
        return;
      }
      let fresh = null;
      const check = kbActiveRow.querySelector("input[data-select-thread]");
      const id = check ? check.value : null;
      if (id) {
        const found = document.querySelector('tr[data-row-href] input[data-select-thread][value="' + id + '"]');
        fresh = found ? found.closest("tr[data-row-href]") : null;
      } else {
        // Rows without a select checkbox (completed/spam tabs) are re-matched by
        // their row href so the highlight survives auto-refresh swaps.
        const href = kbActiveRow.getAttribute("data-row-href");
        if (href) {
          fresh = kbRows().find((row) => row.getAttribute("data-row-href") === href) || null;
        }
      }
      kbSetActive(fresh);
    }).observe(resultsEl, { childList: true });
  }
}
