package portable_test

import (
	"strings"
	"testing"

	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

func TestInvocationRetainsOriginalCommandIdentityAcrossMethodValidation(t *testing.T) {
	method, err := portable.NewMethod[string, string](portable.MethodConfig{Name: "document.replace", Kind: portable.Command})
	if err != nil {
		t.Fatal(err)
	}
	input, err := value.EncodeJSON("original")
	if err != nil {
		t.Fatal(err)
	}
	const identity = "caller-owned-effect"
	command, err := portable.NewCommandInvocation(identity, method.Name(), input)
	if err != nil || method.ValidateInvocation(command) != nil {
		t.Fatalf("command admission: %v", err)
	}
	if command.CommandID() != identity || command.Input() != input || command.Kind() != portable.Command {
		t.Fatal("command projection changed its original evidence")
	}
	query, err := portable.NewQueryInvocation(method.Name(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := method.ValidateInvocation(query); err == nil {
		t.Fatal("query invocation admitted for a command")
	}
	if err := method.ValidateInvocation(portable.Invocation{}); err == nil {
		t.Fatal("zero invocation admitted")
	}
}

func TestInvocationConstructorsRejectInvalidEvidenceAndCountUnicodeCharacters(t *testing.T) {
	input, err := value.EncodeJSON("input")
	if err != nil {
		t.Fatal(err)
	}
	maximum := portable.ProfileLimits().MaxCommandIDCharacters
	for _, id := range []string{"", strings.Repeat("x", maximum+1), strings.Repeat("界", maximum+1), string([]byte{0xff})} {
		if _, err := portable.NewCommandInvocation(id, "write", input); err == nil {
			t.Fatal("invalid command identity admitted")
		}
	}
	for _, id := range []string{strings.Repeat("x", maximum), strings.Repeat("界", maximum), strings.Repeat("𐀀", maximum)} {
		invocation, err := portable.NewCommandInvocation(id, "write", input)
		if err != nil || invocation.CommandID() != id {
			t.Fatalf("valid Unicode identity rejected or changed: %v", err)
		}
	}
	for _, member := range []string{"", "contains space", string([]byte{0xff})} {
		if _, err := portable.NewQueryInvocation(member, input); err == nil {
			t.Fatal("invalid member admitted")
		}
	}
	if _, err := portable.NewQueryInvocation("read", value.JSON{}); err == nil {
		t.Fatal("invalid input admitted")
	}
}
