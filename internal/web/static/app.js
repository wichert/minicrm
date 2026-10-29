/* MiniCRM progressive enhancement: filter auto-submit, confirmations, notice dismissal, frame resize */
document.addEventListener("DOMContentLoaded", function () {
  // Progressive enhancement flag: bulk-bar and selection-mode styling in
  // app.css is scoped under html.js so the no-JS experience stays as today.
  document.documentElement.classList.add("js");

  function setupFilterAutoSubmit() {
    document.querySelectorAll("[data-hx-get]").forEach(function (el) {
      el.addEventListener("change", function () {
        if (el.form) {
          el.form.submit();
        }
      });
    });
  }

  setupFilterAutoSubmit();

  // Tab-strip chrome is re-applied after a fragment swap, so the setup is a
  // function rather than a one-shot DOMContentLoaded pass. Per-strip observers
  // are bound once (the strip element survives a swap); focus listeners are
  // bound per tab because swapResults() replaces the tab children.
  function updateTabOverflow(strip) {
    var tabs = strip.querySelector(".tabs");
    if (!tabs) {
      return;
    }
    strip.dataset.overflowStart = tabs.scrollLeft > 1 ? "true" : "false";
    strip.dataset.overflowEnd = tabs.scrollLeft + tabs.clientWidth < tabs.scrollWidth - 1 ? "true" : "false";
  }

  function setupTabStrips(root) {
    (root || document).querySelectorAll("[data-tab-strip]").forEach(function (strip) {
      var tabs = strip.querySelector(".tabs");
      if (!tabs) {
        return;
      }
      if (strip.dataset.tabStripReady !== "true") {
        strip.dataset.tabStripReady = "true";
        tabs.addEventListener("scroll", function () {
          updateTabOverflow(strip);
        }, { passive: true });
        if (typeof ResizeObserver === "function") {
          new ResizeObserver(function () {
            updateTabOverflow(strip);
          }).observe(tabs);
        }
      }
      tabs.querySelectorAll(".tab").forEach(function (tab) {
        if (tab.dataset.tabFocusBound === "true") {
          return;
        }
        tab.dataset.tabFocusBound = "true";
        tab.addEventListener("focus", function () {
          tab.scrollIntoView({ block: "nearest", inline: "nearest" });
        });
      });
      updateTabOverflow(strip);
    });
  }

  setupTabStrips();
  window.addEventListener("resize", function () {
    document.querySelectorAll("[data-tab-strip]").forEach(updateTabOverflow);
  });

  document.querySelectorAll("form[data-draft-url]").forEach(function (form) {
    var field = form.querySelector("textarea[name=body_text]");
    var revision = form.querySelector("input[name=draft_revision]");
    var status = form.querySelector("[data-draft-status]");
    var formatting = form.querySelector("input[name=body_formatting]");
    function snapshot() { return JSON.stringify([field.value, formatting.value]); }
    var saved = snapshot(), timer, saving = false, conflicted = false, sending = false, initialized = false;
    fetch(form.dataset.draftUrl, { credentials: "same-origin" }).then(function (response) {
      if (!response.ok) { throw new Error("Could not check draft."); }
      return response.json();
    }).then(function (draft) {
      if (draft.revision !== revision.value) { conflicted = true; status.textContent = "Draft changed in another session. Copy your text before reloading."; }
      saved = JSON.stringify(draft.discarded ? ["", ""] : [draft.body || "", draft.formatting || ""]);
      initialized = true;
      if (!conflicted && snapshot() !== saved) { clearTimeout(timer); timer = setTimeout(function () { save(false); }, 3000); }
    }).catch(function () { status.textContent = "Could not check draft. Reload before sending."; });
    function save(discard) {
      if (!initialized || saving || conflicted || sending) { return; }
      var before = snapshot(), value = discard ? "" : field.value, format = discard ? "" : formatting.value;
      saving = true;
      status.textContent = "Saving…";
      fetch(form.dataset.draftUrl, { method: "PUT", credentials: "same-origin", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ revision: revision.value, body: value, formatting: format, discard: discard }) })
        .then(function (response) {
          if (response.status === 409) { conflicted = true; throw new Error("Draft changed in another session. Copy your text before reloading."); }
          if (!response.ok) { throw new Error("Could not save draft."); }
          return response.json();
        }).then(function (draft) {
          revision.value = draft.revision;
          saved = JSON.stringify([value, format]);
          if (discard && snapshot() === before) {
            field.value = "";
            formatting.value = "";
            form.dispatchEvent(new Event("draftdiscarded"));
          }
          status.textContent = discard ? "Draft discarded." : "Saved";
          if (snapshot() !== saved) { clearTimeout(timer); timer = setTimeout(function () { save(false); }, 3000); }
        }).catch(function (error) { status.textContent = error.message || "Could not save draft."; })
        .finally(function () { saving = false; });
    }
    function changed() { clearTimeout(timer); timer = setTimeout(function () { save(false); }, 3000); }
    field.addEventListener("input", changed);
    form.addEventListener("editorchange", changed);
    form.querySelector("[data-save-draft]").addEventListener("click", function () { clearTimeout(timer); save(false); });
    form.querySelector("[data-discard-draft]").addEventListener("click", function () { clearTimeout(timer); save(true); });
    form.addEventListener("submit", function (event) {
      if (event.defaultPrevented) { return; }
      if (!initialized || saving || conflicted) { event.preventDefault(); status.textContent = conflicted ? "Draft changed in another session. Copy your text before reloading." : "Wait for the draft to finish checking or saving."; return; }
      sending = true;
    });
    window.addEventListener("beforeunload", function (event) { if (!sending && (snapshot() !== saved || saving)) { event.preventDefault(); event.returnValue = ""; } });
  });

  // Exposed to swapResults(): after a fragment swap the bulk bar must recount
  // the (re-applied) selection so the buttons and confirmation text stay right.
  var refreshBulkSelection = null;

  // Module-scoped reference to the bulk form so the shared form[data-confirm]
  // submit handler below can recognize it (it runs outside setupBulkForm's
  // scope). Null when the page has no bulk bar.
  var bulkFormRef = null;

  function setupBulkForm() {
    refreshBulkSelection = null;
    bulkFormRef = null;
    var bulkForm = document.getElementById("bulk-spam-form");
    if (!bulkForm) {
      return;
    }
    bulkFormRef = bulkForm;
    var checks = Array.from(document.querySelectorAll("[data-select-thread]"));
    var selectAll = Array.from(document.querySelectorAll("[data-select-all], [data-select-all-mobile]"));
    var count = bulkForm.querySelector("[data-selected-count]");
    var assigneeSelect = bulkForm.querySelector("[data-bulk-assignee]");
    var bulkButtons = Array.from(bulkForm.querySelectorAll("[data-bulk-action]"));
    var completeButton = bulkForm.querySelector('[data-bulk-action="complete"]');
    var spamButton = bulkForm.querySelector('[data-bulk-action="spam"]');
    function setBulkControlDisabled(control, disabled) {
      if (!control) {
        return;
      }
      control.disabled = disabled;
      control.setAttribute("aria-disabled", disabled ? "true" : "false");
    }
    function refreshSelection() {
      var selected = checks.filter(function (check) { return check.checked; });
      var openSelected = selected.filter(function (check) {
        var st = check.dataset.status;
        return !st || st === "new" || st === "in_progress";
      });
      var accounts = Array.from(new Set(openSelected.map(function (check) { return check.dataset.account; }))).filter(Boolean);
      var hasOpen = openSelected.length > 0;
      bulkForm.dataset.hasSelection = selected.length > 0 ? "true" : "false";
      var modeContainer = document.querySelector("[data-inbox-results]");
      if (modeContainer && selected.length === 0) {
        modeContainer.removeAttribute("data-selection-mode");
      }
      count.textContent = selected.length > 0 ? selected.length + " selected" : "";
      setBulkControlDisabled(assigneeSelect, !hasOpen);
      bulkButtons.forEach(function (button) {
        setBulkControlDisabled(button, !hasOpen);
      });
      selectAll.forEach(function (control) { control.checked = selected.length === checks.length && checks.length > 0; control.indeterminate = selected.length > 0 && selected.length < checks.length; });
      var accountSuffix = accounts.length ? " (accounts: " + accounts.join(", ") + ")" : "";
      var spamMsg = "Mark " + openSelected.length + " selected conversation(s) on this page as spam" + accountSuffix + "? Moves to Junk will be queued.";
      var completeMsg = "Mark " + openSelected.length + " selected conversation(s) on this page as completed" + accountSuffix + "? Moves to Archive will be queued.";
      bulkForm.dataset.confirm = spamMsg;
      if (completeButton) {
        completeButton.dataset.confirm = completeMsg;
        completeButton.dataset.confirmAction = "Complete selected";
      }
      if (spamButton) {
        spamButton.dataset.confirm = spamMsg;
        spamButton.dataset.confirmAction = "Mark as spam";
      }
    }
    checks.forEach(function (check) { check.addEventListener("change", refreshSelection); });
    selectAll.forEach(function (control) { control.addEventListener("change", function () { checks.forEach(function (check) { check.checked = control.checked; }); refreshSelection(); }); });
    var clearButton = bulkForm.querySelector("[data-bulk-clear]");
    if (clearButton) {
      clearButton.addEventListener("click", function () {
        checks.forEach(function (check) { check.checked = false; });
        var container = resultsContainer();
        if (container) { container.removeAttribute("data-selection-mode"); }
        refreshSelection();
      });
    }
    // Row "Select" buttons live inside the table's row menus, not in this
    // form, so bind them document-wide; swapResults() re-runs setupBulkForm()
    // after every fragment swap, which re-binds the recreated buttons.
    document.querySelectorAll("[data-row-select]").forEach(function (button) {
      button.addEventListener("click", function (event) {
        event.preventDefault();
        var menu = button.closest("details[data-row-menu]");
        if (menu) { menu.open = false; }
        enterSelectionMode();
        var row = button.closest("tr");
        var check = row && row.querySelector("[data-select-thread]");
        if (check && !check.checked) {
          check.checked = true;
          refreshSelection();
        }
      });
    });
    bulkForm.addEventListener("submit", function (event) {
      var selected = checks.filter(function (check) { return check.checked; });
      var openSelected = selected.filter(function (check) {
        var st = check.dataset.status;
        return !st || st === "new" || st === "in_progress";
      });
      if (openSelected.length === 0) {
        event.preventDefault();
        event.stopImmediatePropagation();
      }
    });
    refreshSelection();
    refreshBulkSelection = refreshSelection;
  }

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
        if (event.defaultPrevented || form.dataset.confirmed === "true") {
          return;
        }
        if (bulkFormRef && form === bulkFormRef && event.submitter && !event.submitter.hasAttribute("data-confirm-submit")) {
          return;
        }
        var trigger = event.submitter || form.querySelector("button[type=submit]") || document.activeElement;
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

  // Mobile selection mode: per-row checkboxes only exist visually while the
  // results container carries data-selection-mode="true" (see app.css).
  function resultsContainer() {
    return document.querySelector("[data-inbox-results]");
  }
  function enterSelectionMode() {
    var container = document.querySelector("[data-inbox-results]");
    if (container) {
      container.setAttribute("data-selection-mode", "true");
    }
  }
  // Long-press (~500ms) on a mobile card enters selection mode and checks that
  // row. Cancelled on movement/scroll and never navigates: the follow-up click
  // is suppressed once.
  var suppressNextRowClick = false;
  function setupLongPress() {
    var timer = null;
    var pressRow = null;
    var startX = 0;
    var startY = 0;
    function cancel() {
      if (timer !== null) { clearTimeout(timer); timer = null; }
      pressRow = null;
    }
    document.addEventListener("pointerdown", function (event) {
      if (event.pointerType === "mouse" || event.button !== 0) { return; }
      if (event.target.closest("a, button, select, input, label, details[data-row-menu], dialog")) { return; }
      var row = event.target.closest("tr.thread-row");
      if (!row || !row.querySelector("[data-select-thread]")) { return; }
      pressRow = row;
      startX = event.clientX;
      startY = event.clientY;
      timer = setTimeout(function () {
        timer = null;
        if (pressRow !== row) { return; }
        enterSelectionMode();
        var check = row.querySelector("[data-select-thread]");
        if (check && !check.checked) {
          check.checked = true;
        }
        if (refreshBulkSelection) { refreshBulkSelection(); }
        suppressNextRowClick = true;
        // iOS long-press often opens the text-selection callout instead of
        // delivering a click; expire the suppression so the NEXT tap isn't
        // swallowed (the real click, when it comes, lands well within 600ms).
        setTimeout(function () { suppressNextRowClick = false; }, 600);
        pressRow = null;
      }, 500);
    }, { passive: true });
    // Small finger drift must not cancel the long-press; only real movement
    // (which usually means a scroll start) does.
    document.addEventListener("pointermove", function (event) {
      if (Math.hypot(event.clientX - startX, event.clientY - startY) > 10) {
        cancel();
      }
    }, { passive: true });
    document.addEventListener("pointercancel", cancel, { passive: true });
    document.addEventListener("pointerup", cancel, { passive: true });
    document.addEventListener("scroll", cancel, { passive: true, capture: true });
    document.addEventListener("click", function (event) {
      if (suppressNextRowClick && event.target.closest("tr.thread-row")) {
        suppressNextRowClick = false;
        event.preventDefault();
        event.stopPropagation();
      }
    }, true);
  }
  setupLongPress();

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
        if (!doc || !doc.body || doc.readyState !== "complete" || doc.URL === "about:blank") {
          button.textContent = "Message is still loading; try again";
          return;
        }
        var images = doc.querySelectorAll("img[data-blocked-src]");
        if (!images.length) {
          button.textContent = "No blocked images found; try again";
          return;
        }
        images.forEach(function (image, index) {
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

  function setupRowLinks() {
    document.querySelectorAll("tr[data-row-href]").forEach(function (row) {
      row.addEventListener("click", function (event) {
        if (event.target.closest("a, button, input, select, label, summary, details")) {
          return;
        }
        if (window.getSelection && String(window.getSelection())) {
          return;
        }
        // Navigate through the row's own anchor instead of assigning a string read
        // from the DOM: the destination stays a URL the browser resolved from the
        // rendered markup, and no script-visible string is ever treated as a URL.
        var link = row.querySelector("a.thread-link[href]");
        if (link) {
          link.click();
        }
      });
    });
  }

  // Row-menu setup runs after every fragment swap, so the document-level
  // click/Escape handlers are hoisted into a one-time setup and query the open
  // menus at event time instead of closing over a stale row list.
  var rowMenuDocumentReady = false;

  function setupRowMenus(root) {
    (root || document).querySelectorAll("details[data-row-menu]").forEach(function (menu) {
      if (menu.dataset.rowMenuReady === "true") {
        return;
      }
      menu.dataset.rowMenuReady = "true";
      menu.addEventListener("toggle", function () {
        if (!menu.open) {
          return;
        }
        document.querySelectorAll("details[data-row-menu][open]").forEach(function (other) {
          if (other !== menu) {
            other.open = false;
          }
        });
      });
    });
    if (rowMenuDocumentReady) {
      return;
    }
    rowMenuDocumentReady = true;
    document.addEventListener("click", function (event) {
      document.querySelectorAll("details[data-row-menu][open]").forEach(function (menu) {
        if (!menu.contains(event.target)) {
          menu.open = false;
        }
      });
    });
    document.addEventListener("keydown", function (event) {
      if (event.key !== "Escape") {
        return;
      }
      document.querySelectorAll("details[data-row-menu][open]").forEach(function (menu) {
        var summary = menu.querySelector("summary");
        menu.open = false;
        if (summary) {
          summary.focus();
        }
      });
    });
  }

  /* Inbox auto-refresh: keeps the thread list current without a Refresh button.
     Polls the current URL every 60s and swaps just the results region, pausing
     while the tab is hidden, text is selected, a row menu or confirm <dialog> is
     open, a bulk selection exists, or an inline edit has unsaved input. Plain
     links keep working without JavaScript.

     The fetch stays a full-page GET on purpose: the server's HX-Request partial
     (inboxTablePartial) contains only the table, while this loop also refreshes
     the .results-count and .tabs toolbar regions from next to it. Fetching the
     full document keeps those three regions correct with one request and no
     duplicated template; the cost is a page-sized response every 60s on the
     inbox only. */
  var inboxRefresh = { refresh: null, repaint: null };

  function setupInboxAutoRefresh() {
    var results = document.querySelector("[data-inbox-results]");
    var statusEl = document.querySelector("[data-refresh-status]");
    var refreshLink = document.querySelector("[data-refresh-link]");
    if (!results) {
      return;
    }
    var intervalMs = 60000;
    var lastLoadedAt = Date.now();
    var inflight = false;

    function hasPendingInlineEdit() {
      var active = document.activeElement;
      if (!active) {
        return false;
      }
      var tag = active.tagName;
      if (tag === "TEXTAREA" || tag === "SELECT") {
        return true;
      }
      if (tag === "INPUT") {
        var type = (active.getAttribute("type") || "text").toLowerCase();
        if (type === "checkbox" || type === "radio" || type === "submit") {
          return false;
        }
        return String(active.value) !== String(active.defaultValue || "");
      }
      return Boolean(active.isContentEditable);
    }

    // Auto-refresh contract: a swap must never destroy work in progress. Pause
    // while the tab is hidden, text is selected, a row menu or confirm <dialog>
    // is open, any thread checkbox is checked, focus is inside the results
    // region, or an inline edit is dirty. swapResults() additionally re-applies
    // checked thread IDs and refreshes the bulk bar counts (belt and braces).
    function isPaused() {
      if (document.visibilityState === "hidden") {
        return true;
      }
      if (window.getSelection && String(window.getSelection())) {
        return true;
      }
      if (document.querySelector("details[data-row-menu][open]")) {
        return true;
      }
      if (document.querySelector("dialog[open]")) {
        return true;
      }
      if (document.querySelector("[data-select-thread]:checked")) {
        return true;
      }
      // Never yank the DOM out from under a focused row action or menu trigger.
      if (results.contains(document.activeElement)) {
        return true;
      }
      return hasPendingInlineEdit();
    }

    function checkedThreadIDs() {
      return Array.from(document.querySelectorAll("[data-select-thread]:checked")).map(function (check) {
        return check.value;
      });
    }

    function formatAgo(ms) {
      var seconds = Math.max(0, Math.round(ms / 1000));
      if (seconds < 45) {
        return "Updated just now";
      }
      var minutes = Math.round(seconds / 60);
      if (minutes < 60) {
        return "Updated " + minutes + "m ago";
      }
      var hours = Math.round(minutes / 60);
      return "Updated " + hours + "h ago";
    }

    function paintStatus() {
      if (!statusEl) {
        return;
      }
      statusEl.textContent = formatAgo(Date.now() - lastLoadedAt);
    }

    // Contract: preserving the selection across the swap is mandatory even
    // though isPaused() normally prevents a swap while rows are checked. Re-apply
    // the checked IDs that still exist after the swap, then recount the bulk bar.
    function swapResults(html) {
      var doc = new DOMParser().parseFromString(html, "text/html");
      var fresh = doc.querySelector("[data-inbox-results]");
      if (!fresh) {
        return false;
      }
      var selected = checkedThreadIDs();
      var selectionMode = results.getAttribute("data-selection-mode");
      results.innerHTML = fresh.innerHTML;
      // Keep selection mode across the swap so long-press-selected rows stay visible.
      if (selectionMode) {
        results.setAttribute("data-selection-mode", selectionMode);
      }
      if (selected.length) {
        var wanted = new Set(selected);
        results.querySelectorAll("[data-select-thread]").forEach(function (check) {
          if (wanted.has(check.value)) {
            check.checked = true;
          }
        });
      }
      var freshCount = doc.querySelector(".results-count");
      var count = document.querySelector(".inbox-toolbar .results-count");
      if (freshCount && count) {
        count.innerHTML = freshCount.innerHTML;
      }
      var freshTabs = doc.querySelector(".tabs");
      var tabs = document.querySelector(".inbox-toolbar .tabs");
      if (freshTabs && tabs) {
        tabs.innerHTML = freshTabs.innerHTML;
        setupTabStrips(document.querySelector(".inbox-toolbar"));
      }
      setupRowLinks();
      setupRowMenus();
      setupBulkForm();
      if (refreshBulkSelection) {
        refreshBulkSelection();
      }
      return true;
    }

    function refresh() {
      if (inflight) {
        return;
      }
      inflight = true;
      var url = window.location.pathname + window.location.search;
      fetch(url, { headers: { "X-Requested-With": "fetch" }, credentials: "same-origin" })
        .then(function (response) {
          if (!response.ok) {
            throw new Error("refresh failed");
          }
          return response.text();
        })
        .then(function (html) {
          if (swapResults(html)) {
            lastLoadedAt = Date.now();
            paintStatus();
          }
        })
        .catch(function () {
          // Keep the current list; the next tick or a manual click retries.
        })
        .then(function () {
          inflight = false;
        });
    }

    if (refreshLink) {
      refreshLink.addEventListener("click", function (event) {
        event.preventDefault();
        refresh();
      });
    }
    inboxRefresh.refresh = refresh;
    inboxRefresh.repaint = paintStatus;

    paintStatus();
    window.setInterval(paintStatus, 30000);
    window.setInterval(function () {
      if (!isPaused()) {
        refresh();
      }
    }, intervalMs);
    // Returning to the tab repaints the age label; a restored back/forward cache
    // entry revalidates the list once (the zero-cost fallback the plan asks for).
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "visible") {
        paintStatus();
      }
    });
    window.addEventListener("pageshow", function (event) {
      if (event.persisted && !isPaused()) {
        refresh();
      }
    });
  }

  /* Pull-to-refresh on touch devices, on top of the browser's own overscroll
     behaviour (which reloads the whole page and loses scroll position). Only
     engages at the very top of the document, ignores gestures that start in a
     paused context, and never fires while another refresh is running. */
  function setupPullToRefresh() {
    if (!("ontouchstart" in window) || !document.querySelector("[data-inbox-results]")) {
      return;
    }
    // The pull feedback uses its own indicator, not [data-refresh-status]: the
    // results-status line is hidden below 768px, so the gesture would otherwise
    // be invisible on a phone.
    var indicator = document.querySelector("[data-pull-indicator]");
    var startY = null;
    var pulled = 0;
    var threshold = 70;
    var hideTimer = null;

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

    function triggerRefresh() {
      showPull("Refreshing…");
      if (inboxRefresh.refresh) {
        inboxRefresh.refresh();
      }
      hideTimer = setTimeout(function () {
        showPull("");
      }, 1200);
    }

    document.addEventListener(
      "touchstart",
      function (event) {
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
      function (event) {
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
      function () {
        var shouldRefresh = startY !== null && pulled >= threshold && window.scrollY === 0;
        startY = null;
        pulled = 0;
        if (shouldRefresh) {
          triggerRefresh();
        } else {
          showPull("");
          if (inboxRefresh.repaint) {
            inboxRefresh.repaint();
          }
        }
      },
      { passive: true }
    );
  }

  setupRowLinks();
  setupRowMenus();
  setupBulkForm();
  setupInboxAutoRefresh();
  setupPullToRefresh();

  if ("serviceWorker" in navigator) {
    window.addEventListener("load", function () {
      navigator.serviceWorker.register("/sw.js").catch(function () {
        // Ignore registration errors in restricted or non-HTTPS dev contexts.
      });
    });
  }
});
