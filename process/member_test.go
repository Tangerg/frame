package process

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

func TestTypedMemberConsumesSchemaIntegerSpellings(t *testing.T) {
	type input struct {
		Count int64 `json:"count"`
	}
	member, err := NewMember[input, int64](portable.MethodConfig{Name: "count", Kind: portable.Query})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := member.ImplementQuery(func(_ context.Context, in input) (int64, error) { return in.Count, nil })
	if err != nil {
		t.Fatal(err)
	}
	contract, err := portable.NewCapability("typed-count", member.Descriptor())
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
	member, err := NewMember[string, undecodableReply](portable.MethodConfig{Name: "mutate", Kind: portable.Command})
	if err != nil {
		t.Fatal(err)
	}
	var effects atomic.Int32
	service := connectedService(t, member.Descriptor(), Handler(func(context.Context, portable.Invocation) (value.JSON, error) {
		effects.Add(1)
		return value.ParseJSON([]byte(`"completed"`))
	}))
	result, err := member.Command(t.Context(), service, "original-command", "input")
	if effects.Load() != 1 || result != "" || !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, errReplyDecode) {
		t.Fatalf("post-dispatch evidence was lost: effects=%d, result=%q, error=%v", effects.Load(), result, err)
	}
}
