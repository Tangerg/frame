# Changelog

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
