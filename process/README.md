# Managed local processes

`process.Driver` implements the `local-process` profile using the independent
`frame-runner` and the [public stdio protocol](WIRE.md). It receives an already
approved Frame `Setup`; it performs no global provider lookup. The optional
[MCP adapter](mcp/README.md) can replace the wire protocol for a registered driver.
All profiles use the same durable attempt, runner, and exit-proof path.

Supply the same SQLite store and Admission used by the Host. `Source` resolves
the approved executable, arguments and optional contribution metadata; the Host supplies exact published descriptors. Configuration hashes
exclude secret values; environment credentials are supplied only to the runner.
The caller explicitly supplies frame and retirement limits.

Each creation is protected by a durable attempt before runner dispatch. The
actual process, handshake, setup and exports belong to the instance Lifetime.
Failed setup, stale publication and cancellation close that same resource. A
timeout or lost runner remains unconfirmed and retains its dependency closure.
Before recording that attempt, the driver checks the Store's complete encoded
capacity for local non-dispatch, confirmed runner exit and sealed absence.
The persisted runner proof contains its immutable `ExitReceipt`; diagnostic
reason, PID and exit code remain on the runner's original observation. The same
receipt identity is acknowledged after durable acceptance, so diagnostic growth
cannot prevent an admitted process from handing off its actual exit.
`Reconcile` consumes late Wait evidence only after the original protocol and
attachment cleanup has completed. It refuses earlier calls before changing the
attempt or acknowledging its exit. It never launches or cleans up again.

On Host restart, call `Recover` behind the recovery barrier before admitting any
new work. It fences older creation epochs and retires old attempts. An absent
attempt proves non-creation only when the original runner's `ProveAbsent`
observes both the durable fence and missing attempt in one locked operation.
The driver verifies that evidence against the original attempt before settling
its SQLite protection. Changing runners between control requests stays unknown;
a new runner cannot infer ownership from a PID or erase an old protection.

The runner's release proof covers its directly owned child. An application that
allows plugins to start additional processes must provide a stronger execution
profile with evidence for that larger resource set.

Go plugins call `ServeStdio` with a `process.Unit`. Setup receives exact imported
services and a structured Lifetime. Preparation, activation and accepted calls
belong to that Lifetime; retirement joins them before cleaning up their resources.
`Service.Invoke` and `StreamingService.Stream`
accept a `portable.Invocation`. Construct queries with `NewQueryInvocation` and
commands with `NewCommandInvocation`; the business caller supplies the original
command identity. Every relay carries that same value to the final handler.
The admitted method owns kind and schema validation. Queries cannot carry command
identity, and commands cannot execute without it. Transport request IDs remain
separate from business command identity and never authorize replay.

`NewMember` derives a unary method from its Go input/output types. Bind its typed
handler with `ImplementQuery` or `ImplementCommand`, then assemble the complete
service with `Implement`. The command handler receives the original command ID.
Typed callers use `Member.Query` or `Member.Command`. Hosts can provide any
implementation of `Service` or `StreamingService` matching the published descriptor.
Typed implementations retain `ErrOutcomeUnknown` when their handler fails or its
result cannot be encoded, including direct calls within the same process. Input
decoding failures occur before dispatch and retain only their original cause.

Typed schema derivation and decoding share `value.ValidateType` admission.
JSON field declarations own presence and representation; schema tags can narrow
values, including requiring an otherwise optional field. Tags that replace a
field's encoding are refused before publication. Pointers represent omission;
nullable material requires an opaque JSON field. Integer decoding accepts exact
decimal and exponent spellings such as `1.0` and `1e0` within the Go type's range,
without rounding through floating-point values. A reply that cannot be decoded
after dispatch retains `ErrOutcomeUnknown` and the decoding cause.

Ordinary `float32`/`float64`, byte sequences and `time.Time` are outside the
typed wire model. Machine floats cannot represent every admitted JSON number;
base64 content annotations do not validate strings; and `time.Time` cannot
represent the full RFC 3339 date-time vocabulary, including leap seconds.
Use `value.Number` for exact numbers and strings with semantic schema
constraints for textual encodings. `value.JSON`, `json.RawMessage`, and
`any` retain opaque JSON, including null. Other custom encodings require both an
encoder and decoder plus a value-receiver `JSONSchemaModel`. That domain owner
must describe exactly the wire values its codec can consume; Frame does not infer
the contract from private fields or a decoder alone.
Map keys may be plain or named strings without custom codecs; the schema
reflector does not project a key codec's constraints onto property names.
Descriptor-only contracts use `portable.AdmitMethod` with explicit schemas.
