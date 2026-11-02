# Frame Node process SDK

Use `defineProcessUnit` to declare a local unit and `serve` to bind its handlers to standard input/output. The peer owns its two streams for this session and closes them at EOF or shutdown. Standard output is reserved for Frame frames. Program diagnostics belong on standard error.

The application publishes its portable descriptor once. A `Capability` checks the descriptor, derives its exact identity and validates calls in both directions. Each import is a frozen binding to the host-selected export. During preparation the unit can acquire resources and register activation; host calls are available after final activation. The owning lifetime joins started work on closure. Losing a response never authorizes a new business operation.

Portable schema size constraints accept mathematical integers within the shared profile's exact nonnegative size range. Equivalent numeric spellings retain their original text in the capability identity.

Format admission follows the fixed list generated from the Go portable profile:
`date`, `time`, `date-time`, `uuid`, `json-pointer`, and `relative-json-pointer`.
Unknown formats are rejected. Pattern validation uses the character classes and
end-of-input semantics exported by Go, including its definitions of `\s`, `.`,
and `$`, instead of JavaScript's different defaults.

## Cancellation and result evidence

`ProcessError.outcomeUnknown` reports whether the dispatched request lacks a
validated result. The error `code` still identifies its protocol category, and
`cause` preserves a locally observed failure or cancellation reason.

| Observation | `code` | `outcomeUnknown` |
| --- | --- | --- |
| Cancelled before Peer attempts the write | `cancelled` | `false` |
| Local frame/capacity rejection, or the remote framework rejects before entering the handler | rejection category | `false` |
| Cancelled while awaiting an already dispatched request, or a remote cancelled reply | `cancelled` | `true` |
| Handler error, invalid reply, interrupted write, or lost response | `outcome_unknown` | `true` |

A handler cannot turn a post-dispatch error into a pre-dispatch refusal by throwing
one of the framework's error codes. Cancellation is an intent to stop waiting or
work; it does not establish rollback, non-execution, or permission to replay.
Only the original business owner can settle its operation. A valid returned result
is preserved even when the remote handler observes cancellation before returning.

Each streaming call owns its accepted chunk deliveries. Consumer errors, invalid
chunks, overlapping emits and an early final result remain failures for that
call, even when the producer catches an emit rejection. Call completion joins any
accepted consumer before releasing its stream handle. Emitting after completion
is rejected. An ignored emit promise cannot create an unhandled rejection outside
the original call's failure path.

See `examples/document-indexer` for a runnable process with two imports and no exports.
