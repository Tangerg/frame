# Plugin hosts

`host.New` creates the owner of plugin installations, contract registration,
provider selection, preparation and accepted Uses. Unit declarations, typed
slots, Point collections and Event routes belong to this same binding context.
The package never selects alternatives after a binding has been admitted.

`Host` and `Admission` are value handles to private owners. Copies observe the
same catalog, generation sequence, recovery barrier and scope authority.
Replacing a caller's handle cannot redirect an in-flight operation. `Config`
captures one `Authority` and the application Lifetime handle. An `Admission` is
a volatile authority; a `Durable` authority such as the SQLite Store also owns
the creation epoch, installation persistence and execution protection, so the
Host derives the gate and epoch from it.

Frame's root package owns callable contracts and structured resource lifetimes.
Each Host has one lifecycle domain for its internal lifetimes. The domain can
check affiliation without exposing an issuing capability through the Lifetime.
Resource dependency inspection uses owner identities, which grant no control of
those lifetimes. No second ownership ledger is maintained.

Work admission and completion read the original Lifetime's registered intervals.
An interval has no independent finished flag; once removed it cannot accept a
resource or release the capacity of a later interval.

Each held dependency pairs its original instance with the Work that retains it.
Instances and Uses derive dependency inspection and retirement from those same
holds, without a parallel instance list. Use admission reads the original Host,
business owner and complete held provider closure directly; queued cancellation
notifications cannot authorize new work. Retirement alone still permits an
accepted Use to drain while its execution remains open.

New admission requires current membership throughout that original held closure.
A captured consumer cannot extend a provider already withdrawn for draining,
including an indirect provider. Existing Uses retain their accepted permission;
they read execution cancellation independently of current membership.

Lifetime cancellation has one cause owner: its original context. Closing a Use
or retiring an instance cannot replace that reason with a later default close
request. Work and resource notifications read the same cause, including late
acquisitions and delayed parent notifications; release still requires real proof.

Instance and provider cancellation follows the original instance context. A
retention Work may stay open until its consumer releases it, so it cannot stand
in for execution permission. Revocation starts the original retirement worker;
the hold remains until actual release and durable settlement. Dependency
cancellation carries the binding-loss category at the consumer boundary, so
notification timing cannot change activation's failure category.

Each parent reservation also observes its original Lifetime context. Host or
business-owner cancellation starts Use close without waiting for that parent's
resource callbacks. When the Host is also the business owner, the Use reserves
and observes that one owner once.

Each Use observes its original ScopeGrant directly in both application and
installation namespaces. A previously activated or draining instance cannot
stand in for that permission's cancellation. Fencing begins the original Use
close; durable settlement must finish before its provider holds release.

Use teardown releases its actual holds and removes the Host index in one Host
commit. Diagnostic forwarding happens afterward because error formatting can
reenter admission and must not observe an already released dependency edge.
`instance.go` owns instance registration, retirement and retirement observation.
Uses, instances and start claims share the rule that confirmed completion wins
over cancellation of an observer's wait.

The instance directly owns retirement. One worker drains preparation and
consumers, then starts its original Lifetime close. Parent resource callbacks
join that close through a notification barrier. The Lifetime owns the operation;
the instance keeps no separate close controller. Successor admission reads actual
release proof, so diagnostic forwarding cannot extend physical overlap. Host
bookkeeping still completes on the original record and cannot withdraw a successor.

Host close traverses that same resource graph. Instance retirement and Watch
termination stay with their original owners, including their cancellation cause.

Withdrawal is represented only by current membership. `instance.go` withdraws
that membership and its exports and dispatches its one retirement worker in the
same Host commit. Membership itself authorizes that worker once; there is no
separate start flag or caller-owned retirement list. Binding refusal, preparation
failure, final start relinquishment, explicit retirement and committed installation
changes all use this transition. Slow, failed or interrupted storage confirmation
cannot strand an already withdrawn instance. Cleanup still runs outside the Host
lock, drains accepted Uses and waits for the original release proof. Preparation,
start claims and observations read the same membership, and final cleanup never
deletes a successor's membership.

A Use has two distinct release facts: its Lifetime proves resource release;
execution protection durably settles the original approval. One Use retirement
owns the complete continuation and its pending/failed/retrying/complete state.
It retains the original close-cause observation until settlement and reference
release complete, even after the Lifetime has physically released. A close may
request a retry only for the failure it observed. Failure identities and state transitions determine
that policy; notification channels cannot authorize another attempt.

`ExecutionProtection.ApproveUse` returns the original `CommitState` and an error
separately. `NotCommitted` creates no durable settlement obligation; an ordinary
refusal does not fence admission. `Committed` still requires settlement when an
error prevents publication. An indeterminate or invalid conclusion, panic or
Goexit requires recovery and settlement of that same original approval. The Host
never retries approval or invents its outcome from an error category.

`Use.Close` joins both facts. Callers that need physical exit evidence use
`Use.Lifetime().BeginClose` explicitly; `Use.WaitRetired` observes full retirement
without authorizing a settlement retry.

`use_retirement.go` owns the complete Use close continuation, settlement retry
coordination and retirement observation. A failed attempt returns its own retry
notification; the worker never looks up a different failure after publishing it.
Waiters recheck the original conclusion before reporting observer cancellation,
including a newly recorded retry failure.
Retirement completion drops its raw-error observation; terminal Use handles keep
bounded diagnostics. `WithPlan` observes its private owner's causes before the
callback can retire a child, so early release cannot hide a cleanup error.
Close errors compose through `CloseCauses.Join`, preserving shared original
failure identities across callback errors, parent closes and settlement retries.

`event.go` derives route participants from the original Uses. Sender and receiver
may share one borrow; that Use owns one route resource and one delivery interval.
Each delivery retains its original Use and Work together and checks both before
and after application code. The composed context only notifies the callback;
queued cancellation cannot become successful completion. Refusal or callback
interruption releases only that delivery's intervals.

Process, browser and persistence adapters depend on these public contracts.
This package depends on the Frame execution kernel and pure values, never on an
adapter, publisher tool, host application or plugin implementation.

`catalog.go` owns export membership and publication receipts together. Each
instance has one export set for its original setup slots and dynamic operations.
Only Catalog membership decides visibility. Catalog watches retain the original
owner's cancellation cause after their stream closes. Reconstructibility follows the
original declared setup slot, so a dynamic replacement cannot acquire its
durable binding identity. It also follows the original instance's complete held
provider graph: portable cross-Realm calls are valid, but a capture cannot become
recovery evidence for another Realm's execution. A shared instance can retain a
Realm-bound provider even when its own execution is Realm-independent.

A publication request retains one prepared native
value; request copies and memberships share that value without another nonce
or operation wrapper. A new operation can create new membership from the same
request, while replay cannot resurrect withdrawn membership.

`preparation.go` owns explicit pending, ready and failed states and the original
execution interval. Graph admission reserves that Work before start claims can
consume its capacity. One conclusion releases it on publication, failure or
abandoned planning; dispatch cannot need a second interval after admission.
Installing the setup
exports and marking readiness commit under the same Host lock. Completion
channels only notify waiters. The preparation owns its pending candidate reads
and drops them on completion, including failure and abandoned graph planning;
callers never clear that evidence separately. Declaration plans retain binding
candidate reads; instance plans retain exact members and Catalog revisions.
Their explicit kind selects admission, independently of whether either read set
is empty.

`plan.go` owns both admission forms and their construction. A captured plan has
one immutable set of original instance roots, including exportless units and
multiple publications of the same instance. Validation and Use holds consume
that set directly. Catalog membership checks visibility; the original instance
and its holds decide execution identity. Acquisition does not reconstruct the
root set from catalog projections or compare declaration hashes as a second
instance selector. `selection.go` owns catalog reads and consumer selection.

Setup copies share one private call. Typed slot bindings and returned exports
feed its complete result; the author convenience wrapper owns no separate
staging lifetime. One seal ends both export staging and activation registration,
including error, panic and Goexit. Whole-result validation rejects duplicate
exports before activation or publication. Host membership remains a separate
admission check; sealing itself needs only the call's lock.

`activation.go` owns one binding preparation from graph admission through
worker dispatch and exact-instance handoff. It retains newly created instances
and reused originals together; failed planning concludes only its own unstarted
instances before admission reopens. It captures its original prepared instances
directly for binding admission; declaration plans own their own validation.
Use acquisition retains that exact plan; it cannot look up a successor by unit,
even when the successor has the same immutable setup graph. Durable binding
restoration starts a new preparation and obtains a new instance plan.
Each reused instance checks its authority, setup identity and original provider
holds against the graph being prepared. A binding hash names declarations, not
physical executions. A shared consumer retaining one Realm's provider cannot
silently realize another Realm's graph, including while its Setup is pending.
Retire the original shared consumers before preparing that graph in another
Realm. This refusal runs no new Setup, leaves accepted borrows intact and does
not require global recovery.
The preparation checks a prospective Use owner's complete dependency closure
before dispatching new workers. A known ownership cycle cannot run Setup first.
Handoff checks the same ownership rule again because preparation may admit new
borrows; an earlier check cannot authorize a cycle introduced by those callbacks.

`running.go` owns start claims directly. The original binding admission reserves
the claim before preparation can execute plugin code. Pending and returned claims
retain the same instance, so one canceled waiter cannot retire another's startup.
The instance indexes actual claims instead of maintaining a separate count.
Each claim composes the original instance hold, sharing the Lifetime's Work
capacity without creating a Use or borrowing another resource slot.
Failed handoff relinquishes only that original claim; its final relinquishment
withdraws the instance and starts its existing retirement. A canceled startup
cannot leave an unclaimed Ready instance for a later caller to adopt. Actual exit
still requires the original Lifetime's proof before a successor can prepare.
Start handoff rechecks the grant and exact prepared plan without consuming Use
capacity or reserving a temporary borrow resource. Host retirement remains
authoritative, and ordinary activation retains its Host-owned preparation policy.
Authoritative withdrawal ends every outstanding start claim outside the Host lock
before draining consumers. Those claims all join the original retirement; a
non-final claim cannot report success while that exit still lacks proof. Terminal
claims retain only identity and completion, including when Host close or a scope
fence retired the instance before the caller stopped its handle.

`admission.go` owns scope generations, fences and recovery together. A ScopeGrant
is a read handle to its original generation, including the same owner, key and
cancellation context. Installation transitions capture the original scope state;
even a repeated fence replaces that state, so delayed restoration cannot erase
new authority changes. Recovery reports are immutable records: their identity
controls recovery, while their count serves diagnostics only.

Every installation mutation uses one commit lifecycle. Create requires an
absent installation; update, enable, replace and remove require an existing one.
Preflight, durable commit, scope projection, retirement and confirmation follow
the same path, preserving the committed outcome if local reconciliation fails.

`Restore(ctx, plugins)` reads the Host's original durable authority and projects
its committed records in one maintenance admission. An application may read
records to load release definitions, but those copies cannot authorize Host
state. A volatile Host has no installation restoration authority. Restoration
requires all original instances and Uses to have retired, including actual exit
proof. Its storage read runs outside the Host mutex and retains original Host
Work until it finishes; cancellation cannot substitute for completion or permit
late projection after close. Restoration does not clear the recovery barrier.
