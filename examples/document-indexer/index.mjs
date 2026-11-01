import { Capability, defineProcessUnit, serve } from "@frame/node";
import contracts from "./contracts.json" with { type: "json" };

const unit = defineProcessUnit({
  id: "document-indexer",
  imports: {
    files: new Capability(contracts.files.descriptor),
    index: new Capability(contracts.index.descriptor),
  },
  exports: {},
  setup({ imports, onActivate }) {
    onActivate((work) => {
      work.start(async (signal) => {
        const path = "guide.md";
        const { text } = await imports.files.read({ path }, { signal });
        await imports.index.replace({ path, text }, { signal });
      });
    });
    return {};
  },
});

await serve(unit);
