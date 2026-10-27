/* MiniCRM progressive enhancement: filter auto-submit, confirmations, notice dismissal, frame resize */
document.addEventListener("DOMContentLoaded", function () {
  document.querySelectorAll("[data-hx-get]").forEach(function (el) {
    el.addEventListener("change", function () {
      if (el.form) {
        el.form.submit();
      }
    });
  });

  document.querySelectorAll("[data-tab-strip]").forEach(function (strip) {
    var tabs = strip.querySelector(".tabs");
    if (!tabs) {
      return;
    }
    function updateOverflow() {
      strip.dataset.overflowStart = tabs.scrollLeft > 1 ? "true" : "false";
      strip.dataset.overflowEnd = tabs.scrollLeft + tabs.clientWidth < tabs.scrollWidth - 1 ? "true" : "false";
    }
    tabs.addEventListener("scroll", updateOverflow, { passive: true });
    tabs.querySelectorAll(".tab").forEach(function (tab) {
      tab.addEventListener("focus", function () {
        tab.scrollIntoView({ block: "nearest", inline: "nearest" });
      });
    });
    window.addEventListener("resize", updateOverflow);
    if (typeof ResizeObserver === "function") {
      new ResizeObserver(updateOverflow).observe(tabs);
    }
    updateOverflow();
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

  function syncEmailFrameHeight(frame) {
    try {
      var doc = frame.contentDocument;
      if (!doc || !doc.body) {
        return;
      }
      var h = Math.ceil(doc.body.getBoundingClientRect().height);
      if (doc.documentElement && doc.documentElement.scrollWidth > doc.documentElement.clientWidth) {
        h += 16;
      }
      if (h > 0) {
        frame.style.height = h + "px";
      }
    } catch (_err) {
      // Fallback to CSS height if contentDocument is unavailable.
    }
  }

  var sessionExpiredReloadTriggered = false;

  function handleExpiredEmailFrame(frame) {
    var notice = document.createElement("div");
    notice.className = "session-expired-notice";
    var text = document.createElement("span");
    text.textContent = "Your session has expired.";
    var link = document.createElement("a");
    link.className = "btn btn-primary btn-small";
    link.href = "/auth/login";
    link.textContent = "Sign in again";
    notice.appendChild(text);
    notice.appendChild(link);
    if (frame.parentNode) {
      frame.parentNode.replaceChild(notice, frame);
    }
    if (!sessionExpiredReloadTriggered) {
      sessionExpiredReloadTriggered = true;
      window.location.reload();
    }
  }

  function observeEmailFrame(frame) {
    try {
      var doc = frame.contentDocument;
      if (doc && doc.body && doc.body.dataset.sessionExpired === "true") {
        handleExpiredEmailFrame(frame);
        return;
      }
      syncEmailFrameHeight(frame);
      if (!doc || !doc.body || doc.body.dataset.resizeObserved === "true") {
        return;
      }
      doc.body.dataset.resizeObserved = "true";
      if (typeof ResizeObserver === "function") {
        var ro = new ResizeObserver(function () {
          syncEmailFrameHeight(frame);
        });
        ro.observe(doc.body);
        if (doc.documentElement) {
          ro.observe(doc.documentElement);
        }
      }
      doc.querySelectorAll("img").forEach(function (img) {
        if (!img.complete) {
          img.addEventListener("load", function () {
            syncEmailFrameHeight(frame);
          });
        }
      });
    } catch (_err) {
      // Ignore if frame document is not yet ready.
    }
  }

  document.querySelectorAll("[data-load-images]").forEach(function (button) {
    button.addEventListener("click", function () {
      var frame = button.closest(".message").querySelector(".email-frame");
      try {
        var doc = frame.contentDocument;
        if (!doc || !doc.body) { return; }
        doc.querySelectorAll("img[data-blocked-src]").forEach(function (image, index) {
          image.src = button.dataset.imagePrefix + index;
          image.removeAttribute("data-blocked-src");
        });
        button.disabled = true;
        button.textContent = "Images requested";
      } catch (_err) {
        button.textContent = "Could not load images";
      }
    });
  });

  document.querySelectorAll(".email-frame").forEach(function (frame) {
    frame.addEventListener("load", function () {
      observeEmailFrame(frame);
    });
    if (frame.contentDocument && frame.contentDocument.readyState === "complete") {
      observeEmailFrame(frame);
    }
  });

  document.querySelectorAll("details.message").forEach(function (details) {
    details.addEventListener("toggle", function () {
      if (details.open) {
        details.querySelectorAll(".email-frame").forEach(function (frame) {
          observeEmailFrame(frame);
        });
      }
    });
  });

  window.addEventListener("resize", function () {
    document.querySelectorAll(".email-frame").forEach(function (frame) {
      syncEmailFrameHeight(frame);
    });
  });

  document.querySelectorAll("[data-auto-submit]").forEach(function (el) {
    el.addEventListener("change", function () {
      if (el.form) {
        el.form.submit();
      }
    });
  });

  document.querySelectorAll("[data-toggle-messages]").forEach(function (button) {
    button.addEventListener("click", function () {
      var expand = button.getAttribute("aria-pressed") !== "true";
      document.querySelectorAll("details.message").forEach(function (d) {
        d.open = expand;
      });
      button.setAttribute("aria-pressed", expand ? "true" : "false");
      button.textContent = expand ? "Collapse older" : "Expand all";
      if (!expand) {
        var all = document.querySelectorAll("details.message");
        if (all.length) {
          all[all.length - 1].open = true;
        }
      }
    });
  });

  document.querySelectorAll("tr[data-row-href]").forEach(function (row) {
    row.addEventListener("click", function (event) {
      if (event.target.closest("a, button, input, select")) {
        return;
      }
      if (window.getSelection && String(window.getSelection())) {
        return;
      }
      window.location.href = row.getAttribute("data-row-href");
    });
  });

  if ("serviceWorker" in navigator) {
    window.addEventListener("load", function () {
      navigator.serviceWorker.register("/sw.js").catch(function () {
        // Ignore registration errors in restricted or non-HTTPS dev contexts.
      });
    });
  }
});
