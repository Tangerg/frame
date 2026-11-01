# Managed local processes

`process.Driver` implements the `local-process` profile using the independent
`frame-runner` and the [public stdio protocol](../protocol/process/README.md). It receives an already
approved Frame `Setup`; it performs no global provider lookup.

Supply the same SQLite store and Admission used by the Host. `Source` resolves
the approved executable, arguments and optional contribution metadata; the Host supplies exact published descriptors. Configuration hashes
exclude secret values; environment credentials are supplied only to the runner.
The caller explicitly supplies frame and retirement limits.

Each creation is protected by a durable attempt before runner dispatch. The
actual process, handshake, setup and exports belong to the instance Lifetime.
Failed setup, stale publication and cancellation close that same resource. A
timeout or lost runner remains unconfirmed and retains its dependency closure.
`Reconcile` consumes late Wait evidence without launching or cleaning up again.

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

Go plugins call `ServeStdio` with a `process.Unit`. Setup receives exact imported services and a structured Lifetime. `NewMember` derives a unary method from its Go input/output types and binds its handler through `Implement`. Hosts can provide any implementation of `Service` or `StreamingService` matching the published descriptor.
