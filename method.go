package frame

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"

	framevalue "github.com/Tangerg/frame/value"
)

// CapabilityProtocol identifies public wire behavior, independently of private releases.
// Only the current contract is admitted; a source relocation does not change it.
const CapabilityProtocol = "io.github.tangerg.frame/portable/1"

type MethodKind string

const (
	Query   MethodKind = "query"
	Command MethodKind = "command"
)

type MethodDescriptor struct {
	Name        string         `json:"name"`
	Kind        MethodKind     `json:"kind"`
	Target      string         `json:"target,omitzero"`
	Description string         `json:"description"`
	Input       jsontext.Value `json:"input"`
	Output      jsontext.Value `json:"output"`
	Stream      jsontext.Value `json:"stream,omitzero"`
}

func (d MethodDescriptor) clone() MethodDescriptor {
	d.Input = append(jsontext.Value(nil), d.Input...)
	d.Output = append(jsontext.Value(nil), d.Output...)
	d.Stream = append(jsontext.Value(nil), d.Stream...)
	return d
}

type Method struct {
	descriptor MethodDescriptor
	identity   framevalue.JSON
	input      framevalue.Schema
	output     framevalue.Schema
	stream     framevalue.Schema
}

func AdmitMethod(descriptor MethodDescriptor) (*Method, error) {
	if !ValidName(descriptor.Name) || (descriptor.Target != "" && !ValidName(descriptor.Target)) {
		return nil, errors.New("frame: method name and declared target must be bounded tokens")
	}
	if !slices.Contains(descriptor.Kind.JSONSchemaEnum(), string(descriptor.Kind)) {
		return nil, errors.New("frame: method kind must be query or command")
	}
	if err := checkMethodDescriptor(descriptor); err != nil {
		return nil, err
	}
	if err := framevalue.CheckDescriptorValue(descriptor); err != nil {
		return nil, err
	}
	descriptor = descriptor.clone()
	input, err := framevalue.AdmitSchema(descriptor.Input)
	if err != nil {
		return nil, fmt.Errorf("frame: %s input contract: %w", descriptor.Name, err)
	}
	if input.Validate([]byte("null")) == nil {
		return nil, errors.New("frame: method input must exclude null")
	}
	output, err := framevalue.AdmitSchema(descriptor.Output)
	if err != nil {
		return nil, fmt.Errorf("frame: %s output contract: %w", descriptor.Name, err)
	}
	var stream framevalue.Schema
	if len(descriptor.Stream) != 0 {
		stream, err = framevalue.AdmitSchema(descriptor.Stream)
		if err != nil {
			return nil, fmt.Errorf("frame: %s stream contract: %w", descriptor.Name, err)
		}
	}
	method := &Method{descriptor: descriptor, input: input, output: output, stream: stream}
	canonical, err := framevalue.EncodeJSON(method.identityModel().Descriptor())
	if err != nil {
		return nil, err
	}
	method.identity = canonical
	return method, nil
}

// Presentation prose does not change callable identity. Exact schemas do.
func (m *Method) identityModel() *Method {
	model := new(*m)
	model.descriptor.Description = ""
	return model
}

func checkMethodDescriptor(descriptor MethodDescriptor) error {
	if len(descriptor.Description) > framevalue.PortableLimits().MaxDescriptorBytes {
		return framevalue.NewLimitError("method description bytes", framevalue.PortableLimits().MaxDescriptorBytes)
	}
	for _, raw := range []jsontext.Value{descriptor.Input, descriptor.Output, descriptor.Stream} {
		if len(raw) != 0 {
			if err := framevalue.CheckDescriptor(raw); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Method) Name() string                 { return m.descriptor.Name }
func (m *Method) Kind() MethodKind             { return m.descriptor.Kind }
func (m *Method) Descriptor() MethodDescriptor { return m.descriptor.clone() }
func (m *Method) Identity() framevalue.JSON    { return m.identity }

func (m *Method) ValidateInput(input framevalue.JSON) error {
	if err := input.Validate(); err != nil {
		return err
	}
	return m.input.Validate(input.Bytes())
}

func (m *Method) ValidateOutput(output jsontext.Value) error {
	return m.output.Validate(output)
}

// Streaming reports whether the contract declares a chunk schema; callers pair
// it with the presence of an emit callback before dispatch.
func (m *Method) Streaming() bool { return len(m.descriptor.Stream) != 0 }

func (m *Method) ValidateChunk(chunk jsontext.Value) error {
	if !m.Streaming() {
		return errors.New("frame: method does not support streaming")
	}
	return m.stream.Validate(chunk)
}

type MethodConfig struct {
	Name        string
	Kind        MethodKind
	Target      string
	Description string
}

func NewMethod[Input, Output any](config MethodConfig) (*Method, error) {
	return deriveMethod[Input, Output](config, nil)
}

func NewStreamingMethod[Input, Chunk, Output any](config MethodConfig) (*Method, error) {
	chunk, err := framevalue.SchemaFor[Chunk]()
	if err != nil {
		return nil, err
	}
	return deriveMethod[Input, Output](config, chunk)
}

func deriveMethod[Input, Output any](config MethodConfig, chunk jsontext.Value) (*Method, error) {
	input, err := framevalue.SchemaFor[Input]()
	if err != nil {
		return nil, err
	}
	output, err := framevalue.SchemaFor[Output]()
	if err != nil {
		return nil, err
	}
	return AdmitMethod(MethodDescriptor{Name: config.Name, Kind: config.Kind, Target: config.Target, Description: config.Description, Input: input, Output: output, Stream: chunk})
}

func (MethodKind) JSONSchemaEnum() []string { return []string{string(Query), string(Command)} }
