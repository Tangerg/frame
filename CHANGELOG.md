# Changelog

## Unreleased

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
- Require `github.com/Tangerg/go-sdk` v1.8.1-scope.5. The SDK now cancels a
  queued MCP write without sending it and closes the connection when an active
  write is canceled, so `process/mcp` no longer wraps SDK connections to provide
  that itself. Frame admission, strict JSON and the outbound frame budget are
  unchanged.
- `make check` and CI run golangci-lint with `.golangci.yml`: errcheck, govet,
  ineffassign, staticcheck and unused. An ignored write, sync or commit error now
  fails the build; closing a read handle and test cleanup follow the
  std-error-handling convention.

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
  `process/runner`, `contractgen` to `portable/contractgen`, and `integrity` and
  `jsonvalue` to `value` under `github.com/Tangerg/frame`.
- Process services accept `portable.Invocation`. Use `NewQueryInvocation` for
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
