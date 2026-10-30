/* Page chrome: filter fallbacks, confirmations, flash dismissal, message
   expansion, and the mobile reply sheet. */

// Without htmx these selects would be inert: fall back to a plain form submit so
// the filter still applies. With htmx present the library owns the change
// (hx-get/hx-include in the markup), so binding here would double-fire.
export function setupFilterAutoSubmit() {
  if (window.htmx) {
    return;
  }
  document.querySelectorAll("[data-hx-get]").forEach((el) => {
    el.addEventListener("change", () => {
      if (el.form) {
        el.form.submit();
      }
    });
  });
}

export function setupAutoSubmit() {
  document.querySelectorAll("[data-auto-submit]").forEach((el) => {
    el.addEventListener("change", () => {
      if (el.form) {
        el.form.submit();
      }
    });
  });
}

export function setupFlashDismiss() {
  document.querySelectorAll("[data-flash-dismiss]").forEach((button) => {
    button.addEventListener("click", () => {
      const flash = button.closest(".flash");
      if (flash) {
        flash.remove();
      }
    });
  });
}

export function setupMessageToggle() {
  document.querySelectorAll("[data-toggle-messages]").forEach((button) => {
    button.addEventListener("click", () => {
      const expand = button.getAttribute("aria-pressed") !== "true";
      document.querySelectorAll("details.message").forEach((d) => {
        d.open = expand;
      });
      button.setAttribute("aria-pressed", expand ? "true" : "false");
      button.textContent = expand ? "Collapse older" : "Expand all";
      if (!expand) {
        const all = document.querySelectorAll("details.message");
        if (all.length) {
          all[all.length - 1].open = true;
        }
      }
    });
  });
}

/* Confirm consequential actions in an accessible in-page dialog. When <dialog> is
   unavailable the form keeps its native submit path and submits directly.

   getBulkForm() is injected because the bulk bar is replaced by list swaps; the
   handler must compare against the live node, not a stale one. */
export function setupConfirmDialog(getBulkForm) {
  const dialog = document.getElementById("confirm-dialog");
  if (!dialog || typeof dialog.showModal !== "function") {
    return;
  }
  const message = dialog.querySelector("[data-confirm-message]");
  const accept = dialog.querySelector("[data-confirm-accept]");
  const cancel = dialog.querySelector("[data-confirm-cancel]");
  let pendingForm = null;
  let pendingTrigger = null;

  document.querySelectorAll("form[data-confirm]").forEach((form) => {
    form.addEventListener("submit", (event) => {
      if (event.defaultPrevented || form.dataset.confirmed === "true") {
        return;
      }
      const bulkForm = getBulkForm();
      if (bulkForm && form === bulkForm && event.submitter && !event.submitter.hasAttribute("data-confirm-submit")) {
        return;
      }
      const trigger = event.submitter || form.querySelector("button[type=submit]") || document.activeElement;
      if (trigger && (trigger.disabled || trigger.getAttribute("aria-disabled") === "true")) {
        event.preventDefault();
        return;
      }
      event.preventDefault();
      pendingForm = form;
      pendingTrigger = trigger;
      message.textContent = (trigger && trigger.getAttribute("data-confirm")) || form.getAttribute("data-confirm");
      accept.textContent = (trigger && trigger.getAttribute("data-confirm-action")) || form.getAttribute("data-confirm-action") || "Confirm";
      dialog.showModal();
    });
  });

  cancel.addEventListener("click", () => {
    dialog.close();
  });

  accept.addEventListener("click", () => {
    const form = pendingForm;
    const trigger = pendingTrigger;
    if (!form) {
      dialog.close();
      return;
    }
    dialog.close();
    form.dataset.confirmed = "true";
    if (typeof form.requestSubmit === "function") {
      form.requestSubmit(trigger);
    } else {
      form.submit();
    }
    delete form.dataset.confirmed;
  });

  // Escape closes the dialog natively; return focus to the trigger for both paths.
  dialog.addEventListener("close", () => {
    if (pendingTrigger) {
      pendingTrigger.focus();
    }
    pendingForm = null;
    pendingTrigger = null;
  });
}
