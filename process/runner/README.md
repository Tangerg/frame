# Local process runner

The runner owns actual direct child processes and their `os.Process.Wait` identity.
It does not select providers, manage installations, keep a second dependency graph,
or decide plugin business results. The Host records an exact durable StartAttempt
before calling Launch.

Build `./cmd/frame-runner` and start it with an absolute private `--directory`
and `--socket` within that directory. A file lock prevents concurrent runner owners.
The Unix socket has mode 0600. Its supported trust boundary is the same local OS
user; it is not a remote or multi-user service. No TCP listener is exposed.
The complete socket pathname must fit the platform's Unix address capacity.
Daemon construction, listeners and clients refuse an overlong path explicitly
before creating state or interpreting it as a missing authority. Configure a
shorter private state directory; the runner does not create another temporary
socket namespace or silently switch transports.

`Server.Listen` publishes the control socket under that Server's directory lock.
`Serve` accepts only its original Server's listener. A live publication cannot be
replaced, and a closed Server cannot publish another endpoint. Shutdown joins the
listener and accepted connections before releasing the directory lock. An old
listener's later `Close` cannot remove a successor's pathname.

Daemon `Close` joins its original process even when startup has not completed.
An interrupt delivered by that close is an expected stop only after the original
Wait observes the matching exit. A process that already crashed keeps its original
exit diagnostic. If shutdown retains unconfirmed resources, restore them and call
`Close` again to advance that same process's shutdown. A cancelled wait leaves the
original owner active; later calls cannot start or substitute another daemon.

Server shutdown seals creation before advancing each original child's close.
A rejected Kill leaves the close request pending and the directory authority held;
a subsequent `CloseAttempt` or `Close` advances the same process handle. Sending a
signal is not exit evidence. Only the original Wait observation permits releasing
the child and its executable snapshot. Unconfirmed shutdown retains its owner and
lock even when the caller drops the Server handle. A Wait without an exit
observation remains Unknown and cannot hand authority to a replacement runner.

`NewClient` validates the static socket address without connecting. Every control
operation opens its own connection and verifies the physical authority directory.
The client API is `NewClient`, `Identity`, `Fence`, `Launch`, `Attach`, `Inspect`,
`CloseAttempt`, `AcknowledgeExit` and `ProveAbsent`. `Identity` identifies the actual runner incarnation. Record that
identity as the execution Realm, and verify the returned running observation before
publishing any plugin export. `Fence` advances the approved Host epoch only after
older Launch operations have settled. Old processes retain their original close
path. `Attach` returns an `io.ReadWriteCloser` suitable for a framed stdio stream
transport. There is one attachment per attempt; disconnect begins its original exit.
The attachment owns a control connection and a separate stdin/stdout connection.
Control is admitted first and carries no payload after its acknowledgement, so
child stdin backpressure cannot conceal a disconnected Host. The stream joins
that original attachment; a missing, closed or already connected attachment
refuses it. Control loss, server cancellation or completion of a forwarding worker
closes both connections and requests the original attempt's close before joining
the forwarding workers. Control is the attachment's independent liveness signal;
normal data backpressure does not expire it or invent an exit observation.
`Attach` still returns one `io.ReadWriteCloser`; its `Close` releases both connections.
The private runner protocol now uses `attach_control` and `attach_stream`; the
former single-connection `attach` operation is removed.

Taking the control attachment transfers the sole stdout reader before its reply
is sent. Failed acknowledgement, stream completion and cancellation all close
that same reader. If the child exits before its data connection joins, the control
attachment closes too; an already connected reader may drain the final output.
Child exit and output-pipe release remain separate resource obligations.

Launch accepts an absolute executable, its exact SHA-256 digest, explicit arguments,
working directory and environment, plus configuration and credential revisions.
Program paths, arguments and environment entries must be valid UTF-8 without NUL.
The request owns this admission through `LaunchRequest.Validate`. Both the driver
and runner validate before recording their respective responsibility, and the
control encoder never replaces malformed bytes with a different character.
It copies and verifies the executable into a private read-only snapshot before
creation. There is no PATH lookup or shell interpretation. Environment values are
not persisted or included in the journal digest; credential revisions identify a
changed injection. Only explicitly supplied environment variables enter the child. An absent or empty list starts it with an empty environment. Secrets must not be put in command-line arguments. Diagnostics
do not log arguments or environment values.

The launch attempt retains its executable snapshot until `Start` succeeds. Every
pre-start failure releases that snapshot and returns any cleanup failure with the
creation error. Successful creation transfers cleanup to the original child Wait;
its completion releases the snapshot before publishing the terminal observation.
Snapshot cleanup diagnostics remain visible in that observation without changing
the direct-child exit proof.

The runner checks its launch context again after snapshot verification and before
process creation. Cancellation during preparation settles the already reserved
attempt as the original runner's durable `NotCreated` observation and releases
the snapshot. It cannot produce `NotDispatched` evidence for an accepted request.
After creation, cancellation still requires the original child's close and Wait.

`Launch` returns a `LaunchResult`. Its observation is the original runner's known
state; `NotDispatched(request)` is narrower evidence from this particular local
invocation, bound to its complete request. Cancellation, encoding refusal and a
failed connection before any write can establish that local fact. Once a write
starts, missing or interrupted replies remain unknown. A nondispatched retry
cannot disprove acceptance of an earlier call with the same attempt ID. The
process driver retains its unique original call and never retries creation to
obtain a different answer.

`NotCreated` is a durable terminal observation from the original runner. Capacity
refusal and failures before child creation retain the original request digest and
proof in the existing receipt store. They consume no active-attempt slot, and the
same identity remains refused after capacity becomes available or the runner
restarts. `Exited` requires an actual original child Wait and an exit code. Both
terminal variants can produce an `ExitReceipt`, whose state is part of its exact
acknowledgment identity. Receipt or journal persistence failure withholds proof,
retains the original terminal observation and fences creation.

The attempt record is synchronized to disk before the first creation effect. The
same attempt never creates twice, including after a failed start or completed exit.
Repeated requests return its original result, current state or Unknown. Capacity is
explicit: unresolved identities and exits awaiting handoff occupy a slot. The Host
commits the exact exit proof before acknowledging it. The runner then moves the
original terminal record to a disk receipt outside the capacity budget. A repeated
launch reads the same receipt, and an existence check cannot mistake it for a
never-created child. Attempt IDs are global, so a small disk receipt containing
the request digest and terminal observation survives later creation epochs and
runner restarts. Reusing the ID with another epoch or request is a conflict.
A crash between handoff and confirmation is reconciled by repeating that exact
acknowledgement against the original state directory.
This journal format and the corresponding private Host schema require fresh state.
The private control response is one result variant admitted against the requested
operation: acknowledgment, identity, observation or absence proof. Acknowledgments
carry explicit `true`; null, missing results and results for another operation
are refused. Only an observation can accompany a failure response.

A failed journal write does not erase the original Wait observation or a known
failure before process creation. The Server retains its immutable terminal fact
and proof identity while withholding that proof from clients. Inspection, close,
replay of the same launch, or a later
durable fence can commit that original observation after storage recovers.
Creation remains fenced, but a durable exit can complete its exact handoff without
a new creation epoch. Shutdown retains directory authority until pending terminal
observations are durable. If the Runner itself crashes first, its uncommitted
memory cannot establish exit after restart.

An interrupt or termination signal starts the executable's shutdown. If shutdown
remains unconfirmed, the executable reports the failure and keeps that same Server
and directory lock alive. Restore the failed resource, then send another interrupt
or termination signal to advance the original shutdown. The executable exits only
after the original authority can be released; a nonzero exit code cannot preserve
an uncommitted Wait observation.

`ProveAbsent` covers a Host attempt whose creation request never reached this
original runner. It checks the exact runner identity, a synchronized fence beyond
the attempt's epoch and ledger absence under the same lock. An earlier identity
observation or `CloseAttempt` returning not found is not this proof. A different
runner, an unfenced epoch or a present attempt cannot establish non-creation.

A surviving runner can terminate and join its owned direct child after the Host
reconnects. If the runner itself crashes, its new incarnation preserves unfinished
attempts as Unknown and never acts on their old PIDs. A new epoch cannot turn that
uncertainty into exit evidence. Inspection and close remain available while new
creation is fenced. The framework keeps the original execution protection until a
supported trusted proof is available.

The default release proof covers the direct child whose wait completed, not every
descendant, arbitrary detached service or business side effect. Plugins must keep
extra work within their declared execution and cleanup contract. The runner does
not claim cgroup, Job Object or whole-process-tree containment. Platform conformance
must verify exactly the scope that a product advertises. The Unix implementation
does not silently substitute a weaker mechanism on unsupported platforms.

Launch, Attach and CloseAttempt take the original AttemptRef, which binds the
attempt ID and creation epoch to the approved runner incarnation. A replacement
runner can replay an immutable terminal receipt, but cannot create an attempt
on behalf of its predecessor. State and socket paths must have trusted owners
and be protected from replacement by other users. Root-owned sticky temporary
directories may contain the private runner directory.
