# Frame TypeScript SDK

`@frame/sdk` contains typed native contracts, unit declarations, structured resource
ownership, and exact JSON. The native entry point has no React or Node runtime dependency.
Public control messages are generated from `frame/control` Go types. They are
projections, not a second handwritten protocol schema.

## Native contracts and units

A consumer owns each contract's name, version, digest, kind, and execution mode.
Construct a `Service<T>`, `Point<Metadata, Value>`, or `Event<Payload>` from that
published identity. `optional(service)` declares explicit absence. Equal
TypeScript types alone never make two contract identities interchangeable.

```ts
import { defineUnit, Service } from "@frame/sdk";

const files = new Service<{ read(): string }>(publishedFilesContract);
const preview = new Service<{ text(): string }>(publishedPreviewContract);

export default defineUnit({
  id: "preview",
  imports: { source: files, comparison: files },
  exports: { preview },
  setup({ imports }) {
    return {
      preview: {
        text: () => `${imports.source.read()} / ${imports.comparison.read()}`,
      },
    };
  },
});
```

The Host approves each named import slot independently. Setup cannot look up or
select another provider. Native objects remain in their JavaScript Realm. A
consumer chooses portable codecs and remote drivers separately.

A static Point export returns one contribution, for example
`return { preview: contribution({ format: "html" }, renderer) }`.
The Unit declaration gives this prepared value its contract and export name.
The Host admits the complete ready batch before any value is public. Dynamic
contributions use a separate, explicitly admitted publication and withdrawal.

## Ownership

`Lifetime.run` registers work before invoking it. `branch` creates an owned
child. `cleanup` registers LIFO resource release before acquisition starts.
`dependsOn` records an already approved actual resource dependency; it performs
no discovery or resolution. Closing broadcasts cancellation to all children,
joins admitted work, then releases dependents before their providers.

A cleanup exception means release is unconfirmed. The original resource adapter
can later call the returned `Cleanup.confirmReleased(evidence)` once actual
termination is known. Cleanup is not replayed. A resource that was released but
reported a diagnostic returns `{ status: "released", error }` explicitly.

`close()` observes the initial settlement. `wait()` observes terminal release.
Cancelling either wait does not cancel the original exit. Concurrent callers
observe one exit identity. Final reports contain bounded diagnostic categories,
not thrown payloads or retained plugin closures.
The cancellation reason is an immutable closed-status error. Use the original
Exit's identity to identify an exit; the reason object does not identify one.

`acquire` and `withResource` build resource handles on that same Lifetime. They
reserve cleanup before asynchronous acquisition and release a late result when
its owner has already begun closing. They do not create an independent cleanup
manager.

`withResource` returns a value only after clean release. Confirmed release with
cleanup diagnostics rejects with `FrameError` code `close_failed`; unconfirmed
release uses `close_unconfirmed`. Both retain the Lifetime's bounded
`CloseOutcome` as their cause without retaining thrown cleanup payloads. Work
failure plus close failure is an `AggregateError` containing the original work
error and that close error. A work failure with clean release retains its original
identity. Late acquisition follows the same close-diagnostic projection.

## Exact JSON

Use `@frame/sdk/json` for values that may contain integers or decimal bounds beyond
JavaScript's exact `number` range. It exports `parseJSON`, `stringifyJSON`,
`cloneJSON`, `ExactNumber`, and numeric inspection helpers. `prettyJSON` serializes
those values with two-space indentation. `formatJSONText` changes only whitespace
in captured source, retaining numeric literals, string escapes and member order;
it is presentation, not validation or decoding. Protocol identities,
epochs, revisions, and generations are opaque strings in the control projection.

Run `npm run check` from the monorepo root. Regenerate
control types through `go run ./cmd/controlgen --out packages/sdk/src/protocol.generated.ts`
in `frame`; never edit the generated TypeScript file.
