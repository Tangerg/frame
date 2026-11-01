# Frame · 帧

Frame is a general-purpose plugin framework for hosts that need exact service bindings and predictable resource lifetimes. A plugin declares its imports and exports. Its host chooses providers and grants access. Frame freezes those choices and protects each implementation while it is in use.

## Try it

Use the Go toolchain specified in [go.mod](go.mod). For Node workspaces use Node 24 or newer and npm. The native build/check example needs only Go.

```sh
printf '  TODO example  \n' | go run ./examples/buildcheck/cmd/buildcheck -apply
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
| Browser and React author | [Browser exercise](examples/authoring/browser) · [React adapter](packages/react) | Native objects remain in their original realm and are held for the duration of their use. |
| Distribution or operations | [Archive format](distribution/README.md) · [Runner](runner/README.md) · [SQLite](sqlite/README.md) | Content identity, installation authority, real process exit and business settlement have separate owners. |

## Install

Install the Go module at the released version:

```sh
go get github.com/Tangerg/frame@v0.0.1
```

The [v0.0.1 GitHub release](https://github.com/Tangerg/frame/releases/tag/v0.0.1) includes installable tarballs for `@frame/sdk`, `@frame/browser`, `@frame/node` and `@frame/react`. Install the packages you need together from their downloaded tarballs. These artifacts are distributed through GitHub Releases; they have not been published to the npm registry.

## Repository

The root is the Go module `github.com/Tangerg/frame`. The private npm workspace builds four public package entry points, `@frame/sdk`, `@frame/browser`, `@frame/node` and `@frame/react`.

- `portable`, `jsonvalue`, `integrity` and `contractgen` own portable values, exact digests and projections of published schemas.
- `process`, `protocol/process` and `runner` implement managed local plugins; the Go author API and Node SDK use the same wire profile.
- `distribution`, `sqlite` and `control/host` supply optional content, durable evidence and browser-host integration.
- `cmd/frame` packages and checks archives. `cmd/frame-runner` serves local process ownership. `examples` contains complete consumers.

[Architecture and user guide](docs/architecture.md) · [Frame design proposal (PDF)](docs/Frame-Framework-Proposal-ZH.pdf) · [Editable proposal](docs/Frame-Framework-Proposal-ZH.md) · [Development and verification](DEVELOPMENT.md) · [Code rules](AGENTS.md)
