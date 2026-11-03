package contractgen

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/Tangerg/frame/portable"
)

const (
	typeScriptNumber       = "JSONNumber"
	typeScriptArray        = "Array"
	typeScriptRecord       = "Record"
	methodInputs           = "MethodInputs"
	methodOutputs          = "MethodOutputs"
	methodChunks           = "MethodChunks"
	methodsConstant        = "METHODS"
	capabilityConstant     = "CAPABILITY"
	capabilityJSONConstant = "CAPABILITY_JSON"
)

type typeScript struct {
	names      map[*projectionNode]string
	usesNumber bool
}

func newTypeScript(graph *projection) *typeScript {
	symbols := newSymbolTable(typeScriptNumber, typeScriptArray, typeScriptRecord, methodInputs, methodOutputs, methodChunks, methodsConstant, capabilityConstant, capabilityJSONConstant)
	emitter := &typeScript{names: make(map[*projectionNode]string, len(graph.definitions))}
	for _, name := range slices.Sorted(maps.Keys(graph.definitions)) {
		emitter.names[graph.definitions[name]] = symbols.claim(name)
	}
	return emitter
}

func (t *typeScript) numberType() string {
	t.usesNumber = true
	return typeScriptNumber
}

func (t *typeScript) Definition(name string, node *projectionNode) string {
	if node.kind == portable.TypeObject && !node.object.impossible() && node.reference == nil && len(node.allOf) == 0 && len(node.anyOf) == 0 && len(node.literals) == 0 && node.object.openness == objectClosed && !node.object.patterns && len(node.object.properties) > 0 {
		var out strings.Builder
		fmt.Fprintf(&out, "export interface %s {\n", name)
		for _, name := range t.propertyNames(node) {
			fmt.Fprintf(&out, "  %s%s: %s;\n", PropertyKey(name), optionalMark(node, name), t.TypeOf(node.object.property(name)))
		}
		out.WriteString("}\n\n")
		return out.String()
	}
	return fmt.Sprintf("export type %s = %s;\n\n", name, t.TypeOf(node))
}

func (t *typeScript) TypeOf(node *projectionNode) string { return t.expression(node, "") }

func (t *typeScript) expression(node *projectionNode, inherited portable.SchemaType) string {
	if node == nil {
		return tsUnknown
	}
	if node.forbidden {
		return tsNever
	}
	if len(node.kinds) > 0 {
		branches := make([]string, len(node.kinds))
		for i, kind := range node.kinds {
			member := *node
			member.kind = kind
			member.kinds = nil
			branches[i] = t.expression(&member, inherited)
		}
		return unionOf(branches)
	}
	kind, contextKind := node.contextualKind(inherited)
	var parts []string
	if node.referencePath != "" {
		name := t.names[node.reference]
		parts = append(parts, name)
	}
	if len(node.literals) > 0 {
		literals := make([]string, len(node.literals))
		for i, value := range node.literals {
			literals[i] = t.literalType(value)
		}
		parts = append(parts, unionOf(literals))
	}
	parts = append(parts, t.keyword(kind, contextKind, node))
	for _, child := range node.allOf {
		parts = append(parts, t.expression(child, contextKind))
	}
	if len(node.anyOf) > 0 {
		branches := make([]string, len(node.anyOf))
		for i, child := range node.anyOf {
			branches[i] = t.expression(child, contextKind)
		}
		parts = append(parts, unionOf(branches))
	}
	return intersectionOf(parts)
}

func (t *typeScript) propertyNames(node *projectionNode) []string {
	names := slices.Sorted(maps.Keys(node.object.properties))
	for _, name := range node.object.required {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func (t *typeScript) keyword(kind, contextKind portable.SchemaType, node *projectionNode) string {
	switch kind {
	case portable.TypeString:
		return "string"
	case portable.TypeInteger, portable.TypeNumber:
		return t.numberType()
	case portable.TypeBoolean:
		return "boolean"
	case portable.TypeNull:
		return "null"
	case portable.TypeArray:
		// Generic array syntax owns its grouping even for unions and intersections.
		return typeScriptArray + "<" + t.TypeOf(node.items) + ">"
	case portable.TypeObject:
		return t.object(node)
	default:
		if contextKind != "" {
			return tsUnknown
		}
		var parts []string
		// Applicator keywords constrain only their instance kind. A nullable or
		// unresolved reference must retain both that constraint and the other kinds.
		if node.object.constrained() {
			parts = append(parts, unionOf([]string{"string", t.numberType(), "boolean", "null", typeScriptArray + "<unknown>", t.object(node)}))
		}
		if node.items != nil {
			parts = append(parts, unionOf([]string{"string", t.numberType(), "boolean", "null", typeScriptRecord + "<string, unknown>", typeScriptArray + "<" + t.TypeOf(node.items) + ">"}))
		}
		return intersectionOf(parts)
	}
}

func (t *typeScript) object(node *projectionNode) string {
	if node.object.impossible() {
		return tsNever
	}
	var fields []string
	for _, name := range t.propertyNames(node) {
		fields = append(fields, fmt.Sprintf("%s%s: %s", PropertyKey(name), optionalMark(node, name), t.TypeOf(node.object.property(name))))
	}
	object := "{ " + strings.Join(fields, "; ") + " }"
	switch {
	case node.object.patterns || node.object.openness == objectOpen:
		if len(fields) == 0 {
			return typeScriptRecord + "<string, unknown>"
		}
		return object + " & " + typeScriptRecord + "<string, unknown>"
	case node.object.openness == objectTyped:
		members := []string{t.TypeOf(node.object.additional)}
		for _, name := range t.propertyNames(node) {
			members = append(members, t.TypeOf(node.object.property(name)))
		}
		index := typeScriptRecord + "<string, " + unionOf(members) + ">"
		if len(fields) == 0 {
			return index
		}
		return object + " & " + index
	default:
		if len(fields) == 0 {
			return typeScriptRecord + "<string, never>"
		}
		return object
	}
}

func optionalMark(node *projectionNode, name string) string {
	if node.required(name) {
		return ""
	}
	return "?"
}

func (t *typeScript) literalType(raw jsontext.Value) string {
	switch raw.Kind() {
	case '0':
		return t.numberType()
	case '[':
		var values []jsontext.Value
		if err := json.Unmarshal(raw, &values); err != nil {
			panic(err)
		}
		members := make([]string, len(values))
		for i, value := range values {
			members[i] = t.literalType(value)
		}
		return "[" + strings.Join(members, ", ") + "]"
	case '{':
		var values map[string]jsontext.Value
		if err := json.Unmarshal(raw, &values); err != nil {
			panic(err)
		}
		var fields []string
		for _, name := range slices.Sorted(maps.Keys(values)) {
			fields = append(fields, PropertyKey(name)+": "+t.literalType(values[name]))
		}
		return "{ " + strings.Join(fields, "; ") + " }"
	default:
		return string(raw)
	}
}

var identifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// JSON escapes preserve JavaScript string values; Go's \a and \U escapes do not.
func TypeScriptStringLiteral(value string) string {
	encoded, err := json.Marshal(value, jsontext.EscapeForJS(true))
	if err != nil {
		panic(fmt.Errorf("contractgen: invalid typescript string: %w", err))
	}
	return string(encoded)
}
func PropertyKey(name string) string {
	if identifier.MatchString(name) {
		return name
	}
	return TypeScriptStringLiteral(name)
}

// Computed keys preserve own data members even for JavaScript prototype names.
func RuntimePropertyKey(name string) string {
	return "[" + TypeScriptStringLiteral(name) + "]"
}
func QuoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = TypeScriptStringLiteral(value)
	}
	return out
}

// The TypeScript top and bottom types absorb or vanish in unions and
// intersections.
const (
	tsUnknown = "unknown"
	tsNever   = "never"
)

func unionOf(parts []string) string {
	var members []string
	for _, part := range parts {
		if part == tsUnknown {
			return tsUnknown
		}
		if part != tsNever && !slices.Contains(members, part) {
			members = append(members, part)
		}
	}
	if len(members) == 0 {
		return tsNever
	}
	return strings.Join(members, " | ")
}
func intersectionOf(parts []string) string {
	var members []string
	for _, part := range parts {
		if part == tsNever {
			return tsNever
		}
		if part == tsUnknown || slices.Contains(members, part) {
			continue
		}
		members = append(members, part)
	}
	if len(members) == 0 {
		return tsUnknown
	}
	if len(members) == 1 {
		return members[0]
	}
	for i, member := range members {
		members[i] = "(" + member + ")"
	}
	return strings.Join(members, " & ")
}
