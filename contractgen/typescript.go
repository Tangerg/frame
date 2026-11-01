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

// TypeScript renders one schema graph; callers finish definitions before rendering.
type TypeScript struct{ defs Definitions }

func NewTypeScript(definitions Definitions) *TypeScript { return &TypeScript{defs: definitions} }

func (t *TypeScript) Definition(name string, node *Schema) string {
	writer := &typeScriptWriter{TypeScript: t}
	writer.define(name, node)
	return writer.out.String()
}

type typeScriptWriter struct {
	*TypeScript
	out strings.Builder
}

func (t *typeScriptWriter) line(format string, arguments ...any) {
	fmt.Fprintf(&t.out, format+"\n", arguments...)
}

func (t *typeScriptWriter) define(name string, node *Schema) {
	if len(node.AllOf) > 0 {
		base := *node
		base.AllOf = nil
		if expression := t.TypeOf(node); expression != t.TypeOf(&base) {
			t.line("export type %s = %s;", name, expression)
			t.line("")
			return
		}
	}
	switch {
	case len(node.Enum) > 0:
		t.line("export type %s = %s;", name, unionOf(QuoteAll(node.Enum)))
	case len(node.OneOf) > 0 && len(node.Properties) > 0:
		t.line("export type %s =", name)
		for _, branch := range node.OneOf {
			t.line("  | %s", t.branch(node, branch))
		}
		t.trimTrailingNewline()
		t.out.WriteString(";\n")
	case node.Type != SchemaTypeObject || len(node.AnyOf) > 0 || len(node.OneOf) > 0 || node.Ref != "" || node.TypeScriptType != "" || node.AdditionalProps != nil:
		t.line("export type %s = %s;", name, t.TypeOf(node))
	default:
		t.line("export interface %s {", name)
		t.out.WriteString(t.ObjectBody(node, ""))
		t.line("}")
	}
	t.line("")
}

// objectBody renders an interface's fields. substitute, when set, is the type name
// to replace with the generic parameter T.
func (t *TypeScript) ObjectBody(node *Schema, substitute string) string {
	var out strings.Builder
	for _, name := range slices.Sorted(maps.Keys(node.Properties)) {
		child, ok := node.Properties[name].(*Schema)
		if !ok {
			continue
		}
		rendered := t.TypeOf(child)
		if substitute != "" {
			rendered = substituteType(rendered, substitute)
		}
		fmt.Fprintf(&out, "  %s%s: %s;\n", PropertyKey(name), optionalMark(node, name), rendered)
	}
	return out.String()
}

// branch renders one variant of a discriminated union: the shared fields this tag permits,
// with the discriminator pinned and the other tags' fields gone.
func (t *TypeScript) branch(unionSchema, variantSchema *Schema) string {
	var fields []string
	for _, name := range discriminatorFirst(unionSchema, variantSchema) {
		unionField, ok := unionSchema.Properties[name].(*Schema)
		if !ok {
			continue
		}
		override, mentioned := variantSchema.Properties[name]
		if excluded, ok := override.(bool); mentioned && ok && !excluded {
			// The boolean schema `false`: this tag may not carry the field at all.
			continue
		}
		rendered := t.TypeOf(unionField)
		if narrowing, ok := override.(*Schema); mentioned && ok {
			rendered = t.narrow(unionField, narrowing)
		}
		fields = append(fields, fmt.Sprintf("%s%s: %s", PropertyKey(name), optionalMark(variantSchema, name), rendered))
	}
	return "{ " + strings.Join(fields, "; ") + " }"
}

// narrow applies a branch's constraint on a nested frame: the tag pins the
// discriminator to a literal, or claims some of the frame's fields and forbids the
// rest.
func (t *TypeScript) narrow(baseChild, narrowing *Schema) string {
	if narrowing.Const != "" {
		return TypeScriptStringLiteral(narrowing.Const)
	}
	if len(narrowing.Enum) > 0 {
		return unionOf(QuoteAll(narrowing.Enum))
	}
	if narrowing.TypeScriptType != "" {
		return narrowing.TypeScriptType
	}
	frame := t.resolve(baseChild)
	if frame == nil {
		return t.TypeOf(baseChild)
	}
	var fields []string
	for _, name := range slices.Sorted(maps.Keys(frame.Properties)) {
		child, ok := frame.Properties[name].(*Schema)
		if !ok {
			continue
		}
		override, mentioned := narrowing.Properties[name]
		if excluded, ok := override.(bool); mentioned && ok && !excluded {
			continue
		}
		rendered := t.TypeOf(child)
		if narrowed, ok := override.(*Schema); mentioned && ok {
			rendered = t.narrow(child, narrowed)
		}
		optional := "?"
		if slices.Contains(frame.Required, name) || slices.Contains(narrowing.Required, name) {
			optional = ""
		}
		fields = append(fields, fmt.Sprintf("%s%s: %s", PropertyKey(name), optional, rendered))
	}
	return "{ " + strings.Join(fields, "; ") + " }"
}

// resolve follows a reference to the definition it names.
func (t *TypeScript) resolve(node *Schema) *Schema {
	name, ok := strings.CutPrefix(node.Ref, RefPrefix)
	if !ok {
		return nil
	}
	return t.defs[name]
}

// typeOf renders a schema node as a TypeScript type expression.
func (t *TypeScript) TypeOf(node *Schema) string {
	if len(node.AllOf) > 0 {
		base := *node
		base.AllOf = nil
		parts := []string{t.TypeOf(&base)}
		for _, member := range node.AllOf {
			parts = append(parts, t.TypeOf(member))
		}
		return intersectionOf(parts)
	}
	if node.TypeScriptType != "" {
		return node.TypeScriptType
	}
	if node.Ref != "" {
		name, _ := strings.CutPrefix(node.Ref, RefPrefix)
		return name
	}
	if len(node.AnyOf) > 0 {
		parts := make([]string, 0, len(node.AnyOf))
		for _, member := range node.AnyOf {
			parts = append(parts, t.TypeOf(member))
		}
		return unionOf(parts)
	}
	if len(node.OneOf) > 0 {
		parts := make([]string, 0, len(node.OneOf))
		for _, member := range node.OneOf {
			parts = append(parts, t.TypeOf(member))
		}
		return unionOf(parts)
	}
	if len(node.Enum) > 0 {
		return unionOf(QuoteAll(node.Enum))
	}
	names := typeKeywords(node.Type)
	if len(names) == 0 {
		// An empty schema is any JSON value. `unknown` forces the reader to narrow,
		// which is exactly right for an opaque passthrough.
		return "unknown"
	}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, t.keyword(name, node))
	}
	return unionOf(parts)
}

func (t *TypeScript) keyword(name SchemaType, node *Schema) string {
	switch name {
	case SchemaTypeString:
		return "string"
	case SchemaTypeInteger, SchemaTypeNumber:
		return "number"
	case SchemaTypeBoolean:
		return "boolean"
	case SchemaTypeNull:
		return "null"
	case SchemaTypeArray:
		if node.Items == nil {
			return "unknown[]"
		}
		element := t.TypeOf(node.Items)
		if strings.Contains(element, "|") {
			return "(" + element + ")[]"
		}
		return element + "[]"
	case SchemaTypeObject:
		if len(node.Properties) == 0 {
			if child, ok := node.AdditionalProps.(*Schema); ok {
				return "Record<string, " + t.TypeOf(child) + ">"
			}
			return "Record<string, never>"
		}
		var fields []string
		for _, property := range slices.Sorted(maps.Keys(node.Properties)) {
			child, ok := node.Properties[property].(*Schema)
			if !ok {
				continue
			}
			fields = append(fields, fmt.Sprintf("%s%s: %s", PropertyKey(property), optionalMark(node, property), t.TypeOf(child)))
		}
		object := "{ " + strings.Join(fields, "; ") + " }"
		if child, ok := node.AdditionalProps.(*Schema); ok {
			return object + " & Record<string, " + t.TypeOf(child) + ">"
		}
		return object
	default:
		panic("contractgen: no TypeScript type for JSON type " + string(name))
	}
}

// substituteType replaces a concrete type name with the generic parameter. It
// matches whole identifiers so `Session` in `SessionRef` is left alone.
func substituteType(rendered, concrete string) string {
	return regexp.MustCompile(`\b`+regexp.QuoteMeta(concrete)+`\b`).ReplaceAllString(rendered, "T")
}

func optionalMark(owner *Schema, property string) string {
	if slices.Contains(owner.Required, property) {
		return ""
	}
	return "?"
}

// typeKeywords keeps the type-emission loop uniform for an optional keyword.
func typeKeywords(value SchemaType) []SchemaType {
	if value == "" {
		return nil
	}
	return []SchemaType{value}
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
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, TypeScriptStringLiteral(value))
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
	for index, member := range members {
		if strings.Contains(member, "|") {
			members[index] = "(" + member + ")"
		}
	}
	return strings.Join(members, " & ")
}

// discriminatorFirst orders a variant's fields with the tag in front. A reader
// scanning a union wants to see which case each line is before its payload, and
// the tag is recognizable without being named: it is the field the branch pins to
// a literal.
func discriminatorFirst(unionSchema, variantSchema *Schema) []string {
	names := slices.Sorted(maps.Keys(unionSchema.Properties))
	slices.SortStableFunc(names, func(a, b string) int {
		return boolCompare(isPinned(variantSchema, b), isPinned(variantSchema, a))
	})
	return names
}

func isPinned(branch *Schema, name string) bool {
	pinned, ok := branch.Properties[name].(*Schema)
	return ok && pinned.Const != ""
}

func boolCompare(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}

func (t *typeScriptWriter) trimTrailingNewline() {
	text := strings.TrimSuffix(t.out.String(), "\n")
	t.out.Reset()
	t.out.WriteString(text)
}
