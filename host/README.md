# Plugin hosts

`host.New` creates the owner of plugin installations, contract registration,
provider selection, preparation and accepted Uses. Unit declarations, typed
slots, Point collections and Event routes belong to this same binding context.
The package never selects alternatives after a binding has been admitted.

`Host` and `Admission` are value handles to private owners. Copies observe the
same catalog, generation sequence, recovery barrier and scope authority.
Replacing a caller's handle cannot redirect an in-flight operation. `Config`
captures the issued Admission and application Lifetime handles.

Frame's root package owns callable contracts and structured resource lifetimes.
Each Host has one lifecycle domain for its internal lifetimes. The domain can
check affiliation without exposing an issuing capability through the Lifetime.
Resource dependency inspection uses owner identities, which grant no control of
those lifetimes. No second ownership ledger is maintained.

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
the hold remains until actual release and durable settlement.

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

Each instance has one retirement worker. It drains preparation and consumers,
then publishes the original Lifetime exit to resource callbacks. Those callbacks
observe the exit instead of independently repeating the drain. Successor admission
reads actual release proof, so diagnostic forwarding cannot extend an execution's
physical overlap. Host retirement bookkeeping still completes on its original
record and cannot withdraw a newer generation.

Withdrawal is represented only by current membership. `instance.go` withdraws
that membership and its exports together, without a parallel retiring flag or
another index deletion during final cleanup. Preparation, start claims and
observations read the same membership; accepted Uses continue draining through
their original held Lifetimes.

A Use has two distinct release facts: its Lifetime proves resource release;
execution protection durably settles the original approval. Settlement owns an
explicit pending/failed/retrying/complete state. A close may request a retry only
for the failure it observed. Failure identities and state transitions determine
that policy; notification channels cannot authorize another attempt.

`use_retirement.go` owns the complete Use close continuation, settlement retry
coordination and retirement observation. A failed attempt returns its own retry
notification; the worker never looks up a different failure after publishing it.
Waiters recheck the original conclusion before reporting observer cancellation,
including a newly recorded retry failure.

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
Only Catalog membership decides visibility. Reconstructibility follows the
original declared setup slot, so a dynamic replacement cannot acquire its
durable binding identity. A publication request retains one prepared native
value; request copies and memberships share that value without another nonce
or operation wrapper. A new operation can create new membership from the same
request, while replay cannot resurrect withdrawn membership.

Preparation has explicit pending, ready and failed states. Installing the setup
exports and marking readiness commit under the same Host lock. Completion
channels only notify waiters. The preparation owns its pending candidate reads
and drops them on completion, including failure and abandoned graph planning;
callers never clear that evidence separately. Declaration plans retain binding
candidate reads; instance plans retain exact members and Catalog revisions.
Their explicit kind selects admission, independently of whether either read set
is empty.

Setup copies share one private call. Typed slot bindings and returned exports
feed its complete result; the author convenience wrapper owns no separate
staging lifetime. One seal ends both export staging and activation registration,
including error, panic and Goexit. Whole-result validation rejects duplicate
exports before activation or publication. Host membership remains a separate
admission check; sealing itself needs only the call's lock.

`activation.go` hands the original prepared instances to binding admission.
Use acquisition retains that exact plan; it cannot look up a successor by unit,
even when the successor has the same immutable setup graph. Durable binding
restoration starts a new preparation and obtains a new instance plan.

`running.go` owns start claims directly. A claim controls when the original
instance retires; it does not borrow values or allocate a Use. Start admission
rechecks the original grant and prepared plan without consuming Use capacity or
reserving a temporary borrow resource. Only the final independent claim requests
retirement, while Host retirement remains authoritative.

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
