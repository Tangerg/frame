package contractgen

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

type typeScript struct{ graph *projection }

func (t *typeScript) Definition(name string, node *projectionNode) string {
	if node.kind == typeObject && !node.object.impossible() && node.reference == nil && len(node.allOf) == 0 && len(node.anyOf) == 0 && len(node.literals) == 0 && node.object.openness == objectClosed && !node.object.patterns && len(node.object.properties) > 0 {
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

func (t *typeScript) expression(node *projectionNode, inherited schemaType) string {
	if node == nil {
		return "unknown"
	}
	if node.forbidden {
		return "never"
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
		name := t.graph.names[node.reference]
		parts = append(parts, name)
	}
	if len(node.literals) > 0 {
		literals := make([]string, len(node.literals))
		for i, value := range node.literals {
			literals[i] = literalType(value)
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

func (t *typeScript) keyword(kind, contextKind schemaType, node *projectionNode) string {
	switch kind {
	case typeString:
		return "string"
	case typeInteger, typeNumber:
		return "JSONNumber"
	case typeBoolean:
		return "boolean"
	case typeNull:
		return "null"
	case typeArray:
		// Generic array syntax owns its grouping even for unions and intersections.
		return "Array<" + t.TypeOf(node.items) + ">"
	case typeObject:
		return t.object(node)
	default:
		if contextKind != "" {
			return "unknown"
		}
		var parts []string
		// Applicator keywords constrain only their instance kind. A nullable or
		// unresolved reference must retain both that constraint and the other kinds.
		if node.object.constrained() {
			parts = append(parts, unionOf([]string{"string", "JSONNumber", "boolean", "null", "Array<unknown>", t.object(node)}))
		}
		if node.items != nil {
			parts = append(parts, unionOf([]string{"string", "JSONNumber", "boolean", "null", "Record<string, unknown>", "Array<" + t.TypeOf(node.items) + ">"}))
		}
		return intersectionOf(parts)
	}
}

func (t *typeScript) object(node *projectionNode) string {
	if node.object.impossible() {
		return "never"
	}
	var fields []string
	for _, name := range t.propertyNames(node) {
		fields = append(fields, fmt.Sprintf("%s%s: %s", PropertyKey(name), optionalMark(node, name), t.TypeOf(node.object.property(name))))
	}
	object := "{ " + strings.Join(fields, "; ") + " }"
	switch {
	case node.object.patterns || node.object.openness == objectOpen:
		if len(fields) == 0 {
			return "Record<string, unknown>"
		}
		return object + " & Record<string, unknown>"
	case node.object.openness == objectTyped:
		members := []string{t.TypeOf(node.object.additional)}
		for _, name := range t.propertyNames(node) {
			members = append(members, t.TypeOf(node.object.property(name)))
		}
		index := "Record<string, " + unionOf(members) + ">"
		if len(fields) == 0 {
			return index
		}
		return object + " & " + index
	default:
		if len(fields) == 0 {
			return "Record<string, never>"
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

func literalType(raw jsontext.Value) string {
	switch raw.Kind() {
	case '0':
		return "JSONNumber"
	case '[':
		var values []jsontext.Value
		if err := json.Unmarshal(raw, &values); err != nil {
			panic(err)
		}
		members := make([]string, len(values))
		for i, value := range values {
			members[i] = literalType(value)
		}
		return "[" + strings.Join(members, ", ") + "]"
	case '{':
		var values map[string]jsontext.Value
		if err := json.Unmarshal(raw, &values); err != nil {
			panic(err)
		}
		var fields []string
		for _, name := range slices.Sorted(maps.Keys(values)) {
			fields = append(fields, PropertyKey(name)+": "+literalType(values[name]))
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
func QuoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = TypeScriptStringLiteral(value)
	}
	return out
}

func unionOf(parts []string) string {
	var members []string
	for _, part := range parts {
		if part == "unknown" {
			return "unknown"
		}
		if part != "never" && !slices.Contains(members, part) {
			members = append(members, part)
		}
	}
	if len(members) == 0 {
		return "never"
	}
	return strings.Join(members, " | ")
}
func intersectionOf(parts []string) string {
	var members []string
	for _, part := range parts {
		if part == "never" {
			return "never"
		}
		if part == "unknown" || slices.Contains(members, part) {
			continue
		}
		members = append(members, part)
	}
	if len(members) == 0 {
		return "unknown"
	}
	if len(members) == 1 {
		return members[0]
	}
	for i, member := range members {
		members[i] = "(" + member + ")"
	}
	return strings.Join(members, " & ")
}
