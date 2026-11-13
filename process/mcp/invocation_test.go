package mcp

import (
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func queryInvocation(t *testing.T, member string, input value.JSON) frame.Invocation {
	t.Helper()
	invocation, err := frame.NewQueryInvocation(member, input)
	if err != nil {
		t.Fatal(err)
	}
	return invocation
}

func commandInvocation(t *testing.T, id, member string, input value.JSON) frame.Invocation {
	t.Helper()
	invocation, err := frame.NewCommandInvocation(id, member, input)
	if err != nil {
		t.Fatal(err)
	}
	return invocation
}
