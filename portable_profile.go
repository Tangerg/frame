package frame

import (
	"reflect"

	"github.com/Tangerg/frame/value"
)

// DescriptorShapes projects the member sets of published descriptors and the
// method kinds to the Node SDK, which admits descriptors with exactly these.
type DescriptorShapes struct {
	Capability  value.Shape `json:"capability"`
	Method      value.Shape `json:"method"`
	MethodKinds []string    `json:"methodKinds"`
}

func ProfileDescriptors() DescriptorShapes {
	return DescriptorShapes{
		Capability:  value.ShapeOf(reflect.TypeFor[CapabilityDescriptor]()),
		Method:      value.ShapeOf(reflect.TypeFor[MethodDescriptor]()),
		MethodKinds: MethodKind("").JSONSchemaEnum(),
	}
}

// The callable limits project Frame's semantic owners alongside the pure value budget.
type PortableProfileLimits struct {
	value.PortableProfileLimits
	MaxMethods             int `json:"maxMethods"`
	MaxTokenBytes          int `json:"maxTokenBytes"`
	MaxCommandIDCharacters int `json:"maxCommandIDCharacters"`
}

// PortableProfile is everything another runtime needs to admit and validate
// portable contracts exactly as Go does.
type PortableProfile struct {
	Protocol    string                `json:"protocol"`
	Limits      PortableProfileLimits `json:"limits"`
	Pattern     value.PatternSyntax   `json:"pattern"`
	Formats     value.StringFormats   `json:"formats"`
	Schema      value.SchemaProfile   `json:"schema"`
	Descriptors DescriptorShapes      `json:"descriptors"`
}

func CurrentPortableProfile() PortableProfile {
	return PortableProfile{
		Protocol: CapabilityProtocol,
		Limits: PortableProfileLimits{
			PortableProfileLimits:  value.PortableLimits(),
			MaxMethods:             MaxCapabilityMethods,
			MaxTokenBytes:          MaxNameBytes,
			MaxCommandIDCharacters: MaxCommandIDCharacters,
		},
		Pattern:     value.ProfilePatternSyntax(),
		Formats:     value.ProfileStringFormats(),
		Schema:      value.ProfileSchema(),
		Descriptors: ProfileDescriptors(),
	}
}
