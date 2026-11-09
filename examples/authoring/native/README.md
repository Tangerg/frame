# Authoring a native document plugin

Run this example's end-to-end author workflow from the repository root:

```sh
GOWORK=off go test -race ./examples/authoring/native
```

The plugin in [plugins.go](plugins.go) uses contracts published by the
[documents host](../../document/contracts.go). Its three units provide file
access, conversion, and validation. Each unit declares a typed token for every
import or export and uses that token during preparation:

```go
files := frame.Required("files", contracts.Files)
upper := frame.Offer("uppercase", contracts.Converters)
unit, err := frame.DefineUnit(frame.UnitSpec{
    ID: "converter", Imports: frame.Imports(files), Exports: frame.Exports(upper),
    Prepare: func(setup *frame.Setup) error {
        reader, err := files.Get(setup)
        if err != nil {
            return err
        }
        return upper.Bind(setup, document.ConverterMetadata{Format: "text"}, makeConverter(reader))
    },
})
```

The snippet leaves the application's converter factory to the author. The
complete example loads and validates a template, acquires its temporary
workspace with `AcquireResource`, and returns the actual converter. Frame
reserves cleanup capacity before invoking the opener and publishes the staged
value only after final admission.

The [workbench](workflow.go) selects a converter and validators from a single
Point snapshot. Their implementations remain protected while a document view is
open. The workbench owns source revisions, file limits, validation policy and
the resulting report. The behavioral tests cover cancellation, cleanup after
partial preparation, changing sources, and retirement with an outstanding view.

Each view reader calls `Use.Enter` before accessing the file. The returned work
keeps the original providers alive and checks the original scope even before an
asynchronous fence notification closes the Use. File cleanup is reserved before
creation; a previously removed temporary path does not invalidate a completed
file close.

The non-test Go files can be copied together into an independent author module.
They import only public Frame packages. The author owns its regular-file and
byte-limit policy; the Unix opener refuses a FIFO without waiting for a writer.
The release digest includes the plugin implementation and its platform-specific
source policy. The external-consumer check in `scripts/go-consumer.py` compiles
these copied files against the committed module artifact without a workspace or
replace directive.
