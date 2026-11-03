package portable

const SchemaDialect = "https://json-schema.org/draft/2020-12/schema"

type SchemaChildren string

const (
	SchemaMap   SchemaChildren = "map"
	SchemaChild SchemaChildren = "schema"
	SchemaArray SchemaChildren = "array"
)

type SchemaEdge string

const (
	SchemaSame    SchemaEdge = "same"
	SchemaDescend SchemaEdge = "descend"
)

// SchemaType is a JSON Schema instance type admitted by the portable dialect.
type SchemaType string

const (
	TypeNull    SchemaType = "null"
	TypeBoolean SchemaType = "boolean"
	TypeObject  SchemaType = "object"
	TypeArray   SchemaType = "array"
	TypeNumber  SchemaType = "number"
	TypeInteger SchemaType = "integer"
	TypeString  SchemaType = "string"
)

type SchemaValueShape string

const (
	SchemaString         SchemaValueShape = "string"
	SchemaBoolean        SchemaValueShape = "boolean"
	SchemaNumber         SchemaValueShape = "number"
	SchemaPositiveNumber SchemaValueShape = "positive-number"
	SchemaSize           SchemaValueShape = "size"
	SchemaTypes          SchemaValueShape = "types"
	SchemaEnum           SchemaValueShape = "enum"
	SchemaRequired       SchemaValueShape = "required"
	SchemaValues         SchemaValueShape = "array"
)

// Schema keywords of the portable dialect. Every package that reads or rewrites
// a schema uses these names; ProfileSchema declares their meaning.
const (
	KeywordSchema               = "$schema"
	KeywordComment              = "$comment"
	KeywordDefs                 = "$defs"
	KeywordDefinitions          = "definitions"
	KeywordTitle                = "title"
	KeywordDescription          = "description"
	KeywordDefault              = "default"
	KeywordExamples             = "examples"
	KeywordReadOnly             = "readOnly"
	KeywordWriteOnly            = "writeOnly"
	KeywordDeprecated           = "deprecated"
	KeywordContentEncoding      = "contentEncoding"
	KeywordContentMediaType     = "contentMediaType"
	KeywordProperties           = "properties"
	KeywordPatternProperties    = "patternProperties"
	KeywordAdditionalProperties = "additionalProperties"
	KeywordItems                = "items"
	KeywordContains             = "contains"
	KeywordPropertyNames        = "propertyNames"
	KeywordNot                  = "not"
	KeywordIf                   = "if"
	KeywordThen                 = "then"
	KeywordElse                 = "else"
	KeywordAllOf                = "allOf"
	KeywordAnyOf                = "anyOf"
	KeywordOneOf                = "oneOf"
	KeywordRef                  = "$ref"
	KeywordType                 = "type"
	KeywordEnum                 = "enum"
	KeywordConst                = "const"
	KeywordRequired             = "required"
	KeywordMinimum              = "minimum"
	KeywordMaximum              = "maximum"
	KeywordExclusiveMinimum     = "exclusiveMinimum"
	KeywordExclusiveMaximum     = "exclusiveMaximum"
	KeywordMultipleOf           = "multipleOf"
	KeywordMinLength            = "minLength"
	KeywordMaxLength            = "maxLength"
	KeywordPattern              = "pattern"
	KeywordFormat               = "format"
	KeywordMinItems             = "minItems"
	KeywordMaxItems             = "maxItems"
	KeywordUniqueItems          = "uniqueItems"
	KeywordMinProperties        = "minProperties"
	KeywordMaxProperties        = "maxProperties"
)

// SchemaKeyword separates schema children from application data: a const or
// annotation containing "$ref" must never become a traversal edge.
type SchemaKeyword struct {
	Children   SchemaChildren   `json:"children,omitempty"`
	Edge       SchemaEdge       `json:"edge,omitempty"`
	Value      SchemaValueShape `json:"value,omitempty"`
	Annotation bool             `json:"annotation,omitempty"`
}

type SchemaProfile struct {
	Dialect  string                   `json:"dialect"`
	Types    []SchemaType             `json:"types"`
	Keywords map[string]SchemaKeyword `json:"keywords"`
}

// Runtime adapters consume this declaration so admitting a keyword cannot
// silently give Go and Node different traversal or resource budgets.
func ProfileSchema() SchemaProfile {
	return SchemaProfile{
		Dialect: SchemaDialect,
		Types:   []SchemaType{TypeNull, TypeBoolean, TypeObject, TypeArray, TypeNumber, TypeInteger, TypeString},
		Keywords: map[string]SchemaKeyword{
			KeywordSchema:               {Value: SchemaString, Annotation: true},
			KeywordComment:              {Value: SchemaString, Annotation: true},
			KeywordDefs:                 {Children: SchemaMap, Annotation: true},
			KeywordDefinitions:          {Children: SchemaMap, Annotation: true},
			KeywordTitle:                {Value: SchemaString, Annotation: true},
			KeywordDescription:          {Value: SchemaString, Annotation: true},
			KeywordDefault:              {Annotation: true},
			KeywordExamples:             {Value: SchemaValues, Annotation: true},
			KeywordReadOnly:             {Value: SchemaBoolean, Annotation: true},
			KeywordWriteOnly:            {Value: SchemaBoolean, Annotation: true},
			KeywordDeprecated:           {Value: SchemaBoolean, Annotation: true},
			KeywordContentEncoding:      {Value: SchemaString, Annotation: true},
			KeywordContentMediaType:     {Value: SchemaString, Annotation: true},
			KeywordProperties:           {Children: SchemaMap, Edge: SchemaDescend},
			KeywordPatternProperties:    {Children: SchemaMap, Edge: SchemaDescend},
			KeywordAdditionalProperties: {Children: SchemaChild, Edge: SchemaDescend},
			KeywordItems:                {Children: SchemaChild, Edge: SchemaDescend},
			KeywordContains:             {Children: SchemaChild, Edge: SchemaDescend},
			KeywordPropertyNames:        {Children: SchemaChild, Edge: SchemaDescend},
			KeywordNot:                  {Children: SchemaChild, Edge: SchemaSame},
			KeywordIf:                   {Children: SchemaChild, Edge: SchemaSame},
			KeywordThen:                 {Children: SchemaChild, Edge: SchemaSame},
			KeywordElse:                 {Children: SchemaChild, Edge: SchemaSame},
			KeywordAllOf:                {Children: SchemaArray, Edge: SchemaSame},
			KeywordAnyOf:                {Children: SchemaArray, Edge: SchemaSame},
			KeywordOneOf:                {Children: SchemaArray, Edge: SchemaSame},
			KeywordRef:                  {Value: SchemaString},
			KeywordType:                 {Value: SchemaTypes},
			KeywordEnum:                 {Value: SchemaEnum},
			KeywordConst:                {},
			KeywordRequired:             {Value: SchemaRequired},
			KeywordMinimum:              {Value: SchemaNumber},
			KeywordMaximum:              {Value: SchemaNumber},
			KeywordExclusiveMinimum:     {Value: SchemaNumber},
			KeywordExclusiveMaximum:     {Value: SchemaNumber},
			KeywordMultipleOf:           {Value: SchemaPositiveNumber},
			KeywordMinLength:            {Value: SchemaSize},
			KeywordMaxLength:            {Value: SchemaSize},
			KeywordPattern:              {Value: SchemaString},
			KeywordFormat:               {Value: SchemaString},
			KeywordMinItems:             {Value: SchemaSize},
			KeywordMaxItems:             {Value: SchemaSize},
			KeywordUniqueItems:          {Value: SchemaBoolean},
			KeywordMinProperties:        {Value: SchemaSize},
			KeywordMaxProperties:        {Value: SchemaSize},
		},
	}
}
