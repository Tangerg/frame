package contractgen

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	framevalue "github.com/Tangerg/frame/value"
)

const RefPrefix = "#/$defs/"
const SchemaDialect = "https://json-schema.org/draft/2020-12/schema"

type schemaType string

const (
	typeArray   schemaType = "array"
	typeBoolean schemaType = "boolean"
	typeInteger schemaType = "integer"
	typeNull    schemaType = "null"
	typeNumber  schemaType = "number"
	typeObject  schemaType = "object"
	typeString  schemaType = "string"
)

type objectOpenness uint8

const (
	objectOpen objectOpenness = iota
	objectClosed
	objectTyped
)

type objectProjection struct {
	properties map[string]*projectionNode
	required   []string
	openness   objectOpenness
	additional *projectionNode
	patterns   bool
}

// One admitted schema graph supplies both renderers. Missing additionalProperties
// is open; boolean schemas and reference targets never share a primitive sentinel.
type projectionNode struct {
	kind          schemaType
	kinds         []schemaType
	forbidden     bool
	reference     *projectionNode
	referencePath string
	object        objectProjection
	items         *projectionNode
	literals      []jsontext.Value
	allOf         []*projectionNode
	anyOf         []*projectionNode
}

type projection struct {
	definitions map[string]*projectionNode
	names       map[*projectionNode]string
	paths       map[string]*projectionNode
}

type schemaDocument struct {
	Type              jsontext.Value            `json:"type"`
	Ref               string                    `json:"$ref"`
	Properties        map[string]jsontext.Value `json:"properties"`
	PatternProperties map[string]jsontext.Value `json:"patternProperties"`
	Definitions       map[string]jsontext.Value `json:"definitions"`
	Defs              map[string]jsontext.Value `json:"$defs"`
	Required          []string                  `json:"required"`
	Additional        jsontext.Value            `json:"additionalProperties"`
	Items             jsontext.Value            `json:"items"`
	AllOf             []jsontext.Value          `json:"allOf"`
	AnyOf             []jsontext.Value          `json:"anyOf"`
	OneOf             []jsontext.Value          `json:"oneOf"`
	Enum              []jsontext.Value          `json:"enum"`
	Const             jsontext.Value            `json:"const"`
	Not               jsontext.Value            `json:"not"`
	If                jsontext.Value            `json:"if"`
	Then              jsontext.Value            `json:"then"`
	Else              jsontext.Value            `json:"else"`
	Contains          jsontext.Value            `json:"contains"`
	PropertyNames     jsontext.Value            `json:"propertyNames"`
}

func newProjection(definitions map[string]jsontext.Value) (*projection, error) {
	p := &projection{definitions: make(map[string]*projectionNode), names: make(map[*projectionNode]string), paths: make(map[string]*projectionNode)}
	for _, name := range slices.Sorted(maps.Keys(definitions)) {
		node, err := p.parse(definitions[name], "/$defs/"+pointerToken(name))
		if err != nil {
			return nil, err
		}
		p.definitions[name], p.names[node] = node, name
	}
	for _, path := range slices.Sorted(maps.Keys(p.paths)) {
		node := p.paths[path]
		if node.referencePath == "" {
			continue
		}
		targetPath, err := referencePath(node.referencePath)
		if err != nil {
			return nil, err
		}
		target := p.paths[targetPath]
		if target == nil {
			return nil, fmt.Errorf("feature contracts: unresolved schema reference %q", node.referencePath)
		}
		node.reference = target
		if p.names[target] != "" {
			continue
		}
		tokens, err := pointerTokens(targetPath)
		if err != nil {
			return nil, err
		}
		name := goFeatureIdentifier(strings.Join(tokens[1:], " "))
		if p.definitions[name] != nil {
			name += "Reference" + framevalue.Hash([]byte(targetPath)).String()
			if p.definitions[name] != nil {
				return nil, fmt.Errorf("feature contracts: referenced node name collision %s", name)
			}
		}
		p.names[target], p.definitions[name] = name, target
	}
	return p, nil
}

func (p *projection) parse(raw jsontext.Value, path string) (*projectionNode, error) {
	n := &projectionNode{}
	p.paths[path] = n
	if string(raw) == "true" {
		return n, nil
	}
	if string(raw) == "false" {
		n.forbidden = true
		return n, nil
	}
	var wire schemaDocument
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	n.referencePath = wire.Ref
	n.literals = wire.Enum
	if len(wire.Const) != 0 {
		n.allOf = append(n.allOf, &projectionNode{literals: []jsontext.Value{wire.Const}})
	}
	var kinds []schemaType
	if len(wire.Type) != 0 {
		if err := json.Unmarshal(wire.Type, &n.kind); err != nil {
			if err := json.Unmarshal(wire.Type, &kinds); err != nil {
				return nil, err
			}
		}
	}
	n.object.required = slices.Clone(wire.Required)
	n.object.properties = make(map[string]*projectionNode, len(wire.Properties))
	n.object.patterns = len(wire.PatternProperties) != 0
	for _, entry := range []struct {
		keyword  string
		children map[string]jsontext.Value
	}{
		{"properties", wire.Properties}, {"patternProperties", wire.PatternProperties}, {"$defs", wire.Defs}, {"definitions", wire.Definitions},
	} {
		for _, name := range slices.Sorted(maps.Keys(entry.children)) {
			child, err := p.parse(entry.children[name], path+"/"+entry.keyword+"/"+pointerToken(name))
			if err != nil {
				return nil, err
			}
			if entry.keyword == "properties" {
				n.object.properties[name] = child
			}
		}
	}
	for _, entry := range []struct {
		keyword string
		raw     jsontext.Value
		target  **projectionNode
	}{
		{"additionalProperties", wire.Additional, &n.object.additional}, {"items", wire.Items, &n.items},
		{"not", wire.Not, nil}, {"if", wire.If, nil}, {"then", wire.Then, nil}, {"else", wire.Else, nil}, {"contains", wire.Contains, nil}, {"propertyNames", wire.PropertyNames, nil},
	} {
		if len(entry.raw) == 0 {
			continue
		}
		child, err := p.parse(entry.raw, path+"/"+entry.keyword)
		if err != nil {
			return nil, err
		}
		if entry.target != nil {
			*entry.target = child
		}
	}
	if n.object.additional != nil {
		if n.object.additional.forbidden {
			n.object.openness = objectClosed
		} else if string(wire.Additional) != "true" {
			n.object.openness = objectTyped
		}
	}
	for _, entry := range []struct {
		keyword  string
		children []jsontext.Value
		target   *[]*projectionNode
	}{
		{"allOf", wire.AllOf, &n.allOf}, {"anyOf", wire.AnyOf, &n.anyOf}, {"oneOf", wire.OneOf, &n.anyOf},
	} {
		if len(entry.children) == 0 {
			continue
		}
		var children []*projectionNode
		for index, raw := range entry.children {
			child, err := p.parse(raw, fmt.Sprintf("%s/%s/%d", path, entry.keyword, index))
			if err != nil {
				return nil, err
			}
			children = append(children, child)
		}
		if entry.keyword == "oneOf" && len(wire.AnyOf) > 0 {
			n.allOf = append(n.allOf, &projectionNode{anyOf: children})
		} else {
			*entry.target = append(*entry.target, children...)
		}
	}
	n.kinds = kinds
	return n, nil
}

func (n *projectionNode) required(name string) bool { return slices.Contains(n.object.required, name) }

func (n *projectionNode) pureReference() bool {
	return n.reference != nil && n.kind == "" && len(n.kinds) == 0 && !n.forbidden && len(n.literals) == 0 && len(n.allOf) == 0 && len(n.anyOf) == 0 && len(n.object.properties) == 0 && len(n.object.required) == 0 && n.object.openness == objectOpen && n.items == nil
}

func (n *projectionNode) stringEnum() []string {
	var values []string
	if n.literals != nil {
		values = []string{}
		for _, raw := range n.literals {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				values = append(values, value)
			}
		}
	}
	for _, child := range n.allOf {
		members := child.stringEnum()
		if members == nil {
			continue
		}
		if values == nil {
			values = members
		} else {
			values = slices.DeleteFunc(values, func(value string) bool { return !slices.Contains(members, value) })
		}
	}
	return values
}

func pointerToken(value string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(value)
}
func referencePath(ref string) (string, error) {
	if !strings.HasPrefix(ref, "#") {
		return "", fmt.Errorf("feature contracts: nonlocal reference %q", ref)
	}
	return url.PathUnescape(strings.TrimPrefix(ref, "#"))
}
func pointerTokens(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("feature contracts: invalid JSON pointer %q", path)
	}
	tokens := strings.Split(path[1:], "/")
	for i, token := range tokens {
		tokens[i] = strings.NewReplacer("~1", "/", "~0", "~").Replace(token)
	}
	return tokens, nil
}

func (o objectProjection) impossible() bool {
	if o.patterns || o.openness != objectClosed {
		return false
	}
	for _, name := range o.required {
		if o.properties[name] == nil || o.properties[name].forbidden {
			return true
		}
	}
	return false
}

func (o objectProjection) constrained() bool {
	return len(o.properties) > 0 || len(o.required) > 0 || o.openness != objectOpen || o.patterns
}

func (o objectProjection) property(name string) *projectionNode {
	if property := o.properties[name]; property != nil {
		return property
	}
	if !o.patterns && o.openness == objectTyped {
		return o.additional
	}
	return nil
}

func (n *projectionNode) contextualKind(inherited schemaType) (schemaType, schemaType) {
	kind, contextKind := n.kind, n.kind
	if contextKind == "" {
		contextKind = inherited
	}
	if contextKind == "" {
		for target := n.reference; target != nil; target = target.reference {
			if target.kind != "" {
				contextKind = target.kind
				break
			}
		}
	}
	if kind == "" {
		if contextKind == typeObject && n.object.constrained() {
			kind = typeObject
		}
		if contextKind == typeArray && n.items != nil {
			kind = typeArray
		}
	}
	return kind, contextKind
}
