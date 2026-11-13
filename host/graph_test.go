package host_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	framehost "github.com/Tangerg/frame/host"
)

func optionalExport(t *testing.T, host framehost.Host, contract framehost.Contract[string], id string) framehost.ExportRef {
	t.Helper()
	plugin := installNative(t, host, id, framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}},
		Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Provide("value", contract, id)}, nil
		},
	})
	return source(id, plugin, "main", "value", contract.Ref())
}

func TestDeclarationPlanRechecksOptionalAbsenceAtAdmission(t *testing.T) {
	for _, boundary := range []string{"activate", "acquire unit", "business commit"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, framehost.Config{})
			output, _ := framehost.NewContract[string]("example.consumer", "1")
			optional, _ := framehost.NewContract[string]("example.optional", "1")
			if err := host.Register(output, optional); err != nil {
				t.Fatal(err)
			}
			var setups atomic.Int64
			consumer := installNative(t, host, "consumer", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: output.Ref()}}, Imports: []framehost.ImportDeclaration{{Slot: "optional", Contract: optional.Ref(), Optional: true}}},
				Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					setups.Add(1)
					value, found, err := framehost.OptionalImport(setup, "optional", optional)
					if err != nil {
						return nil, err
					}
					if !found {
						value = "absent"
					}
					return []framehost.ExportValue{framehost.Provide("value", output, value)}, nil
				},
			})
			root := source("consumer", consumer, "main", "value", output.Ref())
			var plan framehost.BindingPlan
			var err error
			switch boundary {
			case "activate":
				plan, err = host.Plan(ctx, root)
			case "acquire unit":
				plan, err = host.PlanUnit(ctx, "", root.Unit(), nil)
			case "business commit":
				plan, err = host.PlanWith(ctx, "", []framehost.BindingChoice{{Unit: root.Unit(), Slot: "optional", Providers: []framehost.ExportRef{}}}, root)
			}
			if err != nil {
				t.Fatal(err)
			}
			optionalExport(t, host, optional, "late")
			committed := false
			switch boundary {
			case "activate":
				_, err = host.ActivatePlan(ctx, "", plan)
			case "acquire unit":
				owner, scope := ownerAndScope(t, host)
				var use *framehost.Use
				use, err = host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Scope: scope})
				if use != nil {
					t.Fatal("stale plan received a partial Use")
				}
			case "business commit":
				err = host.Admission().Admit(ctx, func(admitted context.Context) error {
					if _, err := host.ValidatePlanUnder(admitted, plan); err != nil {
						return err
					}
					committed = true
					return nil
				})
			}
			if !errors.Is(err, framehost.ErrStaleBinding) || setups.Load() != 0 || committed {
				t.Fatalf("preparation absence escaped revalidation: %v; setups=%d; committed=%v", err, setups.Load(), committed)
			}
			fresh, err := host.Plan(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			owner, scope := ownerAndScope(t, host)
			use, err := host.Acquire(ctx, fresh, framehost.UseConfig{Owner: owner, Scope: scope})
			if err != nil {
				t.Fatal(err)
			}
			if err := framehost.BorrowExport(ctx, use, root, func(_ context.Context, value string) error {
				if value != "late" {
					t.Fatalf("fresh plan did not use the newly admitted provider: %q", value)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOptionalPreparationPremiseSurvivesSlowSetupAndActivation(t *testing.T) {
	for _, phase := range []string{"setup", "activation"} {
		t.Run(phase, func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, framehost.Config{})
			output, _ := framehost.NewContract[string]("example.preparing", "1")
			optional, _ := framehost.NewContract[string]("example.optional", "1")
			if err := host.Register(output, optional); err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			resume := make(chan struct{})
			released := make(chan struct{})
			var activations atomic.Int64
			consumer := installNative(t, host, "consumer", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: output.Ref()}}, Imports: []framehost.ImportDeclaration{{Slot: "optional", Contract: optional.Ref(), Optional: true}}},
				Setup: func(ctx context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					if _, err := setup.Cleanup("original preparation", func() error { close(released); return nil }); err != nil {
						return nil, err
					}
					block := func(ctx context.Context) error {
						close(entered)
						select {
						case <-resume:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					if err := setup.OnActivate(func(ctx context.Context) error {
						activations.Add(1)
						if phase == "activation" {
							return block(ctx)
						}
						return nil
					}); err != nil {
						return nil, err
					}
					if phase == "setup" {
						if err := block(ctx); err != nil {
							return nil, err
						}
					}
					return []framehost.ExportValue{framehost.Provide("value", output, "absent")}, nil
				},
			})
			root := source("consumer", consumer, "main", "value", output.Ref())
			finished := make(chan error, 1)
			go func() {
				_, err := host.Activate(ctx, root)
				finished <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("preparation did not reach its controlled boundary")
			}
			// Another request that sees Preparing must retain the same premise.
			pending, err := host.Plan(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			optionalExport(t, host, optional, "late")
			if _, err := host.ActivatePlan(ctx, "", pending); !errors.Is(err, framehost.ErrStaleBinding) {
				t.Fatalf("preparing instance froze absence before approval: %v", err)
			}
			close(resume)
			if err := <-finished; !errors.Is(err, framehost.ErrStaleBinding) {
				t.Fatalf("late preparation became public: %v", err)
			}
			if phase == "setup" && activations.Load() != 0 {
				t.Fatal("stale preparation invoked its activation callback")
			}
			if phase == "activation" && activations.Load() != 1 {
				t.Fatal("activation boundary was not exercised")
			}
			select {
			case <-released:
			case <-ctx.Done():
				t.Fatal("rejected preparation did not close its original resource")
			}
			snapshot, err := host.Snapshot(ctx, output.Ref())
			if err != nil {
				t.Fatal(err)
			}
			values, err := snapshot.Candidates(output.Ref())
			if err != nil || len(values) != 0 {
				t.Fatalf("failed preparation leaked public exports: %+v, %v", values, err)
			}
		})
	}
}

func TestCommittedAbsenceRestoresExactlyBeforeFirstActivation(t *testing.T) {
	ctx := testContext(t)
	original := testHost(t, framehost.Config{})
	output, _ := framehost.NewContract[string]("example.retained", "1")
	optional, _ := framehost.NewContract[string]("example.optional", "1")
	if err := original.Register(output, optional); err != nil {
		t.Fatal(err)
	}
	consumer := installNative(t, original, "consumer", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: output.Ref()}}, Imports: []framehost.ImportDeclaration{{Slot: "optional", Contract: optional.Ref(), Optional: true}}},
		Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
			value, found, err := framehost.OptionalImport(setup, "optional", optional)
			if err != nil {
				return nil, err
			}
			if !found {
				value = "absent"
			}
			return []framehost.ExportValue{framehost.Provide("value", output, value)}, nil
		},
	})
	root := source("consumer", consumer, "main", "value", output.Ref())
	plan, err := original.Plan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	var retained []byte
	if err := original.Admission().Admit(ctx, func(admitted context.Context) error {
		binding, err := original.ValidatePlanUnder(admitted, plan)
		if err != nil {
			return err
		}
		retained, err = binding.CanonicalJSON()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(original.Observations()) != 0 {
		t.Fatal("committing a binding started execution")
	}
	if _, err := original.Close(ctx); err != nil {
		t.Fatal(err)
	}
	restored := testHost(t, framehost.Config{ReadAcceptedBinding: func(_ context.Context, id string, grant framehost.ScopeGrant) (framehost.BindingSnapshot, error) {
		if id != "original" {
			return framehost.BindingSnapshot{}, framehost.ErrUnavailable
		}
		return framehost.ParseBinding(retained)
	}})
	if err := restored.Register(output, optional); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Install(ctx, framehost.InstallationConfig{ID: "consumer", Plugin: consumer, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	optionalExport(t, restored, optional, "late")

	owner, scope := ownerAndScope(t, restored)
	if use, err := restored.AcquireRecord(ctx, "missing", framehost.UseConfig{Owner: owner, Scope: scope}); use != nil || !errors.Is(err, framehost.ErrUnavailable) {
		t.Fatalf("missing owner record was acquired: %v", err)
	}
	use, err := restored.AcquireRecord(ctx, "original", framehost.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := framehost.BorrowExport(ctx, use, root, func(_ context.Context, value string) error {
		if value != "absent" {
			t.Fatalf("recovery rediscovered optional providers: %q", value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := use.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restored.Retire(ctx, "consumer"); err != nil {
		t.Fatal(err)
	}
	if err := restored.WaitRetired(ctx, "consumer"); err != nil {
		t.Fatal(err)
	}
	fresh, err := restored.Plan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := restored.Acquire(ctx, fresh, framehost.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := framehost.BorrowExport(ctx, rebuilt, root, func(_ context.Context, value string) error {
		if value != "late" {
			t.Fatalf("explicit rebuild did not adopt the new optional binding: %q", value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFixedDeclarationBindingsDoNotReadOtherProviders(t *testing.T) {
	for _, present := range []bool{false, true} {
		name := "declared absent"
		if present {
			name = "declared provider"
		}
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, framehost.Config{})
			output, _ := framehost.NewContract[string]("example.fixed", "1")
			optional, _ := framehost.NewContract[string]("example.optional", "1")
			if err := host.Register(output, optional); err != nil {
				t.Fatal(err)
			}
			providers := []framehost.ExportRef{}
			if present {
				providers = append(providers, optionalExport(t, host, optional, "selected"))
			}
			consumer := installNative(t, host, "consumer", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: output.Ref()}}, Imports: []framehost.ImportDeclaration{{Slot: "optional", Contract: optional.Ref(), Optional: true, Providers: providers}}},
				Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					value, found, err := framehost.OptionalImport(setup, "optional", optional)
					if err != nil {
						return nil, err
					}
					if found != present || found && value != "selected" {
						return nil, errors.New("declared binding changed")
					}
					return []framehost.ExportValue{framehost.Provide("value", output, value)}, nil
				},
			})
			root := source("consumer", consumer, "main", "value", output.Ref())
			plan, err := host.Plan(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			optionalExport(t, host, optional, "unselected")
			if _, err := host.ActivatePlan(ctx, "", plan); err != nil {
				t.Fatalf("unread candidate invalidated a fixed declaration: %v", err)
			}
		})
	}
}

func TestFreshPlanCannotCommitAfterAnotherSetupOwnsItsUnit(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	output, _ := framehost.NewContract[string]("example.setup-owner", "1")
	dependency, _ := framehost.NewContract[string]("example.dependency", "1")
	if err := host.Register(output, dependency); err != nil {
		t.Fatal(err)
	}
	first := optionalExport(t, host, dependency, "first")
	second := optionalExport(t, host, dependency, "second")
	consumer := installNative(t, host, "consumer", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: output.Ref()}}, Imports: []framehost.ImportDeclaration{{Slot: "dependency", Contract: dependency.Ref()}}},
		Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
			value, err := framehost.Import(setup, "dependency", dependency)
			if err != nil {
				return nil, err
			}
			return []framehost.ExportValue{framehost.Provide("value", output, value)}, nil
		},
	})
	root := source("consumer", consumer, "main", "value", output.Ref())
	firstPlan, err := host.PlanWith(ctx, "", []framehost.BindingChoice{{Unit: root.Unit(), Slot: "dependency", Providers: []framehost.ExportRef{first}}}, root)
	if err != nil {
		t.Fatal(err)
	}
	secondPlan, err := host.PlanWith(ctx, "", []framehost.BindingChoice{{Unit: root.Unit(), Slot: "dependency", Providers: []framehost.ExportRef{second}}}, root)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := host.ActivatePlan(ctx, "", secondPlan)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	err = host.Admission().Admit(ctx, func(admitted context.Context) error {
		if _, err := host.ValidatePlanUnder(admitted, firstPlan); err != nil {
			return err
		}
		committed = true
		return nil
	})
	if !errors.Is(err, framehost.ErrStaleBinding) || committed {
		t.Fatalf("fresh business commit bypassed the actual setup owner: %v, committed=%v", err, committed)
	}
	if _, err := host.ActivatePlan(ctx, "", firstPlan); !errors.Is(err, framehost.ErrStaleBinding) {
		t.Fatalf("activation and business admission disagreed: %v", err)
	}
	if err := host.Admission().Admit(ctx, func(admitted context.Context) error {
		return host.ValidateBindingUnder(admitted, accepted)
	}); err != nil {
		t.Fatalf("accepted original binding was invalidated: %v", err)
	}
}

func TestRestoredUseReauthorizesTheOriginalRecordAfterPreparation(t *testing.T) {
	for _, change := range []string{"unchanged", "revoked", "rebound"} {
		t.Run(change, func(t *testing.T) {
			ctx := testContext(t)
			var original framehost.BindingSnapshot
			var refusal error
			var reads int
			h := testHost(t, framehost.Config{ReadAcceptedBinding: func(context.Context, string, framehost.ScopeGrant) (framehost.BindingSnapshot, error) {
				reads++
				return original, refusal
			}})
			entered, resume := make(chan struct{}), make(chan struct{})
			contract, _ := framehost.NewContract[string]("records.text", "1")
			if err := h.Register(contract); err != nil {
				t.Fatal(err)
			}
			plugin := installNative(t, h, "provider", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}},
				Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
					close(entered)
					<-resume
					return []framehost.ExportValue{framehost.Provide("value", contract, "original")}, nil
				},
			})
			plan, err := h.Plan(ctx, source("provider", plugin, "main", "value", contract.Ref()))
			if err != nil {
				t.Fatal(err)
			}
			original, err = plan.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			owner, scope := ownerAndScope(t, h)
			result := make(chan error, 1)
			go func() {
				use, err := h.AcquireRecord(ctx, "record", framehost.UseConfig{Owner: owner, Scope: scope})
				if use != nil {
					_, closeErr := use.Close(ctx)
					err = errors.Join(err, closeErr)
				}
				result <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			err = h.Admission().Under(ctx, func() error {
				switch change {
				case "revoked":
					refusal = framehost.ErrUnavailable
				case "rebound":
					original.Rule = "other"
				}
				return nil
			})
			close(resume)
			if err != nil {
				t.Fatal(err)
			}
			err = <-result
			var want error
			switch change {
			case "revoked":
				want = framehost.ErrUnavailable
			case "rebound":
				want = framehost.ErrStaleBinding
			}
			if !errors.Is(err, want) || reads != 2 {
				t.Fatalf("owner reads=%d, acquisition=%v, want %v", reads, err, want)
			}
		})
	}
}
