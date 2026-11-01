package portable

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Tangerg/frame/jsonvalue"
)

type Composition string

const (
	Collection Composition = "collection"
	Selection  Composition = "selection"
	Decoration Composition = "decoration"
)

type PointOrdering string

const (
	CanonicalKeys PointOrdering = "canonicalKeys"
	ExplicitList  PointOrdering = "explicitList"
)

func (PointOrdering) JSONSchemaEnum() []string {
	return []string{string(CanonicalKeys), string(ExplicitList)}
}

type PointDescriptor struct {
	Protocol      string         `json:"protocol"`
	Reference     string         `json:"reference"`
	Composition   Composition    `json:"composition"`
	Scope         string         `json:"scope"`
	Optional      bool           `json:"optional"`
	Ordering      PointOrdering  `json:"ordering"`
	Value         jsontext.Value `json:"value"`
	Applicability jsontext.Value `json:"applicability"`
}

func (d PointDescriptor) clone() PointDescriptor {
	d.Value = slices.Clone(d.Value)
	d.Applicability = slices.Clone(d.Applicability)
	return d
}

type Point struct {
	descriptor    PointDescriptor
	identity      jsonvalue.Value
	value         Schema
	applicability Schema
}

func ParsePoint(data []byte) (*Point, error) {
	if err := CheckDescriptor(data); err != nil {
		return nil, err
	}
	var descriptor PointDescriptor
	if err := jsonvalue.DecodeValue(data, &descriptor, "point"); err != nil {
		return nil, err
	}
	return AdmitPoint(descriptor)
}

func AdmitPoint(descriptor PointDescriptor) (*Point, error) {
	if descriptor.Protocol != Protocol || !validToken(descriptor.Reference) || !validToken(descriptor.Scope) {
		return nil, errors.New("point: current protocol, reference and scope are required")
	}
	if descriptor.Composition != Collection && descriptor.Composition != Selection && descriptor.Composition != Decoration {
		return nil, errors.New("point: unknown composition")
	}
	if descriptor.Ordering != CanonicalKeys && descriptor.Ordering != ExplicitList || descriptor.Composition != Collection && descriptor.Ordering != ExplicitList {
		return nil, errors.New("point: selection and decoration require explicit composition")
	}
	for _, raw := range []jsontext.Value{descriptor.Value, descriptor.Applicability} {
		if err := CheckDescriptor(raw); err != nil {
			return nil, err
		}
	}
	if err := checkEncodedDescriptor(descriptor); err != nil {
		return nil, err
	}
	descriptor = descriptor.clone()
	value, err := AdmitSchema(descriptor.Value)
	if err != nil {
		return nil, fmt.Errorf("point: value schema: %w", err)
	}
	applicability, err := AdmitSchema(descriptor.Applicability)
	if err != nil {
		return nil, fmt.Errorf("point: applicability schema: %w", err)
	}
	identity, err := jsonvalue.Encode(descriptor)
	if err != nil {
		return nil, err
	}
	return &Point{descriptor: descriptor, identity: identity, value: value, applicability: applicability}, nil
}

func (c *Point) Definition() PointDescriptor { return c.descriptor.clone() }
func (c *Point) Identity() jsonvalue.Value   { return c.identity }
func (c *Point) ValidateValue(value jsonvalue.Value) error {
	if err := value.Validate(); err != nil {
		return err
	}
	return c.value.Validate(value.JSON())
}

// Applicability uses a frozen caller projection so it cannot discover or mutate
// a screen, workspace or business owner while resolving contributions.
type PointContext struct {
	Target     Target         `json:"target"`
	Attributes jsontext.Value `json:"attributes"`
}

func (c *Point) Applicable(context PointContext) (bool, error) {
	if err := context.Target.Validate(); err != nil {
		return false, err
	}
	if context.Target.Scope != c.descriptor.Scope {
		return false, errors.New("point: target scope differs from its contract")
	}
	attributes, err := jsonvalue.Parse(context.Attributes)
	if err != nil {
		return false, err
	}
	context.Attributes = attributes.JSON()
	input, err := jsonvalue.Encode(context)
	if err != nil {
		return false, err
	}
	if err := c.applicability.Validate(input.JSON()); err != nil {
		if errors.Is(err, ErrLimit) {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

type PointValue struct {
	Key  string
	Data jsonvalue.Value
}

// Compose returns a detached, frozen value projection. Explicit decoration
// order is outermost first; it never implies that wrappers have executed.
func (c *Point) Compose(values []PointValue, order []string) ([]PointValue, error) {
	byKey := make(map[string]PointValue, len(values))
	for _, value := range values {
		if value.Key == "" || len(value.Key) > 512 {
			return nil, errors.New("point: canonical contribution key is required")
		}
		if _, exists := byKey[value.Key]; exists {
			return nil, fmt.Errorf("point: duplicate contribution %s", value.Key)
		}
		if err := c.ValidateValue(value.Data); err != nil {
			return nil, fmt.Errorf("point: contribution %s: %w", value.Key, err)
		}
		byKey[value.Key] = value
	}
	if len(values) == 0 && !c.descriptor.Optional {
		return nil, errors.New("point: required point has no applicable contributions")
	}
	if c.descriptor.Ordering == CanonicalKeys {
		if len(order) > 0 {
			return nil, errors.New("point: canonical collection rejects an explicit order")
		}
		result := slices.Clone(values)
		slices.SortFunc(result, func(a, b PointValue) int { return strings.Compare(a.Key, b.Key) })
		return result, nil
	}
	if c.descriptor.Composition == Selection && len(order) > 1 {
		return nil, errors.New("point: selection requires one explicit contribution")
	}
	result := make([]PointValue, 0, len(order))
	seen := make(map[string]bool, len(order))
	for _, key := range order {
		value, found := byKey[key]
		if !found || seen[key] {
			return nil, fmt.Errorf("point: unknown or duplicate ordered contribution %s", key)
		}
		seen[key] = true
		result = append(result, value)
	}
	if c.descriptor.Composition == Collection && len(result) != len(values) {
		return nil, errors.New("point: explicit order omits an applicable contribution")
	}
	if len(result) == 0 && !c.descriptor.Optional {
		return nil, errors.New("point: required point needs an explicit composition")
	}
	return result, nil
}
