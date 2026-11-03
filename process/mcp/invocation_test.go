package mcp

import (
	"testing"

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
