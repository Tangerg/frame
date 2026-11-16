package call_test

import (
	"testing"

	"github.com/Tangerg/frame/call"
)

func TestInvocationValidation(t *testing.T) {
	valid := call.Invocation{ID: "operation", Origin: "consumer", Parent: "parent", Target: call.Target{Scope: "document", Reference: "/notes/报告.md"}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name  string
		apply func(*call.Invocation)
	}{
		{"missing identity", func(i *call.Invocation) { i.ID = "" }},
		{"invalid identity", func(i *call.Invocation) { i.ID = "bad identity" }},
		{"self parent", func(i *call.Invocation) { i.Parent = i.ID }},
		{"invalid parent", func(i *call.Invocation) { i.Parent = "bad parent" }},
		{"missing origin", func(i *call.Invocation) { i.Origin = "" }},
		{"invalid origin", func(i *call.Invocation) { i.Origin = "bad origin" }},
		{"missing scope", func(i *call.Invocation) { i.Target.Scope = "" }},
		{"invalid scope", func(i *call.Invocation) { i.Target.Scope = "bad scope" }},
		{"missing reference", func(i *call.Invocation) { i.Target.Reference = "" }},
		{"invalid utf8", func(i *call.Invocation) { i.Target.Reference = string([]byte{0xff}) }},
		{"control reference", func(i *call.Invocation) { i.Target.Reference = "document\nother" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			candidate := valid
			change.apply(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid calling evidence admitted")
			}
			if err := valid.Validate(); err != nil {
				t.Fatalf("validation changed original evidence: %v", err)
			}
		})
	}
	valid.Parent = ""
	if err := valid.Validate(); err != nil {
		t.Fatalf("root invocation refused: %v", err)
	}
}
