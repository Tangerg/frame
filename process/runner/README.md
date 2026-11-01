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

Daemon `Close` joins its original process even when startup has not completed.
An interrupt delivered by that close is an expected stop only after the original
Wait observes the matching exit. A process that already crashed keeps its original
exit diagnostic.

The client API is `Dial`, `Identity`, `Fence`, `Launch`, `Attach`, `Inspect`,
`CloseAttempt`, `AcknowledgeExit` and `ProveAbsent`. `Identity` identifies the actual runner incarnation. Record that
identity as the execution Realm, and verify the returned running observation before
publishing any plugin export. `Fence` advances the approved Host epoch only after
older Launch operations have settled. Old processes retain their original close
path. `Attach` returns an `io.ReadWriteCloser` suitable for a framed stdio stream
transport. There is one attachment per attempt; disconnect begins its original exit.

Launch accepts an absolute executable, its exact SHA-256 digest, explicit arguments,
working directory and environment, plus configuration and credential revisions.
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
