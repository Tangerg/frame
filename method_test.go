package frame_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

func TestMethodOwnsStructuralAdmissionAndImmutableIdentity(t *testing.T) {
	descriptor := testMethod("acme/read", frame.Query)
	method, err := frame.AdmitMethod(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	original := method.Identity()
	descriptor.Input[0] = 'x'
	descriptor.Output[0] = 'x'
	projection := method.Descriptor()
	projection.Input[0] = 'x'
	projection.Output[0] = 'x'
	if method.Identity() != original {
		t.Fatal("mutable projection changed identity")
	}
	for _, raw := range []string{`{}`, `{"revision":null}`, `{"revision":"one"}`, `{"revision":1,"extra":true}`} {
		input, err := framevalue.ParseJSON([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := method.ValidateInput(input); err == nil {
			t.Fatalf("invalid input admitted: %s", raw)
		}
	}
	if err := method.ValidateInput(framevalue.JSON{}); err == nil {
		t.Fatal("unadmitted input accepted")
	}
	input, err := framevalue.ParseJSON([]byte(`{"revision":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := method.ValidateInput(input); err != nil {
		t.Fatal(err)
	}
	if string(input.Bytes()) != `{"revision":9007199254740993}` {
		t.Fatal("structural admission changed received number")
	}
	if err := method.ValidateOutput([]byte(`"value"`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`null`, `7`, `{}`, `"unfinished`} {
		if err := method.ValidateOutput([]byte(raw)); err == nil {
			t.Fatalf("invalid output admitted: %s", raw)
		}
	}
	// Descriptors may be rendered while the same admitted schemas serve calls.
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 20 {
				projected := method.Descriptor()
				projected.Input[0] = 'x'
				if method.Identity() != original {
					t.Error("concurrent projection changed identity")
				}
				if err := method.ValidateInput(input); err != nil {
					t.Error(err)
				}
				if err := method.ValidateOutput([]byte(`"value"`)); err != nil {
					t.Error(err)
				}
			}
		})
	}
	workers.Wait()
}

func TestMethodRefusesInvalidLocalMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*frame.MethodDescriptor)
	}{
		{"missing name", func(d *frame.MethodDescriptor) { d.Name = "" }},
		{"control in name", func(d *frame.MethodDescriptor) { d.Name = "acme/\x00read" }},
		{"oversized name", func(d *frame.MethodDescriptor) { d.Name = strings.Repeat("a", 257) }},
		{"invalid utf8", func(d *frame.MethodDescriptor) { d.Name = "\xff" }},
		{"space in target", func(d *frame.MethodDescriptor) { d.Target = "work space" }},
		{"unknown kind", func(d *frame.MethodDescriptor) { d.Kind = "other" }},
		{"missing schema", func(d *frame.MethodDescriptor) { d.Input = nil }},
		{"nullable input", func(d *frame.MethodDescriptor) { d.Input = []byte(`{"type":["object","null"]}`) }},
		{"remote input reference", func(d *frame.MethodDescriptor) { d.Input = []byte(`{"$ref":"https://invalid.example/schema"}`) }},
		{"unresolved output", func(d *frame.MethodDescriptor) { d.Output = []byte(`{"$ref":"#/$defs/missing"}`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor := testMethod("acme/read", frame.Query)
			test.change(&descriptor)
			if _, err := frame.AdmitMethod(descriptor); err == nil {
				t.Fatal("invalid local metadata admitted")
			}
		})
	}
}

func TestBooleanOutputContractRejectsIntegerEvidence(t *testing.T) {
	descriptor := testMethod("acme/read", frame.Query)
	descriptor.Output = []byte(`{"type":"boolean"}`)
	method, err := frame.AdmitMethod(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := method.ValidateOutput([]byte(`true`)); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`1`, `18446744073709551615`, `-9223372036854775808`} {
		if err := method.ValidateOutput([]byte(value)); err == nil {
			t.Fatalf("boolean contract admitted integer %s", value)
		}
	}
}
