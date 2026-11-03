package process

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

func queryInvocation(t *testing.T, member string, input value.JSON) portable.Invocation {
	t.Helper()
	invocation, err := portable.NewQueryInvocation(member, input)
	if err != nil {
		t.Fatal(err)
	}
	return invocation
}

func commandInvocation(t *testing.T, id, member string, input value.JSON) portable.Invocation {
	t.Helper()
	invocation, err := portable.NewCommandInvocation(id, member, input)
	if err != nil {
		t.Fatal(err)
	}
	return invocation
}

func TestCommandIdentitySurvivesNeutralRelayAndRejectionsDoNotEnterHandler(t *testing.T) {
	input, err := value.EncodeJSON("original")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []portable.MethodKind{portable.Query, portable.Command} {
		t.Run(string(kind), func(t *testing.T) {
			method, err := portable.NewMethod[string, string](portable.MethodConfig{Name: "invoke", Kind: kind})
			if err != nil {
				t.Fatal(err)
			}
			seen := make(chan portable.Invocation, 1)
			service := connectedService(t, method.Descriptor(), Handler(func(_ context.Context, invocation portable.Invocation) (value.JSON, error) {
				seen <- invocation
				return invocation.Input(), nil
			}))
			invocation := queryInvocation(t, method.Name(), input)
			if kind == portable.Command {
				invocation = commandInvocation(t, "original-身份", method.Name(), input)
			}
			reply, err := service.Invoke(t.Context(), invocation)
			if err != nil || reply != input {
				t.Fatalf("relay changed outcome: %s %v", reply.String(), err)
			}
			if observed := <-seen; observed != invocation {
				t.Fatal("relay changed original invocation")
			}
			invalid := []jsontext.Value{nil, jsontext.Value(`""`), jsontext.Value(`null`), jsontext.Value(`1`)}
			if kind == portable.Query {
				invalid = invalid[1:]
				invalid = append(invalid, jsontext.Value(`"unexpected"`))
			}
			for _, commandID := range invalid {
				var result value.JSON
				err := service.peer.request(t.Context(), wire.Methods.Call, callParams{Handle: service.handle, Member: method.Name(), Input: input, CommandID: commandID}, &result)
				if err == nil || errors.Is(err, ErrOutcomeUnknown) {
					t.Fatalf("invalid identity reached handler: %s %v", commandID, err)
				}
			}
			select {
			case <-seen:
				t.Fatal("rejected identity reached business code")
			default:
			}
		})
	}
}

func TestTypedCommandHandlerReceivesOriginalIdentity(t *testing.T) {
	member, err := NewMember[string, string](portable.MethodConfig{Name: "replace", Kind: portable.Command})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := member.ImplementCommand(func(_ context.Context, id, input string) (string, error) {
		if id != "business-owner" {
			t.Fatal("command identity changed")
		}
		return input, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := portable.NewCapability("typed.commands", member.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	service, err := Implement(contract, implementation)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := member.Command(t.Context(), service, "business-owner", "input"); err != nil || output != "input" {
		t.Fatal(output, err)
	}
	if _, err := member.Query(t.Context(), service, "input"); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("query invoked command: %v", err)
	}
	if _, err := member.Command(t.Context(), service, "", "input"); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("empty command identity: %v", err)
	}
}

func TestWireCommandIdentityRejectsExplicitNullAndPreservesUnicode(t *testing.T) {
	input, _ := value.EncodeJSON("input")
	for _, raw := range []jsontext.Value{jsontext.Value(`null`), jsontext.Value(`""`), jsontext.Value(`false`)} {
		if _, err := (callParams{Member: "write", Input: input, CommandID: raw}).invocation(); err == nil {
			t.Fatal("invalid explicit identity admitted")
		}
	}
	identity := "original-𐀀"
	raw, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := (callParams{Member: "write", Input: input, CommandID: raw}).invocation()
	if err != nil || invocation.CommandID() != identity {
		t.Fatal(invocation, err)
	}
}
