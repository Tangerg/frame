# Contracts, owners and operation results

A service defines behavior. A point describes a collection of contributions to which a host may apply a policy. Multiple hosts can share the same service identity while collecting, decorating or selecting its implementations differently. A unit can consume imports without exporting a service. Go native implementations run as part of the host build. Portable process implementations must agree on the same published descriptors and wire profile.

| Fact | Authority | Lifetime |
| --- | --- | --- |
| Installation, selected immutable release and enabled state | Host | Until an admitted installation transition |
| Candidate order and business authorization | Host application | The application owns the rule, the Host validates the chosen members |
| Exact approved providers and accepted optional absence | Original binding | Reacquisition cannot silently switch provider |
| Catalog read and candidate provenance | Original Snapshot record | Copies retain the same immutable read |
| Graph presented for admission | Original BindingPlan record | Replacing a caller's handle cannot replace an in-flight request |
| Approval of a use | Host admission | Withdrawal fences new approvals |
| Reachability of a borrowed object | Original use lifetime | Holds its accepted providers until admitted work has drained |
| Completion of an admitted work interval | Original interval in its Lifetime | Copies of a Work handle observe and finish that same interval |
| Cleanup progress and release proof | Original private close operation | Close value handles observe that same operation without copying progress |
| Host admission and subscription lifetime | Original Host Lifetime | Its closure fences new Host work and closes registered Watches before release is confirmed |
| Process exit | Runner holding the direct child | Its original wait result; a disconnected connection proves no exit |
| Business result, pending task and command receipt | Original business owner | A lost response may require observing that same operation |

## Contract and value owners

The directory tree follows the facts each package owns. The root `frame` package
owns callable identity, method kind and invocation semantics, typed service calls
and resource lifetimes. `host` owns installation, declarations, catalog selection,
exact binding and admission. `value` owns the pure JSON/schema language,
codec/type admission and bounded validation. `process` translates those admitted
calls into an attached process session. `process/runner` proves direct child exit;
`process/mcp` translates optional external protocols. `browser`, `sqlite` and
`distribution` keep their own realm, transaction and artifact guarantees.
`contractgen` publishes language projections and is never a core dependency.

The root kernel has no dependency on Host application state. Host construction
composes the kernel through its public guarantees. A lifecycle domain issues
internal lifetimes and checks their affiliation; the Host cannot assign a
Lifetime's private authority field. Dependency validation reads immutable
lifetime identities and original resource ownership, never another mutable graph.

Every capability Frame issues is a value handle over private state:
`frame.Lifetime`, `Work`, `Close` and `ResourceHandle`; `host.Host`,
`Admission`, `Use`, `Setup`, `Access`, `Watch`, `RunningUnit`, `EventRoute`
and `PublicationRequest`; `sqlite.Store` and `Database`; `distribution.Store`
and `Lease`. Copying one cannot copy its lock, resource list, generation counter
or recovery barrier, and the zero handle is the single unissued state that
`Valid` reports. Domain grants, work intervals and callbacks retain the original
owners; replacing an application variable cannot redirect them.

Host export membership, original setup slots and dynamic publication receipts
converge in `host/catalog.go`. Each instance delegates its exports to one owned
set. Visibility comes only from the Catalog's member index. Durable
reconstructibility comes from the original declared setup slot, including after
a dynamic value takes its name. Publication requests and member records share
one prepared native value, while operation identity and member identity retain
their distinct replay semantics.

Admission owns each original scope generation. ScopeGrant handles reference
that record instead of copying its context and generation. Scope states are
replaced on every fence, including repeated fences of an already closed
generation. Installation recovery may advance only the exact state it admitted.
Recovery reports likewise retain immutable identity; a later report invalidates
an older restoration even when its cause is unchanged. Diagnostic counts cannot
authorize recovery. These rules live together in `host/admission.go`.

Installation creation, update, enable, replacement and removal share one
transition lifecycle. Their existence and destructive-change policies are checked
before the common commit, scope projection, retirement and confirmation sequence.

Instance preparation owns explicit pending, ready and failed states. Catalog
publication and readiness commit together before waking observers; a visible
setup cannot remain preparing. Binding plans explicitly distinguish declaration
resolution from exact instance capture. Candidate reads and Catalog revisions
retain their own provenance rather than doubling as mode flags.

Activation captures the actual instances it prepared into that same admission
plan. Acquisition cannot replace them by looking up a successor, even if its
setup graph has the same durable identity. A new acquisition may prepare a new
instance plan from an accepted business binding; an in-flight handoff retains
its original instances.

Use settlement follows explicit pending, failed, retrying and complete states.
Only a failed attempt observed by an explicit close may be retried. An immutable
failure identity replaces report-number arbitration; channels project state
changes and cannot independently authorize a transition.

An instance hold pairs the original runtime with its actual Lifetime Work.
Provider dependencies and Use dependencies retain these edges directly instead
of synchronizing separate instance and hold lists. Admission reads the original
Host, business owner and full held runtime closure. Cancellation notifications
only start cleanup; they cannot keep a cancelled owner available for new work.
An instance withdrawn for draining still serves work already accepted by a live
Use. Preparation checks the same held closure before executing callbacks.

New admission checks current membership across that original closure, including
indirect providers. Captured plans cannot create fresh Uses through a consumer
whose provider is already draining. Accepted Uses continue against their original
execution permission; withdrawal alone does not cancel that permission. Both
queries traverse the actual holds without another dependency or retirement index.

Cancellation of an instance or provider follows its original Lifetime context,
not the Work retaining it. That hold may wait for the very consumer cancellation
must release. Revocation ends permission and starts the existing retirement
worker while retaining all original resource and settlement evidence.

Use parent reservations observe the original Host and business-owner contexts
directly. Their cancellation starts the original Use close even when parent
resource callbacks have not run. A Host-owned Use reserves and observes its one
parent once; no second dependency or permission ledger is created.

A Use observes the original grant it accepted, including installation scopes.
Its held instance may have been activated earlier or already be draining, so
instance shutdown cannot substitute for the Use's own scope-fence observation.
That fence starts the original close and settlement without releasing holds early.

Preparation owns its pending candidate evidence. Its completion transition
drops that evidence together with the result, whether publication succeeds,
preparation fails or graph planning abandons the instance. Ready instances keep
their exact provider holds instead of continuing to validate discovery candidates.

Use retirement commits hold release and Host-index removal together before
forwarding diagnostics, which may reenter admission. Instance registration and
retirement live together in `host/instance.go`; its retirement wait policy also
serves Uses and start claims. An observer's cancellation cannot replace an
already confirmed original completion.

Instance retirement has one worker for the drain-to-close transition. Parent
resource callbacks await its original exit instead of starting their own drain.
The original Lifetime alone proves physical release; retaining a Host record for
diagnostic forwarding does not keep an already released execution in the overlap
set. Parent confirmation and Host-index retirement remain attached to that original
instance, so a late finalizer cannot remove its successor.

The current-instance index owns withdrawal. An instance withdraws
its exact membership and exports in one Host commit. There is no independent
retiring flag or finalizer that can repeat that transition. Preparation, start
claims and observations derive withdrawal from this same membership, while
accepted Uses retain their original runtime holds until settlement.

The Use close continuation and its failed-settlement coordination live together
in `host/use_retirement.go`. Retry notifications belong to the particular failed
attempt that produced them. Settlement waits inspect the owner's current
conclusion after wakeup or cancellation; a cancelled observer cannot hide a
confirmed completion or a newly recorded failure.

Event routes derive distinct participants from the original sender and receiver
Uses. A shared borrow reserves one resource and one delivery interval. A delivery
pairs each original Use with its actual Work, checking both before and after the
application callback; a composed context is only a notification. Refused entry,
panic and Goexit release the delivery's own intervals without changing another
caller's capacity or replacing the route.

The SDK's `Lifetime` owns resource acquisition as well as release. Creators
return the resource directly; their disposal policy is supplied before creation.
The creator owns rollback on failure, and the Lifetime owns every successful
result, including one delivered after cancellation. An acquired handle projects
the original exit and drops both the value and disposer on confirmed release.
React mounting composes this capability for its root instead of maintaining a
partially initialized root or a parallel cleanup path. Binding and mount callbacks
are captured once with their original receivers.

Lifecycle wait boundaries reject missing contexts, including typed nils, before
starting an exit or changing admission. A cancelled valid context abandons only
its caller's wait; completed release evidence remains with the original owner.

A portable contract retains its admitted `Capability` beside the immutable
identity document. `NewPortableContract` captures the original model;
`BindContract` and `RegisterDescriptor` admit a received Frame descriptor before
registration, including its reference and canonical identity. `Host.Capability`
and `Setup.Capability` return read handles for that model, resolving a Point to
its value contract through the same registry. Replacing a returned handle cannot
change the registered model. Foreign published descriptor formats remain opaque;
they cannot acquire Frame method semantics through this query.
Method documentation stays in the publisher's descriptor. The registry retains
the identity model, so descriptor-first and local-type-first registration expose
the same callable semantics without importing publisher-only prose.

Use `frame.NewTypedMethod[I, O]` to derive a unary method from its codec models.
Bind a handler with `ImplementQuery` or `ImplementCommand`, then assemble the
complete capability with `frame.Implement`. Typed callers use `TypedMethod.Query`
or `TypedMethod.Command` against `frame.Service` in any execution realm. The
business caller supplies the original command ID; relays preserve it.

Typed callers check cancellation before encoding and dispatch. Implementations
check again after decoding, before entering the handler. Failures at these
boundaries preserve their original cause without implying an accepted effect.
After dispatch, handler or result-projection failures retain
`frame.ErrOutcomeUnknown`; cancellation cannot discard a validated success or
authorize replay. Commands and queries share this result-certainty rule.

Schemas and decoding consume the same `value.ValidateType` model. Integer values
retain exact decimal and exponent spellings within the Go destination range.
Machine floats, byte sequences and `time.Time` cannot represent the complete
portable vocabulary and are refused as typed wire models. Use `value.Number` for
exact numbers, constrained strings for textual encodings, and `value.JSON` for
opaque JSON including null. A custom codec must provide both directions and a
value-receiver `JSONSchemaModel` describing the values it can actually consume.
Descriptor-only methods use `frame.AdmitMethod` with explicit schemas.

## Installation and offline tools

Build each artifact first, list it in the manifest, then package its immutable bytes. All three commands run without executing plugin code.

```sh
go run ./cmd/frame package --manifest plugin.json --out plugin.zip
go run ./cmd/frame inspect plugin.zip --format json
go run ./cmd/frame verify plugin.zip --policy publisher-policy.json
```

The bundled CLI trust policy pins SHA-256 archive digests in a file controlled by the host administrator: `{"allowedArchiveDigests":["<64 lowercase hexadecimal characters>"]}`. Inspect verifies byte integrity and reports publisher trust as unassessed. A matched pin assesses that specific archive. Neither operation grants execution. The Go distribution Store accepts an explicit Trust function for custom signature, publisher and capability policies.

CLI failures emit one versioned JSON diagnostic on stderr and exit unsuccessfully.
`invalid_request` identifies rejected arguments or input; `operation_failed`
identifies an unclassified execution failure, including permission or output I/O
errors. Specific integrity, trust, capacity, existence and contract categories
remain available. Classification preserves the original cause and message;
consumers branch on the code instead of parsing diagnostic text.

Use Store.ImportArchiveHeld to obtain a content lease. Lease.Install validates and projects the verified manifest into the Host. For compiled native units supply native setup functions in distribution.Installation. For an external execution profile register its driver with the Host and implement distribution.ArchiveDriver on that driver. The driver checks the already integrity-validated unit's execution shape. Unknown profiles remain inspectable. Store retention must query both installed release facts and any retained business/execution dependencies. Close a lease to release content; Lease.Snapshot returns a detached projection.

`distribution.Limits.Entries` counts files, explicit directories and implicit
ancestors. Import and cached-release verification use the same path admission.
Every artifact read checks its bytes, size and executable status against the
manifest using one physical file snapshot. A previously acquired lease does
not turn altered cached content into trusted bytes.

A Host.Install error is not sufficient to tell whether a persistence operation committed. InstallationConclusion(err) reads the owning transition's conclusion, including committed with diagnostics, rejected and indeterminate. Inspect the original installation after recovery; do not replay an unknown mutation blindly.

The Host revalidates its original admission immediately before beginning an
installation commit. Cancellation or recovery raised during a retention read
cannot authorize a later in-memory change or storage call. Content collection
applies the same boundary before withdrawing a release from its public directory.

An installation commit or confirmation callback that panics or exits its
goroutine establishes the Admission recovery barrier before releasing admission.
The original interruption still propagates; missing acknowledgement cannot prove
that a durable change did not occur.

An interrupted business retention read or restoration callback also establishes
that barrier before admission is released. Panic and Goexit propagate to their
original caller; only a completed restoration can reopen admission.

The Host retains that original transition's request and admitted scope revision
until restoration projects its durable conclusion. Recovering a committed enable
advances that exact revision once; a later local fence remains effective. A record
whose `Enabled` field is true cannot reopen an older fenced grant by itself.
Clearing the global recovery barrier before this restoration does not authorize
another installation mutation to replace the unresolved transition.

## Execution boundaries

A durable Host has one `Authority`: the SQLite Store. It owns the admission gate, the creation epoch, installation persistence and execution protection, so the Host derives all four from that value instead of receiving copies that could disagree. An `Admission` alone is a volatile authority. The process driver and browser bridge write to their configured Store and refuse a Setup or Host whose `Durable()` is another one. An ordinary scope named like an installation cannot fence the Host's internal installation scope. Validate an authorized grant before starting any unit and revalidate after preparation.

Browser Realm affinity hashes one component-encoded identity tuple. The same
tuple encoding is authenticated by its opaque credential, so punctuation cannot
merge Realms or let a proof settle another Realm's execution. Each component's
name limit remains valid independently of the bounded Frame affinity key.

Accepted-binding reads, provider preparation, export validation and Use approval
may outlast their original grant. Every following execution or publication
boundary checks that same grant directly; asynchronous cancellation notifications
do not authorize a later callback or Use handoff.

Contract descriptors own semantic identity across execution realms. `Host.Register`
also binds that identity to the Host's local Go value and metadata types;
`RegisterDescriptor` admits only the descriptor. Either registration order preserves
the same identity and Go binding. A driver whose realm uses another host-side
representation implements `ExportValidator` and validates the original instance
and export identity for setup values and dynamic publications. Drivers without
that capability require the registered Go value check for service and Point
values; a descriptor alone cannot authorize an arbitrary Go implementation.
Native event endpoints keep their nil value representation, with typed payload
admission at delivery.

`Host.PrepareMetadata` captures and validates incoming wire metadata against its
contract and current local Go projection. Its immutable `PreparedMetadata`
retains the original wire value; `JSON()` exposes that read projection. Pass the
prepared value as `ExportValue.Metadata`. Publication consumes that same value
without encoding a decoded Go view or invoking its decoder again. A different
contract or a newly registered local projection requires fresh preparation.
Portable Point constructors reject unsupported metadata projection types before
they can create an unusable typed binding.

`StartUnit` claims only an instance prepared under the same original scope grant.
An existing installation-scoped root is refused before another preparation can
begin; retire it explicitly before starting that root under a different authority.
Installation-scoped providers remain shareable by consumers with independent
grants. Copies of a `RunningUnit` share one private start claim; independent starts
have independent claims, and only the final claim requests retirement.
Start admission validates the prepared instance directly. A start claim does
not create a Use or consume its capacity, and it needs no temporary borrow
resource to be closed before the instance can drain.

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
descriptor is admitted. `value.ProfileSchema` owns the dialect, keyword value
shapes and schema-child topology, including whether an edge consumes instance
depth. Go exports that declaration, the format inventory, resource budgets,
regular-expression atom meanings and variable-width syntax into
`contract-profile.generated.json`; the
Node adapter consumes that projection while each runtime keeps its own mature
predicate compiler. In particular, whitespace uses Go's
ASCII character set, a dot excludes LF, and the end anchor requires the end of
the entire string. Fixed repetitions remain bounded by the profile. Both
runtimes execute `value/testdata/schema-profile.json`, including Unicode,
pattern properties, references, and positive and negative format cases.

Contract generation consumes one typed projection graph for both languages.
Generated Go numbers retain their JSON lexemes through `value.Number`, and
open objects retain every admitted member. Shapes that cannot be represented
faithfully as static fields retain a lossless JSON value; the original canonical
schema still validates them. Date and time strings keep their original text.
See [contract generation](../contractgen/README.md) for its public
surface and projection guarantees.

Portable Point metadata and metadata received as JSON retain one admitted
immutable JSON value. Candidate and selected-member reads decode fresh Go
projections through the requested metadata type's codec; the publication does
not retain a second decoding target. Registering a local Go binding after a
remote publication cannot change that publication or invalidate an earlier
snapshot's wire value. Listing candidates does not execute a metadata codec.
`Candidate.Metadata()` returns `(M, error)` because decoding can fail. Selection
policies read each candidate once, propagate any error, then compare the detached
metadata. Native metadata stays a detached native snapshot; encoding a candidate
uses another detached value, so a custom marshaler cannot mutate the publication.

## Restoring accepted work

The Go APIs return and consume `Snapshot` and `BindingPlan` value handles. A
Snapshot retains one private immutable catalog read; its candidates and
selections refer to that record rather than a caller's variable. A copied handle
can use the same candidates, while a fresh read cannot rewrite an existing
selection's provenance. Binding admission captures the original plan record
before waiting, so the Use's binding, selected objects and durable approval all
derive from the same graph. Zero handles are unissued and cannot admit work.

A declared export always identifies its original setup value. Dynamic Point
publications have separate operation identities and may reuse a withdrawn name
only in the live catalog. They never replace a declaration's setup slot or gain
durable reconstruction through that name. Withdrawal prevents a new setup or
declaration-root use from acquiring the original export; already accepted uses
and dependent instances retain the object they originally received. Slow setup
rechecks those original exports before activation and publication.

Reconstruction eligibility is derived from that original setup slot. An
`ExportValue` supplies the implementation and metadata, not another decision
about reconstruction. A dynamic member returned during setup remains dynamic
when its name is outside the declaration.

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
An `ImportDeclaration` names a slot, its contract and whether it is optional;
it never names providers. A unit fixes exact providers through its `Fixed`
`SlotProviders`, and a consumer chooses them through a `BindingChoice`. The
presence of either choice is the decision, so an empty provider set is an
explicit absence. Every other slot is resolved from live candidates, whose read
admission rechecks. An `ImportBinding` always holds resolved providers.

Admission captures a business owner's validated ID once at registration. Later
retention reads, ordering and diagnostics use that registered identity without
calling external identity accessors under the Admission state lock.

A Use's owner must be outside the retirement path of its protected instances.
Admission follows the original resource-owner handles and provider graph, and
rejects a direct or nested owner cycle with `ErrDependencyCycle`. An instance
cannot own a Use of itself or of a consumer that keeps it alive. Application
consumers own their Uses independently; accepted external Uses can then drain
before their providers retire.

Acquisition normalizes its realm, scope and execution identity before planning.
A child derives omitted fields from its original Parent and rejects conflicting
fields; `AcquireUnit` and plan-based acquisition use the same rule.

Setup and Use lifetimes retain their original Host's admission authority. A
different Host refuses them as a Use owner or Host lifetime before preparing
work: neither Host can atomically inspect and admit the other's dependency
graph. An application can compose several Hosts under its own `NewLifetime`,
which does not originate from a Host's provider graph.

The live execution path admits the original plan. A durable host can configure ReadAcceptedBinding once at composition. Its loader receives the record identity and the request's scope grant, authorizes that record, then reads the binding accepted by the original business transaction. Host.AcquireRecord consumes that evidence. A diagnostic BindingSnapshot on its own cannot be presented to the Host as a newly authorized operation. The document queue is a working example in examples/document/tasks.go.

## Local causes and terminal close reports

`Lifetime.BeginClose` and `Use.BeginClose` return `Close` value handles. The
Lifetime alone advances its private close operation. Copies observe current
cleanup counters and late proof; replacing a caller's handle cannot reset the
operation or its identity. An unissued Close cannot wait for or prove release.

The original Lifetime context owns cancellation and its first cause. Parent
propagation uses the standard context relationship; a delayed custom parent is
read directly before local cancellation. Closing reads that same cause for Work
and every resource notification, including resources acquired while unwinding.
A later close request cannot introduce a second cancellation reason. Cancellation
still requests shutdown; only the original close operation can prove release.

The Host reads closing state from its original Lifetime. Closing an injected
Lifetime or cancelling its parent fences the same operations as `Host.Close`;
admission observes the original parent's cancellation even before its close
notification runs. Already admitted Work still owns any resources it acquires
while unwinding. There is no independently advanced Host close flag. Each Watch is a resource
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
Public Use handles refer to one private borrow. Copies share its admission,
held instances and settlement; replacing a caller's handle cannot change the
borrow retained by the Host, an Access or an event route. Child admission captures
that original parent before preparation can invoke application code. Retirement
clears the shared borrow's Host and payload references for every retained copy.

Every Setup handle refers to the original private call. Typed slot bindings and
returned exports contribute to that same call's result; the author convenience
wrapper allocates no separate staging owner. One seal closes export staging and
activation registration together and drops their mutable references, including
error, panic and Goexit. It uses the call's lock independently of Host membership.
A retained or replaced public handle cannot redirect that conclusion. Complete
export validation rejects duplicate names across both inputs before activation
or publication.

Before durable approval, the Use reserves capacity in both its owner and the
Host's existing resource graph. Once approval starts, that Use remains there
even when an error, panic or Goexit hides the commit outcome or owner closure
prevents publishing its user handle. `ExecutionProtection.SettleUse` resolves
the original approval: it must durably release the approved Use or prove that
the original approval cannot commit. Its failure retains the same settlement,
approval identity and provider holds. The previous `ReleaseUse` method is removed;
an adapter must implement this stronger reconciliation contract.

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

Setup, activation, metadata codecs and driver export validation share the original
preparation's failure boundary, including panic and `runtime.Goexit`. Failed
preparation retires its own resources and never publishes partial exports.
Resource cleanup that exits without returning settles
as unconfirmed; only an original ResourceHandle proof can then establish release.
Cancellation callbacks are joined separately so one Goexit cannot abandon the
remaining cancellations or the original retirement.

A protected Use keeps its original settlement when `SettleUse` panics or exits
without returning. The failure fences admission and is reported to the waiting
close. Only a later explicit `Use.Close` or `Host.Close` requests another settlement
attempt; enclosing resource cleanup only joins the original attempt. A failure
that first occurs during one Close cannot make that same Close silently retry.
Each failure owns one retry signal. Overlapping closes can join that request but
cannot preauthorize a retry for a later failure that they have not observed.

`Host.Retire` and `Host.RetireInstance` use maintenance admission so recovery can
withdraw original instances and drain their resources. The recovery barrier
continues to reject new execution and binding admission.

`CloseOutcome` is bounded evidence, with at most 32 diagnostic failures and
2,048 UTF-8 bytes per resource label and detail. Resource names are diagnostic
labels; only handles identify resources. `Close.Wait` and `Close.WaitReleased`
report stable `ErrCloseFailed` and `ErrUnconfirmedRelease` categories from that
evidence. They do not reconstruct original error chains from diagnostic text.

Late release proof can unlock provider cleanup after the original Close first
reported pending resources. A later `Close.Wait` joins cleanup now ready or
running in that same graph, then reports its current outcome. A stationary
resource without release proof still returns pending; cancelling the wait does
not cancel that cleanup or create release evidence.

`Close.ReleaseProof` succeeds only after the original Lifetime has released.
Its compact `ReleaseProof` contains the original Close ID, without diagnostic
text or a competing release flag. Execution protection persists this terminal
proof. Proof fields are bounded names and bounded evidence, so a valid proof is
always storable and admission never reserves capacity for it.

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

A native host composes `host` with the root execution kernel. A host that runs local processes
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

## Migrating source imports

| Previous import | Current owner |
| --- | --- |
| Root Host, Setup, Unit, declaration, selection, binding and admission APIs | `github.com/Tangerg/frame/host`; construct with `host.New(ctx, host.Config{})` and `host.Limits` |
| Pointer Lifetime, Host and Admission values | `frame.Lifetime`, `host.Host` and `host.Admission` value handles; check `Valid()` for an issued owner |
| Host-specific stale, unavailable, in-use, ambiguity, cycle, recovery, fenced and resync errors | `host.ErrStaleBinding`, `ErrUnavailable`, `ErrInUse`, `ErrAmbiguous`, `ErrDependencyCycle`, `ErrRecoveryRequired`, `ErrFenced`, `ErrResyncRequired` |
| `frame/integrity`, `frame/jsonvalue` | `frame/value`: `Digest`, `Hash`, `ParseJSON`, `EncodeJSON` and `JSON.Bytes()` |
| `frame/control`, `frame/control/host` | `frame/browser` for both control DTOs and the host bridge |
| `frame/runner` | `frame/process/runner` |
| `frame/portable/contractgen` | `frame/contractgen` |
| `frame/portable` method, capability and invocation APIs | Root `frame` package |
| `frame/portable` schema, codec projection and value-budget APIs | `frame/value` |
| `portable.RuntimeProfile`, `portable.CurrentRuntimeProfile` | `frame.PortableProfile`, `frame.CurrentPortableProfile` |
| `portable.Limits`, `portable.ProfileLimits` | `value.PortableProfileLimits`, `value.PortableLimits` for value budgets; Frame owns `MaxCapabilityMethods`, `MaxNameBytes`, `MaxCommandIDCharacters` |
| `portable.Protocol`, `portable.ValidToken` | `frame.CapabilityProtocol`, `frame.ValidName` |
| `process.Service`, `Handler`, `StreamingService`, `Emit`, `Implement`, `Implementation`, `ErrOutcomeUnknown` | Root `frame` package |
| `process.Member`, `process.NewMember` | `frame.TypedMethod`, `frame.NewTypedMethod` |
| `frame/protocol/process` | The local wire specification lives in `process/wire.json`; Go and Node consume that file |
| `@frame/browser`, `@frame/node`, `@frame/react` | `@frame/sdk/browser`, `@frame/sdk/node`, `@frame/sdk/react` from one tarball |
| SDK `acquire(owner, label, create)` and `AcquiredResource` wrappers | `owner.acquire(label, create, release)`; creators return resources directly |
| SDK `withResource(owner, label, create, work)` | `owner.withResource(label, create, release, work)` |
| Managed invocation, refusal and command-conflict evidence under `frame/portable` | `frame/process/mcp` owns this managed wire vocabulary |
| `frame/cancellation` | Removed. `frame.ContextError` preserves the standard cancellation category and original owner cause across adapters |

Here `frame/` stands for `github.com/Tangerg/frame/`. Native and local wire
identities, browser control protocol values, and SQLite record encodings survive
these source moves. Install the revised SDK tarball in downstream workspaces.
For a managed MCP plugin, publish host bindings from its original descriptor and
select the managed profile explicitly. The plugin author's command receipts and
the host's choices of scopes, grants and business authorization stay at their
original owners.
