import { beforeEach, describe, expect, it, vi } from "vitest";
import { AUTOSAVE_DELAY_MS, createDraftAutoSaver } from "../src/draft.js";

/* The draft module owns the only frontend state that can lose a user's work:
   text typed into a reply that never reached the server. These tests pin the
   three behaviours that protect it — the autosave debounce, the 409 conflict
   path, and the beforeunload guard. */

function makeForm({ body = "", revision = "1", formatting = "markdown" } = {}) {
  document.body.innerHTML = `
    <form data-draft-url="/threads/t1/draft" class="reply-form">
      <textarea name="body_text"></textarea>
      <input type="hidden" name="body_formatting" value="${formatting}">
      <input type="hidden" name="draft_revision" value="${revision}">
      <span data-draft-status></span>
      <button type="button" data-save-draft>Save draft</button>
      <button type="button" data-discard-draft>Discard</button>
      <button type="submit">Send</button>
    </form>`;
  const form = document.querySelector("form");
  form.querySelector("textarea").value = body;
  return {
    form,
    field: form.querySelector("textarea"),
    revision: form.querySelector("input[name=draft_revision]"),
    status: form.querySelector("[data-draft-status]"),
    formatting: form.querySelector("input[name=body_formatting]"),
  };
}

function fakeTimers() {
  const pending = [];
  return {
    setTimeoutImpl: (fn, ms) => {
      pending.push({ fn, ms });
      return pending.length - 1;
    },
    clearTimeoutImpl: (id) => {
      if (pending[id]) {
        pending[id] = null;
      }
    },
    runAll() {
      pending.forEach((entry) => {
        if (entry) {
          entry.fn();
        }
      });
    },
    pending,
  };
}

function jsonResponse(body, status = 200) {
  return Promise.resolve({
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  });
}

// The module reads the current draft with GET (which does carry a credentials
// option) and writes with PUT, so the fake branches on the method, not on
// whether options were passed.
function routedFetch({ onGet, onPut }) {
  return vi.fn((url, options) => {
    const method = (options && options.method) || "GET";
    if (method === "PUT") {
      return onPut(url, options);
    }
    return onGet(url, options);
  });
}

describe("createDraftAutoSaver", () => {
  beforeEach(() => {
    document.body.innerHTML = "";
  });

  it("loads the server draft and adopts its revision", async () => {
    const parts = makeForm({ body: "", revision: "7" });
    const fetchImpl = routedFetch({
      onGet: () => jsonResponse({ revision: "7", body: "server text", formatting: "markdown" }),
      onPut: () => jsonResponse({ revision: "8" }),
    });
    const timers = fakeTimers();
    const saver = createDraftAutoSaver({ ...parts, fetchImpl, ...timers });
    await saver.load();

    expect(saver.state.initialized).toBe(true);
    expect(saver.state.saved).toBe(JSON.stringify(["server text", "markdown"]));
    // Remote text differing from the local field is not written back to the
    // textarea: the user keeps whatever they had typed.
    expect(parts.field.value).toBe("");
  });

  it("debounces changes and PUTs the new body with the current revision", async () => {
    const parts = makeForm({ body: "", revision: "3" });
    const fetchImpl = routedFetch({
      onGet: () => jsonResponse({ revision: "3", body: "", formatting: "markdown" }),
      onPut: () => jsonResponse({ revision: "4" }),
    });
    const timers = fakeTimers();
    const saver = createDraftAutoSaver({ ...parts, fetchImpl, ...timers });
    await saver.load();

    parts.field.value = "hello";
    saver.changed();
    expect(timers.pending.filter(Boolean)).toHaveLength(1);
    expect(timers.pending[0].ms).toBe(AUTOSAVE_DELAY_MS);

    timers.runAll();
    await vi.waitFor(() => expect(parts.revision.value).toBe("4"));
    const put = fetchImpl.mock.calls.find(([, options]) => options && options.method === "PUT");
    expect(JSON.parse(put[1].body)).toEqual({ revision: "3", body: "hello", formatting: "markdown", discard: false });
    expect(saver.state.saved).toBe(JSON.stringify(["hello", "markdown"]));
  });

  it("stops autosaving and blocks submit after a 409 conflict", async () => {
    const parts = makeForm({ body: "", revision: "1" });
    const fetchImpl = routedFetch({
      onGet: () => jsonResponse({ revision: "1", body: "", formatting: "markdown" }),
      onPut: () => jsonResponse({}, 409),
    });
    const timers = fakeTimers();
    const saver = createDraftAutoSaver({ ...parts, fetchImpl, ...timers });
    await saver.load();
    parts.field.value = "my text";
    saver.changed();
    timers.runAll();
    await vi.waitFor(() => expect(saver.state.conflicted).toBe(true));

    expect(parts.status.textContent).toMatch(/changed in another session/i);
    // The user's text is still theirs to copy; nothing overwrote it.
    expect(parts.field.value).toBe("my text");

    const event = { defaultPrevented: false, preventDefault: vi.fn() };
    saver.onSubmit(event);
    expect(event.preventDefault).toHaveBeenCalled();
    expect(parts.status.textContent).toMatch(/changed in another session/i);

    // Further edits do not retry the save while conflicted.
    const callsBefore = fetchImpl.mock.calls.length;
    saver.changed();
    timers.runAll();
    expect(fetchImpl.mock.calls.length).toBe(callsBefore);
  });

  it("blocks submit while the initial draft check is unfinished", async () => {
    const parts = makeForm();
    const saver = createDraftAutoSaver({ ...parts, fetchImpl: routedFetch({ onGet: () => jsonResponse({ revision: "1" }), onPut: () => jsonResponse({ revision: "2" }) }), ...fakeTimers() });
    const event = { defaultPrevented: false, preventDefault: vi.fn() };
    saver.onSubmit(event);
    expect(event.preventDefault).toHaveBeenCalled();
    expect(parts.status.textContent).toMatch(/wait for the draft/i);
  });

  it("allows submit once the draft is checked and clean", async () => {
    const parts = makeForm();
    const saver = createDraftAutoSaver({ ...parts, fetchImpl: routedFetch({ onGet: () => jsonResponse({ revision: "1" }), onPut: () => jsonResponse({ revision: "2" }) }), ...fakeTimers() });
    await saver.load();
    const event = { defaultPrevented: false, preventDefault: vi.fn() };
    saver.onSubmit(event);
    expect(event.preventDefault).not.toHaveBeenCalled();
    expect(saver.state.sending).toBe(true);
  });

  it("warns before unload only while text is unsaved", async () => {
    const parts = makeForm();
    const saver = createDraftAutoSaver({ ...parts, fetchImpl: routedFetch({ onGet: () => jsonResponse({ revision: "1", body: "", formatting: "markdown" }), onPut: () => jsonResponse({ revision: "2" }) }), ...fakeTimers() });
    await saver.load();

    const clean = { preventDefault: vi.fn() };
    saver.onBeforeUnload(clean);
    expect(clean.preventDefault).not.toHaveBeenCalled();

    parts.field.value = "dirty";
    const dirty = { preventDefault: vi.fn() };
    saver.onBeforeUnload(dirty);
    expect(dirty.preventDefault).toHaveBeenCalled();
    expect(dirty.returnValue).toBe("");

    // Once sending, the navigation is intentional: no prompt.
    saver.setSending(true);
    const afterSend = { preventDefault: vi.fn() };
    saver.onBeforeUnload(afterSend);
    expect(afterSend.preventDefault).not.toHaveBeenCalled();
  });

  it("discarding clears the field and emits draftdiscarded", async () => {
    const parts = makeForm({ body: "to be discarded", revision: "2" });
    const fetchImpl = routedFetch({
      onGet: () => jsonResponse({ revision: "2", body: "to be discarded", formatting: "markdown" }),
      onPut: () => jsonResponse({ revision: "3" }),
    });
    const saver = createDraftAutoSaver({ ...parts, fetchImpl, ...fakeTimers() });
    await saver.load();
    const discarded = vi.fn();
    parts.form.addEventListener("draftdiscarded", discarded);
    await saver.save(true);

    expect(discarded).toHaveBeenCalled();
    expect(parts.field.value).toBe("");
    expect(parts.status.textContent).toMatch(/discarded/i);
  });
});
