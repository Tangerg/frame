# Frame Node process SDK

Use `defineProcessUnit` to declare a local unit and `serve` to bind its handlers to standard input/output. The peer owns its two streams for this session and closes them at EOF or shutdown. Standard output is reserved for Frame frames. Program diagnostics belong on standard error.

The application publishes its portable descriptor once. A `Capability` checks the descriptor, derives its exact identity and validates calls in both directions. Each import is a frozen binding to the host-selected export. During preparation the unit can acquire resources and register activation; host calls are available after final activation. The owning lifetime joins started work on closure. Losing a response never authorizes a new business operation.

Portable schema size constraints accept mathematical integers within the shared profile's exact nonnegative size range. Equivalent numeric spellings retain their original text in the capability identity.

`portable.ProfileSchema` owns the admitted dialect, keywords, value shapes and
schema traversal edges. The generated profile supplies these facts to both
runtime adapters; each runtime keeps its existing schema engine. Validation uses
a private JSON projection with prototype-free records, so only declared own
members can satisfy object constraints. Prototype names remain legal contract
names and validation leaves the caller's descriptor and values unchanged.
The bounded traversal captures each caller value once. Descriptor checks,
compilation, retained contracts and identity all use that capture; changing
getters cannot supply a second schema after admission. Method admission returns
the captured input, output or chunk; transport encoding and handler delivery use
that same value. The `admitInput`, `admitOutput`, `admitChunk` and
`admitInvocation` methods return their admitted capture instead of validating a
caller-owned object that would later be read again. Array budgets count every
indexed position, including holes that encode as null, before cloning or schema
execution can expand them.

Format admission follows the fixed list generated from the Go portable profile:
`date`, `time`, `date-time`, `uuid`, `json-pointer`, and `relative-json-pointer`.
Unknown formats are rejected. Go and Node use the portable predicates and their
shared conformance corpus; schema engine defaults do not define these formats.
Date and time fields follow RFC 3339: clock and offset components require two
unsigned decimal digits, and leap seconds are checked at the end of the UTC day.
Pattern validation uses the character classes and
end-of-input semantics exported by Go, including its definitions of `\s`, `.`,
and `$`, instead of JavaScript's different defaults.

`Capability.implement` captures a private handler table, verifies its exact method
set and callable values, and freezes the captured table. Unit preparation uses
the same boundary for every exported service. The caller's original table remains
unmodified; its getters and later mutations cannot replace an admitted handler.
Remote invocation uses that captured table as the method receiver, so methods can
compose through `this` without reading a mutable caller-owned table again.

Inferred client and handler types project primitive kinds, object openness,
required members, arrays and literals from the descriptor. Numeric literals use
`JSONNumber` so equivalent exact spellings remain representable. TypeScript cannot
express numeric equality or every JSON Schema predicate; `Method` still validates
each captured value. Complex reference and composition schemas can use the
published `portable/contractgen` projections.

## Cancellation and result evidence

`ProcessError.outcomeUnknown` reports whether the dispatched request lacks a
validated result. The error `code` still identifies its protocol category, and
`cause` preserves a locally observed failure or cancellation reason.

| Observation                                                                                 | `code`             | `outcomeUnknown` |
| ------------------------------------------------------------------------------------------- | ------------------ | ---------------- |
| Cancelled before Peer attempts the write                                                    | `cancelled`        | `false`          |
| Local frame/capacity rejection, or the remote framework rejects before entering the handler | rejection category | `false`          |
| Cancelled while awaiting an already dispatched request, or a remote cancelled reply         | `cancelled`        | `true`           |
| Handler error, invalid reply, interrupted write, or lost response                           | `outcome_unknown`  | `true`           |

A handler cannot turn a post-dispatch error into a pre-dispatch refusal by throwing
one of the framework's error codes. Cancellation is an intent to stop waiting or
work; it does not establish rollback, non-execution, or permission to replay.
Only the original business owner can settle its operation. A valid returned result
is preserved even when the remote handler observes cancellation before returning.
Request encoding captures user values before reserving a transport identity.
Cancellation, channel closure and call capacity are checked again after encoding,
including when a value getter dispatches another request. A dispatched call keeps
its capacity until its original reply or channel shutdown; cancelling its waiter
does not free that capacity.

Each streaming call owns its accepted chunk deliveries. Consumer errors, invalid
chunks, overlapping emits and an early final result remain failures for that
call, even when the producer catches an emit rejection. Call completion joins any
accepted consumer before releasing its stream handle. Emitting after completion
is rejected. An ignored emit promise cannot create an unhandled rejection outside
the original call's failure path.
Imported client calls belong to the unit's Lifetime, including their local chunk
consumers. Shutdown joins those consumers before running unit cleanup even when
the caller has already stopped waiting for the remote result.

## Query and command identity

The frozen method descriptor determines whether a request is a query or a
command. A command requires the business caller's original `commandID` in
`RequestOptions`; queries reject command identities. The SDK forwards that exact
identity to the handler's `CallContext.commandID`, including streaming calls.
It never substitutes the transport request sequence or generates a replacement
after an uncertain response. A known command method also requires its identity
in the TypeScript call signature.
Streaming queries and commands also require `onChunk` in that signature; unary
methods refuse it. These types project the same kind and streaming declarations
that invocation admission checks at runtime.

Command identities must be nonempty valid Unicode strings within the character
limit exported by `portable.ProfileLimits`. The SDK counts Unicode code points,
not UTF-16 code units or UTF-8 bytes. The business owner retains the command ID
and decides whether a repeated request observes an existing operation; the SDK
does not deduplicate business effects.

See `examples/document-indexer` for a runnable process with two imports and no exports.

`defineProcessUnit` captures the original setup method with its receiver, including
class instances with private fields. Imported calls capture the caller's signal,
command identity and stream callback once at invocation admission. Later property
replacement cannot change a dispatched call or redirect its chunks.

Stream consumer failures remain sticky even if the remote producer catches a
failed chunk acknowledgment and returns a value. Diagnostic error metadata cannot
interrupt recording that failure or closing pending peer work. The resulting
`ProcessError` is immutable and retains the original thrown value as its cause.
