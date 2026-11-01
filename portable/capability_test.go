package portable_test

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"testing"

	"github.com/Tangerg/frame/portable"
)

func TestCapabilityDecodingRequiresDeclaredPrimitiveFields(t *testing.T) {
	contract := testCapability(t, testMethod("acme/read", portable.Query))
	for _, absent := range []bool{false, true} {
		data, err := json.Marshal(contract.Descriptor())
		if err != nil {
			t.Fatal(err)
		}
		var descriptor map[string]jsontext.Value
		if err := json.Unmarshal(data, &descriptor); err != nil {
			t.Fatal(err)
		}
		var methods []map[string]jsontext.Value
		if err := json.Unmarshal(descriptor["methods"], &methods); err != nil {
			t.Fatal(err)
		}
		if absent {
			delete(methods[0], "description")
		} else {
			methods[0]["description"] = jsontext.Value("null")
		}
		descriptor["methods"], err = json.Marshal(methods)
		if err != nil {
			t.Fatal(err)
		}
		data, err = json.Marshal(descriptor)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := portable.ParseCapability(data); err == nil {
			t.Fatalf("description absence/null became empty prose: %s", data)
		}
	}
}

func testMethod(name string, kind portable.MethodKind) portable.MethodDescriptor {
	return portable.MethodDescriptor{Name: name, Kind: kind, Target: "workspace", Description: "Method.", Input: []byte(`{"type":"object","properties":{"revision":{"type":"integer"}},"required":["revision"],"additionalProperties":false}`), Output: []byte(`{"type":"string"}`)}
}

func testCapability(t *testing.T, methods ...portable.MethodDescriptor) *portable.Capability {
	t.Helper()
	contract, err := portable.NewCapability("acme/records", methods...)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func TestCapabilityOwnsAnImmutableCanonicalContract(t *testing.T) {
	read, err := portable.AdmitMethod(testMethod("acme/read", portable.Query))
	if err != nil {
		t.Fatal(err)
	}
	write, err := portable.AdmitMethod(testMethod("acme/write", portable.Mutation))
	if err != nil {
		t.Fatal(err)
	}
	methods := []portable.MethodDescriptor{write.Descriptor(), read.Descriptor()}
	contract := testCapability(t, methods...)
	methods[0].Input[0] = 'x'
	projection := contract.Descriptor()
	if projection.Methods[0].Name != read.Name() || projection.Methods[1].Input[0] == 'x' {
		t.Fatal("capability borrowed mutable author input")
	}
	projection.Methods[0].Input[0] = 'x'
	if contract.Descriptor().Methods[0].Input[0] == 'x' {
		t.Fatal("projection changed the capability owner")
	}
	prose := write.Descriptor()
	prose.Description = "Revised prose."
	if testCapability(t, read.Descriptor(), prose).Identity() != contract.Identity() {
		t.Fatal("method order or prose changed callable identity")
	}
	encoded, err := json.Marshal(contract.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := portable.ParseCapability(encoded)
	if err != nil || parsed.Identity() != contract.Identity() {
		t.Fatalf("generated projection changed callable identity: %v", err)
	}
	for _, change := range []struct {
		name   string
		change func(*portable.CapabilityDescriptor)
	}{
		{"reference", func(d *portable.CapabilityDescriptor) { d.Reference = "acme/other" }},
		{"target", func(d *portable.CapabilityDescriptor) { d.Methods[0].Target = "installation" }},
		{"kind", func(d *portable.CapabilityDescriptor) { d.Methods[0].Kind = portable.Mutation }},
		{"required set", func(d *portable.CapabilityDescriptor) { d.Methods = d.Methods[:1] }},
	} {
		t.Run(change.name, func(t *testing.T) {
			descriptor := contract.Descriptor()
			change.change(&descriptor)
			data, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := portable.ParseCapability(data)
			if err != nil || changed.Identity() == contract.Identity() {
				t.Fatalf("changed callable identity was lost: %v", err)
			}
		})
	}
}

func TestCapabilityRefusesInvalidDeclarationsWithoutActivation(t *testing.T) {
	method, err := portable.AdmitMethod(testMethod("acme/read", portable.Query))
	if err != nil {
		t.Fatal(err)
	}
	contract := testCapability(t, method.Descriptor())
	for _, change := range []func(*portable.CapabilityDescriptor){
		func(d *portable.CapabilityDescriptor) { d.Protocol = "old" },
		func(d *portable.CapabilityDescriptor) { d.Reference = "has space" },
		func(d *portable.CapabilityDescriptor) { d.Methods = nil },
		func(d *portable.CapabilityDescriptor) { d.Methods = append(d.Methods, d.Methods[0]) },
		func(d *portable.CapabilityDescriptor) { d.Methods[0].Input = []byte(`{}`) },
		func(d *portable.CapabilityDescriptor) {
			d.Methods[0].Output = []byte(`{"$ref":"https://invalid.example/schema"}`)
		},
	} {
		descriptor := contract.Descriptor()
		change(&descriptor)
		data, err := json.Marshal(descriptor)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := portable.ParseCapability(data); err == nil {
			t.Fatalf("invalid declaration admitted: %s", data)
		}
	}
	data, err := json.Marshal(contract.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"extra":true}`)...)
	if _, err := portable.ParseCapability(data); err == nil {
		t.Fatal("unknown declaration member admitted")
	}
}
