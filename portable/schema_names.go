package portable

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/Tangerg/scope/core/jsonschema"
)

const definitionRef = "#/$defs/"

// ReadableDefinitions projects unambiguous reflector names; colliding declarations
// retain their original identities so cosmetic aliases cannot change a contract.
func ReadableDefinitions(raw []byte) ([]byte, error) {
	if err := CheckDescriptor(raw); err != nil {
		return nil, err
	}
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("portable: schema: %w", err)
	}
	definitions, ok := document["$defs"].(map[string]any)
	if !ok {
		return bytes.Clone(raw), nil
	}
	renamed := readableNames(slices.Collect(maps.Keys(definitions)))
	rewriteSchemaReferences(document, renamed)
	canonical := make(map[string]any, len(definitions))
	for name, body := range definitions {
		canonical[renamed[name]] = body
	}
	document["$defs"] = canonical
	return jsonv2.Marshal(document, jsonv2.Deterministic(true))
}

// Schema-looking strings in const, enum, examples and default are application values.
func rewriteSchemaReferences(value any, names map[string]string) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, child := range object {
		switch key {
		case "$ref", "$dynamicRef":
			ref, ok := child.(string)
			if !ok || !strings.HasPrefix(ref, definitionRef) {
				continue
			}
			root, suffix, nested := strings.Cut(strings.TrimPrefix(ref, definitionRef), "/")
			if name, found := names[root]; found {
				ref = definitionRef + name
				if nested {
					ref += "/" + suffix
				}
				object[key] = ref
			}
		case "$defs", "definitions", "properties", "patternProperties", "dependentSchemas":
			children, _ := child.(map[string]any)
			for _, schema := range children {
				rewriteSchemaReferences(schema, names)
			}
		case "allOf", "anyOf", "oneOf", "prefixItems":
			children, _ := child.([]any)
			for _, schema := range children {
				rewriteSchemaReferences(schema, names)
			}
		case "additionalProperties", "items", "contains", "propertyNames", "not", "if", "then", "else", "unevaluatedItems", "unevaluatedProperties", "contentSchema":
			rewriteSchemaReferences(child, names)
		}
	}
}

func readableNames(keys []string) map[string]string {
	names := make(map[string]string, len(keys))
	counts := make(map[string]int, len(keys))
	for _, key := range keys {
		name := key
		if pkg, typeName := goDefinitionSource(key); pkg != "" {
			name = strings.ReplaceAll(path.Base(pkg), ".", "_") + "." + typeName
		}
		names[key] = name
		counts[name]++
	}
	for key, name := range names {
		if _, reserved := names[name]; counts[name] > 1 || reserved && name != key {
			names[key] = key
		}
	}
	return names
}

// goDefinitionSource reads the package path and type name from a reflector
// key; any other key is its own name. Base64url and Go type names may both
// contain '_', so the separator is the first one after which the prefix
// decodes to an import path and the rest begins a type name.
func goDefinitionSource(key string) (pkg, name string) {
	encoded, ok := strings.CutPrefix(key, "go_")
	if !ok {
		return "", key
	}
	for index := strings.IndexByte(encoded, '_'); index >= 0; {
		decoded, err := base64.RawURLEncoding.DecodeString(encoded[:index])
		rest := encoded[index+1:]
		if err == nil && importPath(string(decoded)) && rest != "" && unicode.IsLetter([]rune(rest)[0]) {
			return string(decoded), rest
		}
		next := strings.IndexByte(rest, '_')
		if next < 0 {
			break
		}
		index += next + 1
	}
	return "", key
}

func importPath(value string) bool {
	return value != "" && !strings.ContainsFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("./-_~+", r))
	})
}

// SchemaFor derives T's JSON Schema with readable definition names. It is the
// one way Go types become schemas, so every contract, tool and point names a
// type the same way.
func SchemaFor[T any]() ([]byte, error) {
	derived, err := jsonschema.For[T]()
	if err != nil {
		return nil, err
	}
	return ReadableDefinitions(derived.JSON())
}
