package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

type missingContext struct{ context.Context }

func TestManagedBoundariesRejectAMissingContext(t *testing.T) {
	method, err := frame.NewMethod[string, string](frame.MethodConfig{Name: "read", Kind: frame.Query, Target: "entry"})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := frame.NewCapability("context.boundary", method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	service := &ManagedService{contract: &ManagedContract{capability: capability}}
	input, err := value.EncodeJSON("input")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		call func(context.Context) error
	}{
		{name: "invocation evidence", call: func(ctx context.Context) error {
			_, err := WithInvocation(ctx, Invocation{ID: "operation", Origin: "consumer", Target: frame.Target{Scope: "entry", Reference: "original"}})
			return err
		}},
		{name: "input validation", call: func(ctx context.Context) error {
			return service.ValidateInput(ctx, method.Name(), input)
		}},
	} {
		for _, ctx := range []context.Context{nil, (*missingContext)(nil)} {
			t.Run(test.name, func(t *testing.T) {
				if err := test.call(ctx); !errors.Is(err, frame.ErrInvalid) {
					t.Fatalf("missing context escaped admission: %v", err)
				}
			})
		}
	}
}
