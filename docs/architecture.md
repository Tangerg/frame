# Contracts, owners and operation results

A service defines behavior. A point describes a collection of contributions to which a host may apply a policy. Multiple hosts can share the same service identity while collecting, decorating or selecting its implementations differently. A unit can consume imports without exporting a service. Go native implementations run as part of the host build. Portable process implementations must agree on the same published descriptors and wire profile.

| Fact | Authority | Lifetime |
| --- | --- | --- |
| Installation, selected immutable release and enabled state | Host | Until an admitted installation transition |
| Candidate order and business authorization | Host application | The application owns the rule, the Host validates the chosen members |
| Exact approved providers and accepted optional absence | Original binding | Reacquisition cannot silently switch provider |
| Approval of a use | Host admission | Withdrawal fences new approvals |
| Reachability of a borrowed object | Original use lifetime | Holds its accepted providers until admitted work has drained |
| Host admission and subscription lifetime | Original Host Lifetime | Its closure fences new Host work and closes registered Watches before release is confirmed |
| Process exit | Runner holding the direct child | Its original wait result; a disconnected connection proves no exit |
| Business result, pending task and command receipt | Original business owner | A lost response may require observing that same operation |

## Installation and offline tools

Build each artifact first, list it in the manifest, then package its immutable bytes. All three commands run without executing plugin code.

```sh
go run ./cmd/frame package --manifest plugin.json --out plugin.zip
go run ./cmd/frame inspect plugin.zip --format json
go run ./cmd/frame verify plugin.zip --policy publisher-policy.json
```

The bundled CLI trust policy pins SHA-256 archive digests in a file controlled by the host administrator: `{"allowedArchiveDigests":["<64 lowercase hexadecimal characters>"]}`. Inspect verifies byte integrity and reports publisher trust as unassessed. A matched pin assesses that specific archive. Neither operation grants execution. The Go distribution Store accepts an explicit Trust function for custom signature, publisher and capability policies.

Use Store.ImportArchiveHeld to obtain a content lease. Lease.Install validates and projects the verified manifest into the Host. For compiled native units supply native setup functions in distribution.Installation. For an external execution profile register its driver with the Host and implement distribution.ArchiveDriver on that driver. The driver checks the already integrity-validated unit's execution shape. Unknown profiles remain inspectable. Store retention must query both installed release facts and any retained business/execution dependencies. Close a lease to release content; Lease.Snapshot returns a detached projection.

A Host.Install error is not sufficient to tell whether a persistence operation committed. InstallationConclusion(err) reads the owning transition's conclusion, including committed with diagnostics, rejected and indeterminate. Inspect the original installation after recovery; do not replay an unknown mutation blindly.

## Execution boundaries

Pass the same Admission, epoch and SQLite Store to the durable Host and its adapters, for both installation persistence and execution protection. The process driver and browser bridge refuse a mismatched authority. An ordinary scope named like an installation cannot fence the Host's internal installation scope. Validate an authorized grant before starting any unit and revalidate after preparation.

Configure subprocess environments explicitly. An empty environment is the default. Host service grants limit access through Frame, not the process's operating-system rights. Deploy native code only with appropriate OS permissions. Maintain one trusted source for executable artifacts, interpreter pins and dependencies. A local Runner confirms the exit of its direct child, not that child's descendants or effects outside the framework.

The portable schema profile uses the published Scope JSON Schema library as a compiler. No host product or agent transport is required by the framework or its Node SDK.

The profile admits only `date`, `time`, `date-time`, `uuid`, `json-pointer` and
`relative-json-pointer` formats. Unsupported format names are rejected when a
descriptor is admitted. `portable.ProfileSchema` owns the dialect, keyword value
shapes and schema-child topology, including whether an edge consumes instance
depth. Go exports that declaration, the format inventory, resource budgets and
regular-expression atom meanings into `portable/profile.generated.json`; the
Node adapter consumes that projection while each runtime keeps its own mature
predicate compiler. In particular, whitespace uses Go's
ASCII character set, a dot excludes LF, and the end anchor requires the end of
the entire string. Fixed repetitions remain bounded by the profile. Both
runtimes execute `portable/testdata/schema-profile.json`, including Unicode,
pattern properties, references, and positive and negative format cases.

Contract generation consumes one typed projection graph for both languages.
Generated Go numbers retain their JSON lexemes through `value.Number`, and
open objects retain every admitted member. Shapes that cannot be represented
faithfully as static fields retain a lossless JSON value; the original canonical
schema still validates them. Date and time strings keep their original text.
See [contract generation](../portable/contractgen/README.md) for its public
surface and projection guarantees.

## Restoring accepted work

A declared export always identifies its original setup value. Dynamic Point
publications have separate operation identities and may reuse a withdrawn name
only in the live catalog. They never replace a declaration's setup slot or gain
durable reconstruction through that name. Withdrawal prevents a new setup or
declaration-root use from acquiring the original export; already accepted uses
and dependent instances retain the object they originally received. Slow setup
rechecks those original exports before activation and publication.

Native dynamic publication uses `Host.PreparePublication` to freeze metadata
and retain one implementation in an opaque `PublicationRequest`. Pass that
request to `Host.Publish` with an operation ID. Retrying an operation requires
the same request; creating another request identifies another native value even
when its metadata is equal. A returned Publish error proves that call did not
publish. A new operation may reuse a prepared request after withdrawal, but
creates a new membership. No comparison hashes arbitrary Go values or closures.

Import declaration order is not binding identity. `UnitBinding` canonicalizes
slots for both serialized identity and live admission equality. Provider order
within a selected slot remains part of the accepted binding.

A Use's owner must be outside the retirement path of its protected instances.
Admission follows the original resource-owner handles and provider graph, and
rejects a direct or nested owner cycle with `ErrDependencyCycle`. An instance
cannot own a Use of itself or of a consumer that keeps it alive. Application
consumers own their Uses independently; accepted external Uses can then drain
before their providers retire.

The live execution path admits the original plan. A durable host can configure ReadAcceptedBinding once at composition. Its loader receives the record identity and the request's scope grant, authorizes that record, then reads the binding accepted by the original business transaction. Host.AcquireRecord consumes that evidence. A diagnostic BindingSnapshot on its own cannot be presented to the Host as a newly authorized operation. The document queue is a working example in examples/document/tasks.go.

## Local causes and terminal close reports

The Host reads closing state from its original Lifetime. Closing an injected
Lifetime or cancelling its parent fences the same operations as `Host.Close`;
there is no independently advanced Host close flag. Each Watch is a resource
of that Lifetime, so a live subscription consumes both `HostConfig.MaxWatches`
and one `LifetimeLimits.MaxResources` entry. Closing the Watch closes its
channel and then returns both capacities. A Host with no Watches reserves no
subscription resource.

`CloseOutcome` is bounded evidence, with at most 32 diagnostic failures and
2,048 UTF-8 bytes per detail. `Close.Wait` and `Close.WaitReleased` report stable
`ErrCloseFailed` and `ErrUnconfirmedRelease` categories from that evidence.
They do not reconstruct original error chains from diagnostic text.

A first local `Lifetime.Close`, `Use.Close` or `Host.Close` observes cleanup
causes while it waits and preserves them for `errors.Is` and `errors.As` in the
returned error. To inspect causes across asynchronous shutdown or late release
proof, retain `causes := lifetime.ObserveCloseCauses()` before starting closure,
then inspect `causes.Err()` after observing the original close. The Lifetime is
the only writer. Concurrent observers share its current local capability.

The caller owns this opt-in retention of original errors and any objects they
reference. Lifetime retains the capability weakly and terminal handles retain
only the bounded report. Dropping both the capability and returned local errors
releases the original payloads; later close observations still report the same
operation and diagnostic text. An observer created after failures occurred
cannot recover causes that no caller retained.

## How a host chooses an execution protocol

A native host can use the root package alone. A host that runs local processes
composes one process driver per protocol with the same admission, durable store
and original runner. A unit's profile selects that driver. Each driver freezes its
declared service slots before launch. A peer's catalog is evidence of what the
peer can serve. It cannot install additional exports, change a grant, or choose
which host object a plugin receives. A disconnected stream cannot settle a
command or release a durable attempt.

| Fact | Owner | What crosses the boundary |
| --- | --- | --- |
| Physical executable and package revision | Host package source and content store | A pinned program for the approved unit |
| Service interface and identity | Published contract | Exact descriptor and digest in the host binding |
| MCP method mapping and representation | Selected MCP protocol | Verified peer tools, or the managed descriptor projected into both identity formats |
| Permission to start a binding | Frame Host admission | Frozen slots and lifetime-bound exports |
| Permission to call back into the host | Host application | A credential scoped to the original session or call |
| End of a reverse call grant | Broker that issued that grant | Joined callbacks and closed listener, independently of child exit |
| Command result and receipt | Plugin implementing the command | Observation of the original operation |
| Direct child exit | Original runner | The same durable evidence used by every process protocol |

See the [MCP guide](../process/mcp/README.md) for protocol-specific requirements.

## Migrating from the v0.0.1 source layout

| Previous import | Current owner |
| --- | --- |
| `frame/integrity`, `frame/jsonvalue` | `frame/value`: `Digest`, `Hash`, `ParseJSON`, `EncodeJSON` and `JSON.Bytes()` |
| `frame/control`, `frame/control/host` | `frame/browser` for both control DTOs and the host bridge |
| `frame/runner` | `frame/process/runner` |
| `frame/contractgen` | `frame/portable/contractgen` |
| `frame/protocol/process` | The local wire specification lives in `process/wire.json`; Go and Node consume that file |
| `@frame/browser`, `@frame/node`, `@frame/react` | `@frame/sdk/browser`, `@frame/sdk/node`, `@frame/sdk/react` from one tarball |
| Managed invocation, refusal and command-conflict evidence under `frame/portable` | `frame/process/mcp` owns this managed wire vocabulary |
| `frame/cancellation` | Removed. SQLite privately preserves context causes at its transaction boundary |

Here `frame/` stands for `github.com/Tangerg/frame/`. Native and local wire
identities, browser control protocol values, and SQLite record encodings survive
these source moves. Install the revised SDK tarball in downstream workspaces.
For a managed MCP plugin, publish host bindings from its original descriptor and
select the managed profile explicitly. The plugin author's command receipts and
the host's choices of scopes, grants and business authorization stay at their
original owners.
