import { Capability, defineProcessUnit, serve } from "@frame/node";
import declaration from "./service.json" with { type: "json" };

const service = new Capability(declaration);
const unit = defineProcessUnit({
  id: "worker",
  imports: { upstream: service },
  exports: { relay: service },
  setup({ imports }) {
    return {
      relay: {
        read(input, { signal, emit }) {
          return imports.upstream.read(input, { signal, onChunk: emit });
        },
        wait(input, { signal }) { return imports.upstream.wait(input, { signal }); },
        probe(input, { signal }) { return imports.upstream.probe(input, { signal }); },
      },
    };
  },
});
await serve(unit);
