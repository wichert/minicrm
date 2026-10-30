/* Bulk selection: the operations bar, mobile selection mode, and the long-press
   gesture that enters it. Selection state is plain DOM (checkbox.checked plus
   data-selection-mode on the results region), so it survives a fragment swap as
   long as the swap glue re-applies it — see inbox-refresh.js. */

// The bulk form is replaced by every list swap, so the current node is tracked
// in module state instead of a closure captured at load time.
let bulkForm = null;
let refreshSelectionFn = null;

export function currentBulkForm() {
  return bulkForm;
}

export function resultsContainer() {
  return document.querySelector("[data-inbox-results]");
}

export function enterSelectionMode() {
  const container = resultsContainer();
  if (container) {
    container.setAttribute("data-selection-mode", "true");
  }
}

// Recount the current selection after a swap re-applied checked ids.
export function rerenderSelection() {
  if (refreshSelectionFn) {
    refreshSelectionFn();
  }
}

function setBulkControlDisabled(control, disabled) {
  if (!control) {
    return;
  }
  control.disabled = disabled;
  control.setAttribute("aria-disabled", disabled ? "true" : "false");
}

export function setupBulkForm(root) {
  bulkForm = document.getElementById("bulk-spam-form");
  refreshSelectionFn = null;
  if (!bulkForm) {
    return;
  }
  const form = bulkForm;
  // Root-scoped so a region swap re-binds only the fresh rows, never duplicating
  // listeners on the rows that survived (the bulk form itself is replaced too).
  const scope = root || document;
  const checks = Array.from(scope.querySelectorAll("[data-select-thread]"));
  const selectAll = Array.from(scope.querySelectorAll("[data-select-all], [data-select-all-mobile]"));
  const count = form.querySelector("[data-selected-count]");
  const assigneeSelect = form.querySelector("[data-bulk-assignee]");
  const bulkButtons = Array.from(form.querySelectorAll("[data-bulk-action]"));
  const completeButton = form.querySelector('[data-bulk-action="complete"]');
  const spamButton = form.querySelector('[data-bulk-action="spam"]');

  function refreshSelection() {
    const selected = checks.filter((check) => check.checked);
    const openSelected = selected.filter((check) => {
      const st = check.dataset.status;
      return !st || st === "new" || st === "in_progress";
    });
    const accounts = Array.from(new Set(openSelected.map((check) => check.dataset.account))).filter(Boolean);
    const hasOpen = openSelected.length > 0;
    form.dataset.hasSelection = selected.length > 0 ? "true" : "false";
    const modeContainer = resultsContainer();
    if (modeContainer && selected.length === 0) {
      modeContainer.removeAttribute("data-selection-mode");
    }
    count.textContent = selected.length > 0 ? selected.length + " selected" : "";
    setBulkControlDisabled(assigneeSelect, !hasOpen);
    bulkButtons.forEach((button) => {
      setBulkControlDisabled(button, !hasOpen);
    });
    selectAll.forEach((control) => {
      control.checked = selected.length === checks.length && checks.length > 0;
      control.indeterminate = selected.length > 0 && selected.length < checks.length;
    });
    const accountSuffix = accounts.length ? " (accounts: " + accounts.join(", ") + ")" : "";
    const spamMsg = "Mark " + openSelected.length + " selected conversation(s) on this page as spam" + accountSuffix + "? Moves to Junk will be queued.";
    const completeMsg = "Mark " + openSelected.length + " selected conversation(s) on this page as completed" + accountSuffix + "? Moves to Archive will be queued.";
    form.dataset.confirm = spamMsg;
    if (completeButton) {
      completeButton.dataset.confirm = completeMsg;
      completeButton.dataset.confirmAction = "Complete selected";
    }
    if (spamButton) {
      spamButton.dataset.confirm = spamMsg;
      spamButton.dataset.confirmAction = "Mark as spam";
    }
  }

  checks.forEach((check) => check.addEventListener("change", refreshSelection));
  selectAll.forEach((control) => {
    control.addEventListener("change", () => {
      checks.forEach((check) => {
        check.checked = control.checked;
      });
      refreshSelection();
    });
  });
  const clearButton = form.querySelector("[data-bulk-clear]");
  if (clearButton) {
    clearButton.addEventListener("click", () => {
      checks.forEach((check) => {
        check.checked = false;
      });
      const container = resultsContainer();
      if (container) {
        container.removeAttribute("data-selection-mode");
      }
      refreshSelection();
    });
  }
  // Row "Select" buttons live inside the table's row menus, not in this form, so
  // they are bound through the same scoped query and re-bound after a swap.
  scope.querySelectorAll("[data-row-select]").forEach((button) => {
    button.addEventListener("click", (event) => {
      event.preventDefault();
      const menu = button.closest("details[data-row-menu]");
      if (menu) {
        menu.open = false;
      }
      enterSelectionMode();
      const row = button.closest("tr");
      const check = row && row.querySelector("[data-select-thread]");
      if (check && !check.checked) {
        check.checked = true;
        refreshSelection();
      }
    });
  });
  form.addEventListener("submit", (event) => {
    const selected = checks.filter((check) => check.checked);
    const openSelected = selected.filter((check) => {
      const st = check.dataset.status;
      return !st || st === "new" || st === "in_progress";
    });
    if (openSelected.length === 0) {
      event.preventDefault();
      event.stopImmediatePropagation();
    }
  });
  refreshSelection();
  refreshSelectionFn = refreshSelection;
}

// Long-press (~500ms) on a mobile card enters selection mode and checks that
// row. Cancelled on movement/scroll and never navigates: the follow-up click is
// suppressed once.
export function setupLongPress() {
  let suppressNextRowClick = false;
  let timer = null;
  let pressRow = null;
  let startX = 0;
  let startY = 0;
  function cancel() {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
    pressRow = null;
  }
  document.addEventListener("pointerdown", (event) => {
    if (event.pointerType === "mouse" || event.button !== 0) {
      return;
    }
    if (event.target.closest("a, button, select, input, label, details[data-row-menu], dialog")) {
      return;
    }
    const row = event.target.closest("tr.thread-row");
    if (!row || !row.querySelector("[data-select-thread]")) {
      return;
    }
    pressRow = row;
    startX = event.clientX;
    startY = event.clientY;
    timer = setTimeout(() => {
      timer = null;
      if (pressRow !== row) {
        return;
      }
      enterSelectionMode();
      const check = row.querySelector("[data-select-thread]");
      if (check && !check.checked) {
        check.checked = true;
      }
      rerenderSelection();
      suppressNextRowClick = true;
      // iOS long-press often opens the text-selection callout instead of
      // delivering a click; expire the suppression so the NEXT tap isn't
      // swallowed (the real click, when it comes, lands well within 600ms).
      setTimeout(() => {
        suppressNextRowClick = false;
      }, 600);
      pressRow = null;
    }, 500);
  }, { passive: true });
  // Small finger drift must not cancel the long-press; only real movement
  // (which usually means a scroll start) does.
  document.addEventListener("pointermove", (event) => {
    if (Math.hypot(event.clientX - startX, event.clientY - startY) > 10) {
      cancel();
    }
  }, { passive: true });
  document.addEventListener("pointercancel", cancel, { passive: true });
  document.addEventListener("pointerup", cancel, { passive: true });
  document.addEventListener("scroll", cancel, { passive: true, capture: true });
  document.addEventListener("click", (event) => {
    if (suppressNextRowClick && event.target.closest("tr.thread-row")) {
      suppressNextRowClick = false;
      event.preventDefault();
      event.stopPropagation();
    }
  }, true);
}
