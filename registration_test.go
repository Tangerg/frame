package frame_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type absentDriver struct{ frame.Driver }
type absentStore struct {
	frame.InstallationStore
	frame.ExecutionProtection
}

func TestRegistrationRejectsTypedNilCollaborators(t *testing.T) {
	host := testHost(t, frame.HostConfig{})
	if err := host.RegisterDriver((*absentDriver)(nil)); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("typed nil driver admitted: %v", err)
	}
	if err := host.Register((*frame.Contract[string])(nil)); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("typed nil declaration admitted: %v", err)
	}
	if err := host.Admission().RegisterUseOwner((*retainedUseOwner)(nil)); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("typed nil business owner admitted: %v", err)
	}
	for _, config := range []frame.HostConfig{
		{Installations: (*absentStore)(nil)},
		{ExecutionProtection: (*absentStore)(nil)},
		{Drivers: []frame.Driver{(*absentDriver)(nil)}},
	} {
		if host, err := frame.NewHost(t.Context(), config); host != nil || !errors.Is(err, frame.ErrInvalid) {
			t.Fatalf("host retained an absent collaborator: %v, %v", host, err)
		}
	}
}

func TestFailedHostConstructionDoesNotRegisterPartialBusinessOwners(t *testing.T) {
	admission := frame.NewAdmission()
	owner := &diagnosticUseOwner{id: "consumer.original"}
	if host, err := frame.NewHost(t.Context(), frame.HostConfig{Admission: admission, UseOwners: []frame.UseOwner{owner, nil}}); host != nil || !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("invalid construction was accepted: %v, %v", host, err)
	}
	if err := admission.RegisterUseOwner(owner); err != nil {
		t.Fatalf("failed constructor left another registered authority: %v", err)
	}
}

func TestActualExportsRejectTypedNilValues(t *testing.T) {
	for _, value := range []any{(*int)(nil), (func())(nil), (map[string]string)(nil), ([]string)(nil), (chan int)(nil)} {
		host := testHost(t, frame.HostConfig{})
		contract, err := frame.NewContract[any]("example.present", "1")
		if err != nil {
			t.Fatal(err)
		}
		if err := host.Register(contract); err != nil {
			t.Fatal(err)
		}
		plugin := installNative(t, host, "absent-value", frame.Unit{
			Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "actual", Contract: contract.Ref()}}},
			Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
				return []frame.ExportValue{frame.Provide("actual", contract, value)}, nil
			},
		})
		if _, err := host.Activate(t.Context(), source("absent-value", plugin, "main", "actual", contract.Ref())); !errors.Is(err, frame.ErrContractMismatch) {
			t.Fatalf("typed nil %T was published as an actual service: %v", value, err)
		}
	}
}

func TestContractBatchRejectsConflictsWithoutPartialRegistration(t *testing.T) {
	text, _ := frame.NewContract[string]("example.batch", "1")
	number, _ := frame.NewContract[int]("example.batch", "1")
	point, _ := frame.NewPoint[string, string]("example.batch", "1")
	pointMetadata, _ := frame.NewPoint[int, string]("example.batch", "1")
	pointValue, _ := frame.NewPoint[string, int]("example.batch", "1")
	event, _ := frame.NewEvent[string]("example.batch", "1")
	eventPayload, _ := frame.NewEvent[int]("example.batch", "1")
	portable := func(descriptor string) frame.Contract[string] {
		ref := frame.ContractRef{Name: "example.batch", Version: "1", Kind: frame.ServiceKind, Mode: frame.PortableMode, Digest: framevalue.Hash([]byte(descriptor))}
		value, err := frame.BindContract[string](ref, []byte(descriptor))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, test := range []struct {
		name   string
		first  frame.Declaration
		second frame.Declaration
	}{
		{"native service type", text, number},
		{"point metadata type", point, pointMetadata},
		{"point value type", point, pointValue},
		{"event payload type", event, eventPayload},
		{"portable descriptor", portable(`{"method":"one"}`), portable(`{"method":"two"}`)},
		{"execution mode", text, portable(`{"method":"portable"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := testHost(t, frame.HostConfig{})
			unrelated, _ := frame.NewContract[string]("example.unrelated", "1")
			if err := host.Register(unrelated, test.first, test.second); !errors.Is(err, frame.ErrContractMismatch) {
				t.Fatalf("conflicting batch was admitted: %v", err)
			}
			if _, err := host.ContractDescriptor(unrelated.Ref()); !errors.Is(err, frame.ErrContractMismatch) {
				t.Fatalf("failed batch partially registered a declaration: %v", err)
			}
			if err := host.Register(test.second, test.second); err != nil {
				t.Fatalf("failed batch retained its first declaration: %v", err)
			}
			if err := host.Register(test.first); !errors.Is(err, frame.ErrContractMismatch) {
				t.Fatalf("sequential registration differs from batch admission: %v", err)
			}
		})
	}
}

func TestContractKindsKeepIndependentNominalIdentities(t *testing.T) {
	host := testHost(t, frame.HostConfig{})
	service, _ := frame.NewContract[string]("example.same-name", "1")
	point, _ := frame.NewPoint[string, int]("example.same-name", "1")
	event, _ := frame.NewEvent[bool]("example.same-name", "1")
	if err := host.Register(service, point, event, service, point, event); err != nil {
		t.Fatalf("distinct contract kinds collided: %v", err)
	}
}

func TestDescriptorRegistrationPreservesGoTypeAdmission(t *testing.T) {
	for _, typedFirst := range []bool{false, true} {
		name := "descriptor first"
		if typedFirst {
			name = "typed first"
		}
		t.Run(name, func(t *testing.T) {
			host := testHost(t, frame.HostConfig{})
			text, err := frame.NewContract[string]("example.local-type", "1")
			if err != nil {
				t.Fatal(err)
			}
			registerTyped := func() error { return host.Register(text) }
			registerDescriptor := func() error { return host.RegisterDescriptor(text.Ref(), text.Descriptor()) }
			registrations := []func() error{registerDescriptor, registerTyped}
			if typedFirst {
				registrations = []func() error{registerTyped, registerDescriptor}
			}
			for _, register := range registrations {
				if err := register(); err != nil {
					t.Fatal(err)
				}
			}
			number, err := frame.NewContract[int](text.Ref().Name, text.Ref().Version)
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(number); !errors.Is(err, frame.ErrContractMismatch) {
				t.Fatalf("descriptor registration replaced the Go binding: %v", err)
			}
			plugin := installNative(t, host, "invalid", frame.Unit{
				Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "value", Contract: text.Ref()}}},
				Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
					return []frame.ExportValue{{Name: "value", Contract: text.Ref(), Value: 42}}, nil
				},
			})
			if _, err := host.Activate(t.Context(), source("invalid", plugin, "main", "value", text.Ref())); !errors.Is(err, frame.ErrContractMismatch) {
				t.Fatalf("native value bypassed its local type: %v", err)
			}
		})
	}
}

func TestGoExportsRequireTheirLocalTypeBinding(t *testing.T) {
	contract, err := frame.NewContract[string]("example.required-go-type", "1")
	if err != nil {
		t.Fatal(err)
	}
	driver := realmValueDriver{contract: contract}
	for _, profile := range []frame.Profile{frame.NativeGo, driver.Profile()} {
		t.Run(string(profile), func(t *testing.T) {
			host := testHost(t, frame.HostConfig{Drivers: []frame.Driver{driver}})
			if err := host.RegisterDescriptor(contract.Ref(), contract.Descriptor()); err != nil {
				t.Fatal(err)
			}
			unit := frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: profile, Exports: []frame.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}}}
			if profile == frame.NativeGo {
				unit.Setup = func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
					return []frame.ExportValue{frame.Provide("value", contract, "local value")}, nil
				}
			}
			plugin := installNative(t, host, "provider", unit)
			if _, err := host.Activate(t.Context(), source("provider", plugin, "main", "value", contract.Ref())); !errors.Is(err, frame.ErrContractMismatch) {
				t.Fatalf("descriptor admitted an unbound Go value: %v", err)
			}
		})
	}
}
