package host_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestSetupReplacementCannotChangeOriginalPreparationCompletion(t *testing.T) {
	for _, typed := range []bool{false, true} {
		name := "raw setup"
		if typed {
			name = "typed setup"
		}
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, framehost.Config{})
			contract, err := framehost.NewContract[string]("setup.original", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(contract); err != nil {
				t.Fatal(err)
			}
			output := framehost.Export("value", contract)
			var original framehost.Setup
			activations := 0
			prepare := func(setup *framehost.Setup) error {
				original = *setup
				if err := setup.OnActivate(func(context.Context) error {
					activations++
					return nil
				}); err != nil {
					return err
				}
				if typed {
					if err := output.Bind(setup, "original value"); err != nil {
						return err
					}
				}
				*setup = framehost.Setup{}
				return nil
			}
			unit := framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: framehost.Exports(output)},
				Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					return []framehost.ExportValue{framehost.Provide("value", contract, "original value")}, prepare(setup)
				},
			}
			if typed {
				unit, err = framehost.DefineUnit(framehost.UnitSpec{ID: "main", Exports: framehost.Exports(output), Prepare: prepare})
				if err != nil {
					t.Fatal(err)
				}
			}
			plugin := installNative(t, host, "setup-handle", unit)
			if _, err := host.Activate(ctx, source("setup-handle", plugin, "main", "value", contract.Ref())); err != nil {
				t.Fatalf("caller handle replaced original completion: %v", err)
			}
			if activations != 1 {
				t.Fatalf("original activation ran %d times", activations)
			}
			if err := original.OnActivate(func(context.Context) error { return nil }); !errors.Is(err, frame.ErrClosed) {
				t.Fatalf("original registration outlived preparation: %v", err)
			}
			if typed {
				if err := output.Bind(&original, "late replacement"); !errors.Is(err, frame.ErrClosed) {
					t.Fatalf("original export staging outlived preparation: %v", err)
				}
			}
		})
	}
}

func TestSetupOwnsReturnedAndStagedExports(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	contract, err := framehost.NewContract[string]("setup.result", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	bound := framehost.Export("bound", contract)
	returned := framehost.Export("returned", contract)
	var original framehost.Setup
	var activations atomic.Int32
	plugin := installNative(t, host, "setup-result", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: framehost.Exports(bound, returned)},
		Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
			original = *setup
			if err := bound.Bind(&original, "staged value"); err != nil {
				return nil, err
			}
			if err := setup.OnActivate(func(context.Context) error { activations.Add(1); return nil }); err != nil {
				return nil, err
			}
			*setup = framehost.Setup{}
			return []framehost.ExportValue{framehost.Provide("returned", contract, "returned value")}, nil
		},
	})
	boundSource := source("setup-result", plugin, "main", "bound", contract.Ref())
	returnedSource := source("setup-result", plugin, "main", "returned", contract.Ref())
	binding, err := host.Activate(ctx, boundSource, returnedSource)
	if err != nil {
		t.Fatalf("one Setup could not collect its whole result: %v", err)
	}
	if activations.Load() != 1 {
		t.Fatalf("original activation ran %d times", activations.Load())
	}
	owner, grant := ownerAndScope(t, host)
	use, err := acquireExports(host, ctx, binding, framehost.UseConfig{Owner: owner, Scope: grant})
	if err != nil {
		t.Fatal(err)
	}
	defer use.Close(context.Background())
	for _, expected := range []struct {
		source framehost.ExportRef
		value  string
	}{{boundSource, "staged value"}, {returnedSource, "returned value"}} {
		if err := framehost.BorrowExport(ctx, use, expected.source, func(_ context.Context, value string) error {
			if value != expected.value {
				t.Errorf("original result changed: %q", value)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bound.Bind(&original, "late value"); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("export staging outlived the original call: %v", err)
	}
	if err := original.OnActivate(func(context.Context) error { return nil }); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("activation registration outlived the original call: %v", err)
	}
}

func TestSetupRejectsDuplicatesAcrossItsWholeResult(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	contract, err := framehost.NewContract[string]("setup.duplicate", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	output := framehost.Export("value", contract)
	var bound, activated, cleaned atomic.Int32
	plugin := installNative(t, host, "duplicate-result", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: framehost.Exports(output)},
		Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
			if _, err := setup.Cleanup("original result", func() error { cleaned.Add(1); return nil }); err != nil {
				return nil, err
			}
			if err := output.Bind(setup, "staged"); err != nil {
				return nil, err
			}
			bound.Add(1)
			if err := setup.OnActivate(func(context.Context) error { activated.Add(1); return nil }); err != nil {
				return nil, err
			}
			return []framehost.ExportValue{framehost.Provide("value", contract, "returned")}, nil
		},
	})
	if _, err := host.Activate(ctx, source("duplicate-result", plugin, "main", "value", contract.Ref())); !errors.Is(err, frame.ErrInvalid) || bound.Load() != 1 {
		t.Fatalf("duplicate was not refused at whole-result admission: bound=%d error=%v", bound.Load(), err)
	}
	if err := host.WaitRetired(ctx, "duplicate-result"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Snapshot(ctx, contract.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := snapshot.Candidates(contract.Ref())
	if err != nil || len(candidates) != 0 || activated.Load() != 0 || cleaned.Load() != 1 {
		t.Fatalf("refused result escaped its original preparation: candidates=%d activations=%d cleanups=%d error=%v", len(candidates), activated.Load(), cleaned.Load(), err)
	}
}

func TestInterruptedSetupSealsItsWholeResult(t *testing.T) {
	cause := errors.New("original setup result interrupted")
	for _, typed := range []bool{false, true} {
		name := "raw"
		if typed {
			name = "typed"
		}
		for _, interruption := range []struct {
			name          string
			run           func() error
			expectedCause error
		}{
			{name: "error", run: func() error { return cause }, expectedCause: cause},
			{name: "panic", run: func() error { panic(cause) }, expectedCause: cause},
			{name: "goexit", run: func() error { runtime.Goexit(); return nil }},
		} {
			t.Run(name+"/"+interruption.name, func(t *testing.T) {
				ctx := testContext(t)
				host := testHost(t, framehost.Config{})
				contract, err := framehost.NewContract[string]("setup.interrupted", "1")
				if err != nil {
					t.Fatal(err)
				}
				if err := host.Register(contract); err != nil {
					t.Fatal(err)
				}
				output := framehost.Export("value", contract)
				var original framehost.Setup
				var activated, cleaned atomic.Int32
				prepare := func(setup *framehost.Setup) error {
					original = *setup
					if _, err := setup.Cleanup("original preparation", func() error { cleaned.Add(1); return nil }); err != nil {
						return err
					}
					if err := output.Bind(setup, "original value"); err != nil {
						return err
					}
					if err := setup.OnActivate(func(context.Context) error { activated.Add(1); return nil }); err != nil {
						return err
					}
					return interruption.run()
				}
				unit := framehost.Unit{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: framehost.Exports(output)},
					Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
						return nil, prepare(setup)
					},
				}
				if typed {
					unit, err = framehost.DefineUnit(framehost.UnitSpec{ID: "main", Exports: framehost.Exports(output), Prepare: prepare})
					if err != nil {
						t.Fatal(err)
					}
				}
				plugin := installNative(t, host, "interrupted-result", unit)
				_, err = host.Activate(ctx, source("interrupted-result", plugin, "main", "value", contract.Ref()))
				if err == nil || errors.Is(err, context.DeadlineExceeded) || interruption.expectedCause != nil && !errors.Is(err, interruption.expectedCause) {
					t.Fatalf("original interruption did not conclude preparation: %v", err)
				}
				if err := host.WaitRetired(ctx, "interrupted-result"); err != nil {
					t.Fatal(err)
				}
				if activated.Load() != 0 || cleaned.Load() != 1 {
					t.Fatalf("interruption changed the original exit: activations=%d cleanups=%d", activated.Load(), cleaned.Load())
				}
				if err := output.Bind(&original, "late value"); !errors.Is(err, frame.ErrClosed) {
					t.Fatalf("ended result accepted another export: %v", err)
				}
				if err := original.OnActivate(func(context.Context) error { return nil }); !errors.Is(err, frame.ErrClosed) {
					t.Fatalf("ended result accepted another activation: %v", err)
				}
			})
		}
	}
}
