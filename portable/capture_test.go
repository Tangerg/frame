package portable_test

import (
	"testing"

	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

func TestCapabilityOwnsTheMethodBehindItsIdentity(t *testing.T) {
	method, err := portable.NewMethod[string, string](portable.MethodConfig{Name: "read", Kind: portable.Query})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := portable.NewCapability("test.capture", method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	identity := capability.Identity()
	borrowed, ok := capability.Method(method.Name())
	if !ok {
		t.Fatal("admitted method is missing")
	}
	replacement, err := portable.NewMethod[int, bool](portable.MethodConfig{Name: "read", Kind: portable.Command})
	if err != nil {
		t.Fatal(err)
	}
	*borrowed = *replacement
	retained, ok := capability.Method(method.Name())
	if !ok || retained.Identity() != method.Identity() || capability.Identity() != identity {
		t.Fatal("method projection replaced the semantics behind the published identity")
	}
	input, err := value.EncodeJSON("content")
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := portable.NewQueryInvocation(method.Name(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := retained.ValidateInvocation(invocation); err != nil {
		t.Fatalf("original invocation was rejected after replacing a projection: %v", err)
	}
}
