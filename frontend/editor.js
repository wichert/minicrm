import {Schema} from "prosemirror-model";
import {EditorState} from "prosemirror-state";
import {EditorView} from "prosemirror-view";
import {toggleMark, baseKeymap} from "prosemirror-commands";
import {keymap} from "prosemirror-keymap";
import {history, undo, redo} from "prosemirror-history";

const schema = new Schema({
  nodes: {
    doc: {content: "paragraph+"},
    paragraph: {content: "inline*", group: "block", parseDOM: [{tag: "p"}], toDOM() { return ["p", 0]; }},
    text: {group: "inline"},
    hard_break: {inline: true, group: "inline", selectable: false, parseDOM: [{tag: "br"}], toDOM() { return ["br"]; }}
  },
  marks: {
    strong: {parseDOM: [{tag: "strong"}, {tag: "b"}], toDOM() { return ["strong", 0]; }},
    em: {parseDOM: [{tag: "em"}, {tag: "i"}], toDOM() { return ["em", 0]; }}
  }
});

function textDocument(text) {
  return schema.node("doc", null, text.split("\n").map(line => schema.node("paragraph", null, line ? [schema.text(line)] : [])));
}
function plainText(doc) { return doc.textBetween(0, doc.content.size, "\n", "\n"); }

document.addEventListener("DOMContentLoaded", () => {
  document.querySelectorAll("form[data-draft-url]").forEach(form => {
    const field = form.querySelector("textarea[name=body_text]");
    const formatting = form.querySelector("input[name=body_formatting]");
    const controls = form.querySelector("[data-editor-controls]");
    const mount = form.querySelector("[data-editor]");
    const bold = form.querySelector("[data-editor-bold]");
    const italic = form.querySelector("[data-editor-italic]");
    const plain = form.querySelector("[data-editor-plain]");
    let view, rich = true;
    let doc;
    try { doc = formatting.value ? schema.nodeFromJSON(JSON.parse(formatting.value)) : textDocument(field.value); }
    catch (_) { doc = textDocument(field.value); formatting.value = ""; }
    if (formatting.value && plainText(doc).trim() !== field.value.trim()) formatting.value = "";
    if (!formatting.value) doc = textDocument(field.value);
    const plugins = [history(), keymap({"Mod-z": undo, "Shift-Mod-z": redo, "Mod-y": redo, "Mod-b": toggleMark(schema.marks.strong), "Mod-i": toggleMark(schema.marks.em)}), keymap(baseKeymap)];
    function updateButtons() {
      for (const [button, mark] of [[bold, schema.marks.strong], [italic, schema.marks.em]]) {
        const {from, to, empty, $from} = view.state.selection;
        const active = empty ? !!mark.isInSet(view.state.storedMarks || $from.marks()) : view.state.doc.rangeHasMark(from, to, mark);
        button.setAttribute("aria-pressed", String(active));
      }
    }
    view = new EditorView(mount, {
      state: EditorState.create({doc, plugins}),
      attributes: {role: "textbox", "aria-multiline": "true", "aria-label": "Message", class: "reply-editor"},
      dispatchTransaction(transaction) {
        view.updateState(view.state.apply(transaction));
        updateButtons();
        if (transaction.docChanged) {
          field.value = plainText(view.state.doc);
          formatting.value = JSON.stringify(view.state.doc.toJSON());
          form.dispatchEvent(new Event("editorchange"));
        }
      }
    });
    for (const [button, mark] of [[bold, schema.marks.strong], [italic, schema.marks.em]]) {
      button.addEventListener("mousedown", e => e.preventDefault());
      button.addEventListener("click", () => { toggleMark(mark)(view.state, view.dispatch); view.focus(); });
    }
    plain.addEventListener("click", () => {
      if (rich) {
        if (formatting.value && !window.confirm("Switching to plain text removes formatting. Continue?")) return;
        formatting.value = ""; rich = false; field.hidden = false; field.required = true; mount.hidden = true;
        bold.hidden = true; italic.hidden = true; plain.textContent = "Use formatted text"; field.focus();
      } else {
        rich = true; field.hidden = true; field.required = false; mount.hidden = false;
        bold.hidden = false; italic.hidden = false; plain.textContent = "Use plain text";
        view.updateState(EditorState.create({doc: textDocument(field.value), plugins})); view.focus(); updateButtons();
      }
      form.dispatchEvent(new Event("editorchange"));
    });
    form.addEventListener("draftdiscarded", () => { view.updateState(EditorState.create({doc: textDocument(""), plugins})); updateButtons(); });
    form.addEventListener("submit", e => {
      if (!field.value.trim()) { e.preventDefault(); if (rich) view.focus(); else field.focus(); }
    });
    formatting.disabled = false; field.hidden = true; field.required = false; controls.hidden = false; updateButtons();
  });
});
