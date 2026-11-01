# Document indexer

Build the npm workspaces, then run `go run ./examples/document-indexer` from the repository root.
The host creates an isolated temporary SQLite store and a real local runner. The Node
unit receives exactly two portable imports and exports nothing. The reader and writer
enforce the host's document permission. On activation the plugin indexes the document;
the host then stops its original RunningUnit and waits for the instance to retire.

`contracts.json` owns the domain contracts. Both SDKs derive the same digest from each descriptor. The
Go host verifies the descriptors before installing either unit. The example trusts
the installed Node runtime, the workspace package code and the explicitly selected
entry. Importing these services grants no additional OS sandbox.
