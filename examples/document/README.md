# Document workbench consumer

The workbench owns document revisions, conversion
selection, task states, result persistence and authentication. Frame owns exact
binding admission, implementation lifetimes, durable submission evidence, the
browser execution boundary and immutable release content.

The native file service is imported once into the Markdown converter's setup.
The consumer captures one converter and an ordered validator collection in one
Snapshot. `CommitTask` validates that capture under the same Admission and SQLite
transaction that creates the original queued task. The business table retains
its complete binding while queued or running, so replacement is refused even
when no callback is executing. Starting and finishing update the original task row under admission and never allocate a second submission. The owner compacts synchronized, unretained submission evidence when its admission budget fills. The task row remains observable by its original ID.

Completion removes the business reference while
preserving the original task and its submission conclusion. Each original run
belongs to the application Lifetime through result persistence and Use release,
so shutdown cannot close SQLite after the final callback but before settlement.

The same application installs a neutral ZIP release containing a browser preview
Unit and a static-resource theme Unit. Browser modules are verified against their
approved content digests before evaluation. The preview receives native DOM
objects within its own JavaScript Realm; its actual iframe belongs to the
accepted Use. Theme reads return detached immutable bytes through the Go static
resource driver. No universal JSON invocation emulates a native DOM contract.

## Run the CLI

From the Frame module:

```sh
mkdir -p /tmp/frame-documents
printf '# Hello\nAn original document.\n' > /tmp/frame-documents/hello.md
go run ./examples/document/cmd/document \
  -documents /tmp/frame-documents -state /tmp/frame-document-state \
  -document hello.md -task original-task -command convert
```

`-command queue` commits a waiting task. `-command run -task original-task` runs
that original task, and `-command status` observes it. Repeating a completed task
returns its original result without invoking the converter again. A source file
whose digest changed after queuing is rejected. A task found in `running` after
Host reconstruction is not silently replayed; explicit cancellation can release
that old application's business reference.

## Run the Web consumer

Generate its DTO projection from the Go application types, then build the Web
entry from the repository root:

```sh
npm ci
npm run build
go run ./examples/document/cmd/webgen -out examples/document/web/src/api.generated.ts
npm run build --workspace @frame/example-document-web
go run ./examples/document/cmd/document -command serve \
  -documents /tmp/frame-documents -state /tmp/frame-document-state \
  -web examples/document/web/dist
```

The default listener is `127.0.0.1:8088`. The server creates a mode-0600
`application-token` file in its state directory and never logs its contents.
Enter that token in the workbench, connect the preview, and convert a document.
The application token authorizes business commands and Realm bootstrap; the
separate Realm credential authorizes only its original Frame control session.

Use **Release preview** before replacing the page. This seals that Realm in the
Go Host, closes its original JavaScript resources, and waits for durable proof
acknowledgement. Disconnects, a new window and module cache deletion are never
treated as evidence that an old execution stopped. A surviving page can use
**Reconnect Host** to submit old proofs after Host reconstruction; that old
Realm is limited to cleanup.

Once release is acknowledged, **Connect preview** is disabled and **Reload page**
becomes available. Reload goes through the original Runtime's release and proof
gate before replacing the actual page. A closed page cannot manufacture another
Realm or module cache in the same JavaScript environment. Enter the application
token again after reloading to obtain a fresh approval.

The example trusts only the exact archive assembled from assets embedded in its
executable. Its HTTP API does not accept arbitrary package installations. An
application that accepts external packages must supply its own trust policy.

## Verification

```sh
go test -race ./examples/document ./browser
go build -o /tmp/frame-document ./examples/document/cmd/document
npm run build --workspace @frame/example-document-web
FRAME_DOCUMENT_BINARY=/tmp/frame-document npm run test:workflow --workspace @frame/example-document-web
FRAME_DOCUMENT_BINARY=/tmp/frame-document \
  FRAME_CHROME_EXECUTABLE=/path/to/chrome npm run test:browser --workspace @frame/example-document-web
```

The workflow tests start the real Go HTTP process and exercise the actual
JavaScript Runtime, module evaluation, DOM resource, SQLite task and release
proofs. The DOM workflow uses Happy DOM, so it is not browser-engine or
browser-unload coverage. The second workflow publishes a dynamic native object,
selects it through the real Host, verifies original object identity within its
Use, and closes its execution without a separate withdrawal. It loses the Host's
committed proof acknowledgement and verifies that retry sends the same proof
without repeating cleanup.

`test:browser` uses Playwright with an actual Chromium engine. Set
`FRAME_CHROME_EXECUTABLE` to an existing executable, or omit it to use a browser
installed for Playwright. The test verifies a real iframe and stable original
result, committed-proof acknowledgement loss, a killed and reconstructed Go Host
with cleanup-only reconnection, and an actual page reload before a new Realm is
approved. Optional `FRAME_BROWSER_EVIDENCE=/path/to/evidence.json` writes the
observed engine version and scenario results. Neither suite treats a closed page
or a disconnected transport as exit evidence.

The Go tests separately verify retained bindings across Host reconstruction,
source revision rejection, preparation/disable races, stable original results
and the static-resource driver.
