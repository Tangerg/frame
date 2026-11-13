package value

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A missing projection has no proven finite bound; an empty Values slice is a
// closed empty vocabulary. Pointer stays in its original document scope.
type StringEnumeration struct {
	Pointer string
	Values  []string
}

// Renderers must not implement a second predicate evaluator. Candidate literals
// only establish a finite bound; the admitted schema decides every membership.
func ProjectStringEnums(raw []byte) ([]StringEnumeration, error) {
	document, err := newSchemaDocument(raw)
	if err != nil {
		return nil, err
	}
	// Every candidate is an admitted string: one value at depth one. Compute
	// that profile bound once instead of rebuilding every depth for each field.
	scalarWork := document.graph.workAtDepth(document.order, document.graph.workAtDepth(document.order, nil))
	var enumerations []StringEnumeration
	for _, pointer := range slices.Sorted(maps.Keys(document.graph.paths)) {
		if !document.graph.nodes[document.graph.paths[pointer]].stringOnly {
			continue
		}
		schema := document.root
		if pointer != "" {
			location := schemaResourceURL + (&url.URL{Fragment: pointer}).String()
			schema, err = document.compiler.Compile(location)
			if err != nil {
				return nil, err
			}
		}
		members, err := stringEnum(schema, scalarWork[document.graph.paths[pointer]])
		if err != nil {
			return nil, fmt.Errorf("portable: string enum %q: %w", pointer, err)
		}
		if members != nil {
			enumerations = append(enumerations, StringEnumeration{Pointer: pointer, Values: members})
		}
	}
	return enumerations, nil
}

func stringEnum(schema *jsonschema.Schema, work int) ([]string, error) {
	candidates := finiteStringCandidates(schema, make(map[*jsonschema.Schema]bool))
	if candidates == nil {
		return nil, nil
	}
	if len(candidates) > 0 && work > PortableLimits().MaxValidationWork {
		return nil, NewLimitError("schema validation work", PortableLimits().MaxValidationWork)
	}
	members := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if err := schema.Validate(candidate); err != nil {
			var rejection *jsonschema.ValidationError
			if !errors.As(err, &rejection) {
				return nil, err
			}
			continue
		}
		members = append(members, candidate)
	}
	return members, nil
}

func finiteStringCandidates(schema *jsonschema.Schema, visiting map[*jsonschema.Schema]bool) []string {
	if schema == nil || visiting[schema] {
		return nil
	}
	visiting[schema] = true
	defer delete(visiting, schema)
	if schema.Bool != nil && !*schema.Bool || schema.Types != nil && !slices.Contains(schema.Types.ToStrings(), string(TypeString)) {
		return []string{}
	}
	if schema.Enum != nil {
		members := []string{}
		for _, value := range schema.Enum.Values {
			if member, ok := value.(string); ok && !slices.Contains(members, member) {
				members = append(members, member)
			}
		}
		return members
	}
	if schema.Const != nil {
		if member, ok := (*schema.Const).(string); ok {
			return []string{member}
		}
		return []string{}
	}
	if members := finiteStringCandidates(schema.Ref, visiting); members != nil {
		return members
	}
	for _, child := range schema.AllOf {
		if members := finiteStringCandidates(child, visiting); members != nil {
			return members
		}
	}
	for _, branches := range [][]*jsonschema.Schema{schema.AnyOf, schema.OneOf} {
		if members := finiteBranchCandidates(branches, visiting); members != nil {
			return members
		}
	}
	return nil
}

func finiteBranchCandidates(branches []*jsonschema.Schema, visiting map[*jsonschema.Schema]bool) []string {
	if len(branches) == 0 {
		return nil
	}
	members := []string{}
	for _, branch := range branches {
		candidates := finiteStringCandidates(branch, visiting)
		if candidates == nil {
			return nil
		}
		for _, member := range candidates {
			if !slices.Contains(members, member) {
				members = append(members, member)
			}
		}
	}
	return members
}
