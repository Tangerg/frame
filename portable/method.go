package portable

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	framevalue "github.com/Tangerg/frame/value"
)

// Protocol identifies public wire behavior, independently of private releases.
// Only the current contract is admitted; a source relocation does not change it.
const Protocol = "io.github.tangerg.frame/portable/1"

type MethodKind string

const (
	Query    MethodKind = "query"
	Mutation MethodKind = "command"
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
	input      Schema
	output     Schema
	stream     Schema
}

// ValidToken reports whether a portable name fits the profile and has no spaces or controls.
func ValidToken(value string) bool {
	return value != "" && len(value) <= ProfileLimits().MaxTokenBytes && utf8.ValidString(value) && !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func AdmitMethod(descriptor MethodDescriptor) (*Method, error) {
	if !ValidToken(descriptor.Name) || (descriptor.Target != "" && !ValidToken(descriptor.Target)) {
		return nil, errors.New("capability: method name and declared target must be bounded tokens")
	}
	if descriptor.Kind != Query && descriptor.Kind != Mutation {
		return nil, errors.New("capability: method kind must be query or command")
	}
	if err := checkMethodDescriptor(descriptor); err != nil {
		return nil, err
	}
	if err := checkEncodedDescriptor(descriptor); err != nil {
		return nil, err
	}
	descriptor = descriptor.clone()
	input, err := AdmitSchema(descriptor.Input)
	if err != nil {
		return nil, fmt.Errorf("capability: %s input contract: %w", descriptor.Name, err)
	}
	if input.Validate([]byte("null")) == nil {
		return nil, errors.New("capability: method input must exclude null")
	}
	output, err := AdmitSchema(descriptor.Output)
	if err != nil {
		return nil, fmt.Errorf("capability: %s output contract: %w", descriptor.Name, err)
	}
	var stream Schema
	if len(descriptor.Stream) != 0 {
		stream, err = AdmitSchema(descriptor.Stream)
		if err != nil {
			return nil, fmt.Errorf("capability: %s stream contract: %w", descriptor.Name, err)
		}
	}
	// Presentation prose does not change callable identity. Exact schemas do.
	identity := descriptor.clone()
	identity.Description = ""
	canonical, err := framevalue.EncodeJSON(identity)
	if err != nil {
		return nil, err
	}
	return &Method{descriptor: descriptor, identity: canonical, input: input, output: output, stream: stream}, nil
}

func checkMethodDescriptor(descriptor MethodDescriptor) error {
	if len(descriptor.Description) > ProfileLimits().MaxDescriptorBytes {
		return limit("method description bytes", ProfileLimits().MaxDescriptorBytes)
	}
	for _, raw := range []jsontext.Value{descriptor.Input, descriptor.Output, descriptor.Stream} {
		if len(raw) != 0 {
			if err := CheckDescriptor(raw); err != nil {
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

func (m *Method) ValidateChunk(chunk jsontext.Value) error {
	if len(m.descriptor.Stream) == 0 {
		return errors.New("capability: method does not support streaming")
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
	chunk, err := SchemaFor[Chunk]()
	if err != nil {
		return nil, err
	}
	return deriveMethod[Input, Output](config, chunk)
}

func deriveMethod[Input, Output any](config MethodConfig, chunk jsontext.Value) (*Method, error) {
	input, err := SchemaFor[Input]()
	if err != nil {
		return nil, err
	}
	output, err := SchemaFor[Output]()
	if err != nil {
		return nil, err
	}
	return AdmitMethod(MethodDescriptor{Name: config.Name, Kind: config.Kind, Target: config.Target, Description: config.Description, Input: input, Output: output, Stream: chunk})
}
