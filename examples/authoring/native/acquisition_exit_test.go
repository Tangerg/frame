package native

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/Tangerg/frame/examples/document"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestInterruptedAcquisitionClosesItsUndeliveredUse(t *testing.T) {
	cause := errors.New("validator interrupted acquisition")
	for _, interruption := range []struct {
		name  string
		exit  func()
		panic any
	}{
		{name: "panic", exit: func() { panic(cause) }, panic: cause},
		{name: "goexit", exit: runtime.Goexit},
	} {
		t.Run(interruption.name, func(t *testing.T) {
			workbench, _, output, reference, ctx := authorWorkbench(t)
			point := workbench.Contracts.Validators
			plugin, err := framehost.Define(framehost.Definition{
				Revision: value.Hash([]byte("interrupted-acquisition")),
				Units: []framehost.Unit{{
					Declaration: framehost.UnitDeclaration{ID: "validation", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "interrupt", Contract: point.Ref()}}},
					Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
						return []framehost.ExportValue{framehost.Contribute("interrupt", point, document.ValidatorMetadata{Name: "interrupt"}, document.Validator(func(context.Context, document.Converted) ([]document.Diagnostic, error) {
							interruption.exit()
							return nil, nil
						}))}, nil
					},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := workbench.Host.Install(ctx, framehost.InstallationConfig{ID: "interrupted", Plugin: plugin, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			root := framehost.ExportRef{InstallationID: "interrupted", Release: plugin.Revision(), UnitID: "validation", Export: "interrupt", Contract: point.Ref()}
			if _, err := workbench.Host.Activate(ctx, root); err != nil {
				t.Fatal(err)
			}
			prepared, err := workbench.Prepare(ctx)
			if err != nil {
				t.Fatal(err)
			}
			owner := authorOwner(t)
			exited := make(chan any, 1)
			go func() {
				defer func() { exited <- recover() }()
				_, _ = prepared.Acquire(ctx, owner, reference)
				t.Error("acquisition swallowed the callback interruption")
			}()
			select {
			case recovered := <-exited:
				if recovered != interruption.panic {
					t.Fatalf("acquisition changed the original interruption: %v", recovered)
				}
			case <-ctx.Done():
				t.Fatal(context.Cause(ctx))
			}
			if err := workbench.Host.Uninstall(ctx, Installation); err != nil {
				t.Fatalf("acquisition without a returned View retained its original providers: %v", err)
			}
			if err := workbench.Host.Uninstall(ctx, "interrupted"); err != nil {
				t.Fatalf("acquisition retained its failed validator: %v", err)
			}
			assertDirectoryEmpty(t, output)
		})
	}
}
