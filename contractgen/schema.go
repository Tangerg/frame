package contractgen

// refPrefix is where a reusable definition lives inside the bundle. Request
// projections rewrite object references to their closed receiving context.
const RefPrefix = "#/$defs/"

type SchemaType string

const (
	SchemaTypeArray   SchemaType = "array"
	SchemaTypeBoolean SchemaType = "boolean"
	SchemaTypeInteger SchemaType = "integer"
	SchemaTypeNull    SchemaType = "null"
	SchemaTypeNumber  SchemaType = "number"
	SchemaTypeObject  SchemaType = "object"
	SchemaTypeString  SchemaType = "string"
)

// schema is a JSON Schema node, holding only the keywords this contract needs.
//
// Properties values are `any` because a forbidden field is the boolean schema
// `false` — the one keyword that says "this may not be present at all", which is
// how a union variant excludes another variant's fields.
type Schema struct {
	Ref              string         `json:"$ref,omitempty"`
	Type             SchemaType     `json:"type,omitempty"`
	Format           string         `json:"format,omitempty"`
	ContentEncoding  string         `json:"contentEncoding,omitempty"`
	Enum             []string       `json:"enum,omitempty"`
	MinLength        *int           `json:"minLength,omitzero"`
	MaxLength        *int           `json:"maxLength,omitzero"`
	Const            string         `json:"const,omitempty"`
	Pattern          string         `json:"pattern,omitempty"`
	TypeScriptType   string         `json:"-"`
	Minimum          *int64         `json:"minimum,omitzero"`
	ExclusiveMinimum *int64         `json:"exclusiveMinimum,omitzero"`
	Maximum          *int64         `json:"maximum,omitzero"`
	MinItems         *int           `json:"minItems,omitzero"`
	MaxItems         *int           `json:"maxItems,omitzero"`
	MinProperties    *int           `json:"minProperties,omitzero"`
	UniqueItems      bool           `json:"uniqueItems,omitzero"`
	Items            *Schema        `json:"items,omitzero"`
	Properties       map[string]any `json:"properties,omitempty"`
	AdditionalProps  any            `json:"additionalProperties,omitzero"`
	PropertyNames    *Schema        `json:"propertyNames,omitzero"`
	UnevaluatedProps *bool          `json:"unevaluatedProperties,omitzero"`
	Required         []string       `json:"required,omitempty"`
	OneOf            []*Schema      `json:"oneOf,omitempty"`
	AnyOf            []*Schema      `json:"anyOf,omitempty"`
	AllOf            []*Schema      `json:"allOf,omitempty"`
	If               *Schema        `json:"if,omitzero"`
	Then             *Schema        `json:"then,omitzero"`
}

const SchemaDialect = "https://json-schema.org/draft/2020-12/schema"

type Definitions map[string]*Schema
