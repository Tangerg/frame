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

| Direction | Method | Request | Success |
| --- | --- | --- | --- |
| Host → child | hello | version, maxFrameBytes | version, unit, maxFrameBytes, imports and exports (name → contract digest) |
| Host → child | prepare | imports: slot → ordered array of {handle,source} | true |
| Host → child | activate | empty object | true |
| Either | call | handle, member, input; streaming calls include a stream token | final value |
| Either | chunk | stream, value | true once consumed |
| Host → child | close | empty object | true after owned work joins |

A source is the exact Frame ExportRef. Grants for a Point export also carry its registered service reference in `valueContract`; all other grants omit this field. Only the host projects bindings. A missing optional
provider is an empty slot array. No-export units use an empty export object. A handle is
scoped to this execution and its contract. The host rejects service calls before activation
or after retirement begins. Each sender waits for a chunk acknowledgement before sending
the next one. Reader loops must continue processing messages during nested calls. Peers
refuse excess frames and work rather than buffering without bounds.

The host verifies the complete sets of imports and exports and their digests. Crash recovery
uses the original runner's wait or absence evidence, independently of any protocol response.
