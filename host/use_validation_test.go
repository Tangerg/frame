package host_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestInvalidUseIdentityIsRejectedBeforePreparation(t *testing.T) {
	for _, test := range []struct {
		name      string
		realm     framehost.RealmID
		execution string
	}{
		{"realm", "invalid realm", "execution"},
		{"execution", "realm", "invalid execution"},
	} {
		t.Run(test.name, func(t *testing.T) {
			protection := &useRetirementProtection{approve: func() error { return nil }, releases: make(chan recordedUseRelease, 1)}
			host := testHost(t, framehost.Config{ExecutionProtection: protection})
			var prepared atomic.Int32
			plugin := installNative(t, host, "provider", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
				Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
					prepared.Add(1)
					return nil, nil
				},
			})
			owner, scope := ownerAndScope(t, host)
			root := framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}
			use, err := host.AcquireUnit(t.Context(), root, framehost.UseConfig{Owner: owner, Scope: scope, Realm: test.realm, ExecutionID: test.execution})
			if use != nil || !errors.Is(err, frame.ErrInvalid) {
				t.Fatalf("invalid use identity was admitted: %v", err)
			}
			if prepared.Load() != 0 {
				t.Fatalf("invalid use identity started %d plugin preparations", prepared.Load())
			}
		})
	}
}

func TestUseAdmissionReadsOriginalOwners(t *testing.T) {
	for _, authority := range []string{"host", "owner", "instance", "provider", "draining"} {
		t.Run(authority, func(t *testing.T) {
			ctx := testContext(t)
			lifetime := frame.NewLifetime(t.Context())
			host := testHost(t, framehost.Config{Lifetime: lifetime})
			owner, scope := ownerAndScope(t, host)
			input, err := framehost.NewContract[string]("original.input", "1")
			if err != nil {
				t.Fatal(err)
			}
			output, err := framehost.NewContract[func() string]("original.output", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(input, output); err != nil {
				t.Fatal(err)
			}
			var providerLifetime, instanceLifetime frame.Lifetime
			provider := installNative(t, host, "provider", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: input.Ref()}}},
				Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					providerLifetime = setup.Lifetime()
					return []framehost.ExportValue{framehost.Provide("value", input, "original")}, nil
				},
			})
			var calls atomic.Int32
			consumer := installNative(t, host, "consumer", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo,
					Imports: []framehost.ImportDeclaration{{Slot: "input", Contract: input.Ref(), Providers: []framehost.ExportRef{source("provider", provider, "main", "value", input.Ref())}}},
					Exports: []framehost.ExportDeclaration{{Name: "call", Contract: output.Ref()}},
				},
				Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					instanceLifetime = setup.Lifetime()
					value, err := framehost.Import(setup, "input", input)
					return []framehost.ExportValue{framehost.Provide("call", output, func() string { calls.Add(1); return value })}, err
				},
			})
			root := source("consumer", consumer, "main", "call", output.Ref())
			plan, err := host.Plan(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			use, err := host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Scope: scope})
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("original authority cancelled")
			switch authority {
			case "host":
				lifetime.Cancel(cause)
			case "owner":
				owner.Cancel(cause)
			case "instance":
				instanceLifetime.Cancel(cause)
			case "provider":
				providerLifetime.Cancel(cause)
			case "draining":
				if err := host.Retire(ctx, "consumer"); err != nil {
					t.Fatal(err)
				}
			}
			validation := host.ValidateUse(ctx, use)
			err = framehost.BorrowExport(ctx, use, root, func(_ context.Context, call func() string) error {
				if got := call(); got != "original" {
					t.Fatalf("borrow replaced its original value: %q", got)
				}
				return nil
			})
			if authority == "draining" {
				if validation != nil || err != nil || calls.Load() != 1 {
					t.Errorf("retirement invalidated an original borrow while draining: validation=%v borrow=%v calls=%d", validation, err, calls.Load())
				}
			} else if !errors.Is(validation, frame.ErrClosed) || !errors.Is(err, frame.ErrClosed) || calls.Load() != 0 {
				t.Errorf("cancelled original authority still admitted plugin work: validation=%v borrow=%v calls=%d", validation, err, calls.Load())
			}
			if outcome, err := use.Close(ctx); !outcome.Released || err != nil {
				t.Fatalf("original Use could not retire: %+v, %v", outcome, err)
			}
		})
	}
}

func TestUseGrantRechecksRuntimeAfterApproval(t *testing.T) {
	for _, providerCancels := range []bool{false, true} {
		name := "instance"
		if providerCancels {
			name = "provider"
		}
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			protection := &useRetirementProtection{releases: make(chan recordedUseRelease, 1)}
			host := testHost(t, framehost.Config{ExecutionProtection: protection})
			input, err := framehost.NewContract[string]("approval.input", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(input); err != nil {
				t.Fatal(err)
			}
			var providerLifetime, instanceLifetime frame.Lifetime
			provider := installNative(t, host, "provider", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: input.Ref()}}},
				Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					providerLifetime = setup.Lifetime()
					return []framehost.ExportValue{framehost.Provide("value", input, "original")}, nil
				},
			})
			consumer := installNative(t, host, "consumer", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "input", Contract: input.Ref(), Providers: []framehost.ExportRef{source("provider", provider, "main", "value", input.Ref())}}}},
				Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					instanceLifetime = setup.Lifetime()
					return nil, nil
				},
			})
			protection.approve = func() error {
				cancelled := instanceLifetime
				if providerCancels {
					cancelled = providerLifetime
				}
				cancelled.Cancel(errors.New("original runtime stopped during approval"))
				return nil
			}
			owner, scope := ownerAndScope(t, host)
			use, err := host.AcquireUnit(ctx, framehost.UnitRef{InstallationID: "consumer", Release: consumer.Revision(), UnitID: "main"}, framehost.UseConfig{Owner: owner, Scope: scope, Realm: "approval"})
			if use != nil || !errors.Is(err, frame.ErrClosed) {
				if use != nil {
					_, _ = use.Close(ctx)
				}
				t.Fatalf("approval published a Use after its original runtime stopped: %v, %v", use, err)
			}
			select {
			case released := <-protection.releases:
				if released.id != protection.approval.ID || released.proof.CloseID == 0 {
					t.Fatalf("refused handoff did not settle its original approval: %+v", released)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestCapturedBindingCannotExtendADrainingProvider(t *testing.T) {
	for _, transitive := range []bool{false, true} {
		name := "direct"
		if transitive {
			name = "transitive"
		}
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, framehost.Config{})
			input, err := framehost.NewContract[string]("draining.input", "1")
			if err != nil {
				t.Fatal(err)
			}
			output, err := framehost.NewContract[func() string]("draining.output", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(input, output); err != nil {
				t.Fatal(err)
			}
			var providerID string
			var cleanups atomic.Int32
			provider := installNative(t, host, "provider", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: input.Ref()}}},
				Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					providerID = setup.InstanceID()
					_, err := setup.Cleanup("original provider", func() error { cleanups.Add(1); return nil })
					return []framehost.ExportValue{framehost.Provide("value", input, "original")}, err
				},
			})
			dependency := source("provider", provider, "main", "value", input.Ref())
			if transitive {
				middle := installNative(t, host, "middle", framehost.Unit{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "input", Contract: input.Ref(), Providers: []framehost.ExportRef{dependency}}}, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: input.Ref()}}},
					Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
						value, err := framehost.Import(setup, "input", input)
						return []framehost.ExportValue{framehost.Provide("value", input, value)}, err
					},
				})
				dependency = source("middle", middle, "main", "value", input.Ref())
			}
			consumer := installNative(t, host, "consumer", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "input", Contract: input.Ref(), Providers: []framehost.ExportRef{dependency}}}, Exports: []framehost.ExportDeclaration{{Name: "read", Contract: output.Ref()}}},
				Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					value, err := framehost.Import(setup, "input", input)
					return []framehost.ExportValue{framehost.Provide("read", output, func() string { return value })}, err
				},
			})
			root := source("consumer", consumer, "main", "read", output.Ref())
			if _, err := host.Activate(ctx, root); err != nil {
				t.Fatal(err)
			}
			snapshot, err := host.Snapshot(ctx, output.Ref())
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := snapshot.Candidates(output.Ref())
			if err != nil {
				t.Fatal(err)
			}
			selection, err := framehost.SelectRefs(snapshot, output.Ref(), "original", candidates...)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := framehost.Capture(snapshot, selection)
			if err != nil {
				t.Fatal(err)
			}
			owner, scope := ownerAndScope(t, host)
			config := framehost.UseConfig{Owner: owner, Scope: scope}
			original, err := host.Acquire(ctx, plan, config)
			if err != nil {
				t.Fatal(err)
			}
			defer original.Close(context.Background())
			if err := host.RetireInstance(ctx, providerID); err != nil {
				t.Fatal(err)
			}
			if next, err := host.Acquire(ctx, plan, config); next != nil || !errors.Is(err, framehost.ErrStaleBinding) {
				if next != nil {
					defer next.Close(context.Background())
				}
				t.Errorf("captured admission extended an original draining provider: use=%v error=%v", next, err)
			}
			if next, err := host.AcquireUnit(ctx, root.Unit(), config); next != nil || err == nil {
				if next != nil {
					defer next.Close(context.Background())
				}
				t.Errorf("declaration admission extended an original draining provider: use=%v error=%v", next, err)
			}
			if err := framehost.BorrowExport(ctx, original, root, func(_ context.Context, read func() string) error {
				if value := read(); value != "original" {
					t.Errorf("original borrow changed its provider: %q", value)
				}
				return nil
			}); err != nil || cleanups.Load() != 0 {
				t.Fatalf("withdrawal invalidated an already accepted borrow: cleanups=%d error=%v", cleanups.Load(), err)
			}
		})
	}
}
