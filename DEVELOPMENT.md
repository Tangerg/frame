# Development

Use the version in go.mod with GOWORK=off. Use Node 24+ with npm and the checked-in lockfile. Install and build from the repository root. The managed local runner and exclusive file-store profiles require a supported Unix filesystem and local sockets.

```sh
npm ci
make check
make example
```

The Go race suite uses temporary SQLite files, real subprocesses and loopback sockets. The JavaScript check typechecks the workspace, exercises runtime/React behavior and installs and tests the SDK tarball. The example crosses Go and Node through the real local driver. Run `go generate ./...` and `npm run build` after changing a wire declaration or contract owner. Generation drift is checked in CI.

To release, verify both binaries and the single SDK tarball, then choose a new release version for the Go module and npm SDK. Publish an annotated Go tag and a GitHub Release containing the SDK tarball, design proposal and SHA-256 checksums. The maintainer controls names, credentials, license and publisher policy. Protocol versions and domain contract identities are independent of npm or Go module versions. Publishing is a separate, explicit operation.

Run `make hooks` to enable local whitespace and conventional commit checks. Go formatting, vet, races and npm lint, formatting, types, tests and packed-package consumption are checked independently in CI.

`scripts/check-browser-workflows.sh` builds the Go hosts and runs the browser author and document workflows against real local servers. Set `FRAME_CHROME_EXECUTABLE` to a Chromium executable to include real browser crash and reconnect scenarios. `go test -tags frame_interop -count=1 ./conformance` runs the two-way streaming and cancellation exercise after the SDK has been built.

For a committed tree, `python3 scripts/go-consumer.py --version v0.1.1` creates a temporary standard Go module proxy and compiles a separate consumer without a replace or workspace link. `--proxy <directory>` retains the module for local distribution. Verify a public release separately through the public Go module proxy.

To exercise published Flame contracts and unmodified plugin binaries, run
`node scripts/check-flame-interop.mjs /absolute/path/to/flame` from Frame. This
optional suite prepares Flame’s pinned candidate modules using its own tooling, builds its peers in the supplied full Git checkout, then tests the
adapter from the Frame module. It checks the managed descriptor corpus, the
hooks backend, the trajectory renderer and the Qimu stream fixture. It writes
its outputs to a temporary directory. See [managed MCP](process/mcp/MANAGED.md)
for permissions and callback ownership.
