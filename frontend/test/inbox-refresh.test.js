import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { pullTransform, setupInboxAutoRefresh, setupPullToRefresh } from "../src/inbox-refresh.js";

/* The pull gesture must never move the navbar. These tests pin the two pieces
   that make that true: the damped/capped transform helper, and the fact that
   setupPullToRefresh writes that transform to the content wrapper only, while
   leaving the indicator's position entirely to CSS paint space. */

describe("setupInboxAutoRefresh", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    window.htmx = {};
    document.body.innerHTML = `
      <form>
        <select name="account" hx-get="/threads/partial" hx-target="#inbox-results-region">
          <option value="">All accounts</option>
          <option value="support">Support</option>
        </select>
      </form>
      <div data-inbox-results id="inbox-results-region" hx-get="/threads/partial"></div>`;
  });

  afterEach(() => {
    delete window.htmx;
    document.body.innerHTML = "";
    vi.useRealTimers();
  });

  function beforeRequest(elt) {
    const event = new CustomEvent("htmx:beforeRequest", {
      bubbles: true,
      cancelable: true,
      detail: { elt, target: document.querySelector("[data-inbox-results]") },
    });
    elt.dispatchEvent(event);
    return event;
  }

  it("allows a focused filter select's own htmx request", () => {
    setupInboxAutoRefresh();
    const select = document.querySelector("select[name=account]");
    select.focus();

    expect(beforeRequest(select).defaultPrevented).toBe(false);
  });

  it("still pauses polling while a filter select has focus", () => {
    setupInboxAutoRefresh();
    document.querySelector("select[name=account]").focus();

    expect(beforeRequest(document.querySelector("[data-inbox-results]")).defaultPrevented).toBe(true);
  });

  it("keeps unrelated pause guards for filter requests", () => {
    document.body.insertAdjacentHTML("beforeend", "<dialog open></dialog>");
    setupInboxAutoRefresh();
    const select = document.querySelector("select[name=account]");
    select.focus();

    expect(beforeRequest(select).defaultPrevented).toBe(true);
  });
});

describe("pullTransform", () => {
  it("applies 0.4 damping and caps the content shift at 120px", () => {
    expect(pullTransform(0)).toBe("translateY(0px)");
    expect(pullTransform(25)).toBe("translateY(10px)");
    expect(pullTransform(70)).toBe("translateY(28px)");
    expect(pullTransform(500)).toBe("translateY(120px)");
    expect(pullTransform(-10)).toBe("translateY(0px)");
  });
});

function touchEvent(type, clientY) {
  const event = new Event(type, { bubbles: true, cancelable: true });
  const touch = { clientY };
  Object.defineProperty(event, "touches", { value: type === "touchend" ? [] : [touch] });
  return event;
}

function contentTarget() {
  return document.querySelector("[data-pull-content]");
}

function dispatch(node, type, clientY) {
  node.dispatchEvent(touchEvent(type, clientY));
}

describe("setupPullToRefresh", () => {
  let originalOntouchstart;

  beforeEach(() => {
    document.body.innerHTML = `
      <header><nav class="navbar"></nav></header>
      <span class="pull-indicator" data-pull-indicator aria-hidden="true"></span>
      <main data-pull-content>
        <div data-inbox-results></div>
      </main>`;
    // jsdom has no touch support; the module only gates on this property.
    originalOntouchstart = window.ontouchstart;
    window.ontouchstart = null;
    Object.defineProperty(window, "scrollY", { value: 0, configurable: true, writable: true });
    // Stub layout reads so any accidental getBoundingClientRect call in the
    // pull path is observable (and asserted against) in the tests below.
    document.querySelector(".navbar").getBoundingClientRect = () => ({ bottom: 64, top: 0, height: 64 });
  });

  afterEach(() => {
    window.ontouchstart = originalOntouchstart;
    document.body.innerHTML = "";
    vi.useRealTimers();
  });

  it("translates only the content wrapper and keeps the navbar untouched", () => {
    setupPullToRefresh({ trigger: vi.fn(), repaint: vi.fn() });
    const navbar = document.querySelector(".navbar");
    const content = contentTarget();
    navbar.style.transform = "translateY(0px)"; // sentinel: must not be overwritten

    dispatch(contentTarget(), "touchstart", 100);
    dispatch(contentTarget(), "touchmove", 200); // pulled = 100 -> damped 40px

    expect(content.style.transform).toBe("translateY(40px)");
    expect(navbar.style.transform).toBe("translateY(0px)");
  });

  it("shows the indicator without writing an inline top or reading layout", () => {
    // jsdom cannot reproduce iOS rubber-band overscroll (where the document
    // paints shifted while scrollY and getBoundingClientRect stay frozen), so
    // this test only pins the structure: the indicator's position is owned by
    // CSS (absolute inside <header>), and JS must not measure or pin it. The
    // old "72px" expectation encoded the very bug this fixes: a JS-measured
    // top frozen in layout space drifts above the navbar mid-pull.
    const navbar = document.querySelector(".navbar");
    const readSpy = vi.spyOn(navbar, "getBoundingClientRect");
    setupPullToRefresh({ trigger: vi.fn(), repaint: vi.fn() });
    const indicator = document.querySelector("[data-pull-indicator]");

    dispatch(contentTarget(), "touchstart", 100);
    dispatch(contentTarget(), "touchmove", 130); // pulled = 30 -> below threshold

    expect(indicator.dataset.active).toBe("true");
    expect(indicator.textContent).toBe("Pull to refresh");
    expect(indicator.style.top).toBe("");
    expect(readSpy).not.toHaveBeenCalled();
  });

  it("releases to refresh past the threshold and resets the content", () => {
    vi.useFakeTimers();
    const refresh = { trigger: vi.fn(), repaint: vi.fn() };
    setupPullToRefresh(refresh);
    const content = contentTarget();

    dispatch(contentTarget(), "touchstart", 100);
    dispatch(contentTarget(), "touchmove", 190); // pulled = 90 -> over 70 threshold
    dispatch(contentTarget(), "touchend");

    expect(refresh.trigger).toHaveBeenCalledTimes(1);
    expect(content.style.transform).toBe("translateY(0)");
  });

  it("does not refresh when released short of the threshold", () => {
    const refresh = { trigger: vi.fn(), repaint: vi.fn() };
    setupPullToRefresh(refresh);

    dispatch(contentTarget(), "touchstart", 100);
    dispatch(contentTarget(), "touchmove", 130); // pulled = 30
    dispatch(contentTarget(), "touchend");

    expect(refresh.trigger).not.toHaveBeenCalled();
    expect(refresh.repaint).toHaveBeenCalledTimes(1);
  });

  it("ignores gestures that start while a dialog or row menu is open", () => {
    document.body.insertAdjacentHTML("beforeend", "<dialog open></dialog>");
    const refresh = { trigger: vi.fn(), repaint: vi.fn() };
    setupPullToRefresh(refresh);
    const content = contentTarget();

    dispatch(contentTarget(), "touchstart", 100);
    dispatch(contentTarget(), "touchmove", 200);
    dispatch(contentTarget(), "touchend");

    // The gesture never engaged, so the wrapper is only ever reset to neutral.
    expect(content.style.transform).toBe("translateY(0)");
    expect(refresh.trigger).not.toHaveBeenCalled();
  });
});
