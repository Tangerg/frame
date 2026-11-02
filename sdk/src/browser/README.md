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
const modules = new RealmModules(openVerifiedArtifactLease);
const runtime = new Runtime({
  authority: session.authority,
  realm: session.realm,
  proofCapacity: applicationProofBudget,
  control,
  load: (unit, owner, plan) => modules.load(unit, owner, plan),
  remote: createApprovedPortableProxy,
});
const pump = new ControlPump(runtime, control, {
  intervalMilliseconds: applicationPollInterval,
  onError: reportControlFailure,
});
```

Run the pump under the application's bootstrap owner. The pending projection is
complete: unresolved plans and fences remain visible rather than depending on
an event delivery cursor. Fences are applied before new plans. An unavailable
control channel seals new framework work; reconnect requires explicit current
Host revalidation. A new authority epoch makes old executions cleanup-only.

`prepare(plan)` validates Host approval before loading code. Its exact slots may
reference existing instances in another plan of the same Realm. The consuming
Lifetime protects that original provider; the new plan never owns or resolves
it again. The module must declare the approved import and export set.

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
`select` is a pure consumer metadata policy helper; the Host still admits its
result. `Use.with` accesses approved native objects locally without per-call RPC.

Close a discarded or read-only selection with `control.closeSnapshot(snapshot.id)`.
An admitted `acquire` consumes that original lease, including a failed selection;
retry its original operation ID to recover the same grant or refusal. Closing the
snapshot afterward is idempotent and leaves accepted Uses and their release proofs
with their original owners. If selection fails before acquisition, close the
snapshot and preserve both errors when cleanup also fails.

`route` accepts an exact Host-approved native Event route. Dispatch preserves the
actual payload object in the Realm. Changing public subscriptions or acquiring
new routes requires Host admission. Private module and DOM events need no public
framework declaration.

## Publication and exit

Ready acknowledges preparation; it never becomes a local Active or Published
permission flag. A valid Host Use may access the original staged object when a
Ready or publication acknowledgement was lost. Ready retries preserve the exact
report and operation identity without running setup again.
An explicit Ready refusal, including a complete message capacity refusal, closes
the prepared execution. Only an unknown outcome retains preparation for retry.

`publish` creates a contribution under an existing declared Point contract.
Unknown acknowledgement retains its original operation and object. `observe`
queries that operation. `withdraw` requests the Host membership change; existing
Uses continue to protect the actual object until they release. A released
publication produces its own proof after that separate withdrawal is confirmed,
without releasing its parent execution. Closing the execution instead covers
all of its original objects, including accepted and uncertain dynamic
publications. It does not manufacture a separate withdrawal acknowledgement.

Only terminal release enters the bounded bootstrap proof outbox. Proof delivery
reuses the same immutable proof identity and payload after acknowledgement loss.
The current authenticated transport may carry original-epoch evidence during
cleanup-only recovery. An execution proof covers its owned objects and pending
admissions; it never releases another execution's resources.
Reconnect revalidates the bounded execution projection; the outbox then delivers
each pending proof separately. Failure diagnostics stay on the local Exit and
cannot enlarge release evidence. Use the Host's `maxMessageBytes` bootstrap
projection for the transport budget so accepted messages share one effective
wire capacity.

Close the Runtime and obtain durable proof acknowledgement before closing the
transport Lifetime. `reload(callback, signal?)` calls the callback only after all
resources have released and every proof has been durably acknowledged. Aborting
the wait leaves the original exits and proof delivery owned.

## ESM modules

`RealmModules` acquires verified artifact leases before evaluation. The consumer
owns artifact authentication and content verification. A module lease's `close`
receives a late-release confirmation handle for an unconfirmed close.

`load` expects a default Frame unit declaration. `evaluate` lets a consumer adapt
its own module authoring format without synthesizing a LocalPlan. The exact Host
plan remains the only authority for executable units and public contracts.

ESM evaluation belongs to the actual Realm and cannot be cancelled or unloaded.
A retiring execution stops observing evaluation, closes its resource lease, and
refuses late exports. The evaluation promise remains in the Realm cache. An
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
