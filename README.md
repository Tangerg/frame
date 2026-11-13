# Frame · 帧

Frame is a general-purpose plugin framework for hosts that need exact service bindings and predictable resource lifetimes. A plugin declares its imports and exports. Its host chooses providers and grants access. Frame freezes those choices and protects each implementation while it is in use.

## Try it

Use the Go toolchain specified in [go.mod](go.mod). For Node workspaces use Node 24 or newer and npm. The native build/check example needs only Go.

```sh
printf '  TODO example  \n' | go run ./cmd/buildcheck -apply
```

It composes compiler passes, analyzers, a formatter and decorators under one snapshot. The host decides their order. To run a real zero-export Node plugin with two host-owned imports, use Linux or macOS:

```sh
npm ci
npm run build
go run ./examples/document-indexer
```

The indexer receives a document-scoped reader and writer, starts only after activation and stops through its original instance. The example uses a real child process, local runner, SQLite evidence and a temporary working directory.

## Where to start

| Role | Code | Contract |
| --- | --- | --- |
| Native plugin author | [Typed importer and exports](examples/authoring/native/plugins.go) | Declare slots once, obtain the approved values during setup, bind each implementation once. |
| Host author | [Build/check engine](examples/buildcheck/engine.go) | Select from a common snapshot, capture the choices, borrow members within one protected access. |
| Process author | [Node unit](examples/document-indexer/index.mjs) and [Go host](examples/document-indexer/main.go) | Local duplex stdio supports exact session handles, cancellation and backpressured streaming. |
| MCP host integrator | [Standard tools and managed plugins](process/mcp/README.md) | Choose a protocol at composition and bind verified peer exports to already approved services. |
| Browser and React author | [Browser exercise](examples/authoring/browser) · [React adapter](sdk/src/react/README.md) | Native objects remain in their original realm and are held for the duration of their use. |
| Distribution or operations | [Archive format](distribution/README.md) · [Runner](process/runner/README.md) · [SQLite](sqlite/README.md) | Content identity, installation authority, real process exit and business settlement have separate owners. |

## Install

Install the Go module in an application:

```sh
go get github.com/Tangerg/frame@v0.2.0
```

Install the command-line tools when needed:

```sh
go install github.com/Tangerg/frame/cmd/frame@v0.2.0
go install github.com/Tangerg/frame/cmd/frame-runner@v0.2.0
```

Download `frame-sdk-0.2.0.tgz` from the [v0.2.0 release](https://github.com/Tangerg/frame/releases/tag/v0.2.0)
and install it in the consuming application:

```sh
npm install ./frame-sdk-0.2.0.tgz
```

The tarball includes the native, browser, Node and React entries. The SDK is
distributed through GitHub release assets. See the [SDK guide](sdk/README.md)
for import names and optional dependencies, and [release notes](CHANGELOG.md)
for the breaking changes since v0.0.1.

## Repository

| Responsibility | Physical owner |
| --- | --- |
| Callable contract semantics and structured resource lifetimes | Root Go package `frame` |
| Plugin installation, contracts, selection, exact binding and admission | `host/` |
| JSON/schema admission, Go codec models and SHA-256 identities | `value/` |
| Published contract projections for Go and TypeScript | `contractgen/` |
| Managed local execution | `process/` for admission of an attached session, `process/runner/` for child ownership, `process/mcp/` for optional MCP interoperability |
| Browser execution and its control messages | `browser/` for the Go bridge and DTOs, `sdk/src/browser/` for the browser runtime |
| Artifact distribution and durable evidence | `distribution/` and `sqlite/` |
| TypeScript consumption | `sdk/`, published as `@frame/sdk` with subpath imports |
| Executables | `cmd/frame` for archives and contracts, `cmd/frame-runner` for direct child supervision, and `cmd/authorhost`, `cmd/buildcheck`, `cmd/document` for the examples |

[Architecture and migration](docs/architecture.md) · [Original design proposal (PDF)](docs/Frame-Framework-Proposal-ZH.pdf) · [Original proposal source](docs/Frame-Framework-Proposal-ZH.md) · [Development and verification](DEVELOPMENT.md) · [Code rules](AGENTS.md)
