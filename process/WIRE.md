# Local process protocol

`wire.json` owns the protocol revision, limits, methods and stable error codes.
A frame is one UTF-8 JSON object followed by LF. The negotiated byte bound includes LF;
payloads must contain no unescaped newlines. Both endpoints independently issue increasing,
canonical positive decimal string IDs. Commands, attempts and business operations retain
their original identities separately. Duplicate keys and malformed frames terminate the channel.

Requests have `{id,method,params}`; an answer contains exactly one of `result` or
`error:{code,message}` and the original ID. Cancellation is a notification
`{method:"cancel",params:{id}}` referencing the original outbound request. A stopped
wait never proves a mutation did not execute.

The error category reports protocol evidence, not a business settlement:

| Observation | Category | Outcome evidence |
| --- | --- | --- |
| Local cancellation before the request enters the output queue | local cancellation cause | This endpoint did not dispatch the request. |
| Framework rejection before entering the service handler | `invalid_request`, `capacity`, `contract_mismatch`, or `closed` | That invocation did not enter the service handler. |
| Cancellation after dispatch, including a handler's cancelled reply | `cancelled` | The cancellation is confirmed; the original effect remains unknown. |
| Handler failure, panic, invalid result, or lost response after dispatch | `outcome_unknown` | No validated result was obtained; the original effect remains unknown. |
| Validated success reply | result | The service returned that result; durable business settlement remains with its business owner. |

Go's received `cancelled` problem matches both `context.Canceled` and
`process.ErrOutcomeUnknown`. A locally cancelled context before dispatch matches
only its cancellation cause. A handler cannot turn its post-dispatch failure into
a framework rejection by returning one of the framework error codes. Neither
cancellation nor unknown outcome authorizes replay; reconciliation uses the
original business operation identity and owner.

A handler that exits its goroutine without returning also has an unknown outcome.
Its worker still replies and releases the original call slot. Local consumer panic
errors retain their causes for inspection; those causes cannot change the fixed
wire classification or become remote error text.

The sender checks cancellation again when it holds outbound admission, after
payload encoding and any wait for the queue lock. Cancellation already visible
at that boundary consumes no request ID, pending slot or output frame. A
cancellation racing after admission retains the original unknown-outcome rules.

| Direction | Method | Request | Success |
| --- | --- | --- | --- |
| Host → child | hello | version, maxFrameBytes | version, unit, maxFrameBytes, imports and exports (name → contract digest) |
| Host → child | prepare | imports: slot → ordered array of {handle,source} | true |
| Host → child | activate | empty object | true |
| Either | call | handle, member, input; commands require commandID; streaming calls include a stream token | final value |
| Either | chunk | stream, value | true once consumed |
| Host → child | close | empty object | true after owned work joins |

A source is the exact Frame ExportRef. Grants for a Point export also carry its registered service reference in `valueContract`; all other grants omit this field. Only the host projects bindings. A missing optional
provider is an empty slot array. No-export units use an empty export object. A handle is
scoped to this execution and its contract. The host rejects service calls before activation
or after retirement begins. Each sender waits for a chunk acknowledgement before sending
the next one. Reader loops must continue processing messages during nested calls. Peers
refuse excess frames and work rather than buffering without bounds.

The declared method determines query or command semantics. Queries must omit
`commandID`; even an explicit empty string or null is refused. Commands require
the business caller's nonempty, valid Unicode identity within the portable
profile's `maxCommandIDCharacters` limit. Relays carry that identity unchanged.
The `call` request ID identifies a transport exchange and cannot replace it.
This contract is protocol version 2; peers using the earlier command-less format
are refused during the handshake.

Each stream endpoint retains its first delivery failure for the whole call.
Consumer rejection, invalid chunks, panic, goroutine exit without a return,
overlapping deliveries or a final result
that overtakes an accepted delivery prevent success, even if the producer ignores
the failed acknowledgement. Finishing a call joins accepted delivery callbacks;
an escaped emitter cannot publish after the final result.

The host verifies the complete sets of imports and exports and their digests. Crash recovery
uses the original runner's wait or absence evidence, independently of any protocol response.
