# MCP process protocols

A host can run a third-party MCP server as a Frame plugin by registering a process driver with a protocol from `github.com/Tangerg/frame/process/mcp`. Ordinary Frame plugins continue to use their own execution profiles. The MCP endpoint runs over stdio on the child that Frame's runner launched for this binding.

| Remote server implements | Unit profile | Protocol to register |
| --- | --- | --- |
| Standard MCP tools | `mcp.ToolsProfile` | `mcp.Tools` |
| Qimu managed lifecycle, capabilities and command receipts | `mcp.ManagedProfile` | [`mcp.ManagedProtocol`](MANAGED.md) |

## Register a standard tools server

Supply the host's durable store, the existing runner and the source for the approved executable. Register one driver per profile. To run the neutral stdio profile on the same host, register another process driver with its default protocol.

```go
driver, err := process.New(process.Config{
    Store: store, Runner: runner, Source: source,
    CloseTimeout: 5 * time.Second, MaxFrameBytes: 1 << 20,
    Protocol: mcp.Tools{MaxCatalogPages: 16},
})
if err != nil {
    return err
}
if err := host.RegisterDriver(driver); err != nil {
    return err
}
```

Publish a portable service contract whose member names match the allowed MCP tool names. Declare its Frame export with profile `mcp.ToolsProfile` on the unit. For example, a unit named `backend` can export `documents`, a service with the single method `lookup`. Only `lookup` becomes callable through that service. Other exports can have their own sets of members. The standard profile takes no Frame imports.

Before launching the child, the adapter combines all requirements for each global
tool name. Shared tools must have one input schema and one output schema across
the members that require structured results. A full-result view can coexist with
those members without imposing its envelope schema on the tool. Any query view
requires the shared tool to advertise read-only operation. Conflicting local
declarations fail preparation and identify the contributing exports.

At activation the adapter reads the catalog up to the configured page limit. Every declared tool must appear exactly once. Its input schema must match the Frame input schema, including the object root. Member input and output schemas use Frame's portable admission rules. Object key order and whitespace do not affect the comparison. A query requires the tool's `readOnlyHint`. Trust in the executable belongs to the host. A server can misdescribe its own behavior.

## Call a tool

Borrow the declared export using the host's approved plan and use. Call the service with an object, including `{}` when the tool takes no arguments.

```go
input, err := value.ParseJSON([]byte(`{"id":"invoice-37"}`))
if err != nil {
    return err
}
invocation, err := portable.NewQueryInvocation("lookup", input)
if err != nil {
    return err
}
return frame.BorrowExport[process.Service](ctx, use, export, func(ctx context.Context, service process.Service) error {
    reply, err := service.Invoke(ctx, invocation)
    if err != nil {
        return err
    }
    return consume(reply)
})
```

Commands use `portable.NewCommandInvocation` with the business caller's original
identity. The adapter carries it unchanged in the standard MCP request metadata
at `_meta["io.github.tangerg.frame/commandID"]`; queries omit that entry. This
metadata identifies the invocation. It does not add receipt storage or idempotent
execution to a third-party server. Those guarantees belong to the service's
business contract; the managed profile exposes receipts where supported.

By default the host expects `structuredContent`. The tool must advertise the declared output schema, and its actual return value must validate against it. For an unstructured result, configure the output projection when registering the driver. Declare the Frame output as the MCP result object, with a `content` array and any other fields the consumer needs. This lets applications handle text, images and resource links.

```go
protocol := mcp.Tools{
    MaxCatalogPages: 16,
    FullResult: func(export frame.ExportDeclaration, method string) bool {
        return export.Name == "documents" && method == "preview"
    },
}
```

The host freezes this choice before the child starts. A server cannot change it by adding or removing its output schema. An MCP `isError` response produces a `*mcp.ToolError`. Its `Result` retains the response content for the caller. A missing or invalid successful reply, or a canceled call after dispatch, carries `process.ErrOutcomeUnknown`. The SDK's automatic multi-round-trip middleware is disabled, and input-required results retain an unknown outcome. The adapter closes catalog discovery after the original bounded admission. Its SDK sending boundary refuses later `tools/list` requests, including the SDK's implicit header-mismatch refresh. Stdio has no HTTP headers to refresh, so this path never expands the catalog budget or repeats `tools/call`. The original `HeaderMismatch` error remains inspectable with `errors.As`, alongside `process.ErrOutcomeUnknown`; the original command ID is unchanged. Only the original runner's terminal evidence releases a durable process attempt.

Full results and tool errors retain the original response envelope, including
extension fields and exact numbers. The adapter validates the original required
`content` array before returning a result; null or absent content cannot become
an empty successful reply through SDK normalization. The SDK owns request IDs,
dispatch and completion. A per-call wire projection observes the same first
response without generating requests or substituting a later response.

An already cancelled local context is rejected before SDK handoff and retains
only its cancellation cause. Once the SDK owns the request, a cancelled wait
cannot establish whether the peer received it. An explicit ToolError preserves
the tool's reported failure; it does not prove rollback or non-execution.

Canceling a request queued for stdio leaves the active sender and other readers alone. After a request is written, the SDK can notify the peer that the call was canceled. Cancellation while bytes are being written closes the shared stream because the peer may already have received part of a frame. Other in-flight calls on that stream can then lose their replies. The process still requires its original exit proof.

The adapter admits each complete newline-delimited inbound frame within
`MaxFrameBytes`, including its terminator, before the SDK interprets JSON-RPC.
Missing terminators, oversized frames, blank lines and values split across lines
fail the stream. Duplicate object names and malformed Unicode also fail before
the SDK can decode a different value from the original frame. Each line must
contain one complete JSON value. The same budget applies
to each outgoing frame. SDK decoder prefetch cannot move bytes between budgets.

## Connect a custom protocol

A host that defines its own MCP methods, callbacks or receipts implements
`process.Protocol` and keeps its own SDK client. In `Connection.Connect`, wrap
the channel the driver attached with `mcp.NewTransport`, passing the frame
budget the driver gave `Prepare`. The session then has the same inbound and
outbound frame admission and write cancellation as the protocols above.

```go
func (c *connection) Connect(ctx context.Context, channel io.ReadWriteCloser) ([]frame.ExportValue, error) {
    transport, err := mcp.NewTransport(channel, c.maxFrameBytes)
    if err != nil {
        return nil, err
    }
    session, err := c.client.Connect(ctx, transport, nil)
    if err != nil {
        return nil, err
    }
    c.session = session
    return c.exports(ctx, session)
}
```

The transport owns only the stream. Closing the session closes the attached
channel; the driver still owns launch, attachment and exit proof, and the host
protocol still owns its business outcomes.

The standard tools profile supports unary calls. Frame streaming methods and reverse imports require another protocol. MCP prompts, resource browsing, multi-round elicitation and network transports are outside this profile. List-change notifications do not grant methods or change an approved Frame binding. See the [MCP tools specification](https://modelcontextprotocol.io/specification/2025-11-25/server/tools) and the [executable integration test](tools_test.go).
