package host_test

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestKnownCapabilityIsAdmittedBeforeBindingOrRegistration(t *testing.T) {
	method, err := frame.NewMethod[string, string](frame.MethodConfig{Name: "read", Kind: frame.Query})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := frame.NewCapability("test.admitted", method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	invalid := capability.Descriptor()
	invalid.Methods[0].Kind = frame.MethodKind("invented")
	malformed, err := json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		name       string
		reference  string
		descriptor []byte
	}{{"changed reference", "test.other", capability.Identity().Bytes()}, {"invalid method", "test.admitted", malformed}} {
		t.Run(sample.name, func(t *testing.T) {
			ref := framehost.ContractRef{Name: sample.reference, Version: "1", Kind: framehost.ServiceKind, Mode: framehost.PortableMode, Digest: value.Hash(sample.descriptor)}
			if _, err := framehost.BindContract[any](ref, sample.descriptor); !errors.Is(err, frame.ErrContractMismatch) {
				t.Errorf("binding accepted an invalid capability: %v", err)
			}
			host, err := framehost.New(t.Context(), framehost.Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close(context.Background())
			if err := host.RegisterDescriptor(ref, sample.descriptor); !errors.Is(err, frame.ErrContractMismatch) {
				t.Errorf("registration accepted an invalid capability: %v", err)
			}
		})
	}
}

func TestRegistryRetainsOriginalCapabilityAcrossRegistrationAndHandleReplacement(t *testing.T) {
	method, err := frame.NewMethod[string, string](frame.MethodConfig{Name: "read", Kind: frame.Query, Description: "Publisher documentation."})
	if err != nil {
		t.Fatal(err)
	}
	original, err := frame.NewCapability("test.retained", method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := frame.NewCapability("test.replacement", method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	identity := original.Identity()
	for _, construction := range []string{"model", "descriptor"} {
		t.Run(construction, func(t *testing.T) {
			model := new(*original)
			contract, err := framehost.NewPortableContract[frame.Service]("1", model)
			if err != nil {
				t.Fatal(err)
			}
			if construction == "descriptor" {
				descriptor := contract.Descriptor()
				contract, err = framehost.BindContract[frame.Service](contract.Ref(), descriptor)
				if err != nil {
					t.Fatal(err)
				}
				descriptor[0] = '!'
			}
			*model = *replacement
			point, err := framehost.NewPortablePoint[struct{}]("test.retained.members", "1", []byte(`true`), contract)
			if err != nil {
				t.Fatal(err)
			}
			for _, descriptorFirst := range []bool{false, true} {
				host := testHost(t, framehost.Config{})
				if descriptorFirst {
					if err := host.RegisterDescriptor(contract.Ref(), contract.Descriptor()); err != nil {
						t.Fatal(err)
					}
				}
				if err := host.Register(contract, point); err != nil {
					t.Fatal(err)
				}
				for _, ref := range []framehost.ContractRef{contract.Ref(), point.Ref()} {
					view, err := host.Capability(ref)
					if err != nil || view.Identity() != identity {
						t.Fatalf("registered identity changed: %v", err)
					}
					*view = *replacement
					retained, err := host.Capability(ref)
					if err != nil || retained.Identity() != identity {
						t.Fatalf("read handle replaced the registered identity: %v", err)
					}
					read, ok := retained.Method(method.Name())
					if !ok || read.Identity() != method.Identity() {
						t.Fatal("read handle replaced the registered method")
					}
					if read.Descriptor().Description != "" {
						t.Fatal("registered identity acquired publisher-only documentation")
					}
				}
			}
			if original.Descriptor().Methods[0].Description != method.Descriptor().Description {
				t.Fatal("registration changed the publisher's documentation")
			}
		})
	}
}

func TestForeignDescriptorCannotAcquireFrameCapabilitySemantics(t *testing.T) {
	descriptor := []byte(`{"protocol":"example","methods":["read"]}`)
	ref := framehost.ContractRef{Name: "example.foreign", Version: "1", Kind: framehost.ServiceKind, Mode: framehost.PortableMode, Digest: value.Hash(descriptor)}
	contract, err := framehost.BindContract[string](ref, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	host := testHost(t, framehost.Config{})
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Capability(ref); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("foreign descriptor acquired Frame semantics: %v", err)
	}
}
