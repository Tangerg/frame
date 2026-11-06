# Browser Host adapter

This package owns the browser control DTOs, their TypeScript projection, the authenticated Go driver and its HTTP port.
It depends on the original Frame Host and SQLite Store, which share one Admission.
It does not select providers or publish a second catalog.

`WriteTypeScriptTypes` derives fields and omission from the admitted Go wire
declaration. Custom codecs project their explicit `JSONSchemaModel`, including
`value.Number` through the SDK's exact `JSONNumber` representation. Pointer
fields do not introduce null; `value.AllowsNull` owns that distinction for
decoding and projections. String enum values require a string wire model.
Native-width and 64-bit integers use `JSONNumber`; fixed-width integers of at
most 32 bits remain JavaScript numbers. A generated DTO cannot narrow a wide
Go integer to the JavaScript safe integer range. Instantiated generic types
receive target-language names shared by declarations and references; colliding
projected names are refused before publication.
Malformed, cyclic or unsupported models fail before any output is published.

The application creates the Host and Store, constructs `NewBridge`, and registers
the Bridge as the `browser-js` driver. `ModuleSource` projects an approved unit to
one immutable artifact. Opaque installation configuration is withheld by
default; `PublicConfiguration` must explicitly project data intended for the
browser. Optional `RemoteSource` adapters bind real Portable transports to exact
approved imports and register their resources with the original Setup.

Bootstrap callers create an operation ID and a random resume token before
calling `OpenRealm`. The operation-to-Realm mapping and only a credential
verifier are durable. A lost response can recover that original Realm without
creating another one. `ResumeRealm` after Host reconstruction grants cleanup
authority only. The trusted in-process `Credentials` projection lets application
composition reuse the original session without a second Realm registry; it is
not exposed through `Handler`.
`Credentials.maxMessageBytes` projects the Bridge's `MaxMessageBytes` limit;
the application passes that value to its browser control transport. A failed
credential read preserves its storage or cancellation cause. Only a missing
credential or a mismatched verifier is an authentication refusal.

`Handler` accepts authenticated POST requests containing `{method,input}` and
returns the published control DTO. The application owns TLS, origin policy and
bootstrap authentication. The bearer credential belongs to a Realm and must
never appear in plugin configuration, artifact URLs or logs.
The same Go declarations generate the SDK's executable response checks. Control
encoding, identity comparisons, capacity reservations and persisted evidence use
one deterministic JSON policy: nil collections encode as empty collections,
with the same string escaping used when reserving maximum exit identities.
Unknown fields, incorrect nulls and invalid enum values cannot become a valid
SDK acknowledgement merely because the HTTP status was successful.

`Poll` returns complete pending plans and fences without transferring execution
responsibility. Each response reflects the current owners directly, and callers
process its complete contents. The Runtime reserves its original execution and
exit owner, then calls `AdmitPlan` before loading code. `admitted` permits preparation;
`cleanupRequired` permits only the original exit proof. `notAdmitted` and `settled`
observe the original Host's durable settlement and let an unstarted Runtime
release its temporary reservation. A transport or authentication error provides
no such observation. The same LocalPlan resolves a lost admission reply without
inventing another execution. `Ready` observes preparation; only the Core Host
publishes exports. Each prepared object identity names exactly one export;
metadata must reference that complete prepared Point identity. Duplicate object
identities, relabelled metadata owners and orphan metadata are rejected before
the batch can be published. Point acquisition retains the original
multi-Point Snapshot and candidate provenance. Trusted consumers use
`AcquirePlan` for a fresh declaration plan created in the target Realm; its
candidate and setup premises remain subject to the Core's final admission.
A child Use inherits its original accepted parent. The Bridge persists its
Use approval before handing a grant to JavaScript. Durable business records
are restored at their original owner through Host.AcquireRecord.
Each grant carries its original operation and parent Use identity. Acquisition
and direct grant acceptance materialize that same local owner. Replaying an
accepted operation observes its historical receipt; it does not revive a
released Use. A new materialization must pass current Host validation.

Each wire Snapshot is a Realm-owned selection lease. Callers create
`SnapshotRequest{OperationID, Points}` before dispatch; the returned Snapshot ID
is that original operation ID. The Bridge reserves capacity before capturing
the Host's selection. Concurrent requests and retries after a lost response
observe the same capture and candidate tokens. Changing the requested Points
under that ID is a conflict; a different Realm cannot observe or close it.
Snapshot is a mutating control operation, so an incomplete response leaves an
unknown outcome rather than permission to capture again.
If metadata encoding panics or exits its goroutine, the original operation
retains a failure receipt and releases its unfinished lease reservation. A retry
observes that failure; it cannot recapture or report an uncaptured lease as success.

`Acquire` takes the original lease exactly once when its operation is admitted,
including an attempt whose selection later fails. Repeating that acquisition ID
observes its original grant or refusal. Retrying a Snapshot after consumption or
close returns `ErrClosed`; its receipt can never reconstruct candidate provenance.
A fresh selection requires a fresh Snapshot operation ID.

`CloseSnapshot` discards a pending lease and is idempotent after close or
acquisition. It accepts the caller-known ID even when the capture reply was lost.
A close that arrives before capture records a terminal receipt, so a delayed
request cannot recreate the lease after cleanup was acknowledged. Closing does
not cancel an admitted acquisition or release a Use. Close abandoned and read-only
snapshots explicitly. Realm sealing and cleanup-only reconnect discard all of
that Realm's pending leases. Concurrent completion cannot insert a lease after
sealing. Completed receipts occupy the existing authority-epoch journal rather
than unbounded in-memory tombstones.

`SealRealm` revokes new work in one Realm without fencing other windows using the
same installation. An admitted plan remains protected until its original proof
arrives. A Use proof releases the original parent-to-child delegated closure,
including an unknown child acknowledgement. An execution proof releases only
that original execution's Uses. Proof acknowledgement waits for durable release;
repeating it never starts cleanup again or substitutes a new exit identity.
The parent proof fences the complete live descendant closure before joining
admissions and reading durable protection, so a completed child admission cannot
disappear between those observations or admit another descendant during release.

The Realm application's Lifetime owns root Uses. A provider execution cannot own
an application borrow of its own exports: that would create a release cycle.
Delegated Uses remain children of their original parent Use. Each Use retains its
exact execution and scope identity for proof validation; changing its application
owner does not widen the resources an execution proof can release. The Realm
Lifetime survives the request that opened it. Sealing begins its close and retains
its actual Uses until their original proofs arrive. A cleanup-only reconstructed
session creates no new application owner.

A Bridge admits only Setup values from its original Host. A plan returned by Poll
remains unstarted until `AdmitPlan` transfers responsibility to its reserved
Runtime owner. The Host settles an unadmitted plan with its original
`not-admitted` proof and retries it if persistence fails. An admitted execution
still requires its execution owner's proof. A bounded browser scheduler can
defer plans without acquiring cleanup duties for work it has not admitted.

MaxRealms bounds resident Realms and MaxPending bounds active operations. A single
Snapshot occupies one lease slot regardless of its Point count; complete request
and response byte limits bound that capture's payload. On
seal, an empty Realm relinquishes its slot. Its credential can still submit
idempotent cleanup observations. Completed control replies live in the original
authority epoch's SQLite receipt journal and do not consume active capacity.
`sqlite.Config.MaxRealmHistoryBytes` separately bounds retained Realm credentials
and control receipts, including unfinished receipt reservations. Each new control
operation reserves its maximum receipt before capture, selection consumption,
activation or publication. Completion replaces the reservation with the actual
encoded reply. Exhaustion returns `ErrCapacity` before responsibility is accepted;
original observations, accepted snapshot cleanup, withdrawal and exit proofs still progress.
Rebuilding the Host rotates the epoch and its signing key; use ResumeRealm to
submit the old execution's proofs on the new cleanup-only channel.

`MaxMessageBytes` bounds complete encoded control requests and responses,
including their envelopes. Before persisting an attempt, the Bridge checks its
plan admission, declared Ready shape and original execution proof. The Bridge
also composes proof admission with the Store's actual `MaxRequestBytes` capacity,
including the complete stored envelope needed to release Uses after reconstruction.
Use and publication grants apply the same transport and durable-capacity checks
before handing responsibility to the Runtime. Each Realm
also reserves encoded bytes for its complete pending and reconnect projections;
ready executions retain that reservation until their original release. An
admission cannot turn a pending backlog into an oversized response.

Setup can produce additional contributions and metadata that the Host cannot
predict before execution. An oversized Ready report is refused before
publication, and the Runtime closes that prepared execution with its original
proof. Exit diagnostics remain on the local Exit; release evidence contains only
the original identity, covered instances and confirmed release. Proof delivery
uses individual bounded messages, including after reconnect, rather than an
unbounded aggregate of pending proofs.
