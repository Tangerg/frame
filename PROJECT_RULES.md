# Frame repository instructions

Frame is a general-purpose plugin framework. Hosts define domain contracts and business authorization. Frame owns installation, exact binding admission and resource protection. Execution owners alone can prove exit. Business owners alone can settle their operations.

A single Go module lives at the repository root. The npm workspace publishes one SDK with native, Node, browser and React entry points. MCP wire details belong in the optional process/mcp adapter. Read the package README and [development instructions](DEVELOPMENT.md) before structural changes. The reviewed Chinese proposal is shipped with each source distribution.

- Freeze providers once at the Host. Setup consumes those exact slots. A driver must never resolve alternatives.
- Deny invalid grants before executing plugin code. Revalidate after slow preparation. Cancellation of a wait never proves external work has stopped.
- Keep cross-process messages bounded. Keep host credentials and application policy out of public descriptors. Native plugins and local child processes are trusted with the OS privileges under which they run.
- Keep service identity independent of contribution ordering and selection policy. Derive wire and generated values from their semantic owner.
- Validate observable use cases through exported packages and actual release artifacts. An external consumer must work without a checkout of a host application.

- Do not preserve backward compatibility for a wrong design. Fix the semantic owner, migrate every in-scope consumer, and remove obsolete APIs, packages, schemas, aliases, fallbacks, tests, and documentation in the same batch.
- Every abstraction, representation, state, dependency, package, and call path must justify its existence against a proven requirement.
- Use domain-driven design and clean dependency direction to express ownership, not to generate a directory matrix. Domain models own invariants and pure transitions; Application owns use-case ordering; external adapters own translation; delivery owns bindings; bootstrap owns composition and shutdown.
- Prefer behavior-rich owners over procedural orchestration around mutable records. An aggregate or value object validates construction, protects its state, and exposes intention-revealing queries and transitions. Configuration, wire, storage, request, response, and projection structs remain data unless they own a real invariant.
- Keep Domain behavior deterministic and free of I/O. Application coordinates clocks, cancellation, transactions, and external effects around Domain decisions; adapters do not reach back in and mutate aggregate fields.
- Prefer explicit, readable, shallow, sparse Go. A package must own a coherent vocabulary, aggregate invariant, use-case lifecycle, external translation, or reusable technical mechanism. Prefer several responsibility-named files in one package over a package per concept. When a ring has several proven contexts, use one non-package namespace level to expose that context map; reserve direct ring packages for ring-wide mechanisms or an aggregate that names its context.
- Let the import path and package qualifier state an owner once. A leaf must not repeat an enclosing ring or context, and an exported name must not repeat the package qualifier before adding another noun. Exact ubiquitous aggregates such as `run.Run` are allowed when renaming either side would damage the domain language.
- Namespace directories never contain facade Go files. Do not add a single-child namespace, nest beyond `ring/context/package`, or move a cross-context mechanism under one consumer merely to make the tree symmetrical.
- Do not create `service`, `repository`, `manager`, `impl`, `common`, `helpers`, or umbrella packages to make a diagram look complete. A forwarding wrapper or cycle-avoidance package is a refactoring signal, not an architectural boundary.
- Start with concrete types. Define narrow interfaces in consuming packages only when they reverse a dependency, isolate an external boundary, or support real substitution. Return concrete types from constructors.
- Keep composition explicit. Do not use ambient globals, service locators, reflection registries, dependency-injection containers, optional service bags, or configuration objects that leak across boundaries.
- Make invalid states unconstructable where practical. Use closed states and named values instead of primitive sentinels, boolean combinations, magic strings, anonymous maps, or typed-nil ambiguity.
- Constructors validate once and return errors instead of leaving partially valid objects. When optional policy grows beyond a self-evident argument, use an owner-named `Config` with closed values rather than positional primitives or boolean switches.
- Preserve one vocabulary across domain types, protocol values, storage, errors, CLI output, tests, and documentation. Do not retain former names as aliases.
- Prefer the current module Go version and standard library, then an existing mature dependency. Use modern language and library features when they make ownership clearer. A wrapper must own policy, translation, lifecycle, or authority; otherwise delete it.
- Errors preserve their cause, use stable domain or protocol categories only where callers branch, and add concise owner context at boundaries. Error strings are lowercase, contain no credentials, and are not parsed as control flow.
- Derive lifecycle decisions from owner-provided facts and real limits. Do not substitute arbitrary message/item counts, hand-maintained schema epochs, or similar counters for request capacity, compatibility, or state.
- Never edit, format, stage, revert, or generate into a user-owned path outside the active scope.
- Reply to the user in Chinese. Keep code, identifiers, comments, errors, and repository documentation in English. Comments explain why, not what.
