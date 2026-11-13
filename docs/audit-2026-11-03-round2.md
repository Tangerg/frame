# Second audit and remediation, 2026-11-03

## Baseline and scope

This report covers `Tangerg/frame` at commit
`73fb11c13475922d3270811bfa44d7d51a9417c9`, tree
`a00daf2db9e9f117600541db9c5b3b7e47e1b5ee`. The repository and current main
revision were checked through the GitHub connector. The implementation is an
isolated local change based on that exact tree.

The earlier [remediation report](remediation-2026-11-03.md) belongs to the
previous baseline. Its FRAME-001 through FRAME-015 are not counted again here.
This audit confirms **12 additional correctness defects**, five P1 and seven P2,
and one consolidation of a duplicated policy value. All confirmed findings
below have implementation changes and relevant regression coverage.

P1 means a reachable ownership, premature-cleanup or business-outcome defect
with material effects. P2 means an incorrect admission, identity, schema or
boundary behavior with a narrower trigger. These are audit priorities, not CVSS
scores or a claim that every finding is exploitable.

The review covered the root Host, Admission, binding, Use, Lifetime and Watch
models; process, runner and MCP adapters; browser bridge and SDK; typed values
and portable contracts; SQLite and distribution; and the examples, executables,
generation and external-consumer workflows. The baseline contains approximately
65,000 source/script lines including tests. Reviews followed AGENTS.md,
PROJECT_RULES.md and DEVELOPMENT.md, including original fact ownership,
behavior-rich types, explicit composition and exact API failure semantics.

## Confirmed findings

### FRAME-R2-001 — P1: process preparation was outside its resource Lifetime

`process/author.go`, `server.prepare`, called the unit's Prepare callback without
entering `s.life`. A callback could register a resource and still be using it
when Serve cancellation started that resource's cleanup. A retired Lifetime
could also reach plugin preparation.

Preparation now enters the existing Lifetime, uses the resulting work context
and finishes only after the original callback returns. No preparation counter
or second closing state was added.

Regression: `process/author_lifetime_test.go`,
`TestServeRetirementJoinsPreparationBeforeResourceCleanup` and
`TestPreparationDoesNotRunAfterItsLifetimeRetires`. The first test exercises
actual Serve and peer messages through net.Pipe and synchronizes the blocked
callback with testing/synctest. Cleanup cannot happen before that callback exits.

### FRAME-R2-002 — P1: browser load and remote callbacks had the wrong owner

`sdk/src/browser/runtime.ts` tracked activation under the execution Lifetime,
but `load` and `remote` callbacks use resources registered in a unit Lifetime.
Closing that unit could release those resources while one of these callbacks
was still pending. Keeping only the outer execution busy did not protect the
inner owner.

Both callbacks now run through that same unit's `owner.run`. Existing revision
and open-state checks still reject late completion after retirement.

Regression: `sdk/src/browser/runtime.test.ts`, the two cases of
`joins the unit's pending load/remote callback before releasing its resources`.
They require the original callback to exit before resource cleanup and release
proof, and prevent late completion from entering setup.

### FRAME-R2-003 — P1: Node imported stream consumers escaped unit shutdown

`sdk/src/node/index.ts` tracked an imported invocation under the optional caller
Lifetime, but did not retain the plugin unit Lifetime while a local stream
consumer was running. Unit resources could be cleaned before its pending
onChunk callback returned.

The imported call, including stream consumption, now runs through the original
unit Lifetime as well as the existing call owner. The stream registry remains
an index for cancellation; it does not become another completion authority.

Regression: `sdk/src/node/index.test.ts`,
`joins an imported stream's local consumer before unit cleanup`, using two real
Peers and their hello, preparation, activation and streaming paths.

### FRAME-R2-004 — P1: local typed failures lost post-dispatch uncertainty

`process/member.go` returned handler failures and output-encoding failures
without `ErrOutcomeUnknown`. An implementation could apply its effect and then
return an error that looked like an ordinary refusal. The remote typed path
already preserved uncertainty after dispatch; the local path did not.

Errors after calling the implementation now join `ErrOutcomeUnknown` with the
original cause. Input decoding still happens before dispatch and does not gain
an unknown-outcome category. No retry or fallback dispatch was introduced.

Regression: `typed_method_test.go`,
`TestTypedLocalCommandKeepsPostDispatchFailureUnknown` covers cancellation,
contract mismatch and output encoding, while counting the original effect.
`TestTypedInputDecodeFailureRemainsBeforeDispatch` checks the other side of the
boundary.

### FRAME-R2-005 — P1: document shutdown could discard final task settlement

`examples/document/tasks.go`, `App.Run`, protected its Borrow calls but not the
complete business operation. Result encoding, final persistence and readback
could outlive the last Borrow. Shutdown could close SQLite while final
settlement was waiting for admission, leaving the durable task in `running`
after its callback had completed.

Run now enters the App's original Lifetime for the entire operation, including
final settlement and deferred Use release. The business owner retains that
responsibility until it has actually persisted the result.

Regression: `examples/document/task_lifetime_test.go`,
`TestCloseJoinsTaskSettlementBeforeClosingItsStore`. It blocks final admission,
initiates shutdown, requires an unconfirmed close while settlement is pending,
then checks the original completed result in a reopened SQLite connection.

### FRAME-R2-006 — P2: Host.closed competed with the original Lifetime

The Host had an independently advanced `closed` boolean. Closing an injected
Lifetime or cancelling its parent did not advance that flag. The Host could
still admit declarations, plans, activation, snapshots and Watches after its
original resource owner had retired. An existing Watch could remain open after
that Lifetime reported release.

The duplicate field was removed. All former Host close gates now query the
original Lifetime through a derived, uncached query. Host.Close begins that
same close before withdrawing instances. NewHost rejects a retired Lifetime,
and cleans up an internally created Lifetime if later construction fails.

Each Watch now registers one resource in that Lifetime. Its original close
path removes the subscription, closes the channel and then confirms release.
Normal close, context cancellation and overflow return the same resource and
subscription capacities. There is no separate Watch shutdown registry.

Regression: `host/host_lifecycle_test.go`,
`TestHostAdmissionEndsWithItsOriginalLifetime`,
`TestHostWatchResourceBudgetTracksActualSubscriptions` and
`TestNewHostRefusesRetiredLifetime`. An independent review also checked lock
ordering, callbacks outside Lifetime locks, construction failures and every
former `h.closed` gate.

### FRAME-R2-007 — P2: Ready could relabel prepared object metadata

`browser/bridge.go`, Bridge.Ready, checked duplicate export names but not
duplicate ObjectIDs. It also reduced contribution ownership to ObjectID,
discarding the rest of ObjectRef. Two exports could share one identity;
metadata with a relabelled reference or no corresponding object could pass.

Ready now checks unique object identity and exact ObjectRef equality for every
metadata contribution, including its Point kind. Temporary validation indexes
do not persist or advance another object identity.

Regression: `browser/ready_identity_test.go`,
`TestReadyPreservesEachObjectAndItsExactMetadataOwner`. All three invalid cases
are rejected, and the valid original report can still complete preparation.

### FRAME-R2-008 — P2: typed schema admission accepted an unallocatable field

`value/type.go`, declaredFields, admitted an anonymous pointer embedding whose
field was unexported. Schema derivation advertised its flattened fields, but a
fresh decode target could not allocate the private embedded pointer.

The shared field-admission owner now rejects that declaration before publishing
its schema. Unexported value embeddings and exported pointer embeddings remain
supported. No preinitialized-target workaround or alternative schema was added.

Regression: `value/embedding_test.go`,
`TestUnallocatableEmbeddedPointerIsRejectedBeforePublishingItsSchema` and
`TestAllocatableEmbeddedFieldsShareSchemaAndCodec`; they cover constructor
refusal, unchanged decode destinations and supported round trips.

### FRAME-R2-009 — P2: generated root aliases broke descendant references

`contractgen/feature.go`, addFeatureSchema, shared an expanded root's
shape with a named definition by replacing the root with a $ref. References
such as `#/properties/value` were still relocated beneath the now-aliased root.
A JSON Pointer does not follow $ref when traversing the document, so those
addresses no longer existed and generation failed for valid contracts.

Definition projection now participates in selecting the one canonical shape
owner. If the expanded root becomes an alias, all original root-descendant
references in its method document are projected to the actual named shape.
No duplicate generated type or compatibility alias was introduced.

Regression: `contractgen/root_reference_test.go`,
`TestExpandedRootKeepsAddressedSubschemas`. It compares original and generated
schema predicates, executes the generated Go consumer, and compiles positive
and negative TypeScript consumers.

### FRAME-R2-010 — P2: readable definition names broke encoded references

`value/schema_names.go` recognized only the literal prefix `#/$defs/`.
Legal references with URI-fragment encoding, such as `#/%24defs/...`, kept
their old names while the referenced definitions were renamed.

Reference projection now decodes the URI fragment, decodes JSON Pointer tokens,
uses the existing rename map, then encodes the new token and fragment. Only
schema reference positions are rewritten; annotation and literal data remain
data. References whose target is not renamed retain their original spelling.

Regression: `value/schema_names_test.go`,
`TestReadableDefinitionsPreserveEncodedPointerFragments`.

### FRAME-R2-011 — P2: a legal read bound could silently discard all input

`distribution/store.go`, readBounded, passed `limit+1` to io.LimitReader. With
`math.MaxInt64`, that addition wrapped to a negative number. Reading `abc`
returned empty bytes with no read error, corrupting the subsequent integrity
decision for an otherwise legal positive configuration.

The reader now consumes the original limit and, only at that boundary, probes
one additional byte separately. It preserves the complete positive int64 range
and the original context and read-error behavior.

Regression: `distribution/read_bound_test.go`,
`TestReadBoundedAdmitsTheFullPositiveLimitRange`, including zero, negative,
exact-length, excess-byte and MaxInt64 cases.

### FRAME-R2-012 — P2: SQLite inspection depended on PRAGMA spelling

`sqlite/statement_policy.go` compared the authorizer's original PRAGMA spelling
with lowercase vocabulary. Valid `PRAGMA TABLE_INFO(frame_meta)` and mixed-case
forms were denied even though their lowercase form was permitted.

The existing pragmaName policy now rejects non-ASCII names and folds ASCII
case before consulting the same closed inspection-only vocabulary. Unicode
case aliases and connection-changing PRAGMAs remain denied.

Regression: `sqlite/pragma_case_test.go`,
`TestReadScopeKeepsSQLitePragmaCaseInsensitivity`, including uppercase and mixed
case inspection, denied mutation, and the Kelvin-sign alias `foreign_Key_list`.

### FRAME-R2-Q1 — one owner for managed callback token size

`process/mcp/managed_grant.go` now declares managedCallbackTokenBytes once.
Callback generation allocates and splits its random bytes using that policy;
credential validation derives the hexadecimal length with hex.EncodedLen.
The independent 32/64 literals no longer define the same security parameter.
This is policy consolidation, not an additional reproduced runtime defect.

## Ownership and design conclusions

| Fact or guarantee | Owner after the fixes | Other representations |
| --- | --- | --- |
| Host closing and admission fence | Original Host Lifetime | Host methods query it; no independent Host.closed |
| Active preparation, loading and imported consumption | Existing unit Lifetime | Work handles retain that owner until the callback returns |
| Actual Watch completion | Watch's one close path and its original resource entry | Host index and Lifetime budget reflect that completion |
| Business task completion | Original App.Run and durable task transition | Callback return and UI state do not settle the task |
| Prepared object identity | Exact ObjectRef admitted by Ready | Name and ObjectID indexes only validate the same reference |
| Typed JSON field representability | value.declaredFields admission | Schema and decoding use the admitted field model |
| Schema addresses and generated shapes | Original schema document and its projection | Readable names and Go/TypeScript output preserve that meaning |
| Inspection-only PRAGMA policy | pragmaName.inspectionOnly | Authorizer input is normalized before classification |
| Callback credential entropy size | managedCallbackTokenBytes | Buffer, slices and wire length are derived |

The changes reuse existing Lifetime, Work, resource and call capabilities.
They do not introduce a second manager, optional service bag, generic lifecycle
framework, inheritance hierarchy or compatibility path. Data-only wire,
configuration, storage and snapshot types remain data where they own no mutable
invariant. Turning every such type into a service would add indirection without
assigning a real guarantee.

Indexes and immutable projections may repeat a fact. They are not competing
owners unless they can originate an independent transition. Temporary maps for
ObjectRef lookup, schema name projection and graph indexing are justified by
those semantics; they are not anonymous domain state controlled by magic keys.
Comments added here explain only non-obvious ownership, representation or
overflow constraints. Naming and ordinary control flow express the operations.

The repository's use of Scope's generic JSON Schema reflector does not itself
introduce an agent-product policy owner. The portable profile still owns
admission and cross-language predicate requirements. No speculative dependency
replacement or unsupported performance claim is part of this patch.

## Intentional behavior changes

- A retired or parent-cancelled Host refuses new work through the original
  Lifetime, even when Host.Close was not the caller that initiated retirement.
- A live Watch consumes one entry of the Host Lifetime's MaxResources budget
  as well as MaxWatches. Closing it returns both. Capacity planning should include
  subscriptions alongside other actual resources.
- Shutdown waits for the original callback, stream consumer or task settlement.
  A wait deadline can return unconfirmed release while the original work remains
  owned; it cannot manufacture successful cleanup.
- Local typed post-dispatch failures include ErrOutcomeUnknown while preserving
  their original causes. Callers must observe the original operation rather than
  infer rollback from an ordinary error category.
- Ready rejects duplicate identities, relabelled metadata and orphan metadata.
- Automatic typed contracts reject an unexported embedded pointer that cannot
  decode a fresh value. Export that embedding, use value embedding, or define an
  explicit model and codec at the owning type.

No database migration, compatibility alias or fallback reader was added.
Correct case-insensitive SQLite inspection and correct schema relocation
preserve their original intended contracts.

## Verification record

Checks use Go 1.27.3, Node 24.22.1, npm 11.9.0 and GOWORK=off. Regression probes
reproduced the old behavior on the exact baseline, and the new tests pass with
the fixes. The process preparation test uses net.Pipe, the task test uses real
SQLite, and generated-contract tests compile actual Go and TypeScript consumers.

| Gate | Result |
| --- | --- |
| `go test -race -count=1 ./...` | 12 packages with tests passed. The full command returned failure because runner-dependent tests in cmd/frame-runner, process, process/mcp and process/runner cannot use Unix sockets in this environment. The observed socket error is `operation not permitted`; subprocess readiness failures follow the same unavailable runner boundary. |
| `go vet ./...` | Passed across the module. |
| Go formatting and patch whitespace | All Go files are gofmt-clean; `git diff --check` passed. |
| Process/MCP/runner targeted race checks | Passed on supported boundaries, including actual peer exchanges, typed outcomes, callback behavior and original direct-child Wait evidence. These are not substituted for the blocked socket integration suite. |
| `npm run check` | Passed: workspace types, lint, formatting, 12 test files / 232 tests, and installed SDK tarball consumption by independent minimal browser and full Node/React consumers. |
| `go generate ./...` | Passed, with no changes to tracked generated projections. |
| `scripts/check-browser-workflows.sh` | Passed against real Go Hosts: 11 authoring and 3 document workflow tests. |
| `make example` | SDK build passed; document-indexer failed to create its required Unix socket. Its following conformance step was not reached. |
| `go test -tags frame_interop -count=1 ./conformance` | Separately attempted after the SDK build; blocked by the same Unix socket restriction. |
| Independent Go consumer | Passed against the committed implementation via `python3 scripts/go-consumer.py --version v0.0.1`, using a temporary standard module proxy and no replace or workspace link. A subsequent amendment only records this verification result. |
| Real Chromium workflow | Not run in this audit because no Chromium executable is available. The real-Host Node workflows above are not browser-engine coverage. |
| Optional external Flame checkout integration | Not run; no external application checkout is part of this task. |

The full runner and process conformance gate still needs a supported environment
with Unix-domain sockets. No test was disabled, assertion weakened or production
transport replaced to obtain a passing result. These limits prevent a claim of
an all-green repository gate or universal absence of defects.

The supported process regression set was also rerun against the final combined
implementation and passed. The exact targeted race commands are:

```sh
go test -race ./process -run 'TestTyped|TestCommandIdentity|TestWireCommand|TestServeRetirement|TestPreparation|TestHandlerFailure|TestRejectionBefore|TestStream|TestEscaped|TestDuplex|TestCancellation|TestNegotiated|TestProtocol|TestCloseCannot' -count=1
go test -race ./process/mcp -run '^Test(InboundFrame|UnterminatedInbound|CanceledBlocked|CancelQueued|Managed|Receiver|StandardTool|InputRequired|ToolCannot)' -count=1
go test -race ./process/runner -run '^Test(ControlRequestLimitIncludesLineFeed|RunnerCannotServeAnUnrestrictedNetworkListener|InvalidSocketAddressNeverCreatesOrReconnectsAnAuthority|RunnerRejectsAuthorityDirectoryWithForeignWriters|FailedAttachmentReleasesOriginalStdout|DirectChildWaitWithNoControlTransport|ReservationFailureRetainsOriginalNonCreationEvidence|HandedOffIdentitySurvivesRunnerRestartAndNewEpoch)$' -count=1
```

## Relevant external contracts

- [Go context](https://pkg.go.dev/context): cancellation signals work to stop;
  it does not wait for that work to finish. Frame's owner must join it.
- [Go JSON v2](https://pkg.go.dev/encoding/json/v2): typed encoding and decoding
  rules must agree with the schema offered to callers.
- [SQLite authorization](https://www.sqlite.org/c3ref/set_authorizer.html):
  permission classification occurs through the connection's authorizer during
  statement preparation. Case handling is tested against the pinned driver.
- [JSON Schema 2020-12 core](https://json-schema.org/draft/2020-12/json-schema-core):
  reference evaluation and URI-fragment / JSON Pointer addressing are distinct
  operations; replacing a node with $ref does not preserve descendant addresses.
- [MCP transports](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
  and [tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools):
  the optional adapter must preserve transport, invocation and tool-result
  semantics. The pinned SDK's actual behavior and project regressions remain
  the evidence for its integration in this revision.

This report and its regressions belong to the patch containing them. The parent
commit above is the audit baseline; historical reports remain historical.
