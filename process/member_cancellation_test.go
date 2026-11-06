package process

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

func localMemberService(t *testing.T, implementation Implementation) Service {
	t.Helper()
	contract, err := portable.NewCapability("typed.cancellation", implementation.method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	service, err := Implement(contract, implementation)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestTypedLocalMemberRefusesCancellationBeforeDispatch(t *testing.T) {
	for _, kind := range []portable.MethodKind{portable.Query, portable.Command} {
		t.Run(string(kind), func(t *testing.T) {
			member, err := NewMember[string, string](portable.MethodConfig{Name: "perform", Kind: kind})
			if err != nil {
				t.Fatal(err)
			}
			effects := 0
			var implementation Implementation
			if kind == portable.Query {
				implementation, err = member.ImplementQuery(func(context.Context, string) (string, error) {
					effects++
					return "completed", nil
				})
			} else {
				implementation, err = member.ImplementCommand(func(context.Context, string, string) (string, error) {
					effects++
					return "completed", nil
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			service := localMemberService(t, implementation)
			cause := errors.New("caller abandoned operation")
			ctx, cancel := context.WithCancelCause(t.Context())
			cancel(cause)
			input, err := value.EncodeJSON("content")
			if err != nil {
				t.Fatal(err)
			}
			var invocation portable.Invocation
			if kind == portable.Query {
				invocation = queryInvocation(t, "perform", input)
				_, err = member.Query(ctx, service, "content")
			} else {
				invocation = commandInvocation(t, "original-command", "perform", input)
				_, err = member.Command(ctx, service, "original-command", "content")
			}
			if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) || errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("typed caller changed pre-dispatch cancellation: %v", err)
			}
			if _, err := service.Invoke(ctx, invocation); !errors.Is(err, context.Canceled) || !errors.Is(err, cause) || errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("direct service changed pre-dispatch cancellation: %v", err)
			}
			if effects != 0 {
				t.Fatalf("cancelled calls entered their handler %d times", effects)
			}
		})
	}
}

type encodingCancellationInput struct{ encode func() }

func (i encodingCancellationInput) MarshalJSON() ([]byte, error) {
	i.encode()
	return []byte(`"content"`), nil
}
func (*encodingCancellationInput) UnmarshalJSON([]byte) error { return nil }
func (encodingCancellationInput) JSONSchemaModel() any        { return "" }

func TestTypedCallerChecksCancellationAroundInputEncoding(t *testing.T) {
	for _, beforeEncoding := range []bool{true, false} {
		name := "during encoding"
		if beforeEncoding {
			name = "before encoding"
		}
		t.Run(name, func(t *testing.T) {
			member, err := NewMember[encodingCancellationInput, string](portable.MethodConfig{Name: "perform", Kind: portable.Command})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if beforeEncoding {
				cancel()
			}
			encoded := 0
			input := encodingCancellationInput{func() { encoded++; cancel() }}
			service := Handler(func(context.Context, portable.Invocation) (value.JSON, error) {
				t.Error("cancelled typed caller dispatched to its service")
				return value.JSON{}, nil
			})
			_, err = member.Command(ctx, service, "original-command", input)
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("encoding cancellation changed outcome classification: %v", err)
			}
			expected := 1
			if beforeEncoding {
				expected = 0
			}
			if encoded != expected {
				t.Fatalf("cancelled input was encoded %d times, want %d", encoded, expected)
			}
		})
	}
}

type delayedMemberInput string

func (i delayedMemberInput) MarshalJSON() ([]byte, error) { return json.Marshal(string(i)) }
func (i *delayedMemberInput) UnmarshalJSON(raw []byte) error {
	time.Sleep(2 * time.Second)
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	*i = delayedMemberInput(text)
	return nil
}
func (delayedMemberInput) JSONSchemaModel() any { return "" }

func TestTypedLocalMemberChecksCancellationAfterInputDecoding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		member, err := NewMember[delayedMemberInput, string](portable.MethodConfig{Name: "perform", Kind: portable.Command})
		if err != nil {
			t.Fatal(err)
		}
		implementation, err := member.ImplementCommand(func(context.Context, string, delayedMemberInput) (string, error) {
			t.Error("input decoding outlived cancellation and entered its handler")
			return "completed", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		service := localMemberService(t, implementation)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err = member.Command(ctx, service, "original-command", "content")
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("input decoding lost pre-dispatch deadline: %v", err)
		}
	})
}

func TestTypedLocalMemberPreservesResultsAfterDispatchCancellation(t *testing.T) {
	for _, fail := range []bool{true, false} {
		name := "success"
		if fail {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			member, err := NewMember[string, string](portable.MethodConfig{Name: "perform", Kind: portable.Command})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			effects := 0
			implementation, err := member.ImplementCommand(func(ctx context.Context, commandID, input string) (string, error) {
				effects++
				if commandID != "original-command" || input != "content" {
					t.Fatalf("dispatch changed command identity or input: %q %q", commandID, input)
				}
				cancel()
				if fail {
					return "", ctx.Err()
				}
				return "completed", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := member.Command(ctx, localMemberService(t, implementation), "original-command", "content")
			if effects != 1 {
				t.Fatalf("command executed %d times", effects)
			}
			if fail {
				if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatalf("post-dispatch failure became pre-dispatch cancellation: %v", err)
				}
			} else if err != nil || result != "completed" {
				t.Fatalf("cancellation discarded the original success: %q, %v", result, err)
			}
		})
	}
}
