# Contracts, owners and operation results

A service defines behavior. A point describes a collection of contributions to which a host may apply a policy. Multiple hosts can share the same service identity while collecting, decorating or selecting its implementations differently. A unit can consume imports without exporting a service. Go native implementations run as part of the host build. Portable process implementations must agree on the same published descriptors and wire profile.

| Fact | Authority | Lifetime |
| --- | --- | --- |
| Installation, selected immutable release and enabled state | Host | Until an admitted installation transition |
| Candidate order and business authorization | Host application | The application owns the rule, the Host validates the chosen members |
| Exact approved providers and accepted optional absence | Original binding | Reacquisition cannot silently switch provider |
| Approval of a use | Host admission | Withdrawal fences new approvals |
| Reachability of a borrowed object | Original use lifetime | Holds its accepted providers until admitted work has drained |
| Completion of an admitted work interval | Original interval in its Lifetime | Copies of a Work handle observe and finish that same interval |
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

`distribution.Limits.Entries` counts files, explicit directories and implicit
ancestors. Import and cached-release verification use the same path admission.
Every artifact read checks its bytes, size and executable status against the
manifest using one physical file snapshot. A previously acquired lease does
not turn altered cached content into trusted bytes.

A Host.Install error is not sufficient to tell whether a persistence operation committed. InstallationConclusion(err) reads the owning transition's conclusion, including committed with diagnostics, rejected and indeterminate. Inspect the original installation after recovery; do not replay an unknown mutation blindly.

An installation commit or confirmation callback that panics or exits its
goroutine establishes the Admission recovery barrier before releasing admission.
The original interruption still propagates; missing acknowledgement cannot prove
that a durable change did not occur.

## Execution boundaries

Pass the same Admission, epoch and SQLite Store to the durable Host and its adapters, for both installation persistence and execution protection. The process driver and browser bridge refuse a mismatched authority. An ordinary scope named like an installation cannot fence the Host's internal installation scope. Validate an authorized grant before starting any unit and revalidate after preparation.

`StartUnit` claims only an instance prepared under the same original scope grant.
An existing installation-scoped root is refused before another preparation can
begin; retire it explicitly before starting that root under a different authority.
Installation-scoped providers remain shareable by consumers with independent
grants. Copies of a `RunningUnit` share one private start claim; independent starts
have independent claims, and only the final claim requests retirement.

Configure subprocess environments explicitly. An empty environment is the default. Host service grants limit access through Frame, not the process's operating-system rights. Deploy native code only with appropriate OS permissions. Maintain one trusted source for executable artifacts, interpreter pins and dependencies. A local Runner confirms the exit of its direct child, not that child's descendants or effects outside the framework.

The portable schema profile derives schemas with Scope and compiles the admitted profile with `santhosh-tekuri/jsonschema/v6`. No host product or agent transport is required by the framework or its Node SDK.

The Node boundary captures caller-provided schema values once under the
portable resource budget. Validation, the retained declaration and its digest
consume that captured value; getters cannot supply separate versions to each
step. Arrays are read by index, so sparse entries and a custom iterator cannot
change which values are counted and encoded.

Method admission returns the captured payload it validated. Input dispatch,
handler results and stream delivery use that value throughout the call; encoding
does not reread the caller's original object.

The profile admits only `date`, `time`, `date-time`, `uuid`, `json-pointer` and
`relative-json-pointer` formats. Unsupported format names are rejected when a
descriptor is admitted. `portable.ProfileSchema` owns the dialect, keyword value
shapes and schema-child topology, including whether an edge consumes instance
depth. Go exports that declaration, the format inventory, resource budgets,
regular-expression atom meanings and variable-width syntax into
`portable/profile.generated.json`; the
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

Portable Point metadata retains one admitted immutable JSON value. Candidate
and selected-member reads decode fresh Go projections through its declared
codec; the native metadata cloner does not reinterpret that codec's private
representation. `Candidate.Metadata()` returns `(M, error)` because decoding
can fail. Selection policies read each candidate once, propagate any error,
then compare the detached metadata. Encoding a native candidate similarly uses
a detached value, so a custom marshaler cannot mutate the publication.

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
Empty `UnitRoots` have one canonical representation, `[]`, including after a
public JSON round trip. Unresolved import providers retain their distinct nil
meaning.

Admission captures a business owner's validated ID once at registration. Later
retention reads, ordering and diagnostics use that registered identity without
calling external identity accessors under the Admission state lock.

A Use's owner must be outside the retirement path of its protected instances.
Admission follows the original resource-owner handles and provider graph, and
rejects a direct or nested owner cycle with `ErrDependencyCycle`. An instance
cannot own a Use of itself or of a consumer that keeps it alive. Application
consumers own their Uses independently; accepted external Uses can then drain
before their providers retire.

Setup and Use lifetimes retain their original Host's admission authority. A
different Host refuses them as a Use owner or Host lifetime before preparing
work: neither Host can atomically inspect and admit the other's dependency
graph. An application can compose several Hosts under its own `NewLifetime`,
which does not originate from a Host's provider graph.

The live execution path admits the original plan. A durable host can configure ReadAcceptedBinding once at composition. Its loader receives the record identity and the request's scope grant, authorizes that record, then reads the binding accepted by the original business transaction. Host.AcquireRecord consumes that evidence. A diagnostic BindingSnapshot on its own cannot be presented to the Host as a newly authorized operation. The document queue is a working example in examples/document/tasks.go.

## Local causes and terminal close reports

The Host reads closing state from its original Lifetime. Closing an injected
Lifetime or cancelling its parent fences the same operations as `Host.Close`;
there is no independently advanced Host close flag. Each Watch is a resource
of that Lifetime, so a live subscription consumes both `HostLimits.MaxWatches`
and one `LifetimeLimits.MaxResources` entry. Closing the Watch closes its
channel and then returns both capacities. A Host with no Watches reserves no
subscription resource. Public Watch handles refer to one private subscription;
copying or replacing a caller's handle cannot create a second close transition
or change the subscription retained by the catalog and Lifetime.

Closing a Use's original Lifetime also starts that Use's original retirement.
`Use.WaitRetired` joins actual release, durable settlement and removal of Host
references regardless of which close entry point was used. Admission publishes
or refuses the Use before retirement reads its acquired holds.

That initialization barrier also closes when admission unwinds through a
panic. An interrupted durable approval requires recovery because its commit
outcome is unknown. A Use owned directly by the Host Lifetime registers one
resource there, retaining its instance dependencies on that same handle.

`Work` is a handle to a private interval. Copies share its completion and
resource-admission state under the original Lifetime mutex; a copied handle
cannot return capacity twice or acquire resources after completion.

Access handles likewise share one private borrow interval. Returning from the
callback closes every copy before finishing the original work; an independently
open Use cannot prolong an expired Access.

Setup and activation complete their original preparation even when a callback
calls `runtime.Goexit`. Failed preparation retires its own resources and never
publishes partial exports. Resource cleanup that exits without returning settles
as unconfirmed; only an original ResourceHandle proof can then establish release.
Cancellation callbacks are joined separately so one Goexit cannot abandon the
remaining cancellations or the original retirement.

A protected Use keeps its original settlement when ReleaseUse panics or exits
without returning. The failure fences admission and is reported to the waiting
close. Only a later explicit Close requests another settlement attempt; a failure
that first occurs during one Close cannot make that same Close silently retry.
Each failure owns one retry signal. Overlapping closes can join that request but
cannot preauthorize a retry for a later failure that they have not observed.

`CloseOutcome` is bounded evidence, with at most 32 diagnostic failures and
2,048 UTF-8 bytes per resource label and detail. Resource names are diagnostic
labels; only handles identify resources. `Close.Wait` and `Close.WaitReleased`
report stable `ErrCloseFailed` and `ErrUnconfirmedRelease` categories from that
evidence. They do not reconstruct original error chains from diagnostic text.

`Close.ReleaseProof` succeeds only after the original Lifetime has released.
Its compact `ReleaseProof` contains the original Close ID, without diagnostic
text or a competing release flag. Execution protection persists this terminal
proof; admitting a Use must reserve enough capacity for its eventual proof.

A first local `Lifetime.Close`, `Use.Close` or `Host.Close` observes cleanup
causes while it waits and preserves them for `errors.Is` and `errors.As` in the
returned error. To inspect causes across asynchronous shutdown or late release
proof, retain `causes := lifetime.ObserveCloseCauses()` before starting closure,
then inspect `causes.Err()` after observing the original close. The Lifetime is
the only writer. Concurrent observers share its current local capability.
An error raised by a local callback panic remains in that original cause chain;
bounded terminal diagnostics do not replace its inspectable identity.
Formatting those diagnostics is joined separately because an Error method may
itself call Goexit. An interrupted projection cannot change the original cleanup
conclusion or discard a ResourceHandle's release proof.

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
