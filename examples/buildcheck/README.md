# Build and check consumer

This native Go consumer owns
source revisions and the meaning of four independent Points:

- Compiler passes run in an explicit phase order.
- Analyzers run concurrently, then join before a result returns. Required
  failures cancel the batch; optional failures remain diagnostics.
- One formatter is selected by consumer priority.
- Around functions receive a once-only continuation over the captured remainder.

All four Points are captured in one Frame Snapshot. Host.With opens one borrowed Access; each typed selection reads source, metadata and value together during its callback. The
application's `Source.Apply` compares the original revision before writeback.
Frame protects implementation lifetimes; it does not own document revisions or
decide whether a formatter's patch should be applied.

From the Frame module:

```sh
printf '  TODO example  \n' | go run ./cmd/buildcheck -apply
go test -race ./examples/buildcheck
```

The behavioral tests exercise the full Host path, parallel analysis, stale R1
writeback after R2 arrives, required and optional failures, and a continuation
which is called twice or retained after its original invocation.
