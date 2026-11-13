package frame

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame/value"
)

func queryInvocation(t *testing.T, member string, input value.JSON) Invocation {
	t.Helper()
	invocation, err := NewQueryInvocation(member, input)
	if err != nil {
		t.Fatal(err)
	}
	return invocation
}

func commandInvocation(t *testing.T, id, member string, input value.JSON) Invocation {
	t.Helper()
	invocation, err := NewCommandInvocation(id, member, input)
	if err != nil {
		t.Fatal(err)
	}
	return invocation
}

func TestImplementationRequiresAnAdmittedCapability(t *testing.T) {
	for _, contract := range []*Capability{nil, {}} {
		if service, err := Implement(contract); !errors.Is(err, ErrContractMismatch) || service != nil {
			t.Fatalf("unadmitted capability produced a callable service: %v", err)
		}
	}
}

func TestTypedMemberConsumesSchemaIntegerSpellings(t *testing.T) {
	type input struct {
		Count int64 `json:"count"`
	}
	member, err := NewTypedMethod[input, int64](MethodConfig{Name: "count", Kind: Query})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := member.ImplementQuery(func(_ context.Context, in input) (int64, error) { return in.Count, nil })
	if err != nil {
		t.Fatal(err)
	}
	contract, err := NewCapability("typed-count", member.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	service, err := Implement(contract, implementation)
	if err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{"9007199254740993.0", "9.007199254740993e15"} {
		input, err := value.ParseJSON([]byte(`{"count":` + spelling + "}"))
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.Invoke(t.Context(), queryInvocation(t, "count", input))
		if err != nil || result.String() != "9007199254740993" {
			t.Fatalf("schema-valid integer did not reach handler: %s, %v", result.String(), err)
		}
	}
}

var errReplyDecode = errors.New("reply cannot be represented")

type undecodableReply string

func (*undecodableReply) UnmarshalJSON([]byte) error    { return errReplyDecode }
func (r undecodableReply) MarshalJSON() ([]byte, error) { return json.Marshal(string(r)) }
func (undecodableReply) JSONSchemaModel() any           { return "" }

func TestTypedCommandDecodeFailureKeepsUnknownOutcome(t *testing.T) {
	member, err := NewTypedMethod[string, undecodableReply](MethodConfig{Name: "mutate", Kind: Command})
	if err != nil {
		t.Fatal(err)
	}
	var effects atomic.Int32
	service := Handler(func(context.Context, Invocation) (value.JSON, error) {
		effects.Add(1)
		return value.ParseJSON([]byte(`"completed"`))
	})
	result, err := member.Command(t.Context(), service, "original-command", "input")
	if effects.Load() != 1 || result != "" || !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, errReplyDecode) {
		t.Fatalf("post-dispatch evidence was lost: effects=%d, result=%q, error=%v", effects.Load(), result, err)
	}
}

var errReplyEncode = errors.New("reply cannot be encoded")

type unencodableReply string

func (r *unencodableReply) UnmarshalJSON(raw []byte) error {
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*r = unencodableReply(decoded)
	return nil
}
func (unencodableReply) MarshalJSON() ([]byte, error) { return nil, errReplyEncode }
func (unencodableReply) JSONSchemaModel() any         { return "" }

func TestTypedLocalCommandKeepsPostDispatchFailureUnknown(t *testing.T) {
	for _, failure := range []error{context.Canceled, ErrContractMismatch, nil} {
		name := "output encoding"
		if failure != nil {
			name = failure.Error()
		}
		t.Run(name, func(t *testing.T) {
			member, err := NewTypedMethod[string, unencodableReply](MethodConfig{Name: "mutate", Kind: Command})
			if err != nil {
				t.Fatal(err)
			}
			var effects int
			implementation, err := member.ImplementCommand(func(context.Context, string, string) (unencodableReply, error) {
				effects++
				return "completed", failure
			})
			if err != nil {
				t.Fatal(err)
			}
			contract, err := NewCapability("typed.local-outcome", member.Descriptor())
			if err != nil {
				t.Fatal(err)
			}
			service, err := Implement(contract, implementation)
			if err != nil {
				t.Fatal(err)
			}
			_, err = member.Command(t.Context(), service, "original-effect", "input")
			cause := failure
			if cause == nil {
				cause = errReplyEncode
			}
			if effects != 1 || !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, cause) {
				t.Fatalf("local dispatch lost outcome evidence: effects=%d, error=%v", effects, err)
			}
		})
	}
}

func TestTypedInputDecodeFailureRemainsBeforeDispatch(t *testing.T) {
	member, err := NewTypedMethod[undecodableReply, string](MethodConfig{Name: "mutate", Kind: Command})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := member.ImplementCommand(func(context.Context, string, undecodableReply) (string, error) {
		t.Error("undecodable request reached its handler")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := NewCapability("typed.input-decode", member.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	service, err := Implement(contract, implementation)
	if err != nil {
		t.Fatal(err)
	}
	_, err = member.Command(t.Context(), service, "original-effect", "input")
	if !errors.Is(err, errReplyDecode) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("input decoding was classified after dispatch: %v", err)
	}
}
