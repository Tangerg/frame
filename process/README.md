# Managed local processes

`process.Driver` implements the `local-process` profile using the independent
`frame-runner` and the [public stdio protocol](WIRE.md). It receives an already
approved Frame `Setup`; it performs no global provider lookup. The optional
[MCP adapter](mcp/README.md) can replace the wire protocol for a registered driver.
All profiles use the same durable attempt, runner, and exit-proof path.

Supply the same SQLite Store value and Admission used by the Host. `Source` resolves
the approved executable, arguments and optional contribution metadata; the Host supplies exact published descriptors. Configuration hashes
exclude secret values; environment credentials are supplied only to the runner.
The caller explicitly supplies frame and retirement limits.

Each creation is protected by a durable attempt before runner dispatch. The
actual process, handshake, setup and exports belong to the instance Lifetime.
Failed setup, stale publication and cancellation close that same resource. A
timeout or lost runner remains unconfirmed and retains its dependency closure.
Before recording that attempt, the driver validates and freezes the original
launch request and checks the Store's complete encoded capacity for local
non-dispatch, confirmed non-creation, confirmed runner exit and sealed absence.
The persisted runner proof contains its immutable `ExitReceipt`; diagnostic
reason, PID and exit code remain on the runner's original observation. The same
receipt identity is acknowledged after durable acceptance, so diagnostic growth
cannot prevent an admitted process from handing off its actual exit.
A unique original `LaunchResult` can prove that its own request was never
written. The driver retains this exact result; a later nondispatched retry could
not disprove earlier acceptance. Runner capacity refusal is recorded by the
original authority as `NotCreated`, preserving the request and proof even when
all active slots are occupied. Both forms settle current-epoch protection without
manufacturing an exit from NotFound or a timeout.
`Reconcile` advances the original close and consumes late terminal evidence only
after the original protocol and attachment cleanup has completed. It refuses
earlier calls before changing the attempt or acknowledging its result. It never
repeats creation or protocol cleanup.

On Host restart, call `Recover` behind the recovery barrier before admitting any
new work. It fences older creation epochs and retires old attempts. An absent
attempt proves non-creation only when the original runner's `ProveAbsent`
observes both the durable fence and missing attempt in one locked operation.
The driver verifies that evidence against the original attempt before settling
its SQLite protection. Changing runners between control requests stays unknown;
a new runner cannot infer ownership from a PID or erase an old protection.

The runner's release proof covers its directly owned child. An application that
allows plugins to start additional processes must provide a stronger execution
profile with evidence for that larger resource set.

Go plugins call `ServeStdio` with a `process.Unit`. Setup receives exact imported
services and a structured Lifetime. Preparation, activation and accepted calls
belong to that Lifetime; retirement joins them before cleaning up their resources.
Preparation and activation acknowledge only after their callbacks complete with
uncancelled work and an open original Lifetime. Failed or interrupted callbacks
begin that Lifetime's retirement. Lifecycle phases track preparation progress;
the Lifetime alone owns closure, including closure requested by plugin code.
Serve captures the unit's capabilities before the handshake. Replacing an author
capability or a method lookup result cannot change the described instance's contract.
`frame.Service.Invoke` and `frame.StreamingService.Stream`
accept a `frame.Invocation`. Construct queries with `frame.NewQueryInvocation` and
commands with `frame.NewCommandInvocation`; the business caller supplies the original
command identity. Every relay carries that same value to the final handler.
The admitted method owns kind and schema validation. Queries cannot carry command
identity, and commands cannot execute without it. Transport request IDs remain
separate from business command identity and never authorize replay.

Typed method construction, local implementations, schema derivation and invocation
validation belong to [Frame contracts](../docs/architecture.md#contract-and-value-owners).
A process protocol consumes those admitted contracts through `Setup.Capability`;
it never reparses descriptors or chooses a different callable identity.
`frame.Service` and `frame.StreamingService` are shared by native and remote calls.

Local reply projection failures retain their original decode or shape cause
alongside `frame.ErrOutcomeUnknown`. Only incoming request failures become sanitized
wire refusals; an invalid reply cannot masquerade as a refusal before dispatch.
