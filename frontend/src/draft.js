/* Draft autosave for the reply form.

   The draft is server-owned: every save carries the revision the client last
   saw, and a 409 means another session advanced it. That makes the conflict
   path the interesting one — the module must stop autosaving, keep the text on
   screen, block submit and warn the user, rather than overwriting someone
   else's edit. All state lives in this closure; the returned controller is what
   tests drive. */

export const AUTOSAVE_DELAY_MS = 3000;

export function createDraftAutoSaver({
  form,
  field,
  revision,
  status,
  formatting,
  fetchImpl = (...args) => window.fetch(...args),
  delayMs = AUTOSAVE_DELAY_MS,
  setTimeoutImpl = (fn, ms) => setTimeout(fn, ms),
  clearTimeoutImpl = (id) => clearTimeout(id),
}) {
  const state = {
    saved: "",
    timer: undefined,
    saving: false,
    conflicted: false,
    sending: false,
    initialized: false,
  };

  const snapshot = () => JSON.stringify([field.value, formatting.value]);

  function schedule(delay) {
    clearTimeoutImpl(state.timer);
    state.timer = setTimeoutImpl(() => save(false), delay);
  }

  function conflict() {
    state.conflicted = true;
    status.textContent = "Draft changed in another session. Copy your text before reloading.";
  }

  function save(discard) {
    if (!state.initialized || state.saving || state.conflicted || state.sending) {
      return Promise.resolve();
    }
    const before = snapshot();
    const value = discard ? "" : field.value;
    const format = discard ? "" : formatting.value;
    state.saving = true;
    status.textContent = "Saving…";
    return Promise.resolve(
      fetchImpl(form.dataset.draftUrl, {
        method: "PUT",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ revision: revision.value, body: value, formatting: format, discard: discard }),
      })
    )
      .then((response) => {
        if (response.status === 409) {
          conflict();
          throw new Error("Draft changed in another session. Copy your text before reloading.");
        }
        if (!response.ok) {
          throw new Error("Could not save draft.");
        }
        return response.json();
      })
      .then((draft) => {
        revision.value = draft.revision;
        state.saved = JSON.stringify([value, format]);
        if (discard && snapshot() === before) {
          field.value = "";
          formatting.value = "";
          form.dispatchEvent(new Event("draftdiscarded"));
        }
        status.textContent = discard ? "Draft discarded." : "Saved";
        if (snapshot() !== state.saved) {
          schedule(delayMs);
        }
      })
      .catch((error) => {
        status.textContent = error.message || "Could not save draft.";
      })
      .finally(() => {
        state.saving = false;
      });
  }

  function load() {
    return Promise.resolve(
      fetchImpl(form.dataset.draftUrl, { credentials: "same-origin" })
    )
      .then((response) => {
        if (!response.ok) {
          throw new Error("Could not check draft.");
        }
        return response.json();
      })
      .then((draft) => {
        if (draft.revision !== revision.value) {
          conflict();
        }
        state.saved = JSON.stringify(draft.discarded ? ["", ""] : [draft.body || "", draft.formatting || ""]);
        state.initialized = true;
        if (!state.conflicted && snapshot() !== state.saved) {
          schedule(delayMs);
        }
      })
      .catch(() => {
        status.textContent = "Could not check draft. Reload before sending.";
      });
  }

  function changed() {
    schedule(delayMs);
  }

  function onBeforeUnload(event) {
    if (!state.sending && (snapshot() !== state.saved || state.saving)) {
      event.preventDefault();
      event.returnValue = "";
    }
  }

  function onSubmit(event) {
    if (event.defaultPrevented) {
      return;
    }
    if (!state.initialized || state.saving || state.conflicted) {
      event.preventDefault();
      status.textContent = state.conflicted
        ? "Draft changed in another session. Copy your text before reloading."
        : "Wait for the draft to finish checking or saving.";
      return;
    }
    state.sending = true;
  }

  function bind() {
    field.addEventListener("input", changed);
    form.addEventListener("editorchange", changed);
    const saveButton = form.querySelector("[data-save-draft]");
    if (saveButton) {
      saveButton.addEventListener("click", () => {
        clearTimeoutImpl(state.timer);
        save(false);
      });
    }
    const discardButton = form.querySelector("[data-discard-draft]");
    if (discardButton) {
      discardButton.addEventListener("click", () => {
        clearTimeoutImpl(state.timer);
        save(true);
      });
    }
    form.addEventListener("submit", onSubmit);
    window.addEventListener("beforeunload", onBeforeUnload);
    load();
  }

  return {
    state,
    bind,
    load,
    changed,
    save,
    schedule,
    snapshot,
    onBeforeUnload,
    onSubmit,
    setSending: (value) => {
      state.sending = value;
    },
  };
}

export function setupDraftForms(root = document) {
  return Array.from(root.querySelectorAll("form[data-draft-url]")).map((form) => {
    const saver = createDraftAutoSaver({
      form,
      field: form.querySelector("textarea[name=body_text]"),
      revision: form.querySelector("input[name=draft_revision]"),
      status: form.querySelector("[data-draft-status]"),
      formatting: form.querySelector("input[name=body_formatting]"),
    });
    saver.bind();
    return saver;
  });
}
