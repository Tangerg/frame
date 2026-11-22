package value

const SchemaDialect = "https://json-schema.org/draft/2020-12/schema"

type SchemaChildren string

const (
	SchemaChildrenMap    SchemaChildren = "map"
	SchemaChildrenSchema SchemaChildren = "schema"
	SchemaChildrenArray  SchemaChildren = "array"
)

type SchemaEdge string

const (
	SchemaEdgeSame    SchemaEdge = "same"
	SchemaEdgeDescend SchemaEdge = "descend"
)

type SchemaType string

const (
	SchemaTypeNull    SchemaType = "null"
	SchemaTypeBoolean SchemaType = "boolean"
	SchemaTypeObject  SchemaType = "object"
	SchemaTypeArray   SchemaType = "array"
	SchemaTypeNumber  SchemaType = "number"
	SchemaTypeInteger SchemaType = "integer"
	SchemaTypeString  SchemaType = "string"
)

type SchemaValueShape string

const (
	SchemaValueString         SchemaValueShape = "string"
	SchemaValueBoolean        SchemaValueShape = "boolean"
	SchemaValueNumber         SchemaValueShape = "number"
	SchemaValuePositiveNumber SchemaValueShape = "positive-number"
	SchemaValueSize           SchemaValueShape = "size"
	SchemaValueTypes          SchemaValueShape = "types"
	SchemaValueEnum           SchemaValueShape = "enum"
	SchemaValueRequired       SchemaValueShape = "required"
	SchemaValueArray          SchemaValueShape = "array"
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
	Annotation bool             `json:"annotation,omitzero"`
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
		Types:   []SchemaType{SchemaTypeNull, SchemaTypeBoolean, SchemaTypeObject, SchemaTypeArray, SchemaTypeNumber, SchemaTypeInteger, SchemaTypeString},
		Keywords: map[string]SchemaKeyword{
			KeywordSchema:               {Value: SchemaValueString, Annotation: true},
			KeywordComment:              {Value: SchemaValueString, Annotation: true},
			KeywordDefs:                 {Children: SchemaChildrenMap, Annotation: true},
			KeywordDefinitions:          {Children: SchemaChildrenMap, Annotation: true},
			KeywordTitle:                {Value: SchemaValueString, Annotation: true},
			KeywordDescription:          {Value: SchemaValueString, Annotation: true},
			KeywordDefault:              {Annotation: true},
			KeywordExamples:             {Value: SchemaValueArray, Annotation: true},
			KeywordReadOnly:             {Value: SchemaValueBoolean, Annotation: true},
			KeywordWriteOnly:            {Value: SchemaValueBoolean, Annotation: true},
			KeywordDeprecated:           {Value: SchemaValueBoolean, Annotation: true},
			KeywordContentEncoding:      {Value: SchemaValueString, Annotation: true},
			KeywordContentMediaType:     {Value: SchemaValueString, Annotation: true},
			KeywordProperties:           {Children: SchemaChildrenMap, Edge: SchemaEdgeDescend},
			KeywordPatternProperties:    {Children: SchemaChildrenMap, Edge: SchemaEdgeDescend},
			KeywordAdditionalProperties: {Children: SchemaChildrenSchema, Edge: SchemaEdgeDescend},
			KeywordItems:                {Children: SchemaChildrenSchema, Edge: SchemaEdgeDescend},
			KeywordContains:             {Children: SchemaChildrenSchema, Edge: SchemaEdgeDescend},
			KeywordPropertyNames:        {Children: SchemaChildrenSchema, Edge: SchemaEdgeDescend},
			KeywordNot:                  {Children: SchemaChildrenSchema, Edge: SchemaEdgeSame},
			KeywordIf:                   {Children: SchemaChildrenSchema, Edge: SchemaEdgeSame},
			KeywordThen:                 {Children: SchemaChildrenSchema, Edge: SchemaEdgeSame},
			KeywordElse:                 {Children: SchemaChildrenSchema, Edge: SchemaEdgeSame},
			KeywordAllOf:                {Children: SchemaChildrenArray, Edge: SchemaEdgeSame},
			KeywordAnyOf:                {Children: SchemaChildrenArray, Edge: SchemaEdgeSame},
			KeywordOneOf:                {Children: SchemaChildrenArray, Edge: SchemaEdgeSame},
			KeywordRef:                  {Value: SchemaValueString},
			KeywordType:                 {Value: SchemaValueTypes},
			KeywordEnum:                 {Value: SchemaValueEnum},
			KeywordConst:                {},
			KeywordRequired:             {Value: SchemaValueRequired},
			KeywordMinimum:              {Value: SchemaValueNumber},
			KeywordMaximum:              {Value: SchemaValueNumber},
			KeywordExclusiveMinimum:     {Value: SchemaValueNumber},
			KeywordExclusiveMaximum:     {Value: SchemaValueNumber},
			KeywordMultipleOf:           {Value: SchemaValuePositiveNumber},
			KeywordMinLength:            {Value: SchemaValueSize},
			KeywordMaxLength:            {Value: SchemaValueSize},
			KeywordPattern:              {Value: SchemaValueString},
			KeywordFormat:               {Value: SchemaValueString},
			KeywordMinItems:             {Value: SchemaValueSize},
			KeywordMaxItems:             {Value: SchemaValueSize},
			KeywordUniqueItems:          {Value: SchemaValueBoolean},
			KeywordMinProperties:        {Value: SchemaValueSize},
			KeywordMaxProperties:        {Value: SchemaValueSize},
		},
	}
}
