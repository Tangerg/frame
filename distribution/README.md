# Immutable releases

This package owns release-byte validation, bounded archive extraction, immutable
content snapshots and read leases. Installation state, trust decisions, dependency
selection and execution remain with the Host and application.

One `Store` owns an open directory root and an exclusive lock on that directory
inode. A second process or Store cannot open the same original directory while
its owner is alive. Operations use root-relative filesystem APIs and reject a
changed canonical parent, symlink or replacement root. Startup reclaims only
private abandoned staging and retirement directories after obtaining that lock.
`Close(ctx)` seals new operations and joins accepted I/O and all original Leases
before releasing the root and lock. Cancelling a caller's wait does not release
those protections. The Store continues its original close after that wait ends;
once accepted I/O and the final Lease have exited, it releases the directory lock
without another `Close` call. Later callers observe that same close result.
Consumers close their execution owners before the Store. Platforms without the
supported directory lock fail closed rather than claiming exclusive ownership.

A release is a single-disk ZIP32 archive with absolute directory offsets and no archive comment. Its physical central directory is bounded before entries are allocated. It contains a schema-version 1 `manifest.json`, `plugin.lock.json` and
exactly its listed artifacts. `Build` computes artifact sizes and SHA-256 digests
from the declared source files and emits deterministic archive bytes. It does not
invoke a compiler or include unlisted source files. The Release digest covers the
canonical manifest and artifact inventory, excluding the digest field itself.
Canonical JSON preserves numeric lexemes and declaration order; artifact inventory
order is normalized. A package version is a human-facing label, not byte identity.

Exact dependency releases belong to the canonical manifest. `Manifest.Lock()` and
`Build` derive their lock from that declaration; changing a dependency changes the
release identity. Lock-only dependency additions are refused. Authors move those
declarations into the manifest and rebuild the release; the package command no
longer accepts a separate dependency file.

The manifest contains independent Units with typed process variants, browser module
entries and fixed imports, static resources, contract references and contributions.
One release may contain all three. Pure resources need no methods or process. A
release import does not invent an InstallationID; the Host later supplies exact
providers in a BindingSnapshot. Every exported and imported contract must be listed.
Native objects cannot be declared as process imports or exports.

## Sources and validation

`ImportDirectory` first snapshots the directory into private staging and validates
that snapshot. Editing the source afterward cannot change the installed release.
`ImportArchive` requires the exact archive digest. A consumer-controlled HTTPS
fetcher may supply a private local file; this package does not select network sources.

`Open` requires an application Trust function. A matching digest proves consistency,
not publisher trust. Applications check publisher, source, signatures if required,
and requested capabilities before publication. Rejection removes staging.
When a digest is already cached, installation also validates and trusts that final
cached snapshot and returns it. Its receipt and a subsequent Acquire or Inspect
therefore describe the same immutable release.

Only regular files are accepted. Symbolic links, special files, traversal, absolute
paths, platform-reserved names, duplicate destinations and case aliases are refused.
Portable paths use ASCII to avoid filesystem-dependent Unicode aliases. Executable
bits are explicit in the inventory; setuid, setgid and sticky bits are rejected.
`Limits.Entries` counts files, directories and their implicit ancestors. Source
and expanded bytes, manifest bytes and compression ratio have explicit configurable
bounds. ZIP checksums and artifact hashes are both verified.
Staging belongs to one installation operation and is removed unless atomically
published. Validated content is read-only in the content-addressed cache.
Contents are sealed before rename; the release root is sealed after rename under
admission, before its first lease. This also supports macOS filesystems that
refuse renaming a non-writable directory. An incompletely sealed root is refused by readers. Reimport of the same trusted release revalidates the original tree before completing its seal.
Collection changes only the root's mode after excluding every lease and retention
claim, restores that mode if withdrawal fails, and unseals descendants only after
the directory has left the public namespace.

`Acquire` holds a read lease under the Host's same Admission. `Read` returns verified
detached bytes, and `Executable` exposes only a verified declared executable while
its lease remains alive. The process runner makes its own verified executable
snapshot before spawn. Cache inspection repeats path, entry-capacity and file-mode
admission for the entire tree. Every leased read checks size, digest and
executability against bytes and permissions captured from the same physical file.
Read-only permissions are not treated as a cryptographic proof.

`Remove` enters the same admission gate and checks both active leases and the
application's current retention query. That query includes installation, durable
business and execution protections as appropriate. Storage errors or recovery
barriers cannot become permission to collect. Closing a lease releases only that
read; it does not disable an installation or delete application data.

Copies of a Lease share its original content claim. Closing any copy closes that
claim once, while independent acquisitions keep their own protection. A closed
Lease retains only its detached release declaration and cannot keep the Store alive.

Example build and validation commands are provided by the Frame author tools.
Independent consumers can call these public functions without importing Flame.
