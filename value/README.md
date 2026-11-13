# Portable values

`value` owns immutable JSON identity, exact numbers, Go codec models, schema
admission and bounded validation. It is independent of Hosts, execution adapters,
filesystem state and networks. Schemas and decoding share one codec/type model;
a language projection cannot widen the published predicate.

`ParseJSON` captures one JSON document. `EncodeJSON` captures a Go value through
its admitted codec. `SchemaFor` derives a schema from that same model;
`AdmitSchema` checks and compiles an explicit schema. `ProjectStringEnums` and
`ReadableDefinitions` expose projections for consumers such as `contractgen`.
`PortableLimits`, `ProfileSchema`, `ProfilePatternSyntax` and
`ProfileStringFormats` supply the data-language profile that Frame projects to
the Node SDK. The shared admission corpus lives in `testdata`.

JSON field declarations own presence and representation. Schema tags can narrow
values, including requiring an otherwise optional field; tags that replace a
field's encoding are refused before publication. Pointers represent omission.
Use opaque JSON for nullable material. An optional opaque field with `omitzero`
uses its value form: a pointer or interface would collapse decoded null into
absence. `omitempty` intentionally omits JSON null. Named or plain string map keys
cannot carry custom codecs; schema derivation cannot project those constraints
onto property names.

Callable identity, method kinds, commands and service implementations belong to
the root `frame` package. They consume these value guarantees without making
`value` depend on an execution context.
