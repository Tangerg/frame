package portable

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
		MethodKinds: MethodKind("").Values(),
	}
}

// RuntimeProfile is everything another runtime needs to admit and validate
// portable contracts exactly as Go does.
type RuntimeProfile struct {
	Protocol    string           `json:"protocol"`
	Limits      Limits           `json:"limits"`
	Pattern     PatternSyntax    `json:"pattern"`
	Formats     StringFormats    `json:"formats"`
	Schema      SchemaProfile    `json:"schema"`
	Descriptors DescriptorShapes `json:"descriptors"`
}

func CurrentRuntimeProfile() RuntimeProfile {
	return RuntimeProfile{
		Protocol:    Protocol,
		Limits:      ProfileLimits(),
		Pattern:     ProfilePatternSyntax(),
		Formats:     ProfileStringFormats(),
		Schema:      ProfileSchema(),
		Descriptors: ProfileDescriptors(),
	}
}
