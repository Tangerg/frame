# Frame TypeScript SDK

`@frame/sdk` contains typed native contracts, unit declarations, structured resource
ownership, and exact JSON. The native entry point has no React or Node runtime dependency.

For consumers running on Node, the published `package.json` owns the supported
runtime range in `engines.node`. CI derives the minimum version from that field
and runs the installed tarball's native, browser, Node and React entry checks on
that runtime. The repository development toolchain is specified separately in
`DEVELOPMENT.md`.
Public control messages and executable response decoders are generated from
`frame/browser` Go types. They share the original fields, omission rules, enum
values and encoding models.

## Installation and entry points

Download `frame-sdk-0.2.0.tgz` from the [v0.2.0 release](https://github.com/Tangerg/frame/releases/tag/v0.2.0),
then install the single SDK artifact and import the entry point for your role:

```sh
npm install ./frame-sdk-0.2.0.tgz
```

The SDK is distributed through GitHub release assets.

| Import | Use |
| --- | --- |
| `@frame/sdk` | Declare native contracts and units, acquire resources and manage lifetimes. |
| `@frame/sdk/browser` | Execute approved plans inside a browser Realm. [Browser guide](src/browser/README.md) |
| `@frame/sdk/node` | Implement a portable unit speaking Frame over stdio. [Node guide](src/node/README.md) |
| `@frame/sdk/react` | Bind an approved React component to an owned mount. [React guide](src/react/README.md) |
| `@frame/sdk/json` | Parse, clone and serialize exact JSON values. |
| `@frame/sdk/lifetime` | Use lifetime and release primitives directly. |
| `@frame/sdk/protocol` | Consume the generated control-plane messages and protocol constants. |

The native and browser entries work with `npm install --omit=optional ./frame-sdk-0.2.0.tgz`.
The Node entry uses the SDK's optional validation dependencies, included in a
standard installation. For React, install `react` and `react-dom` (version 19)
in the rendering application. TypeScript React applications also need their
corresponding `@types` packages.

## Native contracts and units

A consumer owns each contract's name, version, digest, kind, and execution mode.
Construct a `Service<T>`, `Point<Metadata, Value>`, or `Event<Payload>` from that
published identity. `optional(service)` declares explicit absence. Equal
TypeScript types alone never make two contract identities interchangeable.

Contract and Unit constructors capture declaration fields and optional import
wrappers before validation. Their frozen declarations retain that same capture;
later changes to caller-owned objects cannot relabel an admitted identity or slot.

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
Exit observers may return promises. Their synchronous and asynchronous failures
are isolated from release evidence, and their completion is not a resource
dependency. This also applies to observing an exit that has already completed.

`Lifetime.acquire` and `Lifetime.withResource` build resource handles on that same Lifetime. They
reserve cleanup before asynchronous acquisition and release a late result when
its owner has already begun closing. They do not create an independent cleanup
manager.
Pass the creation and release functions separately:
`await owner.acquire("file", openFile, file => file.close())`.
The Lifetime reserves cleanup before calling the creator and borrows its returned
value directly. The creator transfers ownership only on success; it owns rollback
if creation fails. The release policy is fixed before acquisition, without reading
resource properties or constructing a separate resource wrapper. A terminal
handle retains only the original exit observation, releasing both the borrowed
value and the disposal callback's captured data.

`Lifetime.withResource(label, create, release, work)` returns a value only after clean release. Confirmed release with
cleanup diagnostics rejects with `FrameError` code `close_failed`; unconfirmed
release uses `close_unconfirmed`. Both retain the Lifetime's bounded
`CloseOutcome` as their cause without retaining thrown cleanup payloads. Work
failure plus close failure is an `AggregateError` containing the original work
error and that close error. A work failure with clean release retains its original
identity. Late acquisition follows the same close-diagnostic projection.

## Exact JSON

Use `@frame/sdk/json` for values that may contain integers or decimal bounds beyond
JavaScript's exact `number` range. It exports `parseJSON`, `stringifyJSON`,
`cloneJSON`, `freezeJSON`, `ExactNumber`, and numeric inspection helpers.
`freezeJSON` detaches and recursively freezes one exact JSON value. Array
serialization reads indexed values, including nulls for holes; a custom iterator
cannot replace the array's wire contents. Both capture helpers return the
closed `JSONValue` or `FrozenJSONValue` domain (or `undefined` for an undefined
root), rather than promising to preserve an arbitrary caller type. A Date becomes
its JSON string or null, an omitted object member disappears, and a small bigint
may become a number. Custom `toJSON` methods on records are not invoked; an
enumerable function remains an unsupported JSON value.
`equalJSON` captures both operands under these same rules before comparing their
JSON values, with mathematical equality for numbers. Typed control captures use
the Go-generated protocol validators after normalization.
`prettyJSON` serializes
those values with two-space indentation. `formatJSONText` changes only whitespace
in captured source, retaining numeric literals, string escapes and member order;
it is presentation, not validation or decoding. Protocol identities,
epochs, revisions, and generations are opaque strings in the control projection.

Run `npm run check` from the monorepo root. Regenerate
control types through `go generate ./...`
in `frame`; never edit the generated TypeScript file.
