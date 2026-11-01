# Contracts, owners and operation results

A service defines behavior. A point describes a collection of contributions to which a host may apply a policy. Multiple hosts can share the same service identity while collecting, decorating or selecting its implementations differently. A unit can consume imports without exporting a service. Go native implementations run as part of the host build. Portable process implementations must agree on the same published descriptors and wire profile.

| Fact | Authority | Lifetime |
| --- | --- | --- |
| Installation, selected immutable release and enabled state | Host | Until an admitted installation transition |
| Candidate order and business authorization | Host application | The application owns the rule, the Host validates the chosen members |
| Exact approved providers and accepted optional absence | Original binding | Reacquisition cannot silently switch provider |
| Use grant and object reachability | Host and the original use | Withdrawal refuses new uses; already admitted work retains its resources |
| Process exit | Runner holding the direct child | Its original wait result; a disconnected connection proves no exit |
| Business result, pending task and command receipt | Original business owner | A lost response may require observing that same operation |

## Installation and offline tools

Build each artifact first, list it in the manifest, then package its immutable bytes. All three commands run without executing plugin code.

```sh
go run ./cmd/frame package --manifest plugin.json --out plugin.zip
go run ./cmd/frame inspect plugin.zip --format json
go run ./cmd/frame verify plugin.zip --policy publisher-policy.json
```

The bundled CLI trust policy pins SHA-256 archive digests in a file controlled by the host administrator: `{"allowedArchiveDigests":["<64 lowercase hexadecimal characters>"]}`. Inspect verifies byte integrity and reports publisher trust as unassessed. A matched pin assesses that specific archive. Neither operation grants execution. The Go distribution Store accepts an explicit Trust function for custom signature, publisher and capability policies.

Use Store.ImportArchiveHeld to obtain a content lease. Lease.Install validates and projects the verified manifest into the Host. For compiled native units supply native setup functions in distribution.Installation. For an external execution profile register its driver with the Host and implement distribution.ArchiveDriver on that driver. The driver checks the already integrity-validated unit's execution shape. Unknown profiles remain inspectable. Store retention must query both installed release facts and any retained business/execution dependencies. Close a lease to release content; Lease.Snapshot returns a detached projection.

A Host.Install error is not sufficient to tell whether a persistence operation committed. InstallationConclusion(err) reads the owning transition's conclusion, including committed with diagnostics, rejected and indeterminate. Inspect the original installation after recovery; do not replay an unknown mutation blindly.

## Execution boundaries

Pass the same Admission, epoch and SQLite Store to the durable Host and its adapters, for both installation persistence and execution protection. The process driver and browser bridge refuse a mismatched authority. An ordinary scope named like an installation cannot fence the Host's internal installation scope. Validate an authorized grant before starting any unit and revalidate after preparation.

Configure subprocess environments explicitly. An empty environment is the default. Host service grants limit access through Frame, not the process's operating-system rights. Deploy native code only with appropriate OS permissions. Maintain one trusted source for executable artifacts, interpreter pins and dependencies. A local Runner confirms the exit of its direct child, not that child's descendants or effects outside the framework.

The portable schema profile uses the published Scope JSON Schema library as a compiler. No host product or agent transport is required by the framework or its Node SDK.

## Restoring accepted work

The live execution path admits the original plan. A durable host can configure ReadAcceptedBinding once at composition. Its loader receives the record identity and the request's scope grant, authorizes that record, then reads the binding accepted by the original business transaction. Host.AcquireRecord consumes that evidence. A diagnostic BindingSnapshot on its own cannot be presented to the Host as a newly authorized operation. The document queue is a working example in examples/document/tasks.go.
