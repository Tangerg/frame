# Development

Use the version in go.mod with GOWORK=off. Use Node 24+ with npm and the checked-in lockfile. Install and build from the repository root. The managed local runner and exclusive file-store profiles require a supported Unix filesystem and local sockets.

```sh
npm ci
make check
make example
```

The Go race suite uses temporary SQLite files, real subprocesses and loopback sockets. The JavaScript check typechecks the workspace, exercises runtime/React behavior and tests packages as tarballs. The example crosses Go and Node through the real local driver. Run `go generate ./...` and `npm run build` after changing a wire declaration or contract owner. Generation drift is checked in CI.

To release, verify clean binaries and npm tarballs, then tag the independent Go module and publish the four packages under their respective identities. The maintainer controls names, credentials, license and publisher policy. Protocol versions and domain contract identities are independent of npm or Go module versions. Publishing is a separate, explicit operation.

Run `make hooks` to enable local whitespace and conventional commit checks. Go formatting, vet, races and npm lint, formatting, types, tests and packed-package consumption are checked independently in CI.

`scripts/check-browser-workflows.sh` builds the Go hosts and runs the browser author and document workflows against real local servers. Set `FRAME_CHROME_EXECUTABLE` to a Chromium executable to include real browser crash and reconnect scenarios. `go test -tags frame_interop -count=1 ./conformance` runs the two-way streaming and cancellation exercise after the npm packages have been built.

For a committed tree, `python3 scripts/go-consumer.py --version v0.1.0` creates a temporary standard Go module proxy and compiles a separate consumer without a replace or workspace link. `--proxy <directory>` retains the unpublished module for local distribution. The public registry is not used to pretend that an unpublished Frame version already exists.
