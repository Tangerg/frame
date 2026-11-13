# Managed MCP operations

Use `mcp.ManagedProtocol` for a process implementing Qimu's `portable/1` operations. A host that wants ordinary MCP tools can register `mcp.Tools` separately. Both profiles use Frame's process driver and the MCP SDK over the runner's single attached stdio stream.

## Register a backend

Read the trusted capability descriptor from the approved package or contract catalog. `ParseManagedContract` admits its schemas once and projects the Frame capability and Qimu wire identity. Qimu includes scope and composition in its capability fingerprint. Descriptions remain presentation text. The package source owns which physical export implements each Frame export.

Capability and method lookups are detached projections. The adapter captures a
managed export before retaining it, and callback grants retain their admitted
method independently of the author's or broker's handle. Replacing a projection
cannot alter validation behind an already published identity.

```go
contract, err := mcp.ParseManagedContract(descriptor)
if err != nil {
	return err
}
service, err := framehost.NewPortableContract[frame.Service]("1", contract.Capability())
if err != nil {
	return err
}

// This example registers one pinned backend. A multi-package source resolves
// the mapping using the installation, release and unit in binding.Source.
cfg.Protocol = mcp.ManagedProtocol{
	Exports: func(binding framehost.UnitBinding) (map[string]mcp.ManagedExport, error) {
		if binding.Source != approvedUnit {
			return nil, frame.ErrContractMismatch
		}
		return map[string]mcp.ManagedExport{
			"hooks": {Name: "hook-provider", Contract: contract},
		}, nil
	},
}
driver, err := process.New(cfg)
```

Pass the driver to `framehost.New`, register `service` and declare the logical export `hooks` with `service.Ref()` and the unit profile `mcp.ManagedProfile`. Supply the executable digest, directory and exact environment through the process source. Frame owns installation, grants, process launch and release. The host owns package discovery, credentials and application policy.

Before launch, preparation admits one contract per physical export and one
identity per global method name. Multiple exports may share a method only when
its identity agrees, including kind, target and schemas. Conflicting declarations
fail before the child executes and identify their method and source exports.

After MCP initialization the adapter calls `operations.describe`. It compares the complete set of physical capabilities and their member identities with the frozen declaration. It then sends `operations.setup` with the logical contribution IDs and physical export names. Frame's activation transition alone sends `operations.activate`. A refused setup or a mismatched description fails acquisition. Closing the original session retires the original child.

## Call semantics

Borrow the registered `frame.Service` through Frame. `Invoke` accepts the same
immutable `frame.Invocation` used by native and neutral process services.
`NewQueryInvocation` constructs a query; `NewCommandInvocation` requires the
business caller's original identity. A managed command can pass through a neutral
process relay without a concrete MCP type assertion or a replacement identity.
Inside the borrow, use `*mcp.ManagedService` for backend input admission and receipts.

Cancellation before handing an invocation to the SDK returns its local cause.
After that handoff, cancellation, transport failure and invalid results retain
`frame.ErrOutcomeUnknown` together with their cause for both queries and
commands. The SDK owns the sending queue, so this adapter does not infer
non-dispatch from a cancelled SDK wait. A validated `*mcp.Refusal` is the backend's
explicit business reply and remains distinct from missing outcome evidence; it
does not assert that the command was never dispatched or authorize a new command.
Only the original command's business owner and receipt can settle its effects.

Only decoding the original backend reply can establish a known refusal. A local
`Refusal` returned by a consumer, used as a cancellation cause or wrapped inside
another error remains a local failure. Cancellation after a validated reply
arrives cannot replace that reply. A stream's successful final value additionally
requires all admitted chunks to have completed without rejection; a final value
cannot repair a failed or still-pending delivery. An explicit backend refusal
remains known alongside any separate delivery diagnostic.

Callback retirement is separate from the business outcome. If its release wait
fails after the result is validated, the result remains available together with
the cleanup error; that error alone does not add `frame.ErrOutcomeUnknown`.
The original lifetime resource continues waiting for its own release evidence.

| Operation | Calling contract |
| --- | --- |
| `ValidateInput(ctx, member, input)` | Asks the exact backend to admit its typed input after local schema checks. No command identity or business dispatch. |
| `Invoke(ctx, invocation)` | Unary query or command, checked against the frozen method. Commands retain the caller's original identity. |
| `Stream(ctx, invocation, emit)` | Query or command with synchronous chunk delivery through a per-call grant. Acknowledge a chunk only after its consumer accepts it. The separately validated final result completes the stream. |
| `ReadReceipt(ctx, originalInvocation)` | Queries the backend's exact `commands.get` contract using the original command identity, input digest and the same export. A found receipt can represent a successful reply or a typed business refusal. |

Use `mcp.WithInvocation` when the caller owns invocation and target evidence. Invocation ID and command ID have separate meanings. A transport failure after command dispatch can leave an unknown effect. Inspect the original receipt where supported. Missing evidence never authorizes replay. Keep borrowed services and their grants within their owning lifetime.

## Reverse calls

| Grant | Authority and lifetime |
| --- | --- |
| `OpenCallbacks` | The host issues a session grant for declared dependencies or an explicitly approved background role. Each `ManagedImport` links a dependency ID to a Frame import slot and its upstream descriptor. The bridge projects optionality and the original installed providers from `Setup.Imports`. A background target uses that installation's ID. |
| `OpenCall` | The host selects permitted effects using the caller's context, original session and immutable `ManagedCall`. It includes the frozen Frame export (installation, release, unit, logical name and contract), admitted method, original input and command ID. A nil lease grants no call-scoped callback. This grant can coexist with stream delivery. |
| `OpenStream` | Use the callback receiver's `Stream` method to issue a fresh credential for the Qimu chunk delivery operation. A stream cannot publish without a receiver. |

For a loopback route use `mcp.NewCallbackReceiver` with explicit call count, request and response bytes, and read timeout. Its `Grant` method can authorize one admitted host operation. The application selects the exact handler and verifies domain access. A host with an existing Qimu broker may implement the same hooks. It must bind its ACL to the original setup or call, authenticate the credential and call token, and bound its work.

A `ManagedCallback` contains an endpoint, two ephemeral tokens and a response budget. `Revoke` requests retirement. Close `Released` only after the route and every admitted callback have exited. Frame reserves a lifetime resource before the grant is issued. Unconfirmed callback cleanup keeps that instance pending even after the runner proves process exit. Never serialize credentials into a package, command argument or business input.

An issuing broker that panics or exits without returning cannot establish that no
grant escaped. The reserved resource stays unconfirmed. If revocation is
interrupted, the lease continues observing its original `Released` evidence.
A stream consumer that exits without returning cannot acknowledge its chunk.
Callback panic errors preserve an error-valued cause through local `errors.Is`
and `errors.As`, while their diagnostic text remains redacted. An embedded
`Refusal` in a panic is not a business reply and cannot be sent to the backend.
The loopback receiver also contains unary handler panics and non-returning
goroutine exits. The original grant retains their redacted diagnostic and error
cause until `Revoke` joins the admitted handler. A panic never reaches the default
HTTP panic logger or becomes a positive callback acknowledgment.

The Qimu provider address carries installation, release and contribution. Frame also has a unit ID. The adapter rejects ambiguous projections before launch. Catalogs must map each managed dependency to its original Frame slot. Hosts must explicitly provide background and reverse-operation authorization for packages that require them. Agent tools, prompts, filesystem paths, Flame roles, browser UI components and package installation rules remain application-owned.

## Reproduce independent interoperability tests

Use a full Git checkout of `Tangerg/flame` at the revision pinned in [the release workflow](../../.github/workflows/flame-interop.yml). The test script builds the unmodified upstream peers in that checkout and runs Frame from its own module:

```sh
node scripts/check-flame-interop.mjs /absolute/path/to/flame
```

The corpus is derived from the published contribution declarations in that checkout. The hooks test invokes a query, dispatches a command and reads the original receipt. The trajectory renderer reads one host-authorized document range, sends Markdown through an independent stream grant, and returns the content identity of the rendered document. A third peer built against upstream Qimu checks the ordered chunk protocol. Each execution uses the real Host, SQLite store and local runner.

A descriptor check establishes schema admission. Exercising a plugin's external provider, desktop components, scheduler or other product effects requires its host's permissions and integration tests.
