package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/call"
	"github.com/Tangerg/frame/value"
)

type missingContext struct{ context.Context }

func TestManagedInvocationRetainsOriginalCallingEvidence(t *testing.T) {
	original := call.Invocation{ID: "operation", Origin: "consumer", Target: call.Target{Scope: "entry", Reference: "original"}}
	ctx, err := WithInvocation(t.Context(), original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WithInvocation(ctx, original); err != nil {
		t.Fatalf("same evidence refused: %v", err)
	}
	changed := original
	changed.Target.Reference = "replacement"
	if _, err := WithInvocation(ctx, changed); err == nil {
		t.Fatal("bound calling evidence replaced")
	}
	if _, err := WithInvocation(ctx, original); err != nil {
		t.Fatalf("refused replacement changed original binding: %v", err)
	}
	for _, code := range []string{InvocationConflictCode, "independent-refusal"} {
		failure := &Refusal{Code: code, Message: "backend refused"}
		if errors.Is(failure, call.ErrConflict) != (code == InvocationConflictCode) {
			t.Fatalf("refusal %q changed conflict classification", code)
		}
	}
}

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
			_, err := WithInvocation(ctx, call.Invocation{ID: "operation", Origin: "consumer", Target: call.Target{Scope: "entry", Reference: "original"}})
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
