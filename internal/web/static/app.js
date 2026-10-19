/* MiniCRM progressive enhancement: filter auto-submit, confirmations, notice dismissal, frame resize */
document.addEventListener("DOMContentLoaded", function () {
  document.querySelectorAll("[data-hx-get]").forEach(function (el) {
    el.addEventListener("change", function () {
      if (el.form) {
        el.form.submit();
      }
    });
  });

  // Confirm consequential actions in an accessible in-page dialog. When <dialog> is
  // unavailable the form keeps its native submit path and submits directly.
  var dialog = document.getElementById("confirm-dialog");
  var canConfirmInPage = dialog && typeof dialog.showModal === "function";
  var pendingForm = null;
  var pendingTrigger = null;

  function resetConfirm() {
    pendingForm = null;
    pendingTrigger = null;
  }

  if (canConfirmInPage) {
    var message = dialog.querySelector("[data-confirm-message]");
    var accept = dialog.querySelector("[data-confirm-accept]");
    var cancel = dialog.querySelector("[data-confirm-cancel]");

    document.querySelectorAll("form[data-confirm]").forEach(function (form) {
      form.addEventListener("submit", function (event) {
        if (form.dataset.confirmed === "true") {
          return;
        }
        event.preventDefault();
        pendingForm = form;
        pendingTrigger = form.querySelector("button[type=submit]") || document.activeElement;
        message.textContent = form.getAttribute("data-confirm");
        accept.textContent = form.getAttribute("data-confirm-action") || "Confirm";
        dialog.showModal();
      });
    });

    cancel.addEventListener("click", function () {
      dialog.close();
    });

    accept.addEventListener("click", function () {
      var form = pendingForm;
      var trigger = pendingTrigger;
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
    dialog.addEventListener("close", function () {
      if (pendingTrigger) {
        pendingTrigger.focus();
      }
      resetConfirm();
    });
  }

  document.querySelectorAll("[data-flash-dismiss]").forEach(function (button) {
    button.addEventListener("click", function () {
      var flash = button.closest(".flash");
      if (flash) {
        flash.remove();
      }
    });
  });

  document.querySelectorAll("[data-frame-expand]").forEach(function (button) {
    button.addEventListener("click", function () {
      var message = button.closest(".message");
      var frame = message ? message.querySelector(".email-frame") : null;
      if (!frame) {
        return;
      }
      var expanded = frame.classList.toggle("email-frame-expanded");
      button.textContent = expanded ? "Collapse" : "Expand";
    });
  });
});
