package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

const ToolsProfile frame.Profile = "mcp-tools"

// Tools matches each declared unary member to an MCP tool of the same name.
// MaxCatalogPages must be positive and bounds discovery, including empty pages.
type Tools struct {
	MaxCatalogPages int
	// FullResult selects members that return the entire MCP result envelope,
	// including unstructured content. By default members return structuredContent
	// and require an exact advertised output schema. Selection is frozen at setup.
	FullResult func(frame.ExportDeclaration, string) bool
}

func (Tools) Profile() frame.Profile { return ToolsProfile }

func (p Tools) Prepare(setup *frame.Setup, metadata process.ExportMetadata, maximum int) (process.Connection, error) {
	if setup == nil || maximum <= 0 || p.MaxCatalogPages <= 0 {
		return nil, fmt.Errorf("%w: MCP tools requires a setup and positive limits", frame.ErrInvalid)
	}
	unit := setup.Unit()
	if len(unit.Imports) != 0 {
		return nil, fmt.Errorf("%w: standard MCP tools cannot implement Frame imports", frame.ErrContractMismatch)
	}
	c := &toolsConnection{maximum: maximum, pages: p.MaxCatalogPages, required: make(map[string][]*toolMember)}
	for _, export := range unit.Exports {
		contract, err := process.DeclaredCapability(setup, export.Contract)
		if err != nil {
			return nil, err
		}
		service := &toolService{connection: c, members: make(map[string]*toolMember)}
		for _, descriptor := range contract.Descriptor().Methods {
			if len(descriptor.Stream) != 0 {
				return nil, fmt.Errorf("%w: MCP tool %q must be unary", frame.ErrContractMismatch, descriptor.Name)
			}
			var schema struct{ Type string }
			if err := json.Unmarshal(descriptor.Input, &schema); err != nil || schema.Type != "object" {
				return nil, fmt.Errorf("%w: MCP tool %q needs an object input schema", frame.ErrContractMismatch, descriptor.Name)
			}
			method, _ := contract.Method(descriptor.Name)
			member := &toolMember{method: method, fullResult: p.FullResult != nil && p.FullResult(export, method.Name())}
			service.members[method.Name()] = member
			c.required[method.Name()] = append(c.required[method.Name()], member)
		}
		c.values = append(c.values, metadata.Export(export, service))
	}
	return c, nil
}

type toolsConnection struct {
	maximum  int
	pages    int
	required map[string][]*toolMember
	values   []frame.ExportValue
	session  *sdk.ClientSession
}

func (c *toolsConnection) Connect(ctx context.Context, channel io.ReadWriteCloser) ([]frame.ExportValue, error) {
	session, err := connect(ctx, channel, c.maximum, nil)
	if err != nil {
		return nil, err
	}
	c.session = session
	caps := session.InitializeResult().Capabilities
	if caps == nil || caps.Tools == nil {
		return nil, fmt.Errorf("%w: peer has no tools capability", frame.ErrContractMismatch)
	}
	if err := c.discover(ctx); err != nil {
		return nil, err
	}
	return c.values, nil
}

// Discovery can verify or refuse the frozen contract, never add a callable name.
func (c *toolsConnection) discover(ctx context.Context) error {
	found := make(map[string]bool, len(c.required))
	cursor := ""
	for range c.pages {
		page, err := c.session.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return err
		}
		for _, tool := range page.Tools {
			if tool == nil {
				return fmt.Errorf("%w: null MCP tool", frame.ErrContractMismatch)
			}
			members := c.required[tool.Name]
			if len(members) == 0 {
				continue
			}
			if found[tool.Name] {
				return fmt.Errorf("%w: duplicate MCP tool %q", frame.ErrContractMismatch, tool.Name)
			}
			for _, member := range members {
				if err := member.bind(tool); err != nil {
					return err
				}
			}
			found[tool.Name] = true
		}
		if page.NextCursor == "" {
			for name := range c.required {
				if !found[name] {
					return fmt.Errorf("%w: missing MCP tool %q", frame.ErrContractMismatch, name)
				}
			}
			return nil
		}
		if page.NextCursor == cursor {
			return fmt.Errorf("%w: repeated MCP tools cursor", frame.ErrContractMismatch)
		}
		cursor = page.NextCursor
	}
	return fmt.Errorf("%w: MCP tools catalog exceeds %d pages", frame.ErrCapacity, c.pages)
}

func (c *toolsConnection) Close(context.Context) error {
	if c.session != nil {
		return c.session.Close()
	}
	return nil
}

type toolMember struct {
	method     *portable.Method
	fullResult bool
}

func (m *toolMember) bind(tool *sdk.Tool) error {
	d := m.method.Descriptor()
	if d.Kind == portable.Query && (tool.Annotations == nil || !tool.Annotations.ReadOnlyHint) {
		return fmt.Errorf("%w: MCP tool %q does not declare read-only operation", frame.ErrContractMismatch, d.Name)
	}
	if !matchesSchema(d.Input, tool.InputSchema) {
		return fmt.Errorf("%w: MCP tool %q input schema differs", frame.ErrContractMismatch, d.Name)
	}
	if !m.fullResult && (tool.OutputSchema == nil || !matchesSchema(d.Output, tool.OutputSchema)) {
		return fmt.Errorf("%w: MCP tool %q output schema differs", frame.ErrContractMismatch, d.Name)
	}
	return nil
}

func matchesSchema(declared []byte, advertised any) bool {
	want, err := value.ParseJSON(declared)
	if err != nil {
		return false
	}
	got, err := sdkValue(advertised)
	return err == nil && want == got
}

// Preserve the SDK's json.Number and its content-specific wire marshaling.
func sdkValue(v any) (value.JSON, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return value.JSON{}, err
	}
	return value.ParseJSON(data)
}

type toolService struct {
	connection *toolsConnection
	members    map[string]*toolMember
}

// ToolError is an MCP tool's explicit failure. Result includes its content and metadata.
type ToolError struct{ Result value.JSON }

func (*ToolError) Error() string { return "MCP tool reported an error" }

func (s *toolService) Call(ctx context.Context, name string, input value.JSON) (value.JSON, error) {
	if ctx == nil {
		return value.JSON{}, frame.ErrInvalid
	}
	member := s.members[name]
	if member == nil {
		return value.JSON{}, fmt.Errorf("%w: undeclared MCP member %q", frame.ErrContractMismatch, name)
	}
	if err := member.method.ValidateInput(input); err != nil {
		return value.JSON{}, errors.Join(frame.ErrContractMismatch, err)
	}
	raw := input.Bytes()
	if len(raw) == 0 || raw[0] != '{' {
		return value.JSON{}, fmt.Errorf("%w: MCP tool arguments must be an object", frame.ErrContractMismatch)
	}
	if err := context.Cause(ctx); err != nil {
		return value.JSON{}, err
	}
	result, err := s.connection.session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: json.RawMessage(raw)})
	if err != nil {
		return value.JSON{}, errors.Join(process.ErrOutcomeUnknown, err, context.Cause(ctx))
	}
	if result.NeedsInput() {
		return value.JSON{}, fmt.Errorf("%w: MCP tool requested an unsupported input round", process.ErrOutcomeUnknown)
	}
	var payload any = result
	if result.IsError {
		reply, err := sdkValue(result)
		if err != nil {
			return value.JSON{}, errors.Join(process.ErrOutcomeUnknown, err)
		}
		return value.JSON{}, &ToolError{Result: reply}
	}
	if !member.fullResult {
		if result.StructuredContent == nil {
			return value.JSON{}, fmt.Errorf("%w: MCP tool %q omitted structuredContent", process.ErrOutcomeUnknown, name)
		}
		payload = result.StructuredContent
	}
	reply, err := sdkValue(payload)
	if err == nil {
		err = member.method.ValidateOutput(reply.Bytes())
	}
	if err != nil {
		return value.JSON{}, errors.Join(process.ErrOutcomeUnknown, err)
	}
	return reply, nil
}
