# Frame Node process SDK

Use `defineProcessUnit` to declare a local unit and `serve` to bind its handlers to standard input/output. The peer owns its two streams for this session and closes them at EOF or shutdown. Standard output is reserved for Frame frames. Program diagnostics belong on standard error.

The application publishes its portable descriptor once. A `Capability` checks the descriptor, derives its exact identity and validates calls in both directions. Each import is a frozen binding to the host-selected export. During preparation the unit can acquire resources and register activation; host calls are available after final activation. The owning lifetime joins started work on closure. Losing a response never authorizes a new business operation.

Portable schema size constraints accept mathematical integers within the shared profile's exact nonnegative size range. Equivalent numeric spellings retain their original text in the capability identity.

See `examples/document-indexer` for a runnable process with two imports and no exports.
