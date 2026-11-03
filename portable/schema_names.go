package portable

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"maps"
	"net/url"
	"path"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/Tangerg/frame/value"
	"github.com/Tangerg/scope/core/jsonschema"
)

const definitionPointer = "/$defs/"

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
	definitions, ok := document[KeywordDefs].(map[string]any)
	if !ok {
		return bytes.Clone(raw), nil
	}
	renamed := readableNames(slices.Collect(maps.Keys(definitions)))
	rewriteSchemaReferences(document, renamed, ProfileSchema())
	canonical := make(map[string]any, len(definitions))
	for name, body := range definitions {
		canonical[renamed[name]] = body
	}
	document[KeywordDefs] = canonical
	return jsonv2.Marshal(document, jsonv2.Deterministic(true))
}

// Schema-looking strings in const, enum, examples and default are application values.
func rewriteSchemaReferences(value any, names map[string]string, profile SchemaProfile) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, child := range object {
		switch key {
		case KeywordRef:
			ref, ok := child.(string)
			if ok {
				object[key] = readableReference(ref, names)
			}
		default:
			switch profile.Keywords[key].Children {
			case SchemaMap:
				children, _ := child.(map[string]any)
				for _, schema := range children {
					rewriteSchemaReferences(schema, names, profile)
				}
			case SchemaArray:
				children, _ := child.([]any)
				for _, schema := range children {
					rewriteSchemaReferences(schema, names, profile)
				}
			case SchemaChild:
				rewriteSchemaReferences(child, names, profile)
			}
		}
	}
}

func readableReference(ref string, names map[string]string) string {
	if !strings.HasPrefix(ref, "#") {
		return ref
	}
	fragment, err := url.PathUnescape(ref[1:])
	if err != nil {
		return ref
	}
	definition, rooted := strings.CutPrefix(fragment, definitionPointer)
	if !rooted {
		return ref
	}
	root, suffix, nested := strings.Cut(definition, "/")
	root = strings.NewReplacer("~1", "/", "~0", "~").Replace(root)
	name, found := names[root]
	if !found || name == root {
		return ref
	}
	fragment = definitionPointer + pointerToken(name)
	if nested {
		fragment += "/" + suffix
	}
	return (&url.URL{Fragment: fragment}).String()
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

// Base64url and Go type names may both contain '_', so the separator is the first
// one after which the prefix decodes to an import path and the rest begins a type name.
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

// Every contract, tool and point must derive the same name for a Go type.
func SchemaFor[T any]() ([]byte, error) {
	if err := value.ValidateType(reflect.TypeFor[T]()); err != nil {
		return nil, err
	}
	derived, err := jsonschema.For[T]()
	if err != nil {
		return nil, err
	}
	return ReadableDefinitions(derived.JSON())
}
