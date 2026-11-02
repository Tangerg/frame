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
	Types    []string                 `json:"types"`
	Keywords map[string]SchemaKeyword `json:"keywords"`
}

// Runtime adapters consume this declaration so admitting a keyword cannot
// silently give Go and Node different traversal or resource budgets.
func ProfileSchema() SchemaProfile {
	return SchemaProfile{
		Dialect: SchemaDialect,
		Types:   []string{"null", "boolean", "object", "array", "number", "integer", "string"},
		Keywords: map[string]SchemaKeyword{
			"$schema":              {Value: SchemaString, Annotation: true},
			"$comment":             {Value: SchemaString, Annotation: true},
			"$defs":                {Children: SchemaMap, Annotation: true},
			"definitions":          {Children: SchemaMap, Annotation: true},
			"title":                {Value: SchemaString, Annotation: true},
			"description":          {Value: SchemaString, Annotation: true},
			"default":              {Annotation: true},
			"examples":             {Value: SchemaValues, Annotation: true},
			"readOnly":             {Value: SchemaBoolean, Annotation: true},
			"writeOnly":            {Value: SchemaBoolean, Annotation: true},
			"deprecated":           {Value: SchemaBoolean, Annotation: true},
			"contentEncoding":      {Value: SchemaString, Annotation: true},
			"contentMediaType":     {Value: SchemaString, Annotation: true},
			"properties":           {Children: SchemaMap, Edge: SchemaDescend},
			"patternProperties":    {Children: SchemaMap, Edge: SchemaDescend},
			"additionalProperties": {Children: SchemaChild, Edge: SchemaDescend},
			"items":                {Children: SchemaChild, Edge: SchemaDescend},
			"contains":             {Children: SchemaChild, Edge: SchemaDescend},
			"propertyNames":        {Children: SchemaChild, Edge: SchemaDescend},
			"not":                  {Children: SchemaChild, Edge: SchemaSame},
			"if":                   {Children: SchemaChild, Edge: SchemaSame},
			"then":                 {Children: SchemaChild, Edge: SchemaSame},
			"else":                 {Children: SchemaChild, Edge: SchemaSame},
			"allOf":                {Children: SchemaArray, Edge: SchemaSame},
			"anyOf":                {Children: SchemaArray, Edge: SchemaSame},
			"oneOf":                {Children: SchemaArray, Edge: SchemaSame},
			"$ref":                 {Value: SchemaString},
			"type":                 {Value: SchemaTypes},
			"enum":                 {Value: SchemaEnum},
			"const":                {},
			"required":             {Value: SchemaRequired},
			"minimum":              {Value: SchemaNumber},
			"maximum":              {Value: SchemaNumber},
			"exclusiveMinimum":     {Value: SchemaNumber},
			"exclusiveMaximum":     {Value: SchemaNumber},
			"multipleOf":           {Value: SchemaPositiveNumber},
			"minLength":            {Value: SchemaSize},
			"maxLength":            {Value: SchemaSize},
			"pattern":              {Value: SchemaString},
			"format":               {Value: SchemaString},
			"minItems":             {Value: SchemaSize},
			"maxItems":             {Value: SchemaSize},
			"uniqueItems":          {Value: SchemaBoolean},
			"minProperties":        {Value: SchemaSize},
			"maxProperties":        {Value: SchemaSize},
		},
	}
}
