package portable

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"strings"

	"github.com/Tangerg/scope/core/jsonschema"
)

// Schema owns profile admission and bounded evaluation preflight. Scope remains
// the compiler and the only owner of JSON Schema validation semantics.
type Schema struct {
	compiled jsonschema.Schema
	work     []int
	unique   bool
}

func AdmitSchema(raw []byte) (Schema, error) {
	if err := CheckDescriptor(raw); err != nil {
		return Schema{}, err
	}
	value, err := decodeBoundedJSON(raw)
	if err != nil {
		return Schema{}, err
	}
	work, unique, err := schemaWork(value)
	if err != nil {
		return Schema{}, err
	}
	compiled, err := jsonschema.Parse(raw)
	if err != nil {
		return Schema{}, err
	}
	return Schema{compiled: compiled, work: work, unique: unique}, nil
}

func (s Schema) Validate(raw []byte) error {
	if !s.compiled.Valid() || len(s.work) == 0 {
		return jsonschema.ErrInvalid
	}
	size, err := checkJSON(raw, 0)
	if err != nil {
		return err
	}
	if s.unique && size.equality > ProfileLimits().MaxEqualityWork {
		return limit("schema equality work", ProfileLimits().MaxEqualityWork)
	}
	// The precomputed bound explores every schema branch, independently of its
	// predicate. Object names also count because propertyNames can validate them.
	maximum := ProfileLimits().MaxValidationWork
	if s.work[size.depth] > maximum/(size.nodes+size.keys) {
		return limit("schema validation work", maximum)
	}
	return s.compiled.Validate(raw)
}

type schemaNode struct {
	same, descend []int
	local         int
	unique        bool
}

type schemaReference struct {
	source int
	path   string
}

var scalarSchemaKeywords = map[string]bool{
	"$schema": true, "$comment": true, "title": true, "description": true,
	"default": true, "examples": true, "readOnly": true, "writeOnly": true,
	"deprecated": true, "contentEncoding": true, "contentMediaType": true,
	"type": true, "enum": true, "const": true, "required": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true,
	"exclusiveMaximum": true, "multipleOf": true, "minLength": true,
	"maxLength": true, "pattern": true, "format": true, "minItems": true,
	"maxItems": true, "uniqueItems": true, "minProperties": true, "maxProperties": true,
}

// schemaGraph counts possible schema traversal, not predicates or verdicts.
// A property/item edge (descend) consumes instance depth; a ref/composition
// edge (same) does not. Only cycles of the latter are forbidden, preserving
// structural recursion.
type schemaGraph struct {
	limits     Limits
	nodes      []schemaNode
	paths      map[string]int
	references []schemaReference
}

func schemaWork(value any) ([]int, bool, error) {
	graph := schemaGraph{limits: ProfileLimits(), paths: map[string]int{}}
	if _, err := graph.visit(value, ""); err != nil {
		return nil, false, err
	}
	if err := graph.resolveReferences(); err != nil {
		return nil, false, err
	}
	order, err := graph.expansionOrder()
	if err != nil {
		return nil, false, err
	}
	return graph.work(order), graph.unique(), nil
}

func (g *schemaGraph) visit(value any, path string) (int, error) {
	index := len(g.nodes)
	g.nodes = append(g.nodes, schemaNode{local: 1})
	g.paths[path] = index
	if _, ok := value.(bool); ok {
		return index, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return 0, errors.New("portable: schema must be an object or boolean")
	}
	for key, field := range object {
		if err := g.visitKeyword(index, key, field, path+"/"+pointerToken(key)); err != nil {
			return 0, err
		}
	}
	return index, nil
}

func (g *schemaGraph) visitKeyword(index int, key string, field any, path string) error {
	switch key {
	case "$ref":
		return g.reference(index, field)
	case "$defs", "definitions", "properties", "patternProperties":
		return g.visitMap(index, key, field, path)
	case "additionalProperties", "items", "contains", "propertyNames":
		return g.visitChild(index, field, path, true)
	case "not", "if", "then", "else":
		return g.visitChild(index, field, path, false)
	case "allOf", "anyOf", "oneOf":
		return g.visitComposition(index, field, path)
	case "$schema":
		if field != "https://json-schema.org/draft/2020-12/schema" {
			return errors.New("portable: unsupported schema dialect")
		}
	case "pattern":
		pattern, ok := field.(string)
		if !ok {
			return errors.New("portable: schema pattern must be a string")
		}
		return checkPattern(pattern)
	case "format":
		return checkFormat(field)
	case "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties":
		number, ok := field.(json.Number)
		if !ok {
			return fmt.Errorf("portable: %s must be a nonnegative integer", key)
		}
		size, ok := new(big.Rat).SetString(number.String())
		if !ok || !size.IsInt() || size.Sign() < 0 || !size.Num().IsInt64() || size.Num().Int64() > g.limits.MaxSizeConstraint {
			return fmt.Errorf("portable: %s must be an integer between 0 and %d", key, g.limits.MaxSizeConstraint)
		}
	case "enum":
		return g.enum(index, field)
	case "const":
		g.nodes[index].local += int(equalityWeight(field))
	case "uniqueItems":
		g.nodes[index].unique, _ = field.(bool)
	default:
		if !scalarSchemaKeywords[key] {
			return fmt.Errorf("portable: unsupported schema keyword %q", key)
		}
	}
	return nil
}

// visitChild links a subschema that applies to a nested instance (descend) or
// to the same instance.
func (g *schemaGraph) visitChild(index int, field any, path string, descend bool) error {
	child, err := g.visit(field, path)
	if err != nil {
		return err
	}
	if descend {
		g.nodes[index].descend = append(g.nodes[index].descend, child)
	} else {
		g.nodes[index].same = append(g.nodes[index].same, child)
	}
	return nil
}

func (g *schemaGraph) visitComposition(index int, field any, path string) error {
	children, ok := field.([]any)
	if !ok || len(children) == 0 {
		return errors.New("portable: schema composition requires a nonempty array")
	}
	for n, child := range children {
		if err := g.visitChild(index, child, path+"/"+strconv.Itoa(n), false); err != nil {
			return err
		}
	}
	return nil
}

func (g *schemaGraph) enum(index int, field any) error {
	values, ok := field.([]any)
	if !ok || len(values) == 0 {
		return errors.New("portable: enum requires a nonempty array")
	}
	weight := equalityWeight(field) - 1
	if equalityCost(0, len(values)-1, weight) > g.limits.MaxEqualityWork {
		return limit("schema equality work", g.limits.MaxEqualityWork)
	}
	g.nodes[index].local += int(weight)
	return nil
}

func (g *schemaGraph) reference(index int, field any) error {
	ref, ok := field.(string)
	if !ok || ref != "#" && !strings.HasPrefix(ref, "#/") {
		return errors.New("portable: schema references must be local JSON pointers")
	}
	decoded, err := url.PathUnescape(ref[1:])
	if err != nil {
		return fmt.Errorf("portable: schema reference: %w", err)
	}
	g.references = append(g.references, schemaReference{source: index, path: decoded})
	if len(g.references) > g.limits.MaxReferences {
		return limit("schema references", g.limits.MaxReferences)
	}
	return nil
}

func (g *schemaGraph) visitMap(index int, key string, field any, path string) error {
	children, ok := field.(map[string]any)
	if !ok {
		return errors.New("portable: schema map must be an object")
	}
	for name, child := range children {
		if key == "patternProperties" {
			if err := checkPattern(name); err != nil {
				return err
			}
		}
		childIndex, err := g.visit(child, path+"/"+pointerToken(name))
		if err != nil {
			return err
		}
		if key == "properties" || key == "patternProperties" {
			g.nodes[index].descend = append(g.nodes[index].descend, childIndex)
		}
	}
	return nil
}

func (g *schemaGraph) resolveReferences() error {
	for _, ref := range g.references {
		target, ok := g.paths[ref.path]
		if !ok {
			return errors.New("portable: unresolved schema reference")
		}
		g.nodes[ref.source].same = append(g.nodes[ref.source].same, target)
	}
	return nil
}

// expansionOrder orders nodes so every same-value target precedes its source,
// refusing a same-value cycle and bounding its expansion.
func (g *schemaGraph) expansionOrder() ([]int, error) {
	state := make([]byte, len(g.nodes))
	expansion := make([]int, len(g.nodes))
	depths := make([]int, len(g.nodes))
	var order []int
	var expand func(int) error
	expand = func(index int) error {
		if state[index] == 1 {
			return errors.New("portable: schema reference or composition cycle does not consume instance depth")
		}
		if state[index] == 2 {
			return nil
		}
		state[index] = 1
		expansion[index], depths[index] = 1, 1
		for _, child := range g.nodes[index].same {
			if err := expand(child); err != nil {
				return err
			}
			expansion[index] += expansion[child]
			depths[index] = max(depths[index], 1+depths[child])
			if expansion[index] > g.limits.MaxReferenceWork {
				return limit("same-value schema expansion", g.limits.MaxReferenceWork)
			}
			if depths[index] > g.limits.MaxReferenceDepth {
				return limit("same-value schema depth", g.limits.MaxReferenceDepth)
			}
		}
		state[index] = 2
		order = append(order, index)
		return nil
	}
	for index := range g.nodes {
		if err := expand(index); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// work bounds validation cost per instance depth with saturating dynamic
// programming, which bounds recursion without unfolding it. Summing every
// possible child and multiplying by instance nodes is deliberately
// conservative; admission never executes schema predicates to guess a cheaper path.
func (g *schemaGraph) work(order []int) []int {
	work := make([]int, g.limits.MaxJSONDepth+1)
	previous := make([]int, len(g.nodes))
	for depth := range work {
		current := make([]int, len(g.nodes))
		for _, index := range order {
			cost := g.nodes[index].local
			for _, child := range g.nodes[index].same {
				cost = min(g.limits.MaxValidationWork+1, cost+current[child])
			}
			if depth > 0 {
				for _, child := range g.nodes[index].descend {
					cost = min(g.limits.MaxValidationWork+1, cost+previous[child])
				}
			}
			current[index] = cost
		}
		work[depth] = current[0]
		previous = current
	}
	return work
}

// unique reports whether any schema reachable from the root requires unique items.
func (g *schemaGraph) unique() bool {
	visited := make([]bool, len(g.nodes))
	queue := []int{0}
	for len(queue) > 0 {
		index := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if visited[index] {
			continue
		}
		visited[index] = true
		if g.nodes[index].unique {
			return true
		}
		queue = append(queue, g.nodes[index].same...)
		queue = append(queue, g.nodes[index].descend...)
	}
	return false
}

func equalityWeight(value any) uint64 {
	weight := uint64(1)
	switch value := value.(type) {
	case string:
		weight += uint64(len(value))
	case json.Number:
		weight += uint64(len(value))
	case []any:
		for _, child := range value {
			weight += equalityWeight(child)
		}
	case map[string]any:
		for key, child := range value {
			weight += uint64(max(1, len(key))) + equalityWeight(child)
		}
	}
	return weight
}

func pointerToken(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
