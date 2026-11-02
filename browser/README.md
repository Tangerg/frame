# Browser Host adapter

This package owns the browser control DTOs, their TypeScript projection, the authenticated Go driver and its HTTP port.
It depends on the original Frame Host and SQLite Store, which share one Admission.
It does not select providers or publish a second catalog.

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

`Poll` returns complete pending plans and fences. `Ready` observes preparation;
only the Core Host publishes exports. Point acquisition retains the original
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

Each wire Snapshot is a Realm-owned selection lease. `Acquire` takes it exactly
once when the operation is admitted, including an attempt whose selection later
fails. Repeating that operation ID observes its original grant or refusal; a new
selection needs a new Snapshot. `CloseSnapshot` discards a pending lease and is
idempotent after close or acquisition. It does not cancel an admitted acquisition
or release a Use. Close abandoned and read-only snapshots explicitly. Realm
sealing and cleanup-only reconnect discard all of that Realm's pending leases.
Concurrent Snapshot completion cannot insert a lease after sealing.

`SealRealm` revokes new work in one Realm without fencing other windows using the
same installation. An exposed plan remains protected until its original proof
arrives. A Use proof releases the original parent-to-child delegated closure,
including an unknown child acknowledgement. An execution proof releases only
that original execution's Uses. Proof acknowledgement waits for durable release;
repeating it never starts cleanup again or substitutes a new exit identity.

A Bridge admits only Setup values from its original Host. Poll does not expose a
retired plan for the first time. The Host settles an unexposed plan with its
original non-delivery evidence and retries that evidence if persistence fails.
An already exposed plan still requires its execution owner's proof.

MaxRealms bounds resident Realms and MaxPending bounds active operations. On
seal, an empty Realm relinquishes its slot. Its credential can still submit
idempotent cleanup observations. Completed control replies live in the original
authority epoch's SQLite receipt journal and do not consume active capacity.
Rebuilding the Host rotates the epoch and its signing key; use ResumeRealm to
submit the old execution's proofs on the new cleanup-only channel.

`MaxMessageBytes` bounds complete encoded control requests and responses,
including their envelopes. Before persisting an attempt, the Bridge checks its
plan validation, declared Ready shape and original execution proof. Each Realm
also reserves encoded bytes for its complete pending and reconnect projections;
ready executions retain that reservation until their original release. An
exposure cannot turn an admitted backlog into an oversized response.

Setup can produce additional contributions and metadata that the Host cannot
predict before execution. An oversized Ready report is refused before
publication, and the Runtime closes that prepared execution with its original
proof. Exit diagnostics remain on the local Exit; release evidence contains only
the original identity, covered instances and confirmed release. Proof delivery
uses individual bounded messages, including after reconnect, rather than an
unbounded aggregate of pending proofs.
