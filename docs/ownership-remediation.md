# Ownership and contract remediation

This change addresses the repository audit against
`dbc32590121e8f4d8fc6fd1cb9b31d9a38ea9620`. The audit identifiers below refer to
that baseline. Development interfaces change together; there are no compatibility
aliases or database migration adapters in this change.

## Facts and their owners

| Fact | Owner | Derived representations |
| --- | --- | --- |
| Exact provider and prepared binding | Host admission and the original Instance | Binding plans, approved roots, retained Use members |
| A browser acquisition's identity and ancestry | Original Use admission and its parent Lifetime | Operation receipt, grant, public handle, proof reservation |
| Resource release | Original Lifetime/resource transition | Consumer counts, ready queue, pending diagnostics, exit evidence |
| Whether a SQL connection may return to its pool | Original connection transaction | BEGIN acknowledgement, mode restoration, rollback and discard obligation |
| Accepted stream delivery | Original call's delivery owner | Chunk acknowledgement, consumer failure, final-result eligibility |
| Local process exit | Original runner child handle and Wait | Attempt observation, exit proof, directory-lock release |
| Portable predicate semantics | Admitted canonical descriptor and Go profile | Node predicate adapters, generated profile, shared corpus |
| Generated structural shape | One typed projection graph | Go and TypeScript declarations |
| A UI operation's task | Command captured at the event entrance | HTTP requests and selected-task rendering |

Indexes and projections do not originate a competing release, grant, commit or
business-result transition. A late acknowledgement advances the original owner.

## Correctness changes

| Audit | Change | Regression evidence in the repository |
| --- | --- | --- |
| F01 | Delegated browser Uses branch from their retained parent; parent exit waits for child work. | `sdk/src/browser/runtime.test.ts` |
| F02 | Acquire and accept share one operation admission. Terminal handles remain until proof acknowledgement; historical receipts must pass current Host validation before another materialization. | `sdk/src/browser/runtime.test.ts` |
| F03 | Cleanup is installed before BEGIN and query-only mutation. The original connection owns overlapping read scopes and restoration; discard obligations survive caught errors or panics. | `sqlite/acquisition_test.go`, `sqlite/read_mode_panic_test.go`, `sqlite/read_scope_test.go` |
| F04 | Exact roots use the driver's realm rule and the original prepared binding identity. A replacement setup cannot be combined with an older captured binding. | `host/use_realm_test.go`, `host/use_identity_test.go` |
| F05 | Go exports pattern atom meanings and the admitted format inventory. Both runtimes execute the same admission, predicate and identity cases. | `value/schema_profile_test.go`, `portable_profile_test.go`, `sdk/src/node/profile.test.ts` |
| F06 | Generated numbers retain exact JSON lexemes, including large integers, exponent notation and negative zero. | `value/number_test.go`, `contractgen/consumers_test.go` |
| F07 | Open and mixed objects retain every admitted member. Optional explicit null remains distinct from absence. | `contractgen/consumers_test.go` |
| F08 | TypeScript arrays use `Array<T>` so compound elements retain their grouping. | `contractgen/consumers_test.go` |
| F09 | Base requiredness, branch constraints, references and object openness are interpreted by one projection graph. | `contractgen/consumers_test.go` |
| F10 | Shared definitions use canonical JSON identity, including nested literal objects. | `contractgen/consumers_test.go` |
| F11 | Bridge admission reserves complete encoded control messages and aggregate pending projections. Exit evidence remains bounded independently of diagnostics. | `browser/capacity_test.go`, `sdk/src/browser/runtime.test.ts` |
| F12 | Consumer rejection, invalid chunks and pending deliveries veto successful stream completion in Go and Node. Accepted delivery must join before retirement. | `process/service_outcome_test.go`, `sdk/src/node/index.test.ts` |
| F13 | Realm resume preserves storage and cancellation causes. | `browser/credential_test.go` |
| F14 | Lifetime retirement uses consumer counts and a ready queue updated by original release transitions. Running cleanup continues to hold dependencies after late proof. | `lifetime_graph_test.go`, `lifetime_test.go` |
| F15 | Convert and cancel retain the task captured before awaiting; stale results cannot overwrite a different selected task. | `examples/document/web/workflow.test.mjs` |
| F16 | Authored source and host input use the existing nonblocking regular-file opener. | `examples/authoring/native/source_unix_test.go`, `internal/localfile/regular_unix_test.go` |

The related design findings are also addressed:

- **R01:** Store exposes scoped `Reader` and `Executor` capabilities. The raw
  `Store.Database` and `WithTransaction` surfaces are removed. Read views cannot
  recover write or pool-lifetime authority through a type assertion.
- **R02:** Failed Kill leaves the original runner close operation and directory
  lock retained. A retry advances the same child handle; only Wait proves exit.
- **R03:** Local predispatch refusal remains a known refusal. After handler
  dispatch, cancellation or an absent/invalid result cannot prove rollback.
  Go preserves cancellation and `ErrOutcomeUnknown`; Node projects this through
  `ProcessError.outcomeUnknown`. Validated business refusals remain business
  results and do not imply that dispatch or effects never occurred.
- **G01:** Generation resolves complete local JSON Pointers, escaped tokens,
  URI-fragment encoding, nested targets and structural recursion.
- **Q01–Q06:** Closed states and CLI classifications have semantic types; schema
  projection uses a typed graph; unused builders and reflection wrappers are
  deleted; affected comments and names match their responsibility; owner-specific
  defaults are named locally.

## Intentional interface changes

### Browser control

`BridgeConfig.MaxMessageBytes` bounds complete requests and responses.
`Credentials.MaxMessageBytes` projects that budget to `HTTPControl` consumers.
`UseGrant` includes the original `operationId` and optional `parentUseId`.

Close diagnostics remain on the local Exit. They are no longer part of wire
`CloseProof`. Reconnect carries original execution identities; the existing proof
outbox sends original proofs individually. Aggregate `pendingProofs` and
`acceptedProofs` reconnect fields are removed. Upgrade Host and SDK together and
regenerate application protocol declarations.

### SQLite composition

`Store.Read` and `OwnerSchema.References` receive `sqlite.Reader`. Business writes
remain on their original `Owner.Update` or `Owner.Commit` executor. Standalone
database adapters compose `RunInTx` and `RunReadSnapshot` over the concrete
`sqlite.Database` returned by `OpenSchema`. Its private pool and original physical
connections share the Store lifecycle implementation. `Connection` grants only a
transaction-scoped executor; independent reads use `Database.Read`. A failed
physical Close retains the original resource and seals new admission until that
connection is released. No raw `*sql.DB` compatibility entry remains.

### Portable contracts

Supported formats are `date`, `time`, `date-time`, `uuid`, `json-pointer` and
`relative-json-pointer`. Other names are rejected during admission. Pattern
repetition is fixed and bounded, and applies to consuming atoms; repeated
anchors or word-boundary assertions are outside the shared profile.

`contract-profile.generated.json` replaces `portable/limits.generated.json`.
It is generated from Go and includes limits, pattern atoms and formats. The
unused `ajv-formats` dependency is removed.

The former `value.Number(...)` cast is replaced by the owned `value.Number` type
and `value.ParseNumber`. Generated number fields, open objects, nullable values,
date/time strings and object references have new representations. The unused
public Schema/TypeScript builder DSL and reflection wrappers are removed.
`contractgen.Generate` remains the supported generation entrance.

Static declarations preserve representable structure and values. They do not
replace canonical runtime validation of numeric bounds, pattern, format,
conditional predicates or exclusive alternatives. See
[the generator contract](../contractgen/README.md).

## Verification

Run from a clean checkout after `npm ci`, using the Go version in `go.mod`:

```sh
go generate ./...
git diff --exit-code
make check
make example
./scripts/check-browser-workflows.sh
python3 scripts/go-consumer.py
```

The independent Go consumer uses the committed tree. The npm gate installs the
SDK tarball into an independent consumer. Generator tests compile actual Go and
TypeScript consumers and compare value round trips and predicate behavior.

`go test -run '^$' -bench '^BenchmarkLifetimeDependencyChain$' .` measures the
retirement graph without an absolute timing assertion. Graph bookkeeping is
linear in resources plus dependency edges; cleanup callbacks still determine
external release latency.

The optional Flame suite uses the external checkout supplied to
`scripts/check-flame-interop.mjs`. Real browser-engine checks additionally require
`FRAME_CHROME_EXECUTABLE`. The default browser workflow uses real Go/HTTP
transports with a DOM implementation and does not substitute for browser-engine
crash or unload evidence.
