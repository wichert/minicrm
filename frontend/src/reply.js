/* Mobile reply sheet + Cmd/Ctrl+Enter send. */

export function setupReplySheet() {
  const replyCard = document.querySelector(".reply-card");
  const stickyBar = document.querySelector(".reply-sticky-bar");
  if (!replyCard || !stickyBar) {
    return;
  }
  let lastFocused = null;
  const setSheetState = (open) => {
    if (open) {
      replyCard.dataset.sheetOpen = "true";
      document.body.dataset.replySheetOpen = "true";
    } else {
      delete replyCard.dataset.sheetOpen;
      delete document.body.dataset.replySheetOpen;
    }
    stickyBar.setAttribute("aria-expanded", open ? "true" : "false");
  };
  const closeReplySheet = () => {
    if (!replyCard.matches("[data-sheet-open=true]")) {
      return;
    }
    setSheetState(false);
    if (lastFocused) {
      lastFocused.focus();
      lastFocused = null;
    }
  };
  stickyBar.setAttribute("aria-controls", "reply");
  stickyBar.setAttribute("aria-expanded", "false");
  stickyBar.addEventListener("click", () => {
    lastFocused = document.activeElement;
    setSheetState(true);
    replyCard.scrollIntoView({
      behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth",
      block: "start",
    });
    const body = replyCard.querySelector("textarea[name=body_text]") || replyCard.querySelector("[data-editor] [contenteditable]");
    if (body) {
      body.focus();
    }
  });
  const closeBtn = replyCard.querySelector("[data-close-reply-sheet]");
  if (closeBtn) {
    closeBtn.addEventListener("click", closeReplySheet);
  }
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape" || !replyCard.matches("[data-sheet-open=true]")) {
      return;
    }
    closeReplySheet();
  });

  // Cmd/Ctrl+Enter sends from both the plain-text textarea and the rich-text
  // ProseMirror editor. Bound at form level because editor.js hides the textarea,
  // and preventDefault stops ProseMirror's own Mod-Enter block split.
  const sendForm = replyCard.querySelector("form.reply-form");
  const sendBtn = replyCard.querySelector("button[type=submit]");
  const bodyField = replyCard.querySelector("textarea[name=body_text]");
  let sending = false;
  if (!sendForm || !sendBtn) {
    return;
  }
  // Only latch once the submit actually proceeds: the draft module's submit
  // listener may preventDefault (draft still saving/conflicted), and latching
  // there would leave Cmd/Ctrl+Enter dead until the next navigation.
  sendForm.addEventListener("submit", (event) => {
    if (event.defaultPrevented) {
      sending = false;
      return;
    }
    sending = true;
  });
  // Capture phase: ProseMirror binds its own Mod-Enter handler on the editor
  // element, which would run first and split the block. Capturing on the form lets
  // us claim the shortcut before the editor sees it.
  sendForm.addEventListener(
    "keydown",
    (event) => {
      if ((event.metaKey || event.ctrlKey) && event.key === "Enter" && !event.altKey && !event.shiftKey) {
        event.preventDefault();
        event.stopPropagation();
        if (event.stopImmediatePropagation) {
          event.stopImmediatePropagation();
        }
        if (sending || sendBtn.disabled) {
          return;
        }
        if (!bodyField || !bodyField.value.trim()) {
          return;
        }
        sending = true;
        sendBtn.click();
      }
    },
    true
  );
}
