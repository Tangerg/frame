package process_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
)

func TestRealProcessDriverRejectsNativeObjectsBeforeCreationOrBusiness(t *testing.T) {
	for _, direction := range []string{"export", "import"} {
		t.Run(direction, func(t *testing.T) {
			e := newEnvironment(t, false)
			native, err := frame.NewContract[func() *int]("document.shared-native", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := e.host.Register(native); err != nil {
				t.Fatal(err)
			}
			declaration := frame.UnitDeclaration{ID: "backend", Profile: frame.LocalProcess, Exports: []frame.ExportDeclaration{{Name: "native", Contract: native.Ref()}}}
			if direction == "import" {
				declaration.Exports = []frame.ExportDeclaration{{Name: "echo", Contract: e.ref.Contract}}
				declaration.Imports = []frame.ImportDeclaration{{Slot: "shared", Contract: native.Ref(), Optional: true}}
			}
			plugin, err := frame.Define(frame.Definition{Revision: integrity.Hash([]byte(direction)), Units: []frame.Unit{{Declaration: declaration}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.host.Install(e.ctx, frame.InstallationConfig{ID: "invalid-native", Plugin: plugin, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			root := frame.ExportRef{InstallationID: "invalid-native", Release: plugin.Revision(), UnitID: "backend", Export: declaration.Exports[0].Name, Contract: declaration.Exports[0].Contract}
			if _, err := e.host.Activate(e.ctx, root); !errors.Is(err, frame.ErrContractMismatch) {
				t.Fatalf("process profile accepted a synchronous shared object: %v", err)
			}
			if e.source.programCalls.Load() != 0 {
				t.Fatal("incompatible profile reached executable selection")
			}
			if _, err := os.Stat(filepath.Join(e.directory, "prepared")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incompatible profile reached the actual backend setup: %v", err)
			}
			// The same real runner, store and driver must still admit Portable
			// execution after rejecting the native contract.
			binding, err := e.host.Activate(e.ctx, e.ref)
			if err != nil {
				t.Fatal(err)
			}
			if len(binding.Roots) != 1 || binding.Roots[0] != e.ref || e.source.programCalls.Load() != 1 {
				t.Fatal("profile rejection changed the independent Portable binding")
			}
			if err := e.host.SetEnabled(context.WithoutCancel(e.ctx), "echo", false); err != nil {
				t.Fatal(err)
			}
			if err := e.host.WaitRetired(e.ctx, "echo"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUnsupportedPortableDescriptorDoesNotCreateAnAttempt(t *testing.T) {
	e := newEnvironment(t, false)
	descriptor := []byte(`{"type":"string"}`)
	ref := frame.ContractRef{Name: "domain.data", Version: "1", Kind: frame.ServiceKind, Mode: frame.PortableMode, Digest: integrity.Hash(descriptor)}
	contract, err := frame.BindContract[string](ref, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.host.Register(contract); err != nil {
		t.Fatal(err)
	}
	definition, err := frame.Define(frame.Definition{Revision: integrity.Hash([]byte("data")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "backend", Profile: frame.LocalProcess, Exports: []frame.ExportDeclaration{{Name: "data", Contract: ref}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.host.Install(e.ctx, frame.InstallationConfig{ID: "data", Plugin: definition, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.host.Activate(e.ctx, frame.ExportRef{InstallationID: "data", Release: definition.Revision(), UnitID: "backend", Export: "data", Contract: ref})
	if !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("descriptor not refused: %v", err)
	}
	attempts, err := e.store.Attempts(e.ctx)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("invalid descriptor created attempts: %v %v", attempts, err)
	}
}
