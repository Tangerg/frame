# Frame Browser Runtime

`@frame/sdk/browser` executes immutable Host-approved LocalPlans. The Go Host owns
installation state, public contribution membership, exact binding selection,
public event routing, and admission. The Browser Runtime owns the resulting
JavaScript objects, admitted work, resource dependencies, and release evidence.
It does not contain another catalog, resolver, package manager.

## Control and execution

A trusted application creates a Realm session through its Host integration. The
session supplies an authority identity, Realm identity, and credential. Keep that
credential in the bootstrap control adapter rather than exposing it to units.
`HTTPControl` validates successful replies and problem bodies with Go-generated
decoders before accepting them. A malformed mutation acknowledgement remains
`outcome_unknown`; a malformed observation reports `unavailable`. A void
acknowledgement must carry JSON null on the wire.

The control endpoint must use HTTPS or HTTP to a loopback host, following W3C
Secure Contexts. The artifact adapter supplies bounded bytes; `RealmModules`
hashes their captured content before importing those exact bytes, using the
digest in the Host-approved plan. See the ESM module contract below.

```ts
import { Lifetime } from "@frame/sdk";
import { ControlPump, HTTPControl, RealmModules, Runtime } from "@frame/sdk/browser";

const transport = new Lifetime("realm-control");
const control = new HTTPControl({
  endpoint: session.endpoint,
  credential: session.credential,
  maximumMessageBytes: session.maxMessageBytes,
  lifetime: transport,
});
const modules = new RealmModules(openArtifactBytes);
const runtime = new Runtime({
  authority: session.authority,
  realm: session.realm,
  proofCapacity: applicationProofBudget,
  control,
  load: (unit, owner, plan) => modules.load(unit, owner, plan),
  remote: createApprovedPortableProxy,
});
const pump = new ControlPump(runtime, {
  intervalMilliseconds: applicationPollInterval,
  preparationCapacity: applicationPreparationBudget,
  onError: reportControlFailure,
});
```

Run the pump under the application's bootstrap owner. `synchronize()` completes
one control poll without waiting for module loading, setup or Ready delivery.
The pump retains at most `preparationCapacity` preparation tasks, with one task
per original execution; deferred plans remain in the Host's complete pending
projection. Fences and proof delivery continue while setup is pending. Proof
acknowledgements also run independently of polling. No timeout substitutes for
resource release.

`onError` may return a promise. A synchronous exception or asynchronous rejection
stops the pump through its original failure policy. Diagnostic observers do not
retain runtime resources or delay shutdown; a failure arriving after shutdown
cannot change that completed resource-release observation.

The original execution remains locally owned after resource release until its
proof is acknowledged. Polling cannot schedule that terminal execution again.
Acknowledgement invalidates older plan observations without retaining execution
tombstones or holding proof capacity for a stalled poll. Such a response still
delivers its fences; the next poll supplies the Host's current pending plans.
`runtime.poll(schedule)` delivers plans to a synchronous callback so scheduling
reserves their original owners before another acknowledgement can invalidate the
observation. A custom scheduler must start its bounded preparations in that
callback, without deferring the received plans.

The Runtime owns the current control channel and consumes its authority, Realm
and fence projection. An unavailable current channel seals new framework work;
reconnect requires Host revalidation. Call `runtime.reconnect(replacement)` to
replace the authenticated channel while the same pump runs. A late poll from the
retired channel cannot fence or disconnect the replacement. A new authority
epoch makes old executions cleanup-only.
Every new `Use.with` interval checks this current admission. A paused channel
does not cancel an already admitted interval or release its Use; successful
revalidation can admit a later interval on the same original Use.
While revalidation is pending, polling joins that same observation instead of
reconnecting the retired channel. An explicit replacement may supersede the
observation; its late completion cannot replace or discard the newer channel.

`run(signal?)` owns the pump's bootstrap lifetime. Aborting its signal, or calling
`pump.close()`, stops new scheduling, begins Runtime closure and joins scheduled
polls, preparations and proof acknowledgements. Successful completion requires
resource release and durable proof acknowledgement. An unconfirmed close remains an
error with the original Runtime; aborting is never evidence that setup exited.
Close the transport Lifetime only after this join. A failed close can be retried
to observe the original resources and redeliver the same proofs; it never
restarts scheduling or setup. A pump cannot be restarted.
If recovery needs a replacement authenticated transport, a closed Runtime still
accepts `reconnect(replacement)` for cleanup. It remains closed to new work;
retry the original close after that channel is available.

`prepare(plan)` reserves one original execution and proof slot before sending
`admitPlan`. Only an `admitted` response permits loading code. An uncertain reply
retains that original admission; ordinary errors, including an authentication
failure, cannot be interpreted as proof that the Host never admitted it. A later
`cleanupRequired` response joins the original exit without running setup.
`notAdmitted` or `settled` releases the unstarted reservation using the Host's
original settlement, without manufacturing a competing browser proof. Runtime
closure joins and resolves pending admissions before reporting release.

The plan's exact slots may
reference existing instances in another plan of the same Realm. The consuming
Lifetime protects that original provider; the new plan never owns or resolves
it again. The module must declare the approved import and export set.
Module loading, remote import preparation and unit setup run under that unit's
Lifetime. Closing an execution joins those callbacks before releasing the unit's
resources, including a late result that arrives after cancellation.

`accept(grant)` validates a Use obtained from the Host. `acquire(request)` requests
one using a captured snapshot and consumer selections. Retrying an unknown
acquisition uses the original `operationId`, input, resource owner, and Host Use.
The grant carries that original operation ID and its delegated parent. Both
entrances share one admission, including concurrent calls and an unknown
acknowledgement. Closing a parent joins its actual delegated subtree before
emitting the parent proof. A terminal Use remains observable until proof
acknowledgement; afterward, a historical receipt must pass current Host
validation before it can materialize any local object. Released Uses cannot be
restarted by replaying a receipt or switching entrances.
Event routing captures the complete grant before Host validation. Changes to the
caller's object while validation is pending cannot replace the Use that the
approved route will materialize.
`select` is a pure consumer metadata policy helper; the Host still admits its
result. `Use.with` accesses approved native objects locally without per-call RPC.

Create the Snapshot operation identity before dispatch and retain the request
while its outcome is unknown:

```ts
const capture = {
  operationId: crypto.randomUUID(),
  points: [previews.ref.name],
};
const snapshot = await control.snapshot(capture);
```

Retry `control.snapshot(capture)` with that same request after a lost response;
the Host returns the original capture rather than allocating another lease.
Close a discarded or read-only selection with
`control.closeSnapshot(capture.operationId)`, including when the reply containing
`snapshot.id` was lost. A close acknowledged before a delayed capture prevents
that capture from starting. An admitted `acquire` consumes the original lease,
including a failed selection; retry the acquisition's operation ID to recover
the same grant or refusal. Replaying a consumed or closed Snapshot returns
`closed`; only an explicitly new capture may select again. Closing the Snapshot
afterward is idempotent and leaves accepted Uses and release proofs with their
original owners. Preserve both errors when selection and cleanup fail.
`isControlRefusal(error)` identifies a call's definitive refusal categories.
A first-dispatch Snapshot refusal creates no lease to close. After an unavailable
or unknown response, retain the original identity for retry or explicit close:
a later refused call cannot rule out acceptance of an earlier dispatch. Runtime
applies the same rule to each original pending acquisition.
Both `acquire` and `accept` retain their original admission when validation of an
acknowledged grant fails. Retry that original request or grant; its parent or
execution still owns cleanup if validation cannot recover.

`route` accepts an exact Host-approved native Event route. Dispatch preserves the
actual payload object in the Realm. Changing public subscriptions or acquiring
new routes requires Host admission. Private module and DOM events need no public
framework declaration.

## Publication and exit

Ready acknowledges preparation; it never becomes a local Active or Published
permission flag. A valid Host Use may access the original staged object when a
Ready or publication acknowledgement was lost. Ready retries preserve the exact
report and operation identity without running setup again.
A first-dispatch Ready refusal, including a complete message capacity refusal,
closes the prepared execution. After an unknown acknowledgement, later transport
refusals retain the original report and objects for retry. Only a terminal Ready
decision or the original execution's close can settle that earlier uncertainty.

`publish` creates a contribution under an existing declared Point contract.
Unknown acknowledgement retains its original operation and object. `observe`
queries that operation. `withdraw` requests the Host membership change; existing
Uses continue to protect the actual object until they release. A released
publication produces its own proof after that separate withdrawal is confirmed,
without releasing its parent execution. Closing the execution instead covers
all of its original objects, including accepted and uncertain dynamic
publications. It does not manufacture a separate withdrawal acknowledgement.

A complete `rejected` decision proves that its publication or withdrawal call
made no membership change. Publication refusal releases the staged object.
Withdrawal addresses the original publication directly; it has no separate
operation identity or receipt journal. Retrying `withdraw()` observes or performs
the same idempotent removal at the Host, including after a cancellation or lost
response. Publication observations cannot reverse a confirmed decision. Request
cancellation before the Host's synchronous mutation is a known refusal, not a
permanently unknown command.

Only terminal release enters the bounded bootstrap proof outbox. Proof delivery
reuses the same immutable proof identity and payload after acknowledgement loss.
The current authenticated transport may carry original-epoch evidence during
cleanup-only recovery. An execution proof covers its owned objects and pending
admissions; it never releases another execution's resources.
Reconnect revalidates the bounded execution projection; the outbox then delivers
each pending proof separately. A replacement channel starts its own delivery
attempt immediately, even when the retired transport has not answered. Both
attempts carry the same immutable proof; the first valid acknowledgement advances
that proof once. A late result cannot reverse its acknowledgement or affect a
different proof. Cancellation of a `close(signal)` observer ends that wait only;
resource closure and proof delivery continue independently.
The transport Lifetime still owns every dispatched request, including obsolete
proof sends. An acknowledgement completes the proof obligation without claiming
that those requests exited; closing the transport cancels and joins them.
Failure diagnostics stay on the local Exit and
cannot enlarge release evidence. Use the Host's `maxMessageBytes` bootstrap
projection for the transport budget so accepted messages share one effective
wire capacity.

Close the pump and obtain durable proof acknowledgement before closing the
transport Lifetime. A Runtime used without a pump must be closed directly.
`reload(callback, signal?)` calls the callback only after all
resources have released and every proof has been durably acknowledged. Aborting
the wait leaves the original exits and proof delivery owned.

## ESM modules

Create one `RealmModules` owner for the actual JavaScript Realm and reuse it
across plans. Constructing a new helper is not a Realm replacement.

`RealmModules` acquires artifact bytes before evaluation. The consumer owns the
authenticated, bounded fetch and returns a `ModuleLease` containing `bytes` plus
its resource closure capability. `RealmModules` captures those bytes once, checks
their SHA-256 digest against the approved plan, and constructs the evaluation
data URL from that exact capture. A lease supplies neither a trusted digest nor
a separately fetched import URL. Serve standalone or bundled ESM: relative
module imports cannot resolve against a data URL. Browser CSP must permit these
approved data-module imports.
A module lease's `close` receives a late-release confirmation handle for an
unconfirmed close.

`load` expects a default Frame unit declaration. `evaluate` lets a consumer adapt
its own module authoring format without synthesizing a LocalPlan. The exact Host
plan remains the only authority for executable units and public contracts.

ESM evaluation belongs to the actual Realm and cannot be cancelled or unloaded.
A retiring execution stops observing evaluation, closes its resource lease, and
refuses late exports. The evaluation promise remains in the Realm cache. An
identical byte sequence has one native evaluation identity across artifact
aliases; its leases share that identity and a retired alias cannot revive it. An
evaluated installation cannot change release or authority in place; a retired or
failed module cannot be restarted by deleting a promise or appending a URL query.
A controlled Realm replacement is required.

Native JavaScript is trusted application code, not an isolation boundary. An
iframe, worker, or other isolated runtime must supply its own execution driver
and explicit portable contracts; changing the renderer does not create a sandbox.

Run `npm run check` from the monorepo root. Its required
test command launches Node with `--expose-gc`: the retention regression keeps
terminal execution, Use, Exit, publication, and event-route handles alive while
verifying that their Runtime and native payloads can be collected. The test
fails if explicit garbage collection is unavailable.
