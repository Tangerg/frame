# SQLite settlement and execution protection

This optional adapter uses one local SQLite file and the Host's existing
`frame.Admission`. A held operating-system lock on a persistent `.frame-lock`
sidecar grants one Store authority; opening another Host on the same database
fails. The sidecar stays in place after release and must not be replaced while a
Store is alive. Database and lock paths must be regular files without hard links.
The sidecar keeps authority separate from SQLite's own byte-range locks on macOS.
The database is configured and
checked for WAL, synchronous FULL, foreign keys and disabled dirty reads. The
profile requires a reliable local filesystem. It does not support network filesystems,
independent writers or atomic transactions across databases.

`Open` accepts the complete current set of `OwnerSchema` declarations. A different
schema requires a fresh data directory. Each owner supplies its original business
tables and a trusted reference query. Register the resulting `Owner` with the same
Admission through `RegisterUseOwner` before its first destructive dependency check.
That check seals the owner set for the life of this Admission. Frame does not create a competing business
reference-count ledger. Every admission read uses a new current transaction.

## Business submission

`Owner.Prepare` allocates a monotonic `SubmissionID` and writes its original request
in one transaction. The identity becomes usable only after that transaction has
settled. Preparation does not authorize plugin execution.

`Owner.Commit` enters `Admission.Admit`, starts `BEGIN IMMEDIATE`, checks the original
request and terminal state, and invokes a trusted application mutation with that
admitted context and transaction. Validate the prepared binding through
`Host.ValidateBindingUnder` or `Host.ValidatePlanUnder` inside this callback, and
write the original business record in this same transaction. Use the supplied
context and Executor for all SQL. Do not call another admission or transaction
entry from the callback. Plugin effects and network calls run after Commit returns.

For a later transition identified by the original business row, `Owner.Update`
admits the row's compare-and-set in its own transaction. When its commit cannot
be proved, recovery reads the original row before any new admission.
`Store.Read` exposes only a scoped `Reader`; its queries run with SQLite query-only
mode, including queries that try to execute DML. Owner callbacks receive a scoped
`Executor`, and every issued capability and cursor expires when its callback ends.
The Store never exposes its pool, so pool closure, independent writers and
connection settings remain with its original lifecycle owner. Application adapters
compose by passing the issued capability explicitly. The standalone transaction
helpers apply only to separately owned databases returned by `OpenSchema`.

The mutation returns bounded evidence locating its committed change. This is
submission evidence, not a plugin's business receipt. Actual business results stay
with their original owner. An optional synchronization callback updates the local
authority after a confirmed commit and before admission reopens. Its failure
preserves Committed and raises the recovery barrier.

Committed and NotCommitted are terminal. Repeating the same identity returns its
original conclusion; changing its request is a conflict. Context cancellation is
checked before COMMIT. Cleanup responsibility starts before BEGIN or a connection
mode change is attempted, since a failed acknowledgement does not prove that the
change failed. Transaction rollback or connection disposal is joined
before the owner releases admission. An unresolved connection or storage error
raises RecoveryRequired instead of authorizing a retry.

Read-mode restoration failures belong to that original connection owner, including
nested standalone read scopes. Catching a callback panic or ignoring its returned
error cannot clear this cleanup obligation: the owner refuses COMMIT and discards
the uncertain connection after rollback. Propagated panics retain both their
original cause and any mode-restoration diagnostics.

Overlapping `RunReadSnapshot` callbacks share read-mode leases on their original
connection, including scopes entered with an already read-only context. Only the
last lease restores the previous mode; a sibling or nested reader cannot regain
write access because another reader returned first. Callbacks run without holding
the lease mutex, so nested scopes do not deadlock. Once the transaction callback
returns, its owner refuses new read leases and joins those already admitted before
COMMIT or rollback. Accepted callbacks must finish their own work; cancellation
does not let transaction settlement bypass them.

`Close` seals new Store transactions and joins those already admitted. A failed
rollback cannot return its original connection to the pool. The Store retains
that exact connection and its authority lock until the original driver confirms
release. Repeated `Close` calls join or advance the same shutdown; a later
`database/sql.Close` returning nil is not release evidence. If release remains
unconfirmed, the outstanding shutdown keeps the owner alive until the process
exits, even when a caller drops its handle.

If initialization fails and its cleanup is also unconfirmed, `Open` returns the
sealed Store together with an error. Callers must preserve that cleanup handle
and call `Close`; it is not a partially usable database. Normal initialization
failures whose resources were released return a nil Store. An indeterminate
business commit reports `Indeterminate`, without speculative committed evidence.

`Settlement` reads the original conclusion. `RecoverPrepared` may settle remaining
Prepared intents only after this Store has acquired exclusive authority and old
connections have ended. `Pending` exposes records requiring restoration or local
synchronization. `Confirm` records synchronization but does not clear the Host's
recovery barrier. The composition root calls `Admission.Recover` after restoring
all necessary owners and execution scopes; recovery does not replay mutations.

## Evidence retention

`RetainEvidence` pins detailed evidence for an explicit recovery holder.
`ReleaseEvidence` removes that holder monotonically. `Compact` advances only over a
continuous terminal, synchronized, unreferenced sequence prefix. Deletion and the
watermark commit in one transaction. Old queries return `ErrEvidenceExpired`;
expiration never authorizes recreating an old identity.

## Execution protection

`RecordAttempt` persists the exact Unit, BindingSnapshot, profile, Realm and Host
epoch before first creation. A short-lived child uses its own attempt and exact
binding; it cannot silently enlarge an earlier approval. Repeated recording must
match the original identity and bytes.

`Attempt` and `Use` look up an immutable operation by ID. `PendingAttempts`
returns approved attempts and outstanding exit acknowledgements.
`ExecutionUses` reads one execution's approvals. `ExecutionDependencies` reads
the still-protected rows from both tables in the same transaction.
`Attempts` and `ExecutionDependencies` remain separate from business `Held`
queries. An idle Ready process may be retired by Host policy; its bytes and setup
dependencies remain protected until actual exit is proved. A runner crash, lost
connection or new Host epoch does not prove exit.

Only the trusted driver that verified the actual owner's evidence may call
`ReleaseAttempt`. The proof identifies the original attempt, Realm and exit. Its
durable acceptance is idempotent, and a different proof cannot replace it. These
monotonic settlements use `Admission.Maintenance`, which is the same gate and does
not independently reopen recovery. Native in-memory Hosts do not need this package.
