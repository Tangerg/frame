# Published contract projections

`Generate` reads one admitted portable capability descriptor. The descriptor
remains the callable authority. Its schema bundle and generated Go and
TypeScript types are consumer projections; neither generated language can
statically express every format, numeric or cross-field predicate. Validate
inputs, stream chunks and outputs with their admitted method contract.
An empty method description remains valid. Consumer code generation adds no
documentation precondition to an already admitted callable descriptor.
Invalid descriptors, Go package names or prefixes, and conflicting output
destinations return `ErrInvalid` before publication. Filesystem failures retain
their original causes so callers can distinguish invalid requests from an
unavailable destination. A Go package name must be a non-blank identifier.

The generator resolves local JSON Pointer references, including escaped names,
nested subschemas and structural recursion, into one projection graph shared by
both renderers. Object openness, required fields, composition and reference
targets are interpreted once. Each method input, output and stream schema retains
its own document scope. Equal local definition names or bodies in different
documents do not merge their reference targets. Ambiguous projected type names
include their method frame; local aliases remain within that frame. Missing
`additionalProperties` is open. Object key order does not change JSON identity;
numeric lexemes still follow `value.JSON` identity.
An expanded root shares its named projection only after every reference to its
original descendants has moved to that same definition.

Go projections depend on `github.com/Tangerg/frame/value` when a shape needs
exact numeric or opaque JSON storage. Every JSON `number` and `integer` uses
`value.Number`: machine numeric types cannot preserve all permitted spellings,
including `-0`, `1.0`, `1e0`, large integers and large exponents. A generated
named numeric root embeds that value, retaining its JSON codec. Construct a
number with `value.ParseNumber`; the zero value represents absence and cannot
be encoded as a valid number. Numeric bounds remain in the descriptor and are
checked by its validator.

Open or mixed-member objects preserve their members with exact JSON values.
Closed objects retain typed fields when their names fit Go's struct encoding;
other names remain exact map keys. Unions and other shapes without a faithful
Go structural representation retain an owned `value.JSON`, including nullable
fields. Typed decoding and schema admission follow that codec's explicit wire
model, so named output and chunk wrappers retain the same null semantics.
Typed authors omit `jsonschema:"nullable"`: nullability already belongs to the
encoding model, and a second null branch would conflict with it.
An optional opaque field uses its value form: a pointer or interface with
`omitzero` would collapse a decoded null into absence and is refused at type
admission. `omitempty` still intentionally omits JSON null. References to generated object structs use pointers so structural
recursion remains representable; the descriptor owns nullability and requiredness.
String formats remain strings: converting an admitted date-time to
`time.Time` would discard supported precision or reject otherwise valid values.
These representations intentionally replace the former lossy machine-number
and implicitly closed-object projections; there are no compatibility aliases.

TypeScript uses the SDK's `JSONNumber` and explicit object index signatures.
Composition preserves base and branch constraints; arrays use generic type
syntax so an element intersection cannot change meaning through precedence.
Types express their supported structural constraints while the canonical
validator remains responsible for the full JSON Schema predicate.

Each renderer owns its complete generated namespace, including imports, runtime
constants, helper interfaces and built-in types. Colliding source names receive
deterministic numeric suffixes in that language's projection. Method dictionaries
use computed own keys, so names such as `__proto__` remain ordinary declared
methods. Neither projected identifiers nor helper names change the published
capability descriptor or its digest. Numeric imports follow the projected schema
shape; application strings containing `JSONNumber` do not create dependencies.

The regression suite generates actual Go and TypeScript consumers. Run
`npm ci` before `go test ./portable/contractgen ./value`; the TypeScript consumer
checks use the repository's pinned compiler and SDK numeric type. Positive
assignments and `@ts-expect-error` negative assignments run together so a
weakened generated type also fails the check.
