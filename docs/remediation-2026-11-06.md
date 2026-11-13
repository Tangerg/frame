# Ownership boundary remediation — 2026-11-06

This change addresses the findings against
`c96c33be19d26a966940454008ecd59ae82c366c`. The F01–F13 identifiers below
belong to that audit, not to earlier remediation documents. Existing owners and
composition boundaries are retained where they already express the contract.
Incorrect interfaces are replaced together with their in-repository consumers.

## Facts and original owners

| Fact | Owner that may advance it | Other representations |
| --- | --- | --- |
| Durable installation outcome | Original InstallationStore transition, coordinated by Host | Host installation projection and Admission recovery barrier |
| Registered business identity | Admission registration | Retention ordering, diagnostics and retained-dependency owner IDs |
| Authority of a running root | Instance's original start grant | Binding roots and RunningUnit handles |
| One start or content claim | Private startClaim or leaseClaim | Public Go handle copies and store/instance protection counts |
| Accepted Node payload | Method admission's captured value | Handler input, result, stream chunk and encoded frame |
| Admitted SDK declaration | Constructor's validated captured definition | Contract references, import slots and unit metadata |
| Snapshot operation result | Original Bridge control operation | Live capture lease, receipt and replay response |
| Uncertain preview capture | PreviewSnapshots' retained request | Retried HTTP request and UI rendering |
| Resource exit | Original Lifetime resource | Bounded diagnostics and eventual exit evidence |
| Binding identity | BindingSnapshot canonical encoding | Public JSON and persisted binding digest |
| Target-language declaration | Browser TypeScript emitter | Named definitions, field references and response-decoder signatures |

An index, encoded value or immutable projection may be duplicated. None receives
a competing grant, release, commit or retry transition.

## Correctness changes and regression coverage

| Audit | Root correction | Regression coverage |
| --- | --- | --- |
| F01 | Installation commit and confirmation callbacks must return normally before their outcome is trusted. Panic and goroutine exit establish recovery before admission is released, preserve the interruption and still start an already accepted retirement. | `host/installation_interruption_test.go`: install/disable × commit/confirm × panic/Goexit, with live instances; `sqlite/submission_admission_test.go`: rollback, receipt capacity and explicit Host recovery |
| F02 | Node method admission returns its captured and validated input, output or chunk. Dispatch and encoding use that returned value. | `sdk/src/node/payload_capture.test.ts`: real in-memory stdio frames and single getter reads |
| F03 | A scoped root must match its Instance's original start grant. Incompatible eager roots are refused within activation admission before new preparation. Shared installation providers remain reusable. | `host/running_scope_test.go`: eager/scoped contrast, in-progress preparation, revocation, independent consumers and shared providers |
| F04 | Snapshot capture starts with an interrupted failure outcome; only completed capture and response construction can publish success. Replays require a completed lease. | `browser/snapshot_interruption_test.go`: panic/Goexit, repeated refusal, identity conflict and released capacity |
| F05 | Admission reads each owner ID once, validates it and retains that identity. Sorting and reports never call external identity code under its state mutex. | `host/use_owner_identity_test.go`: reentrant identity reads, later name mutation, ordering and getter counts |
| F06 | Public RunningUnit and Lease values contain pointers to original private claims. Copies close one claim; independent acquisitions retain separate protection. | `host/running_ownership_test.go`, `distribution/lease_test.go`: concurrent copies, independent clients and new/cached publication paths |
| F07 | SDK constructors capture before validation and retain that captured declaration, including optional import wrappers. Capability owns handler-table capture, shape and callability admission; setup retains its returned table. | `sdk/src/contracts.test.ts`, `sdk/src/unit.test.ts`, `sdk/src/node/index.test.ts` |
| F08 | Lifetime diagnostics and publication refusal classification cannot interrupt settlement or lose the original operation handle when inspecting an exception fails. Publication acknowledgements are captured before validation. | `sdk/src/lifetime_failure.test.ts`, `sdk/src/browser/control.test.ts`, `sdk/src/browser/publication.test.ts`, `sdk/src/browser/runtime.test.ts`: hostile proxies/accessors, late release, retained publication handles and ACK capture |
| F09 | Canonical binding JSON uses one empty UnitRoots representation. Public JSON round trips preserve identity; unresolved providers retain their independent semantics. | `host/binding_identity_test.go` |
| F10 | Wide Go integers use the SDK's existing exact JSONNumber representation. Bounded 8/16/32-bit integers retain number. | `browser/typescript_test.go`: compiled TypeScript consumers and exact-number round trips |
| F11 | One target-name function handles Go generic and target-language reserved names for declarations and references. Distinct Go types cannot claim the same emitted name. | `browser/typescript_test.go`: compiled nested/generic consumers, reserved names, legal namespace sharing and collision refusal before output |
| F12 | Preview capture retains the same request across every failed attempt and coalesces concurrent observers. Only success allows a later operation ID. | `examples/document/web/preview-capture.test.mjs`: actual TypeScript owner, unknown-then-refused sequence and concurrent callers |
| F13 | Typed process callers and handlers check cancellation before encoding/decoding and again before dispatch. Cancellation after effects still preserves a successful result or the original unknown-outcome failure. | `process/member_cancellation_test.go`: local/direct paths, custom codecs and dispatch boundary |

The F06 tests now copy only pointer-backed handles; they do not disable the Go
copylock check. The F13 contract is a predispatch guarantee at these adapters,
not a claim that context cancellation universally prevents all Go side effects.

## Structural cleanup

Browser execution resources and dynamic publication lifecycles are represented
by concrete behavior-owning objects. Runtime retains realm/channel coordination
and cross-execution indexes. Preparation, resource release, publication withdrawal
and exit proof continue to compose the existing Lifetime, receipt and proof
mechanisms; no generic manager or inheritance hierarchy is introduced.
Regressions cover competing slow admissions for one instance, channel replacement
during publication observation, terminal-handle garbage collection and original
proof delivery. A late withdrawal acknowledgement after execution release creates
no new resource or proof obligation; the execution proof already covers it.

The numeric emitter reuses the existing exact JSON number representation.
The browser Go-type emitter and portable JSON-Schema emitter keep their distinct
input policies. They are not combined into a generator abstraction that owns
neither source contract.

Example response and module byte limits have local semantic names. Redundant
operation-narrating comments and the superseded void-validation call path are
removed. Workspace unit tests execute the preview request owner directly and are
included in the root npm test gate.

## Intentional contract changes

- A previously activated installation-scoped root cannot be adopted by
  `StartUnit`. Continue using its shared activation semantics, or retire it
  before starting under an explicit scope grant. No running grant is overwritten.
- The digest of a binding whose old canonical representation used null for empty
  UnitRoots changes. Development state should be recreated where it stores the
  old digest. No alternate digest lookup or migration adapter is added.
- Registered use-owner identity is fixed at registration. Mutating an external
  owner object's ID does not rename an admitted authority.
- The Node Method interface exposes `admitInput`, `admitOutput`,
  `admitChunk` and `admitInvocation`, returning the value callers must use.
  The former void-returning validators and all in-repository calls are removed.
- `Capability.implement` returns a new frozen handler table after validating
  method names and callability. It no longer freezes or returns the caller's
  original table. The ServiceContract consumer interface includes this admission
  method so preparation can use the same owner.
- SDK constructors retain the documented fields they admit. Optional import
  wrappers and captured limits cannot later change through their source objects.
- `Failure.category` is a closed union of known ErrorCode values, `exception`
  and `thrown_value`; arbitrary strings or mutable exception data do not enter
  long-lived exit reports. Uninspectable control errors cannot prove refusal.
- A SQLite projection panic can complete rollback and release its private
  receipt while still requiring Host recovery: the Host did not receive a normal
  commit acknowledgement. Restore and confirm the original store records before
  clearing that barrier. Accepted retirement still starts after confirmation
  interruption; an unknown commit never invents a retirement decision.
- Generated TypeScript can use JSONNumber for wide integer fields and new legal
  names for generic or reserved Go names. Regenerate consuming declarations and
  use `projectNumber` only when a finite safe JavaScript number is required.
- A failed preview Snapshot attempt keeps its original operation identity until
  capture succeeds. A later refusal cannot disprove an earlier accepted request.
- Copies of RunningUnit and Lease share a single release claim; independent calls
  to StartUnit or Acquire still create independent claims.

There are no compatibility aliases, obsolete-path fallbacks, database migration
adapters or new runtime dependencies in this change.

## Verification

Use the Go version in go.mod, golangci-lint v2.14.0 and Node 24 or newer:

```sh
go generate ./...
go test -race ./...
go vet ./...
golangci-lint run ./...
npm run check
./scripts/check-browser-workflows.sh
go build ./cmd/frame ./cmd/frame-runner
python3 scripts/go-consumer.py
```

Compare generated tracked files before and after generation. The npm check
builds, typechecks, lints, checks formatting, executes SDK and workspace unit
tests, then installs and tests the SDK tarball in an independent consumer.
The Go consumer reads the committed source tree and builds an independent host
without a workspace or replace link.

The complete Go race suite requires Unix domain sockets for managed runner
tests. The real HTTP workflows require loopback networking. Browser-engine crash
and reconnect evidence additionally requires a Chromium executable passed through
FRAME_CHROME_EXECUTABLE. A missing environment capability is a blocked check,
not a successful test or a reason to disable its assertions.
