package mcp

import (
	json "encoding/json/v2"
	"fmt"
	"maps"
	"slices"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

type toolSchema struct {
	value  value.JSON
	export string
}

type toolRequirement struct {
	input    toolSchema
	output   *toolSchema
	readOnly bool
}

// A server has one tool per name even when several exports expose it.
// Full-result views impose no requirement on its structured output schema.
type toolRequirements map[string]toolRequirement

func (r toolRequirements) admit(export string, member *toolMember) error {
	descriptor := member.method.Descriptor()
	if member.method.Streaming() {
		return fmt.Errorf("%w: MCP tool %q must be unary", frame.ErrContractMismatch, descriptor.Name)
	}
	var schema struct {
		Type value.SchemaType `json:"type"`
	}
	if err := json.Unmarshal(descriptor.Input, &schema); err != nil || schema.Type != value.SchemaTypeObject {
		return fmt.Errorf("%w: MCP tool %q needs an object input schema", frame.ErrContractMismatch, descriptor.Name)
	}
	input, err := value.ParseJSON(descriptor.Input)
	if err != nil {
		return err
	}
	required := toolRequirement{input: toolSchema{value: input, export: export}, readOnly: descriptor.Kind == frame.Query}
	if member.result == StructuredResult {
		output, err := value.ParseJSON(descriptor.Output)
		if err != nil {
			return err
		}
		required.output = &toolSchema{value: output, export: export}
	}
	if prior, exists := r[descriptor.Name]; exists {
		if prior.input.value != required.input.value {
			return fmt.Errorf("%w: MCP tool %q input schemas conflict between exports %q and %q", frame.ErrContractMismatch, descriptor.Name, prior.input.export, export)
		}
		if prior.output != nil && required.output != nil && prior.output.value != required.output.value {
			return fmt.Errorf("%w: MCP tool %q output schemas conflict between exports %q and %q", frame.ErrContractMismatch, descriptor.Name, prior.output.export, export)
		}
		required.input = prior.input
		if prior.output != nil {
			required.output = prior.output
		}
		required.readOnly = prior.readOnly || required.readOnly
	}
	r[descriptor.Name] = required
	return nil
}

// bindPage binds one page's declared tools; found carries the names earlier
// pages bound, so a tool repeated across pages is refused.
func (r toolRequirements) bindPage(tools []*sdk.Tool, found map[string]bool) error {
	for _, tool := range tools {
		if tool == nil {
			return fmt.Errorf("%w: null MCP tool", frame.ErrContractMismatch)
		}
		required, declared := r[tool.Name]
		if !declared {
			continue
		}
		if found[tool.Name] {
			return fmt.Errorf("%w: duplicate MCP tool %q", frame.ErrContractMismatch, tool.Name)
		}
		if err := required.bind(tool); err != nil {
			return err
		}
		found[tool.Name] = true
	}
	return nil
}

func (r toolRequirements) requireAll(found map[string]bool) error {
	for _, name := range slices.Sorted(maps.Keys(r)) {
		if !found[name] {
			return fmt.Errorf("%w: missing MCP tool %q", frame.ErrContractMismatch, name)
		}
	}
	return nil
}

func (r toolRequirement) bind(tool *sdk.Tool) error {
	if r.readOnly && (tool.Annotations == nil || !tool.Annotations.ReadOnlyHint) {
		return fmt.Errorf("%w: MCP tool %q does not declare read-only operation", frame.ErrContractMismatch, tool.Name)
	}
	if !matchesSchema(r.input.value, tool.InputSchema) {
		return fmt.Errorf("%w: MCP tool %q input schema differs", frame.ErrContractMismatch, tool.Name)
	}
	if r.output != nil && !matchesSchema(r.output.value, tool.OutputSchema) {
		return fmt.Errorf("%w: MCP tool %q output schema differs", frame.ErrContractMismatch, tool.Name)
	}
	return nil
}

func matchesSchema(declared value.JSON, advertised any) bool {
	actual, err := sdkValue(advertised)
	return err == nil && declared == actual
}
