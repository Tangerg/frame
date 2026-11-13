# SQLite settlement and execution protection

This optional adapter uses one local SQLite file and the Host's existing
`framehost.Admission`. A held operating-system lock on a persistent `.frame-lock`
sidecar grants one Store authority; opening another Host on the same database
fails. The sidecar stays in place after release and must not be replaced while a
Store is alive. Database and lock paths must be regular files without hard links.
The sidecar keeps authority separate from SQLite's own byte-range locks on macOS.
The database is configured and
checked for WAL, synchronous FULL, foreign keys and disabled dirty reads. The
profile requires a reliable local filesystem. It does not support network filesystems,
independent writers or atomic transactions across databases.

`Open` accepts the complete current set of `OwnerSchema` declarations. A different
schema requires a fresh data directory. The declaration identity encodes sorted
owner identities and each statement boundary; SQL comments cannot impersonate
another owner declaration. Reopening also checks the persisted owner set. Each
owner supplies its original business tables and a trusted reference query.
Register the resulting `Owner` with the same
Admission through `RegisterUseOwner` before its first destructive dependency check.
That check seals the owner set for the life of this Admission. Frame does not create a competing business
reference-count ledger. Every admission read uses a new current transaction.
`Store.Owner` returns an `Owner` value capability. Store that value and register it
directly: reassigning a local variable cannot replace the Store's declaration or
the reference reader already captured by Admission. A zero Owner refuses all
operations with `frame.ErrInvalid`; its diagnostic `ID` is empty.

`Open` returns a `Store` value handle. Hosts and drivers retain that value, so
reassigning the caller's variable cannot redirect their original database. Copies
compare by their one private authority. Issued Owners and receipt reservations
retain that authority directly. Copying or replacing a caller's Store handle
cannot redirect an accepted capability to another database, admission gate or
epoch. Every retained copy joins the same physical database shutdown.
Host construction validates each declared durable authority against its Admission
and epoch before driver callbacks. When both installation persistence and execution
protection are durable, they must retain the same original Store.
Standalone transactions capture their original database owner before application
callbacks. A lost commit acknowledgement fences and closes that owner even when
the callback replaces its public Database handle.

Schema admission derives each owner's write grant from the relations its original
statements create. There is no separately maintained table allowlist. An owner
may seed those relations, but cannot modify Frame's tables or another owner's
schema during initialization. Once Open returns, schema changes are closed;
business callbacks can only change rows in their admitted relations. Views,
triggers and foreign-key cascades remain subject to the same physical connection
authorizer. A view or trigger cannot delegate another owner's write authority.
SQLite's own catalog and sequence tables never become business write grants.
Schema declarations cannot create virtual tables: their modules can expose
physical database pages or other writes outside the declared relation, which
this grant cannot assign to a business owner.

`OwnerSchema.ProjectInstallation` writes that owner's installation projection in
the original installation transaction, under the same schema grant as its other
mutations. Frame alone changes installation state and settles the private
submission. Projection callbacks run in owner identity order; each grant expires
and joins its cursors before the next owner receives the connection.

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
`Store.Read` exposes only a scoped `Reader`; a SQLite authorizer on the original
physical connection rejects writes, including DML expressed through query APIs.
Owner callbacks receive a scoped `Executor`, and every issued capability and
cursor expires when its callback ends. `QueryRowContext` returns `*Row`, which
projects a cursor held by that same scope. The scope closes and joins all its
cursors before restoring connection authority; canceling a query alone cannot
prove that a later `Rows.Next` or `Row.Scan` has stopped.
Each physical cursor retains its first driver `Close` result, including closure
initiated by the caller, EOF or cancellation. Transaction settlement reads that
original evidence after joining the cursor. A later `database/sql.Rows.Close`
returning nil cannot erase a cleanup failure. SQLite's finalizer may repeat the
last statement-evaluation error after destroying the statement; that original
evaluation error remains distinguishable from an additional cleanup failure.
The Store never exposes its pool, so pool closure, independent writers and
connection settings remain with its original lifecycle owner. Application adapters
compose by passing the issued capability explicitly. The standalone transaction
helpers accept the concrete `*Database` returned by `OpenSchema`, never a raw
`*sql.DB`. `Connection` grants an executor only inside the original transaction
context. For independent reads, `Database.Read` lends a scoped `Reader`; direct
pool queries, settings and closure are not exposed. Store and standalone
transactions compose the same transaction and physical-connection owner.
`OpenSchema` uses modernc's registered
connector and preserves its registered functions, collations and connection hooks.
Both read and write callbacks reject transaction control, attachments and
configurable PRAGMAs. Metadata-only PRAGMAs may inspect databases, tables, columns,
indexes, foreign keys and the SQLite runtime. Changing or removing an authorizer
expires prepared statements, so a previously compiled statement cannot bypass a
later read scope. Standalone nested readers receive their own expiring capability;
the original writer regains its authority only after the final reader returns.
Nested `RunInTx` joins the original transaction and reports its callback's error
and cancellation cause together. This completion rule is the same as an independent
transaction; returning that error to the outer owner prevents COMMIT. A caller
that catches an ordinary nested error still owns its decision to continue the
outer transaction.

The mutation returns bounded evidence locating its committed change. This is
submission evidence, not a plugin's business receipt. Actual business results stay
with their original owner. An optional synchronization callback updates the local
authority after a confirmed commit and before admission reopens. Its failure
preserves the committed conclusion and raises the recovery barrier.
These obligations also survive `runtime.Goexit`: an entered mutation settles its
original rolled-back intent before admission returns, and an interrupted
post-commit synchronization keeps admission fenced until the owner restores it.

`framehost.CommitSucceeded` and `framehost.CommitRejected` are terminal. Repeating the same identity returns its
original conclusion; changing its request is a conflict. Context cancellation is
checked before COMMIT. Cleanup responsibility starts before BEGIN or an authorizer
change is attempted, since a failed acknowledgement does not prove that the
change failed. A failed BEGIN disposes its original connection and preserves
the caller's cancellation cause, whether BEGIN ran or not. Transaction rollback or connection disposal is joined
before the owner releases admission. An unresolved connection or storage error
raises RecoveryRequired instead of authorizing a retry.

Statement-policy restoration failures belong to that original connection owner,
including nested standalone read scopes. Catching a callback panic or ignoring its returned
error cannot clear this cleanup obligation: the owner refuses COMMIT and discards
the uncertain connection after rollback. Propagated panics retain both their
original cause and any policy-restoration diagnostics.

Overlapping `RunReadSnapshot` callbacks share read leases on their original
connection, including scopes entered with an already read-only context. Only the
last lease restores the transaction policy; a sibling or nested reader cannot regain
write access because another reader returned first. Callbacks run without holding
the lease mutex, so nested scopes do not deadlock. Once the transaction callback
returns, its owner refuses new read leases and joins those already admitted before
COMMIT or rollback. Accepted callbacks must finish their own work; cancellation
does not let transaction settlement bypass them.

`Close` seals new Store or standalone Database transactions and joins those already
admitted. A failed rollback cannot return its original connection to the pool.
The database owner retains that exact connection and any authority lock until the original driver confirms
release. Repeated `Close` calls join or advance the same shutdown; a later
`database/sql.Close` returning nil is not release evidence. Standalone commit
acknowledgement loss fences admission before the original connection is released.
Transactions already queued for that connection refuse their callbacks. The
application must reconcile the original business operation and open a new Database.
If release remains
unconfirmed, the outstanding shutdown keeps the owner alive until the process
exits, even when a caller drops its handle.

If initialization fails and its cleanup is also unconfirmed, `Open` or
`OpenSchema` returns the sealed Store or Database together with an error. Callers
must preserve that cleanup handle and call `Close`; it is not a partially usable
database. Normal initialization failures whose resources were released return a
zero Store value or nil Database pointer; closing either is safe. An indeterminate
business commit reports `Indeterminate`, without speculative committed evidence.

`Settlement` reads the original conclusion. `RecoverPrepared` may settle remaining
Prepared intents only after this Store has acquired exclusive authority and old
connections have ended. Storage uses a closed submission state: prepared,
committed or rejected awaiting synchronization, and committed or rejected with
synchronization confirmed. `Settlement.Synchronized` is derived from that state;
there is no independently writable synchronization flag. Only committed states
may carry business evidence. Every settlement, pending read, confirmation,
recovery and compaction validates the original request digest and state evidence
before advancing it. Corrupt records fence admission instead of being repaired
into an actionable intent. Issuance and compaction watermarks define the exact
retained interval. New issuance, full reads and reopening require every sequence
in that interval; reading, confirming or retaining an issued identity whose
original evidence is missing also fences admission. An unknown identity or one
below the compaction watermark remains an ordinary caller error.
`Pending` exposes records requiring restoration or local
synchronization. `Confirm` records synchronization but does not clear the Host's
recovery barrier. The composition root calls `Admission.Recover` after restoring
all necessary owners and execution scopes; recovery does not replay mutations.

## Evidence retention

`RetainEvidence` pins detailed evidence for an explicit recovery holder.
`ReleaseEvidence` removes that holder monotonically. `Compact` advances only over a
continuous terminal, synchronized, unreferenced sequence prefix. Deletion and the
watermark commit in one transaction. Old queries return `ErrEvidenceExpired`;
expiration never authorizes recreating an old identity.

Installation control receipts have no external recovery holder. The installation
call owns its private intent from preparation onward and concludes it on every
exit, including cancellation before mutation, panic and `runtime.Goexit`. A
rejected intent releases capacity only after its original submission has settled
and synchronized; an unresolved or committed receipt remains available for its
original recovery and confirmation. This cleanup never infers rejection for a
public submission whose original request the caller has not proved.

## Execution protection

`RecordAttempt` persists the exact Unit, BindingSnapshot, profile, Realm and Host
epoch before first creation. A short-lived child uses its own attempt and exact
binding; it cannot silently enlarge an earlier approval. Repeated recording must
match the original identity and bytes.

The encoded request remains the original approval; the SQL identity is only its
lookup projection. Replay, delegated admission, reconstruction and settlement all
verify the same request digest, matching identity and complete domain invariants.
An approved record cannot carry exit evidence, and a settled record must carry
complete proof for its original execution and Realm. Corruption discovered while
advancing protection raises the Host's recovery barrier before the transaction can
change its conclusion. Attempt epochs live only in their original request.

`Attempt` and `Use` look up an immutable operation by ID. `PendingAttempts`
returns approved attempts and outstanding exit acknowledgements.
`ExecutionUses` reads one execution's approvals. `ExecutionDependencies` reads
both tables in the same transaction and verifies every original request and proof
before excluding terminal records. Pending-attempt queries use that same complete
record admission. Reopening validates all protection records before advancing the
epoch or discarding old control replies; a terminal label without matching proof
cannot bypass recovery or release dependencies.
`Attempts` and `ExecutionDependencies` remain separate from business `Held`
queries. An idle Ready process may be retired by Host policy; its bytes and setup
dependencies remain protected until actual exit is proved. A runner crash, lost
connection or new Host epoch does not prove exit.

Only the trusted driver that verified the actual owner's evidence may call
`ReleaseAttempt`. The proof identifies the original attempt, Realm and exit. Its
durable acceptance is idempotent, and a different proof cannot replace it. These
monotonic settlements use `Admission.Maintenance`, which is the same gate and does
not independently reopen recovery. Native in-memory Hosts do not need this package.

Host Use settlement persists the original `frame.ReleaseProof`, projected by its
confirmed `Close`. Cleanup diagnostics remain on that Lifetime; they cannot grow
the durable release record or originate another release transition. `ApproveUse`
checks capacity for that fixed terminal representation before accepting the Use.
`SettleUse` runs after the original approval callback has ended, including when its
commit acknowledgement was lost. Its write transaction either persists the
original release proof or confirms that the approval did not commit. An absent
row is conclusive only behind that transaction's writer barrier. An unconfirmed
original connection seals the Store and cannot be mistaken for absent approval.

`Config.MaxRequestBytes` defaults to 1 MiB and bounds complete encoded protection
records, including proof envelopes. Drivers can compose admission with
`CheckExitEvidenceCapacity` for subordinate proof bytes, `CheckExitCapacity`
for attempt settlement and `CheckRealmUseExitCapacity` for reconstructed Uses.
Each check uses its settlement's stored encoding, including base64 expansion of
the original evidence. Drivers admit every possible exit path before recording
an attempt. A configured capacity refusal returns `frame.ErrCapacity`
before a driver accepts responsibility for work it cannot later settle.

## Browser history capacity

`Config.MaxRealmHistoryBytes` defaults to 256 MiB and must be positive when set.
It bounds the UTF-8 bytes of retained Realm credential fields and receipt
identities and request digests, plus completed receipt bytes or the full reserved reply envelope of
each unfinished control operation. SQLite derives each row's charge from that
row; no independently mutable memory counter or quota ledger decides admission.
This is a retained-payload budget, not a limit on SQLite pages, WAL files,
application tables or execution-protection evidence.

`ReserveRealmReceipt` fixes the original request digest and returns a
`ReceiptReservation` before a caller starts the operation. Reusing an identity
for a different request conflicts even before the first reply exists.
If the transaction body accepted the request but its commit was not acknowledged,
the original reservation accompanies the error. The caller must retain and settle
that handle; `Record` returning `sql.ErrNoRows` proves that the reservation rolled
back. An unavailable settlement leaves the original identity unresolved.
`Record` settles only that reservation and cannot replace a
completed result. A completed reply releases unused envelope bytes in the same
transaction. Exhaustion returns `frame.ErrCapacity`; replay and completion of an
already admitted operation do not need new capacity.

Opening a new authority epoch discards its predecessor's live control replies.
Realm verifiers remain charged because old execution owners still need their
original cleanup authentication. No timer or capacity eviction invents proof of
exit. Configure enough retained history for the deployment, or reopen with a
larger budget when that retained responsibility fills it.
