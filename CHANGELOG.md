# Changelog

## Unreleased

- **Breaking:** a Host has one `host.Config.Authority`. An `Admission` is a
  volatile authority; a `host.Durable` such as `sqlite.Store` also owns the
  creation epoch, installation persistence and execution protection, and the
  Host derives the gate and epoch from it. `Config.Admission`, `Config.Epoch`,
  `Config.Installations`, `Config.ExecutionProtection`, `sqlite.Store.IsAuthority`,
  `host.DurableAuthority` and `UsesAuthority` are removed; adapters compare
  `Host.Durable()` or `Setup.Durable()` with their configured Store.
- **Breaking:** `host.ImportDeclaration` names only a slot, contract and
  optionality. A unit fixes exact providers with `UnitDeclaration.Fixed`
  (`SlotProviders`); bindings hold resolved `host.ImportBinding` values. A nil
  provider list no longer means "unresolved", and `distribution.Import` and the
  authoring fixture's `AuthorImport` are replaced by `host.ImportDeclaration`.
- **Breaking:** `BindingSnapshot` drops `FormatVersion` and replaces the
  string `Rule` with the typed `Selection` digest. Snapshot identity uses one
  JSON encoder, so nil and empty arrays share a spelling.
- **Breaking:** `InstallationRecord` no longer repeats the release's unit
  declarations; the restored `Plugin` owns them. `Host.DeclaredExports` lists
  the exports enabled installations declare for a contract.
- **Breaking:** move `frame.Target` to `mcp.Target`, its only consumer.
  `frame.ResolveLimit` returns the resolved bound instead of writing through a
  pointer. Root errors use the `frame:` prefix and Host errors `host:`.
- **Breaking:** runner exit proofs use one `runner-exit` kind; the receipt's
  state distinguishes exit from refusal. The runner journal drops its
  hand-maintained `version` member.
- Add `frame.Method.Streaming`. `frame.ContextError` returns the bare category
  when a context carries no distinct cause.
- `distribution.Open` defaults each zero limit independently; narrowing one
  limit no longer refuses the store.
- **Breaking:** every issued capability is a value handle with one unissued
  zero state reported by `Valid`: `frame.Work` and `ResourceHandle`;
  `host.Use`, `Setup`, `Access`, `Watch`, `RunningUnit`, `EventRoute` and
  `PublicationRequest`; `sqlite.Database`; `distribution.Lease` and
  `ResourceDriver`. Pointer-to-handle signatures (`*host.Setup`, `*frame.Work`,
  `*host.Use` and the rest) take and return values; `UseConfig.Parent` is a
  `Use` whose zero value means no parent.
- **Breaking:** exit proofs no longer consume `sqlite.Config.MaxRequestBytes`.
  Proof fields are bounded names and evidence is bounded by
  `sqlite.MaxExitEvidenceBytes`, so any valid proof is storable. The probing
  methods `CheckExitCapacity`, `CheckExitEvidenceCapacity` and
  `CheckRealmUseExitCapacity` are removed.
- Binding plans are a closed pair of declared and captured variants; a
  publication's metadata is either its admitted wire document or a detached
  native snapshot. Neither carries fields meaningful only to the other.
- Typed decoders use strict JSON v2 (case-sensitive members, no duplicates or
  trailing data), and the SQLite store encodes persisted rows with the same
  codec it decodes them with.
- SDK: `ConnectionState` names the browser Runtime's connection lifecycle,
  `Release` is one shape over `ReleaseState`, and the Node SDK refuses a profile
  keyword topology it cannot interpret instead of ignoring it.
- **Breaking:** move plugin Host, declaration, binding, selection, admission and
  authoring APIs to `github.com/Tangerg/frame/host`. Construct a Host with
  `host.New(ctx, host.Config{})`; Host budgets use `host.Limits`. The root
  package owns callable contract semantics and resource lifetimes. No aliases
  or forwarding facade remain at the module root.
- **Breaking:** Lifetime, Host and Admission are value handles over private
  owners. Copying a handle shares the original resources, catalog, generation
  sequence and scope/recovery authority; replacing a caller's variable cannot
  replace any of those facts. Invalid constructors return unissued handles.
- Give each Host an issuing lifecycle domain and inspect resource dependency
  identities without exposing owning Lifetimes or retaining a parallel ledger.
- Replace release-settlement report counters and channel-derived policy with
  explicit states and observed failure identities. An overlapping close cannot
  authorize a retry of a failure it did not observe.
- **Breaking:** remove the `portable` package. Root `frame` owns capabilities,
  methods, invocations and typed calls; `value` owns schema and codec admission.
  Move `portable/contractgen` to `contractgen`. Move process-neutral service
  interfaces, implementations and `ErrOutcomeUnknown` to `frame`; replace
  `process.NewMember` and `process.Member` with `frame.NewTypedMethod` and
  `frame.TypedMethod`. No compatibility aliases are retained.
- Share one identifier grammar through `frame.ValidName`. The generated portable
  profile derives its token bound from `frame.MaxNameBytes`; callable limits
  belong to Frame and data validation budgets belong to `value`.
- Refuse a zero, unadmitted capability before assembling a typed service.
- Admit known Frame capability semantics at contract construction and descriptor
  registration. Reject mismatched references and invalid methods before setup.
  Retain the admitted model in the contract registry and expose it through
  `Host.Capability` and `Setup.Capability`, replacing adapter-side descriptor
  lookup and reparsing. Point and service lookups use the same value-contract owner.
- Share cancellation reporting through `frame.ContextError` and typed-nil
  classification through `value.IsNil`, including native author sources and
  codec model admission.
- **Breaking:** move example commands to `cmd/authorhost`,
  `cmd/buildcheck` and `cmd/document`. The document command
  replaces the separate Web generator with `-command generate -out <path>`.
- Reject invalid document commands before opening state. `run` and `status`
  require the original task identity. Each invocation owns its flags and shares
  one cancellation context across task execution and server shutdown.
- Publish repository protocol projections through atomic file replacement,
  preserving previous hard links and refusing symbolic-link targets.

## v0.2.0 — 2026-11-10

- Protect native resource work through `Use.Enter` and retain dynamic publication
  callbacks in the original instance lifetime. Reserve cleanup before acquisition.
- Conclude private installation receipts on every unsuccessful exit and require
  recovery when retained submission evidence is missing or incomplete.
- Sync final release permissions and nested directory entries before issuing
  artifact leases. Validate operation-specific runner requests before dispatch.
- Observe asynchronous SDK callback failures without manufacturing release proof,
  capture browser selection data once, and repair generated type and enum edges.
- Require `github.com/Tangerg/go-sdk` v1.9.0-scope.1. MCP sessions bound retained
  incoming calls and notifications until their handlers and response writes
  finish, and close on overload. Negotiated protocol versions reach the original
  transport through the tool-result connection. Caller cancellation retains its
  error category when connection shutdown races with it.
- Reject dynamic Use ownership cycles using the original resource and provider
  edges. Caller-owned EventRoute handles no longer hold the Host's mutable
  release identity; copies share the same opaque authorization.
- **Breaking:** a Host-created Setup or Use lifetime cannot own another Host's
  Use or serve as another Host's lifetime. Its dependency graph belongs to its
  original admission authority. Application-created lifetimes can still own
  work across multiple Hosts.
- Keep portable metadata as one admitted immutable wire value. Typed reads
  decode detached projections through the original codec model. **Breaking:**
  `Candidate.Metadata` now returns `(M, error)`; callers must handle decoding
  failure before ordering or using metadata. Native metadata encoders receive
  detached values and cannot mutate published state.
- Resolve typed value admission, nullability and generated schemas from the
  same codec model. Refuse incomplete codecs, private-only unencodable structs,
  lossy nullable `omitzero` declarations and a second `jsonschema:"nullable"`
  declaration. Keep explicit `omitempty` semantics.
- Close browser publication interruptions, transient reconnect failures,
  oversized route admission and uncertain Snapshot receipt reservations through
  their original owners. A lost reservation acknowledgement no longer prevents
  closing that Snapshot identity.
- Capture SDK option values and callbacks once, retain their original method
  receivers, and capture React object identities at binding. Node error
  diagnostics cannot replace a stream's first failure or prevent Peer closure.
- Bound directory enumeration before allocation and keep MCP discovery inside
  the original page budget. Preserve cancellation categories and causes across
  process, distribution and example boundaries.
- Keep the document preview's original Snapshot until confirmed consumption or
  closure; bound author-client diagnostics without retaining thrown objects.
  Buildcheck records a non-returning analyzer as failure. The native author
  sources compile in an independent module using only public dependencies.
- **Breaking:** replace `runner.Dial` with `runner.NewClient(socket)` returning
  `(*Client, error)`. Construction validates the static socket path; operations
  still validate the current authority and establish their own connection.
  Internal unit instances are no longer exported as `frame.Instance`.
- **Breaking:** runner launch returns `LaunchResult`, preserving the original
  invocation's nondispatch evidence separately from the runner's observation.
  Durable `NotCreated` receipts settle definite refusals without consuming an
  active-attempt slot. `Exited` still requires the original child Wait. Receipt
  admission shares the observation state rules, including custom Runner results.
  Control responses use operation-specific result variants and explicit true
  acknowledgments. Runner journal version 3 requires fresh development state;
  no old-protocol adapter or persisted-state migration is included.

- Preserve original ownership across interrupted installation persistence,
  browser Snapshot capture, copied Go handles, and uncertain preview requests.
  Installation callback interruption requires recovery; an incomplete Snapshot
  cannot be replayed as a successful capture.
- Scoped `StartUnit` handles require the instance's original start grant.
  Installation-scoped service providers remain shareable, but an eager
  installation activation cannot become a scoped root by acquiring a new handle.
- Capture use-owner identities and SDK declarations once at admission. Node
  method admission returns the captured input, output or chunk that was actually
  validated, and callers transmit that value. The old void-returning method
  validators are removed.
- Keep Lifetime failure diagnostics bounded even when inspecting a thrown value
  fails. Browser execution and dynamic publication lifecycles have concrete
  owners instead of externally advanced mutable records.
- Canonicalize empty binding unit roots, preserve exact wide-integer values in
  generated TypeScript, and reject conflicting generated type names. Typed local
  process calls reject cancellation before dispatching handler effects.

- Add `mcp.NewTransport` and `mcp.Transport` so a custom `process.Protocol` can
  connect its own MCP client through the frame admission and write
  cancellation that `mcp.Tools` and `mcp.ManagedProtocol` use. The transport
  rejects a missing channel or a non-positive frame budget with `frame.ErrInvalid`.
- `@frame/sdk/browser` accepts a control endpoint or module lease URL only when
  it is potentially trustworthy: HTTPS or HTTP to a loopback host, plus `file:`
  and `data:` for module leases. Plain HTTP to any other host is refused, since
  it would expose the realm credential or let the network replace module code
  that the approved digest cannot verify.
- Cancel a queued MCP write without sending it. Canceling an active write closes
  the connection, so `process/mcp` no longer wraps SDK connections to provide
  that itself. Frame admission, strict JSON and the outbound frame budget are
  unchanged.
- `make check` and CI run golangci-lint with `.golangci.yml`: errcheck, govet,
  ineffassign, staticcheck and unused. An ignored write, sync or commit error now
  fails the build; closing a read handle and test cleanup follow the
  std-error-handling convention.

### Upgrade from v0.1.1

- Upgrade the Go module, SDK tarball, Host and runner together, and regenerate
  application contracts and browser control declarations.
- Handle the error returned by `Candidate.Metadata`. Replace `runner.Dial` with
  `runner.NewClient(socket)` and handle its construction error. Use the returned
  `LaunchResult` to distinguish definite refusal from execution observations.
- Implement `ExecutionProtection.SettleUse` in place of `ReleaseUse`; settlement
  must prove release or non-commit after the original approval callback ends.
- Accept the `sqlite.Owner` value returned by `Store.Owner`, move installation
  projections to `OwnerSchema.ProjectInstallation`, and use `*sqlite.Database`
  rather than `*sql.DB` with the standalone transaction helpers.
- Remove `ExportValue.Durable`. Drivers with a remote Go representation implement
  `ExportValidator` against the original instance and export identity. The
  internal `frame.Instance` aggregate is no longer exported.
- Use `Use.Enter` before native work that retains a Use's resources; entering its
  Lifetime alone does not validate the Use's original grant.
- The private Host schema and runner journal require fresh state. Stop existing
  executions through their original owners before switching state directories.
  No automatic persisted-state migration or old-protocol adapter is included.
  See [SQLite](sqlite/README.md) and [runner](process/runner/README.md) contracts.

## v0.1.1 — 2026-11-05

- Dispose the original SQLite connection when BEGIN is not acknowledged, so
  cancellation before BEGIN cannot introduce a spurious rollback failure.
- Preserve both cancellation and the caller's original cause when transaction
  admission fails. A BEGIN that ran before its acknowledgement was lost is
  settled by closing that same connection before further operations can proceed.
- Keep existing commit uncertainty, rollback failure and retained connection
  cleanup guarantees. There are no storage or wire format changes from v0.1.0.

## v0.1.0 — 2026-11-05

Frame v0.1.0 consolidates the TypeScript SDK and strengthens exact binding,
resource retirement and durable settlement across native, process and browser
execution. It contains breaking changes from v0.0.1.

### Added and improved

- One `@frame/sdk` artifact with native, browser, Node, React, lifetime, exact JSON
  and protocol entry points.
- Optional MCP adapters for approved standard tools and managed plugin contracts,
  carrying the original command identity and preserving uncertain outcomes.
- Original-owner cleanup across delegated Uses, preparation, streams, callbacks,
  runner attachments and late exit evidence. Work handle copies complete the same
  admitted interval.
- Durable receipt reservations, encoded proof capacity admission and integrity
  checks before protected state can advance.
- Exact typed JSON codecs and generated Go/TypeScript projections, including
  custom codec models, independent schema scopes and recursive references.
- Generated executable browser response validation, deterministic control JSON,
  bounded immutable Node schema capture and shared portable format semantics.
- Cached archive path, permission and executable validation, plus retained runner
  shutdown that advances its original process after storage recovers.

### Upgrade from v0.0.1

- Upgrade the Host, SDK, runner and plugin implementations together. The private
  runner attachment protocol, process invocation values and durable proof formats
  have changed.
- Replace `@frame/browser`, `@frame/node` and `@frame/react` imports with
  `@frame/sdk/browser`, `@frame/sdk/node` and `@frame/sdk/react`. Install the single
  release tarball in place of the four v0.0.1 tarballs.
- Move Go imports from `control` and `control/host` to `browser`, `runner` to
  `process/runner`, `contractgen` to `contractgen`, and `integrity` and
  `jsonvalue` to `value` under `github.com/Tangerg/frame`.
- Process services accept `frame.Invocation`. Use `NewQueryInvocation` for
  queries and `NewCommandInvocation` with the original business command ID for
  commands; typed members provide `Query` and `Command` calls.
- Prepare a native publication once with `Host.PreparePublication` and retain that
  request for `Host.Publish` retries. Browser withdrawal addresses its original
  publication identity.
- Regenerate contracts and control declarations. Typed JSON admission rejects
  representations that cannot consume their advertised schema; use `value.Number`
  for exact numbers and explicit codec models for custom encodings.
- Use fresh development databases and runner state for the changed private
  formats. Automatic database migration is not provided. See the
  [architecture](https://github.com/Tangerg/frame/blob/v0.1.0/docs/architecture.md)
  and [contract changes](https://github.com/Tangerg/frame/blob/v0.1.0/docs/audit-2026-11-04.md#intentional-contract-changes).

### Install

```sh
go get github.com/Tangerg/frame@v0.1.0
go install github.com/Tangerg/frame/cmd/frame@v0.1.0
go install github.com/Tangerg/frame/cmd/frame-runner@v0.1.0
```

Download `frame-sdk-0.1.0.tgz` from this release, then run:

```sh
npm install ./frame-sdk-0.1.0.tgz
```

The Go module requires Go 1.27.3 or newer. SDK development and Node execution
require Node 24 or newer; the optional React entry targets React 19. Managed
runner execution requires a supported Unix filesystem and local sockets.
