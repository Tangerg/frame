# Audit remediation — 2026-11-07

Baseline: `7ce13dc98fa6037f7583c599450fe5105d7cb20f`.

This change addresses the completed audit's 19 finding groups (A01–A19) and
four P3 cleanup groups. It includes production code, affected consumers,
regressions and contract documentation. It preserves the previous remediation
history. The patch is an intentionally breaking development change; it does not
include compatibility adapters or persisted-state migrations.

## Ownership decisions

| Fact | Authoritative owner | Dependent representations |
| --- | --- | --- |
| Use ownership and held-provider dependencies | Accepted Uses, original ResourceHandles and instances | Cycle detection derives a temporary graph under the original Host admission; it keeps no second graph ledger. |
| Whether the original launch was dispatched | The original invocation's captured LaunchResult | A private request-bound nondispatch proof cannot be recreated from a later failed retry. |
| Whether an attempt was created or exited | The original runner's terminal observation | Journal and receipts persist the same immutable observation and request digest; SQLite advances durable protection only from admitted evidence. |
| Published portable metadata | One admitted immutable wire value | Typed reads decode detached values; encoding, selection and access cannot mutate the publication. |
| Legal typed wire values | The value encoding model and its codec | Schema, type admission, nullability and generated consumers derive their rules from that model. |
| SDK configuration and callback identity | Captured constructor/call inputs and the original capability receiver | Later mutation of the caller's object cannot redirect admitted work. |
| Snapshot cleanup | The original operation and preview lease owner | A response, failed cleanup or reconnect cannot replace the original operation identity. |
| Realm currentness | Original authority and binding evidence | Cancellation and recovery errors remain failed observations, not permanent revocation facts. |
| Route authorization and capacity | The Host's private route identity and resources | Public handles carry opaque identity; rejected wire envelopes never allocate a route slot. |
| Failure and completion | The original operation, stream, analyzer or Peer | Diagnostic projection is not a prerequisite for recording failure or completing shutdown. |

Host-created Setup and Use lifetimes remain inside their Host's admission
authority. A different Host cannot inspect those dependency edges atomically,
so it rejects them as owners. Application-created lifetimes remain usable
across Hosts. No global Host registry or cross-Host lifecycle ledger was added.

Access retains its original Work until all admitted metadata reads finish.
Codecs run outside the interval mutex and may reenter the Access. Closing the
callback seals new reads, joins already admitted readers, then finishes that
same Work. There is no additional work-budget charge or replacement lifetime.

## Finding closure

| ID | Root correction | Regression evidence in this patch |
| --- | --- | --- |
| A01 | Derive cycle detection from actual ownership and provider edges, including accepted dynamic Uses. Reject foreign Host-owned lifetimes at admission. | `host/use_owner_test.go`, `host/use_owner_authority_test.go`: two/three-owner cycles, acyclic retirement, same-Host behavior, external lifetime composition and foreign authority rejection. |
| A02 | Validate and capture the program before recording an attempt. Retain one original LaunchResult. Add durable NotCreated distinct from real-child Exited. Settle definite refusal through the existing receipt owner without consuming an active slot. | `process/launch_settlement_test.go`, `process/runner/launch_result_test.go`, `process/runner/handoff_admission_test.go`: real Host/SQLite/child, capacity refusal, unknown dispatch, partial write, original proof persistence/restart, and invalid terminal shapes. |
| A03 | Initialize publication completion to an interrupted failure before invoking application code; only a normal return records its actual result. | `browser/publication_interruption_test.go`: panic and Goexit preserve the original failed operation for lookup/replay. |
| A04 | Give native custom encoders detached metadata; return portable metadata's admitted bytes directly. | `host/metadata_ownership_test.go`: a mutating encoder cannot change the publication or another read. |
| A05 | Replace the Host's second portable cloning grammar with immutable wire capture and decoding through the original codec. Make decoding failure explicit. | `host/portable_metadata_test.go`, `host/metadata_decode_test.go`, `host/access_codec_test.go`: publication through snapshot, typed/raw reads, selection and Access; pointer isolation, decoder failure, codec reentry and original Work protection. |
| A06 | Capture once at every audited boundary and preserve the original method receiver. Freeze React object identity at binding. | SDK unit, browser Pump/HTTP/Runtime, Node and React tests: accessor counts, prototype methods/private fields, post-admission mutation and default/explicit native fetch controls. All seven audited entrance families are covered. |
| A07 | PreviewSnapshots owns both the original request and its successful lease until confirmed consumption or Close acknowledgment. Retry cleanup through that owner. | `examples/document/web/preview-capture.test.mjs`: unknown capture, refused retry, failed close, original-ID recovery, mismatched reply and concurrent capture. The real document workflow also passes. |
| A08 | Restrict SDK tools/list to the original bounded discovery window. Refuse implicit HeaderMismatch refresh before it reaches the transport. | `process/mcp/catalog_refresh_contract_test.go`: exact scope.5 SDK over net.Pipe; query and command, exhausted and unused page budgets, no hidden refresh/replay, normal cursor controls. |
| A09 | Preserve the I/O error, standard cancellation category and original cause at each audited boundary. Transfer the complete cancellation error between contexts. A completed original result remains authoritative. | Process stream, managed stream, runner close and distribution cancellation contract tests; native author cancellation tests. Includes real child Wait, preflight/write/read timing and late-success controls. |
| A10 | Separate the caller's replaceable EventRoute handle from the Host's private immutable route identity. | `host/event_handle_test.go`: copied handles work, overwriting the original cannot corrupt cleanup, closing either owner invalidates copies and restores the one-slot capacity. |
| A11 | Return currentness separately from verification errors. Seal only on definitive stale/closed/fenced authority evidence. | `browser/reconnect_test.go`: healthy/cancelled/transient recovery/stale controls, including a recovery error containing ErrFenced that is not itself revocation evidence. |
| A12 | Share codec/type/nullability rules. Refuse incomplete codecs, misleading text codecs, private-only unencodable structs and lossy nullable omitzero declarations. | `value/model_test.go`, `contractgen/nullable_consumer_test.go`, browser TypeScript consumers: actual generated Go/TS, root/nested null, absent/present values and negative non-nullable controls. |
| A13 | Bound directory ReadDir before allocation by remaining entries plus one overflow probe. Apply the same inventory owner to import and staging. | `distribution/directory_test.go`: real bounded enumeration, directory and path controls, original source/cache validation. A fixed two-entry budget was also measured against 16 and 16,384 physical entries. |
| A14 | Validate the complete worst-case route response envelope before allocating the Host route. | `browser/route_capacity_test.go`: a refused oversized route does not consume the original one-slot Host capacity; a subsequent legal route succeeds. |
| A15 | Initialize each analyzer outcome as non-returning failure; normal return alone replaces that outcome. Required failure cancels sibling work. | `examples/buildcheck/analyzer_completion_test.go`: required/optional crossed with ordinary error/Goexit. A missing completion cannot form a successful Patch. |
| A16 | Remove the native author's internal-package dependency. The application owns its source-file admission, revision and template policy. Exercise the real copied sources as an external consumer. | `scripts/go-consumer.py` copies all non-test author Go sources from committed HEAD into a separate module, alongside the existing public Host consumer; no replace or workspace is used. |
| A17 | Replace raw unbounded errors with one bounded ConnectionFailures projection. Observer failures enter the same bounded projection and have no shutdown authority. | `examples/authoring/browser/failures.test.mjs`: bounded count/text, hostile diagnostics, synchronous observer throw, rejected Promise and hostile thenable. Actual author workflows retain their original cleanup behavior. |
| A18 | Make Node error classification total, capture one immutable ProcessError and record the stream's first failure before diagnostic work can interfere. Classify once before Peer shutdown. | Node tests use throwing name/code/prototype access and ordinary-error controls. A failed chunk remains failed even if the producer catches its acknowledgment and returns a value. |
| A19 | Preserve the original receipt handle after an accepted reservation whose commit acknowledgment is unknown. Reobserve that identity; if rollback left no reservation, Close claims the same operation ID before acknowledging it. | `sqlite/realm_receipt_integration_test.go`: actual SQLite COMMIT fault with public Snapshot/CloseSnapshot, rollback, committed/lost ACK, temporarily unreadable outcomes, both call orderings and concurrent close. |

The A19 fault injection exists only in test code. It does not add a production
storage seam, alternate receipt registry or retry identity. Store remains the
owner of durable reservation transitions; Bridge retains its original operation
while observation is uncertain.

## P3 cleanup

| Group | Correction |
| --- | --- |
| Constructor naming and admission | Replace non-dialing `runner.Dial` with `runner.NewClient(socket) (*Client, error)`. Validate absolute path, NUL and platform length at construction. Each operation still verifies physical authority and connects independently. |
| Internal model visibility | Rename the non-public implementation aggregate from exported `frame.Instance` to private `unitInstance`; no compatibility alias. Public observation types remain public. |
| Comments | Remove the audited operation-narration comments in cancellation, MCP transport, contract generation and Node formatting, plus the duplicate browser binding comment. Added comments explain ownership, concurrency or failure semantics. |
| Control result states | Replace the optional-field controlResponse with closed result variants admitted by the requested operation. ACK is explicit true. Null, empty success, false ACK, foreign result variants, unknown fields/error codes and contradictory terminal observations are rejected. |

The terminal Observation validation is shared by wire decoding, journal/receipt
admission and the public ExitReceipt projection. A custom Runner cannot bypass
the same rule by returning NotCreated with a PID or Exited without original
Wait fields. Seven invalid shapes and two valid terminal controls exercise
that public boundary.

## Intentional breaking changes

1. `Candidate[M, V].Metadata()` returns `(M, error)`. Decode once before ordering
   or consuming metadata, then use that local projection. In-repository examples
   follow this pattern; sorting callbacks do not run codecs or suppress errors.
2. `runner.NewClient(socket)` replaces `runner.Dial`. Construction can fail.
3. `Runner.Launch` returns `(runner.LaunchResult, error)`. Zero result is unknown,
   not proof of non-creation. Driver never launches again during settlement to
   manufacture newer evidence for the original attempt.
4. `runner.NotCreated` is a terminal state distinct from `runner.Exited`.
   `ExitReceipt.State` participates in the acknowledgment identity. The runner's
   private journal version is 3; old development state is not migrated.
5. Runner control responses use the new operation-specific wire shape. Client
   and runner must use the same source distribution. Old-protocol fallbacks are
   deliberately absent.
6. Host-created lifetimes cannot own another Host's Use or lifetime. Use an
   application-created lifetime for application-owned work across Hosts.
7. `frame.Instance` is no longer exported. It was an internal aggregate, not a
   supported construction boundary.
8. Codec declarations with contradictory or unrepresentable null/omission
   semantics are rejected at admission. Remove redundant
   `jsonschema:"nullable"` declarations and let the actual encoding model own
   nullability. Explicit `omitempty` and valid empty structs remain supported.
9. The author browser example exposes bounded `failures` and `omittedFailures`
   instead of a raw `errors` collection. An optional observer receives the
   original error synchronously; its returned rejection is recorded without
   stopping recovery or becoming a new resource to await on close.

## Verification

The runtime used Go 1.27.3, golangci-lint 2.14.0 and Node 24.22.1. The pinned MCP
dependency remains `github.com/Tangerg/go-sdk v1.8.1-scope.5`; no dependency
upgrade or validation-policy exemption was added.

| Check | Observed result |
| --- | --- |
| Go formatting and whitespace | Passed; `git diff --check` passed. |
| `go generate ./...` | Passed; no generated artifact drift. |
| `go vet ./...` | Passed. |
| `golangci-lint run ./...` | Passed, zero issues. |
| Root, browser, SQLite, value, contractgen, distribution and application race tests | Passed, including the combined final source in the full Go race invocation. |
| A02/control/terminal receipt targeted race tests | Passed with real Host, SQLite, runner, child and net.Pipe control exchange. |
| A08/A09 targeted race tests | Passed: six top-level tests and 32 subcases, including the exact SDK, original child and transport timing boundaries. |
| `npm run check` | Passed: typecheck, lint, formatting, 22 SDK test files / 338 tests, five author diagnostic tests, five preview tests, and installed SDK tarball consumers. |
| `sh scripts/check-browser-workflows.sh` | Passed after the final observer correction: 11 author and three document workflows across Go Host, SQLite, HTTP and actual JavaScript execution. |
| CLI/application Go builds | Passed for frame, frame-runner, authorhost and document. |
| Committed-tree external Go consumer | Passed: a module artifact from committed HEAD, a fresh external module/cache, the public Host test and all copied non-test native author sources. No replace, workspace or checkout import was used. Dependencies came from the existing local module proxy cache. |
| Complete `make check` | Blocked in `go test -race ./...`: this execution environment denies Unix domain socket creation with `operation not permitted`. Remaining independent vet, lint and npm gates were run separately and passed. |
| `make example` | npm build passed; document-indexer stopped at the same Unix socket denial. |
| `go test -tags frame_interop -count=1 ./conformance` | Independently attempted; the Unix socket fixture was denied before the duplex workflow could run. |
| Real browser-engine workflow | Not run: no Chromium executable was available. The successful Node/HTTP workflows are not presented as browser-engine evidence. |

The existing Unix socket, runner daemon and conformance tests remain enabled
with their original assertions. The new net.Pipe and direct-runner regressions
exercise the changed behavior without replacing those integration gates. The
complete runner transport and daemon suite still needs a normal host with Unix
socket support.

The directory allocation measurement held the entry limit at two: 16 physical
entries allocated 17,080 bytes / 136 allocations, and 16,384 physical entries
allocated 33,208 bytes / 137 allocations. Both returned ErrCapacity. These are
observed local measurements showing bounded enumeration behavior, not portable
allocation thresholds asserted by a test.

This record closes the identified audit scope. Passing these checks is not a
proof that arbitrary plugins, codecs or environments contain no other defects.
