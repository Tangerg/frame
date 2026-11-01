// The package builder prepends the exact published Preview Point reference.
export default Object.freeze({
  id: "preview",
  imports: Object.freeze({}),
  exports: Object.freeze({ preview: point }),
  setup({ lifetime }) {
    const preview = Object.freeze({
      mount(element, document, theme, owner) {
        lifetime.assertOpen();
        owner.assertOpen();
        const frame = window.document.createElement("iframe");
        frame.setAttribute("sandbox", "");
        frame.title = document.title;
        frame.className = "document-preview";
        owner.cleanup(() => frame.remove(), "document-preview-frame");
        // Conversion already escaped the document. The built-in theme is an
        // immutable resource; the sandbox grants no script or same-origin rights.
        frame.srcdoc = `<!doctype html><html><head><meta charset="utf-8"><style>${theme}</style></head><body>${document.html}</body></html>`;
        element.replaceChildren(frame);
      },
    });
    return { preview: { metadata: { format: "html", priority: 10 }, value: preview } };
  },
});
