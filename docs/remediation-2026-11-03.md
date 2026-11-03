# Remediation of the 2026-11-03 audit

This change addresses `FRAME-001` through `FRAME-015` from the audit of
`Tangerg/frame` at commit
`b8b6e022f94202292fc6285088f7fc711bd775fa`. The identifiers in this document
belong to that baseline. This is an implementation and migration record; the
earlier [ownership remediation](ownership-remediation.md) describes a different
baseline and remains historical.

Development interfaces, their consumers and their tests change together. No
compatibility aliases or database migration adapters are introduced.

## Verification commands and their purpose

Run commands from the repository root with `GOWORK=off`, the Go version in
`go.mod`, and Node 24 or newer. Install the checked-in npm dependency graph with
`npm ci`. The Go and workflow suites use isolated databases, temporary files,
real subprocesses and local HTTP or socket endpoints.

| Command | Evidence it provides |
| --- | --- |
| `go test -race -count=1 ./...` | Go behavior and concurrency regressions, including real SQLite transactions, bridge receipts, resource ownership, typed codecs and the pinned MCP SDK. |
| `go vet ./...` | Static Go diagnostics across the module. |
| `npm run check` | Workspace type checking, lint, formatting, SDK tests with exposed GC, and consumption of the packed SDK artifact. The exposed GC run is required for the acquired-payload retention regression. |
| `go generate ./...` | Rebuilds generated contracts and protocol/profile projections from their semantic declarations. In a clean checkout of the completed change, generation must leave no tracked or untracked drift. |
| `make example` | Builds the SDK, executes the real document-indexer process example, and runs the `frame_interop` conformance suite. |
| `./scripts/check-browser-workflows.sh` | Builds the real Go hosts and browser application fixtures, then runs the authoring and document workflows against them. `FRAME_CHROME_EXECUTABLE` adds the real Chromium workflow; a Node workflow alone is not browser-engine evidence. |
| `python3 scripts/go-consumer.py --version v0.0.1` | Builds an independent Go consumer through a temporary module proxy. Run this against a committed snapshot containing the change, rather than exporting the audit baseline. |
| `git diff --check` | Checks whitespace in the reviewable patch. |

The finding-to-test mapping below describes what the tests establish. It is not
a claim that every command above has completed in a particular environment.

## Facts and their owners

| Fact | Authoritative owner | Projections and consequences |
| --- | --- | --- |
| Equality of an exact unit binding | `UnitBinding` canonicalization and equality | Binding digests, restored snapshots and live Capture/admission agree. Import slot declaration order is normalized; provider order within a slot remains significant. |
| The implementation and metadata of a native publication request | Immutable `PublicationRequest` created by its Host | An operation retains that prepared request identity. Another preparation is another request, even when its metadata is equal. Arbitrary Go functions and pointers are never structurally hashed. |
| Whether a publication is visible | The original Host catalog transition | Bridge replies and SDK publication handles observe the catalog's result. Withdrawal addresses that publication, rather than creating a new command identity on every attempt. |
| Actual resource release | Original `Lifetime` and resource entry | A cancellation, timeout, cleanup diagnostic or lost acknowledgement cannot substitute for release evidence. A later proof advances the same exit. |
| Local cleanup causes | Lifetime records the causes; callers retain the `CloseCauses` observation | Error chains are available locally while an observer retains them. Terminal `CloseOutcome` is a bounded diagnostic projection, not another owner of arbitrary plugin error objects. |
| The consumer lifetime of browser root Uses | The Realm application's Lifetime | The provider execution does not own a borrow of its own exports. Original execution, scope, Realm and proof identities remain unchanged; delegated Uses still belong to their parent Use. |
| Reserved and retained browser history bytes | Original SQLite credential and receipt rows | Generated `history_bytes` derives each charge from the actual payload or pending envelope. Transactions decide admission from the rows; there is no independently mutable quota counter. |
| The request entitled to a receipt reservation | The original receipt row's request fingerprint | The fingerprint covers control kind, Realm and original request digest. Different Bridges cannot share a pending slot merely because their reply bounds have equal lengths. |
| The supported typed JSON representation | `value.ValidateType`, declared fields and the domain's explicit custom codec/model | Schema derivation and typed decoding consume the same admission rules. Schema annotations may narrow values, but cannot silently replace a field's encoding. |
| Portable string-format semantics | Frame's admitted portable format predicates and shared conformance corpus | The Go compiler receives those predicates; the Node adapter implements the same profile. Agreement between two permissive engine defaults is insufficient evidence of RFC conformance. |
| A conversion's application intent | The `ConversionTask` created before dispatch | Snapshot and acquisition requests, uncertain acknowledgements, the admitted Use and the eventual result belong to one task. The application retains unsettled tasks through shutdown. |
| Progress of content-store shutdown | The original `distribution.Store` close operation | Individual waiters may cancel. The Store still joins accepted I/O and leases and releases its original directory authority once they finish. |
| Sealed content permissions | Named policies inside `distribution` | Build, staging, sealing, reading and recovery use the same appropriate policy. Directory modes, file modes and executable bits retain distinct meanings. |

An index, receipt, generated DTO or diagnostic view can repeat immutable facts.
It cannot originate a competing admission, publication, release or business
result transition.

## Finding-to-change and regression mapping

| Audit ID | Change | Regression evidence |
| --- | --- | --- |
| **FRAME-001** | The MCP client explicitly disables the pinned SDK's automatic multi-round retry middleware. An input-required response returns through the original invocation's outcome path; it does not authorize another dispatch. | `process/mcp/replay_test.go`: `TestInputRequiredCannotReplayAnInvocation` uses the real SDK and counts remote tool dispatches. |
| **FRAME-002** | `UnitBinding` owns canonical import ordering and semantic equality. Capture merging and existing-instance validation no longer compare raw, differently ordered JSON. | `binding_identity_test.go`: `TestCanonicalBindingRetainsSharedProviderIdentity` combines a live graph with a graph restored through canonical JSON and `AcquireRecord`. Existing binding/conformance tests continue to check actual provider differences. |
| **FRAME-003** | Typed schema derivation and decoding share type/field admission. Integer conversion preserves exact numeric value across decimal and exponent spellings, with range checking. Unrepresentable null declarations and incomplete custom encodings fail construction. | `process/member_test.go`, `value/integer_test.go`, `value/codec_test.go`, `value/type_test.go` and `portable/type_admission_test.go` cover equivalent integer spellings, overflow, opaque null, omission, narrowing tags and explicit codec models. |
| **FRAME-004** | Typed command result decoding preserves both `process.ErrOutcomeUnknown` and the original error after dispatch. A decoding failure does not imply rollback or permit automatic command replay. | `process/member_test.go`: `TestTypedCommandDecodeFailureKeepsUnknownOutcome` checks the post-dispatch error category and the single original effect. |
| **FRAME-005** | Bridge publication projects the Host's synchronous refusal as a terminal rejection. Withdrawal retries address the original publication. Ready plan-validation errors retain their causes instead of masquerading as terminal rejection. | `browser/publication_test.go` covers publication refusal during recovery, cancelled withdrawal retry and Ready recovery under the same report. `sdk/src/browser/runtime.test.ts` checks publication identity, refusal/uncertainty handling and unchanged withdrawal targets. |
| **FRAME-006** | `PreparePublication` fixes one native request. `Publish` requires that request for an operation replay, so changing only the implementation no longer succeeds as if it were the original request. | `publication_scope_test.go` checks changed implementation, changed metadata, a separately prepared equal request, cross-Host authority, withdrawal/republication and grant revalidation after preparation. |
| **FRAME-007** | Store shutdown runs once under the Store's ownership, independently of callers waiting on it. | `distribution/owner_test.go`: `TestCloseJoinsAcceptedIOAndLeasesAfterItsOnlyWaiterCancels` has no second Close waiter; final lease release must complete shutdown and permit a successor Store. |
| **FRAME-008** | Use admission follows the existing resource-owner handles and instance provider graph to reject direct and indirect retirement cycles. Browser application Uses have a real Realm consumer owner, separate from their provider execution. | `use_owner_test.go` covers direct, provider and nested owner cycles plus valid ownership. `browser/ownership_test.go` covers request-independent Realm lifetime and provider protection until original execution proof. |
| **FRAME-009** | `Acquired` observes the original Lifetime's release from construction. Parent close and late confirmation clear the same payload reference without requiring a separate handle close and without starting cleanup merely to subscribe. | `sdk/src/use.test.ts` retains the handle while GC verifies that parent- and handle-initiated late release no longer retain the payload. Terminal borrowing remains refused. |
| **FRAME-010** | `createConversion` returns an owned task before network dispatch. `run` shares the original in-flight admission and retries the original Snapshot/acquisition identities; `close` resolves uncertain acquisition without running conversion. Application shutdown retains and closes unfinished tasks. | `examples/authoring/browser/workflow.test.mjs` exercises lost Snapshot and acquisition acknowledgements against the real Go Host, closing an uncertain task, single conversion and application shutdown. SDK acquisition regressions reside in `sdk/src/browser/runtime.test.ts`. |
| **FRAME-011** | Frame supplies the Go format predicates explicitly, and Node requires exactly two unsigned decimal digits for each clock component. The shared corpus rejects signed fields while preserving the profile's offset, fractional-second and leap-second rules. | `portable/testdata/schema-profile.json` is consumed by `portable/profile_test.go` and `sdk/src/node/profile.test.ts`; generated-profile tests check that the declaration and projection agree. |
| **FRAME-012** | Explicit local cause observation preserves `errors.Is`/`errors.As`; terminal replay reports bounded evidence and stable close categories. Parent observations reuse original cause identities, and truncated diagnostics detach their backing strings. | `close_causes_test.go` covers local cause preservation, late proof, discarded-observer GC, Host propagation and eight nested Uses without multiplying the original cause. Existing lifetime/retention tests cover release ordering and terminal owner retention. |
| **FRAME-013** | A separate retained-history byte quota admits the maximum receipt before new responsibility is accepted. Completion consumes its reservation and returns unused bytes. Pending reservations bind the original request fingerprint, including Realm. | `sqlite/browser_receipt_test.go` covers reservation settlement, conflicting identity, quota sharing, replay and epoch rotation. `browser/history_capacity_test.go` checks refusal before effects plus replay/cleanup at capacity. `browser/receipt_identity_test.go` checks two Bridges competing for the same Snapshot ID without a stranded lease. |
| **FRAME-014** | `distribution/permissions.go` owns named directory/file policies and executable bits. The corresponding build, seal, validation and recovery paths consume those declarations. | Existing distribution archive, sealed-content, cached-reimport and directory-owner tests verify unchanged behavior; this is a policy-consolidation change, not a new permission model. |
| **FRAME-015** | Internal comments that merely narrated `publishUse`, related Use plumbing and proof binding are removed or replaced by specific ownership/concurrency reasons. Necessary public contracts remain. | Source review and formatting/static checks. No behavioral test is invented for comment removal; affected lifecycle and proof behavior remains covered by the tests above. |

## Additional integration invariants

### A reservation belongs to the request, not its byte count

The pending SQLite receipt row fixes both the request fingerprint and its
maximum reply size before capture, selection consumption, activation or
publication. A conflicting kind, Realm or request is refused before those
effects, even when two Bridge instances share one Store and would reserve the
same number of bytes. Completed bytes can only confirm that original row.

This matters independently of one Bridge's in-memory operation map. The
cross-Bridge regression uses `MaxPending = 1` on each Bridge, requires exactly
one successful capture, closes the winner, then verifies that both Bridges can
capture again. A conflict after capture must not be hidden as a harmless refusal
while leaving the losing Bridge's only lease slot occupied.

### Realm sealing and late proof join the original exit

`SealRealm` initiates close of the Realm application's Lifetime and fences its
executions; it does not turn sealing into a blocking proof of release. A later
proof confirms the original browser resource, waits for that same Use exit to
reach release, and joins its original durable settlement. An earlier
unconfirmed close observation cannot be reused as the final answer after new
release evidence arrives. Execution proof still releases only Uses covered by
that execution's original identity.

### An unknown acknowledgement remains attached to its original operation

A lost response can follow successful admission. A later refusal to observe or
retry that request does not prove that the earlier admission never happened.
The Runtime, ReadySubmission and ConversionTask retain the original pending identity and its
cleanup responsibility until the original operation's outcome or release is
established. A confirmed refusal before any uncertain dispatch may return its
provisional capacity. This distinction prevents both permanent refusal leaks
and erasure of a potentially accepted Use after an acknowledgement was lost.

## Breaking migration

### Native publication

Prepare one request and retain it for every replay of that operation:

```go
request, err := host.PreparePublication(ctx, instanceID, export)
if err != nil {
    return err
}
publication, err := host.Publish(ctx, operationID, request)
```

The former `Publish(ctx, instanceID, operationID, ExportValue)` call is removed.
Do not prepare a new request to replay an existing operation. A new operation
may reuse a prepared request after withdrawal, but receives a new membership.
Preparation grants no lasting permission: Publish revalidates the original
instance authority after potentially slow metadata work.

### Browser withdrawal

`Withdrawal.OperationID` is removed from Go and the generated TypeScript
protocol. Send the authority and original publication ID. Retry or observe the
same publication after refusal or uncertainty. Its actual exit still requires
the original publication proof; withdrawal acknowledgement alone does not prove
that native objects have stopped being used.

### Conversion task intent

Use the authoring example's synchronous `client.createConversion(reference)`
to obtain a task before dispatch. Call `task.run()` to perform or observe that
task's work, and `task.close()` to resolve and close it. Retry an uncertain run
or close on the same task. Creating another task creates another application
intent; it is not recovery of the lost acknowledgement. A task closing an
uncertain acquisition must not invoke the Converter just to obtain a handle.

### Typed wire declarations

The supported typed set is intentionally narrower. Non-opaque pointer fields
represent omission, rather than explicit JSON null. Ordinary `float32` and
`float64`, byte sequences and `time.Time` are refused by automatic typed schema
admission where their codecs cannot consume the complete advertised value set.
Use `value.Number` for exact numbers, explicitly constrained strings for textual
representations, and an opaque JSON field for explicit null or unrestricted JSON.

A custom encoding requires both directions of its codec and a value-receiver
`JSONSchemaModel` owned by that type. Schema tags may narrow constraints or
requiredness; they cannot replace a typed field's representation. Map keys must
use ordinary string encoding: custom key codecs are refused because automatic
map schemas do not project their restrictions onto property names. Plain and
unmodified named string keys remain supported. For an
explicit descriptor contract, use `portable.AdmitMethod` and its explicit
schemas. Constructors now reject unsupported declarations instead of publishing
contracts that fail only when a conforming caller reaches the handler.

### Local close errors and terminal observations

Retain `lifetime.ObserveCloseCauses()` before asynchronous shutdown when original
causes must remain available across waits or late proof. Inspect its `Err()`
alongside the original close result. First local `Lifetime.Close`, `Use.Close`
and `Host.Close` waits preserve their observed causes automatically.

`Close.Wait` and `Close.WaitReleased` replay structured `CloseOutcome` with stable
`ErrCloseFailed` and `ErrUnconfirmedRelease` categories. They do not reconstruct
error chains from diagnostic strings. Keeping a `CloseCauses` observation or a
returned local error deliberately retains its original error payloads; dropping
them permits collection while terminal reports remain available. Observations
created after unretained failures cannot recover discarded causes. Release and
cleanup failure remain separate facts.

### Fresh development database

The receipt schema now includes the admitted request digest, reservation and
actual-reply representation, and generated history-byte projections. Open a
fresh development database for this schema. There is no migration or fallback
reader for the audit baseline. Within the new schema, an ordinary authority
epoch rotation removes old epoch-local control receipts while preserving Realm
verifiers needed to authenticate outstanding cleanup.

### Retained-history backpressure

`sqlite.Config.MaxRealmHistoryBytes` defaults to **256 MiB**. It counts retained
credential fields, operation identities, request digests, completed reply bytes
and the full reserved reply envelopes of pending operations. It is a payload
quota, **not a physical SQLite database, page, WAL or filesystem size limit**.
Application tables and execution-protection evidence have their own owners and
are not included in this number.

New responsibility is refused with `frame.ErrCapacity` before its effect when
that history cannot fit. Already admitted completion, original receipt reads
and established cleanup paths do not require another history reservation. No
TTL, quota eviction or restart is used as evidence of resource exit. Capacity
planning must account for credentials that remain necessary for old execution
cleanup.

## Recorded integration results

These checks were run against the combined implementation on 2026-11-03. The
patch header identifies the committed snapshot; its parent is the audited
baseline above. No failing test was disabled, skipped or weakened to obtain a
pass. Unix-domain socket restrictions are environment failures and remain an
uncompleted integration gate.

| Gate | Final result and evidence |
| --- | --- |
| Tested revision and toolchains | Combined patch based on `b8b6e022f94202292fc6285088f7fc711bd775fa`; Go 1.27.3 on linux/amd64, Node 24.22.1, npm 11.9.0. |
| Go race suite and vet | `make check` reached the race suite: 12 packages with tests passed; `cmd/frame-runner`, `process`, `process/mcp` and `process/runner` cannot finish their runner-dependent tests because Unix sockets return `operation not permitted`. The final browser change additionally passed the entire `go test -race ./browser` suite. `go vet ./...` passed. |
| Changed protocol regressions | `go test -race -count=1 -run '^(TestTyped\|TestInputRequiredCannotReplayAnInvocation)' ./process ./process/mcp` passed. These use the real pinned SDK over `net.Pipe` and verify dispatch count and post-dispatch outcomes without substituting another process transport. |
| npm check and packed SDK consumption | Final `npm run check` passed: 12 test files and 229 tests, workspace types, lint, formatting, and independent browser/Node/React consumers of the installed SDK tarball. |
| Generation drift | `go generate ./...` passed and left all four tracked generated projections unchanged. |
| Independent Go consumer | `python3 scripts/go-consumer.py --version v0.0.1` passed against the committed implementation through a temporary standard module proxy, without `replace` or workspace links. The subsequent amendment only records these verification results. |
| Process/interoperability example | `make example` built the SDK, then the document-indexer runner failed to create its Unix socket. The separately invoked `go test -tags frame_interop -count=1 ./conformance` hit the same restriction. Neither integration is recorded as passed. |
| Real-Host workflows and Chromium | Final `scripts/check-browser-workflows.sh` passed: authoring 11 tests, document HTTP 3 tests, Chromium 1 test covering 4 scenarios. Chromium 156.0.8078.4 exercised native preview, lost proof ACK retry, Host SIGKILL with original-Realm cleanup and durable release, and fresh approval after original exit. |
| Patch whitespace and applicability | `git diff --cached --check` and `git apply --check --whitespace=error-all` passed. Applying the exported patch to a clean baseline produced exactly the committed Git tree, including new files and generated protocol changes. |

The optional external Flame checkout suite was not run. Native runner and
`frame_interop` integration still require execution in an environment that permits
the repository's Unix-domain sockets. A successful browser workflow does not
claim coverage of those process-runner paths.
