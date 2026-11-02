// The package builder owns the declaration shared with the manifest.
const point = Object.freeze({ ref: published.point });
export default Object.freeze({
  id: published.unitId,
  imports: Object.freeze({}),
  exports: Object.freeze({ [published.export]: point }),
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
    return { [published.export]: { metadata: published.metadata, value: preview } };
  },
});
