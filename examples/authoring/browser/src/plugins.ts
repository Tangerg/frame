import { contribution, defineUnit, type Unit } from "@frame/sdk";
import {
  files,
  converters,
  validators,
  sha256,
  type Converter,
  type FileReader,
  type Validator,
} from "./contracts";
import type { AuthorPackage, SourceConfiguration } from "./contracts.generated";

function source(value: unknown): SourceConfiguration {
  if (
    value === null ||
    typeof value !== "object" ||
    !("documentBase" in value) ||
    typeof value.documentBase !== "string"
  )
    throw new TypeError("documentBase is required");
  const url = new URL(value.documentBase);
  if (
    !["http:", "https:"].includes(url.protocol) ||
    url.username ||
    url.password
  )
    throw new TypeError("documentBase must be an HTTP source URL");
  return { documentBase: url.href };
}

export const fileReader = defineUnit({
  id: "files",
  imports: {},
  exports: { files },
  setup(context) {
    const configuration = source(context.configuration);
    const read: FileReader = async (reference, signal) => {
      if (!/^[a-z][a-z0-9-]*\.md$/.test(reference.path))
        throw new TypeError("document path must name one Markdown file");
      signal.throwIfAborted();
      const response = await fetch(
        new URL(reference.path, configuration.documentBase),
        { signal, redirect: "error" },
      );
      if (!response.ok)
        throw new Error(`document source returned ${response.status}`);
      const bytes = new Uint8Array(await response.arrayBuffer());
      signal.throwIfAborted();
      if ((await sha256(bytes)) !== reference.revision)
        throw new Error("document source revision changed");
      return {
        ref: reference,
        text: new TextDecoder("utf-8", { fatal: true }).decode(bytes),
      };
    };
    return { files: read };
  },
});

export const converter = defineUnit({
  id: "converter",
  imports: { files },
  exports: { markdown: converters },
  setup(context) {
    const read = context.imports.files;
    const convert: Converter = async (reference, signal) => {
      const document = await read(reference, signal);
      signal.throwIfAborted();
      const lines = document.text.trimEnd().split("\n");
      const title = lines[0]?.startsWith("# ") ? lines[0].slice(2) : "";
      const escape = (value: string) =>
        value.replace(
          /[&<>"']/g,
          (character) =>
            ({
              "&": "&amp;",
              "<": "&lt;",
              ">": "&gt;",
              '"': "&quot;",
              "'": "&#39;",
            })[character]!,
        );
      return {
        source: document.ref,
        title,
        html: lines
          .map((line) =>
            line.startsWith("# ")
              ? `<h1>${escape(line.slice(2))}</h1>`
              : `<p>${escape(line)}</p>`,
          )
          .join("\n"),
      };
    };
    return {
      markdown: contribution({ format: "markdown", priority: 10 }, convert),
    };
  },
});

export const validator = defineUnit({
  id: "validator",
  imports: {},
  exports: { title: validators },
  setup() {
    const validate: Validator = async (document, signal) => {
      signal.throwIfAborted();
      return document.title
        ? []
        : [{ rule: "title", message: "A document title is required." }];
    };
    return { title: contribution({ name: "title", order: 10 }, validate) };
  },
});

export class PartialSetupFailure extends Error {
  constructor(readonly releasedURL: string) {
    super(
      "The author example intentionally fails after reserving a Blob URL resource.",
    );
    this.name = "PartialSetupFailure";
  }
}

export const partial = defineUnit({
  id: "partial",
  imports: {},
  exports: { partial: validators },
  setup(context) {
    let url: string | undefined;
    context.lifetime.cleanup(() => {
      if (url) URL.revokeObjectURL(url);
    }, "partial setup Blob URL");
    url = URL.createObjectURL(new Blob(["unpublished result"]));
    throw new PartialSetupFailure(url);
  },
});

export const units: readonly Unit[] = Object.freeze([
  fileReader,
  converter,
  validator,
  partial,
]);

// Packaging projects the actual SDK declarations. Authors do not repeat slots,
// exports, contract digests or a framework protocol table in a second manifest.
export const declaration: AuthorPackage = {
  units: units.map((unit) => ({
    id: unit.id,
    imports: Object.entries(unit.imports).map(([slot, contract]) => ({
      slot,
      contract: "contract" in contract ? contract.contract.ref : contract.ref,
      optional: "optional" in contract && contract.optional,
    })),
    exports: Object.entries(unit.exports).map(([name, contract]) => ({
      name,
      contract: contract.ref,
    })),
  })),
};
