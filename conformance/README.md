# Conformance

Frame's executable checks live beside their semantic owners. Each case below names its observable boundary and the command that executes it. Protocol values are defined in process/wire.json; the document indexer shares its domain descriptors between both languages.

| Scenario | Public path | Verification |
| --- | --- | --- |
| Same snapshot, several points, typed metadata and scoped values | Build/check host | `go test ./examples/buildcheck` |
| Optional absence vs mismatch; no activation on invalid grant | Host authoring and admission | `go test . ./examples/authoring/native` |
| Real zero-export process receiving two exact imports | Go host, Node backend, SQLite, real Runner | `npm run build && go run ./examples/document-indexer` |
| External archive profile, offline inspection and fail-closed installation | Distribution and CLI | `go test ./distribution ./cmd/frame` |
| Identical epoch in different authorities; retirement, stale plans and proof replay | Durable browser/SQLite | `go test ./browser ./sqlite` |
| Exit handoff before and after original acknowledgement; streaming and cancellation | Managed process and local Runner | `go test -race ./process ./process/runner` |
| Independent ESM/type entry points and resource ownership | One SDK tarball tested with a minimal install and with Node/React dependencies | `npm run check` |

After building the SDK, run `go test -tags frame_interop -count=1 ./conformance`.
The Node relay streams a Go import back through the original Go export call. A separate
request propagates cancellation across both directions, then probes the still-live session
and checks exactly one durable execution with its original exit handoff settled.
