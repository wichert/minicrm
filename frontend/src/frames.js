/* Email iframe plumbing: autoresize, blocked-image activation, session-expiry
   notice. The iframe is sandboxed and served from /emails/{id}/body, so this code
   only ever touches attributes and dimensions — never the message HTML. */

function syncEmailFrameHeight(frame) {
  try {
    const doc = frame.contentDocument;
    if (!doc || !doc.body) {
      return;
    }
    let h = Math.ceil(doc.body.getBoundingClientRect().height);
    if (doc.documentElement && doc.documentElement.scrollWidth > doc.documentElement.clientWidth) {
      h += 16;
    }
    if (h > 0) {
      frame.style.height = h + "px";
    }
  } catch {
    // Fallback to CSS height if contentDocument is unavailable.
  }
}

let sessionExpiredReloadTriggered = false;

function handleExpiredEmailFrame(frame) {
  const notice = document.createElement("div");
  notice.className = "session-expired-notice";
  const text = document.createElement("span");
  text.textContent = "Your session has expired.";
  const link = document.createElement("a");
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

export function observeEmailFrame(frame) {
  try {
    const doc = frame.contentDocument;
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
      const ro = new ResizeObserver(() => {
        syncEmailFrameHeight(frame);
      });
      ro.observe(doc.body);
      if (doc.documentElement) {
        ro.observe(doc.documentElement);
      }
    }
    doc.querySelectorAll("img").forEach((img) => {
      if (!img.complete) {
        img.addEventListener("load", () => {
          syncEmailFrameHeight(frame);
        });
      }
    });
  } catch {
    // Ignore if the frame document is not ready yet.
  }
}

export function setupEmailFrames() {
  document.querySelectorAll("[data-load-images]").forEach((button) => {
    button.addEventListener("click", () => {
      const frame = button.closest(".message").querySelector(".email-frame");
      try {
        const doc = frame.contentDocument;
        if (!doc || !doc.body || doc.readyState !== "complete" || doc.URL === "about:blank") {
          button.textContent = "Message is still loading; try again";
          return;
        }
        const images = doc.querySelectorAll("img[data-blocked-src]");
        if (!images.length) {
          button.textContent = "No blocked images found; try again";
          return;
        }
        images.forEach((image, index) => {
          image.src = button.dataset.imagePrefix + index;
          image.removeAttribute("data-blocked-src");
        });
        button.disabled = true;
        button.textContent = "Images requested";
      } catch {
        button.textContent = "Could not load images";
      }
    });
  });

  document.querySelectorAll(".email-frame").forEach((frame) => {
    frame.addEventListener("load", () => {
      observeEmailFrame(frame);
    });
    if (frame.contentDocument && frame.contentDocument.readyState === "complete") {
      observeEmailFrame(frame);
    }
  });

  document.querySelectorAll("details.message").forEach((details) => {
    details.addEventListener("toggle", () => {
      if (details.open) {
        details.querySelectorAll(".email-frame").forEach((frame) => {
          observeEmailFrame(frame);
        });
      }
    });
  });

  window.addEventListener("resize", () => {
    document.querySelectorAll(".email-frame").forEach((frame) => {
      syncEmailFrameHeight(frame);
    });
  });
}
