/* Tab-strip chrome. The strips live in the toolbar, outside the swapped results
   region, so this setup is re-run per strip because the htmx swap replaces the
   tab children (and their focus listeners) via the out-of-band update. */

export function updateTabOverflow(strip) {
  const tabs = strip.querySelector(".tabs");
  if (!tabs) {
    return;
  }
  strip.dataset.overflowStart = tabs.scrollLeft > 1 ? "true" : "false";
  strip.dataset.overflowEnd = tabs.scrollLeft + tabs.clientWidth < tabs.scrollWidth - 1 ? "true" : "false";
}

export function setupTabStrips(root) {
  (root || document).querySelectorAll("[data-tab-strip]").forEach((strip) => {
    const tabs = strip.querySelector(".tabs");
    if (!tabs) {
      return;
    }
    if (strip.dataset.tabStripReady !== "true") {
      strip.dataset.tabStripReady = "true";
      tabs.addEventListener(
        "scroll",
        () => {
          updateTabOverflow(strip);
        },
        { passive: true }
      );
      if (typeof ResizeObserver === "function") {
        new ResizeObserver(() => {
          updateTabOverflow(strip);
        }).observe(tabs);
      }
    }
    tabs.querySelectorAll(".tab").forEach((tab) => {
      if (tab.dataset.tabFocusBound === "true") {
        return;
      }
      tab.dataset.tabFocusBound = "true";
      tab.addEventListener("focus", () => {
        tab.scrollIntoView({ block: "nearest", inline: "nearest" });
      });
    });
    updateTabOverflow(strip);
  });
}

export function setupTabOverflowResize() {
  window.addEventListener("resize", () => {
    document.querySelectorAll("[data-tab-strip]").forEach(updateTabOverflow);
  });
}
