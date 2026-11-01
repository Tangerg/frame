package frame_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
)

func TestCandidateProvenanceCannotCrossCaptureBoundaries(t *testing.T) {
	ctx := testContext(t)
	point, _ := frame.NewPoint[string, func() string]("conformance.primary", "1")
	other, _ := frame.NewPoint[string, func() string]("conformance.other", "1")
	var calls atomic.Int64
	makeHost := func() (*frame.Host, *frame.Snapshot) {
		host := testHost(t, frame.HostConfig{})
		if err := host.Register(point, other); err != nil {
			t.Fatal(err)
		}
		plugin := installNative(t, host, "provider", frame.Unit{
			Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "primary", Contract: point.Ref()}, {Name: "other", Contract: other.Ref()}}},
			Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
				value := func() string { calls.Add(1); return "original" }
				return []frame.ExportValue{frame.Contribute("primary", point, "primary", value), frame.Contribute("other", other, "other", value)}, nil
			},
		})
		if _, err := host.Activate(ctx, source("provider", plugin, "main", "primary", point.Ref())); err != nil {
			t.Fatal(err)
		}
		snapshot, err := host.Snapshot(ctx, point.Ref(), other.Ref())
		if err != nil {
			t.Fatal(err)
		}
		return host, snapshot
	}
	host, snapshot := makeHost()
	foreignHost, foreignSnapshot := makeHost()
	candidates, err := frame.Candidates(snapshot, point)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("original candidates: %v, %v", candidates, err)
	}
	otherCandidates, _ := frame.Candidates(snapshot, other)
	foreignCandidates, _ := frame.Candidates(foreignSnapshot, point)
	for _, test := range []struct {
		name      string
		candidate frame.Candidate[string, func() string]
	}{
		{"forged zero candidate", frame.Candidate[string, func() string]{}},
		{"another Point", otherCandidates[0]},
		{"another Host with the same source identities", foreignCandidates[0]},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := frame.Select(snapshot, point, "consumer.first/1", test.candidate); !errors.Is(err, frame.ErrContractMismatch) {
				t.Fatalf("foreign candidate accepted: %v", err)
			}
		})
	}
	selection, err := frame.Select(snapshot, point, "consumer.first/1", candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := frame.Capture(snapshot, frame.SelectionRef{}); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("forged selection accepted: %v", err)
	}
	if _, err := frame.Capture(foreignSnapshot, selection.Ref()); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("foreign selection accepted: %v", err)
	}
	plan, err := frame.Capture(snapshot, selection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	owner, scope := ownerAndScope(t, foreignHost)
	if use, err := foreignHost.Acquire(ctx, plan, frame.UseConfig{Owner: owner, Scope: scope}); err == nil || use != nil {
		t.Fatalf("another Host admitted the original plan: %v, %v", use, err)
	}
	if err := host.Withdraw(ctx, candidates[0].ID()); err != nil {
		t.Fatal(err)
	}
	fresh, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := frame.Select(fresh, point, "consumer.first/1", candidates[0]); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("old publication crossed into a fresh snapshot: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("a rejected provenance dispatched a contribution")
	}
}

func TestSameContributionKeyKeepsOldAndNewUsesDistinct(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, frame.HostConfig{})
	point, _ := frame.NewPoint[string, func() string]("conformance.replaceable", "1")
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	var instanceID string
	plugin := installNative(t, host, "provider", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "value", Contract: point.Ref()}}},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			instanceID = setup.InstanceID()
			return []frame.ExportValue{frame.Contribute("value", point, "old", func() string { return "old implementation" })}, nil
		},
	})
	if _, err := host.Activate(ctx, source("provider", plugin, "main", "value", point.Ref())); err != nil {
		t.Fatal(err)
	}
	capture := func() (frame.Candidate[string, func() string], frame.Selection[string, func() string], *frame.BindingPlan) {
		snapshot, err := host.Snapshot(ctx, point.Ref())
		if err != nil {
			t.Fatal(err)
		}
		candidates, err := frame.Candidates(snapshot, point)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("visible publication: %v, %v", candidates, err)
		}
		selection, err := frame.Select(snapshot, point, "consumer.first/1", candidates[0])
		if err != nil {
			t.Fatal(err)
		}
		plan, err := frame.Capture(snapshot, selection.Ref())
		if err != nil {
			t.Fatal(err)
		}
		return candidates[0], selection, plan
	}
	oldCandidate, oldSelection, oldPlan := capture()
	owner, scope := ownerAndScope(t, host)
	oldUse, err := host.Acquire(ctx, oldPlan, frame.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Withdraw(ctx, oldCandidate.ID()); err != nil {
		t.Fatal(err)
	}
	publication, err := host.Publish(ctx, instanceID, "replace-original-key", frame.Contribute("value", point, "new", func() string { return "new implementation" }))
	if err != nil {
		t.Fatal(err)
	}
	newCandidate, newSelection, newPlan := capture()
	if oldCandidate.Source() != newCandidate.Source() || oldCandidate.ID() == newCandidate.ID() || publication.ID != newCandidate.ID() {
		t.Fatalf("same key lost publication identity: old=%s new=%s publication=%+v", oldCandidate.ID(), newCandidate.ID(), publication)
	}
	if use, err := host.Acquire(ctx, oldPlan, frame.UseConfig{Owner: owner, Scope: scope}); !errors.Is(err, frame.ErrStaleBinding) || use != nil {
		t.Fatalf("old publication was newly admitted after republishing: %v, %v", use, err)
	}
	newUse, err := host.Acquire(ctx, newPlan, frame.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	read := func(use *frame.Use, selection frame.Selection[string, func() string], want string) {
		t.Helper()
		if err := frame.Borrow(ctx, use, selection, func(_ context.Context, values []func() string) error {
			if len(values) != 1 || values[0]() != want {
				t.Fatalf("Use changed its original publication: want %q", want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	read(oldUse, oldSelection, "old implementation")
	read(newUse, newSelection, "new implementation")
	if _, err := oldUse.Close(ctx); err != nil {
		t.Fatal(err)
	}
	read(newUse, newSelection, "new implementation")
	if err := host.Withdraw(ctx, newCandidate.ID()); err != nil {
		t.Fatal(err)
	}
	read(newUse, newSelection, "new implementation")
}

func TestUnresolvableGraphsDoNotStartOrPublishPartialInstances(t *testing.T) {
	for _, scenario := range []string{"required missing", "service ambiguous", "dependency cycle"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, frame.HostConfig{})
			input, _ := frame.NewContract[string]("conformance.input", "1")
			output, _ := frame.NewContract[string]("conformance.output", "1")
			if err := host.Register(input, output); err != nil {
				t.Fatal(err)
			}
			var setups atomic.Int64
			providerSetup := func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
				setups.Add(1)
				return []frame.ExportValue{frame.Provide("input", input, "value")}, nil
			}
			want := frame.ErrUnavailable
			if scenario == "service ambiguous" {
				want = frame.ErrAmbiguous
				for _, id := range []string{"left", "right"} {
					installNative(t, host, id, frame.Unit{Declaration: frame.UnitDeclaration{ID: "provider", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "input", Contract: input.Ref()}}}, Setup: providerSetup})
				}
			}
			consumer := frame.Unit{
				Declaration: frame.UnitDeclaration{ID: "consumer", Profile: frame.NativeGo, Imports: []frame.ImportDeclaration{{Slot: "required-input", Contract: input.Ref()}}, Exports: []frame.ExportDeclaration{{Name: "output", Contract: output.Ref()}}},
				Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
					setups.Add(1)
					return []frame.ExportValue{frame.Provide("output", output, "value")}, nil
				},
			}
			units := []frame.Unit{consumer}
			if scenario == "dependency cycle" {
				want = frame.ErrDependencyCycle
				units = append(units, frame.Unit{Declaration: frame.UnitDeclaration{ID: "provider", Profile: frame.NativeGo, Imports: []frame.ImportDeclaration{{Slot: "cycle-output", Contract: output.Ref()}}, Exports: []frame.ExportDeclaration{{Name: "input", Contract: input.Ref()}}}, Setup: providerSetup})
			}
			plugin := installNative(t, host, "graph", units...)
			_, err := host.Activate(ctx, source("graph", plugin, "consumer", "output", output.Ref()))
			if !errors.Is(err, want) || !strings.Contains(err.Error(), "required-input") {
				t.Fatalf("graph rejection did not identify its cause and slot: %v", err)
			}
			if scenario == "dependency cycle" && (!strings.Contains(err.Error(), "graph/consumer") || !strings.Contains(err.Error(), "graph/provider")) {
				t.Fatalf("cycle diagnostic lost its actual path: %v", err)
			}
			if setups.Load() != 0 {
				t.Fatalf("invalid graph started %d units", setups.Load())
			}
			snapshot, err := host.Snapshot(ctx, input.Ref(), output.Ref())
			if err != nil {
				t.Fatal(err)
			}
			for _, contract := range []frame.ContractRef{input.Ref(), output.Ref()} {
				candidates, err := snapshot.Candidates(contract)
				if err != nil || len(candidates) != 0 {
					t.Fatalf("failed graph published partial exports: %v, %v", candidates, err)
				}
			}
		})
	}
}

func TestOneReleaseHasIndependentInstallationConfigurationAndLifetime(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, frame.HostConfig{})
	type value struct {
		configuration string
		instance      string
		installation  frame.InstallationID
	}
	contract, _ := frame.NewContract[value]("conformance.configured", "1")
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	released := map[frame.InstallationID]chan struct{}{"left": make(chan struct{}), "right": make(chan struct{})}
	plugin, err := frame.Define(frame.Definition{Revision: integrity.Hash([]byte("one immutable release")), Units: []frame.Unit{{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			id := setup.Binding().Source.InstallationID
			if _, err := setup.Cleanup("installation resource", func() error { close(released[id]); return nil }); err != nil {
				return nil, err
			}
			return []frame.ExportValue{frame.Provide("value", contract, value{configuration: string(setup.Configuration()), instance: setup.InstanceID(), installation: id})}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := ownerAndScope(t, host)
	type accepted struct {
		root    frame.ExportRef
		binding frame.BindingSnapshot
		scope   frame.ScopeGrant
		use     *frame.Use
		value   value
	}
	acceptedByID := map[frame.InstallationID]accepted{}
	for _, id := range []frame.InstallationID{"left", "right"} {
		configuration := []byte(`{"label":"` + string(id) + `"}`)
		if _, err := host.Install(ctx, frame.InstallationConfig{ID: id, Plugin: plugin, Configuration: configuration, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		root := source(string(id), plugin, "main", "value", contract.Ref())
		binding, err := host.Activate(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		scope, err := host.Admission().OpenScope(ctx, frame.ScopeID("consumer:"+string(id)))
		if err != nil {
			t.Fatal(err)
		}
		use, err := acquireExports(host, ctx, binding, frame.UseConfig{Owner: owner, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		var original value
		if err := frame.BorrowExport(ctx, use, root, func(_ context.Context, actual value) error { original = actual; return nil }); err != nil {
			t.Fatal(err)
		}
		if original.configuration != string(configuration) || original.installation != id {
			t.Fatalf("installation configuration or source was shared: %+v", original)
		}
		acceptedByID[id] = accepted{root, binding, scope, use, original}
	}
	left, right := acceptedByID["left"], acceptedByID["right"]
	if left.root.Release != right.root.Release || left.value.instance == right.value.instance || left.use.ID() == right.use.ID() {
		t.Fatal("installations did not share only their immutable release")
	}
	host.Admission().FenceScope(left.scope.ID())
	if err := host.ValidateUse(ctx, left.use); !errors.Is(err, frame.ErrFenced) && !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("fenced installation scope still authorized its Use: %v", err)
	}
	if use, err := acquireExports(host, ctx, left.binding, frame.UseConfig{Owner: owner, Scope: left.scope}); !errors.Is(err, frame.ErrFenced) || use != nil {
		t.Fatalf("fenced scope admitted another Use: %v, %v", use, err)
	}
	if _, err := left.use.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := host.Uninstall(ctx, "left"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released["left"]:
	case <-ctx.Done():
		t.Fatal("left installation did not release its original resource")
	}
	select {
	case <-released["right"]:
		t.Fatal("left retirement released the right installation")
	default:
	}
	if err := frame.BorrowExport(ctx, right.use, right.root, func(_ context.Context, actual value) error {
		if actual != right.value {
			t.Fatal("the independent right binding changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := right.use.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := host.Uninstall(ctx, "right"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released["right"]:
	case <-ctx.Done():
		t.Fatal("right installation did not release independently")
	}
}

func TestPluginDeclaredPointDoesNotCreateAnImplicitSetupDependency(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, frame.HostConfig{})
	point, err := frame.NewPoint[string, func() string]("publisher.validators", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	var declarerSetups, contributorSetups, calls atomic.Int64
	installNative(t, host, "declaration-publisher", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "optional-runtime", Profile: frame.NativeGo},
		Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			declarerSetups.Add(1)
			return nil, nil
		},
	})
	contributor := installNative(t, host, "independent-contributor", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "validate", Contract: point.Ref()}}},
		Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			contributorSetups.Add(1)
			return []frame.ExportValue{frame.Contribute("validate", point, "plain-text", func() string { calls.Add(1); return "valid" })}, nil
		},
	})
	if _, err := host.Activate(ctx, source("independent-contributor", contributor, "main", "validate", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := frame.Candidates(snapshot, point)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("independent contribution: %v, %v", candidates, err)
	}
	selection, err := frame.Select(snapshot, point, "application.validators/1", candidates...)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := frame.Capture(snapshot, selection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	owner, scope := ownerAndScope(t, host)
	use, err := host.Acquire(ctx, plan, frame.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Uninstall(ctx, "declaration-publisher"); err != nil {
		t.Fatalf("contract reference invented a setup dependency: %v", err)
	}
	if err := frame.Borrow(ctx, use, selection, func(_ context.Context, validators []func() string) error {
		if validators[0]() != "valid" {
			t.Fatal("independent contribution changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if declarerSetups.Load() != 0 || contributorSetups.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("declaration ownership caused implicit execution: declarer=%d contributor=%d calls=%d", declarerSetups.Load(), contributorSetups.Load(), calls.Load())
	}
}

func TestConvenientAndExplicitUsesKeepTheSameAdmissionAndOutcome(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, frame.HostConfig{})
	point, _ := frame.NewPoint[string, func() error]("conformance.operations", "1")
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	unknown := errors.New("original mutation response was lost")
	var outcome error
	var calls, closed atomic.Int64
	plugin := installNative(t, host, "operation", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "run", Contract: point.Ref()}}},
		Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{frame.Contribute("run", point, "operation", func() error { calls.Add(1); return outcome })}, nil
		},
	})
	if _, err := host.Activate(ctx, source("operation", plugin, "main", "run", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := frame.Candidates(snapshot, point)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("operation candidates: %v, %v", candidates, err)
	}
	selection, err := frame.Select(snapshot, point, "consumer.operation/1", candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	plan, err := frame.Capture(snapshot, selection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	_, scope := ownerAndScope(t, host)
	var bindingIdentity integrity.Digest
	var lastUse *frame.Use
	call := func(use *frame.Use) error {
		lastUse = use
		identity, err := use.Binding().Identity()
		if err != nil {
			return err
		}
		if !bindingIdentity.IsZero() && bindingIdentity != identity {
			t.Fatal("convenience path changed the original binding")
		}
		bindingIdentity = identity
		if _, err := use.Lifetime().Cleanup("returned resource", func() error { closed.Add(1); return nil }); err != nil {
			return err
		}
		return frame.Borrow(ctx, use, selection, func(_ context.Context, values []func() error) error { return values[0]() })
	}
	paths := []struct {
		name string
		run  func() error
	}{
		{"WithPlan", func() error { return frame.WithPlan(ctx, host, plan, scope, call) }},
		{"Acquire and Borrow", func() error {
			owner := frame.NewLifetime(context.Background())
			use, err := host.Acquire(ctx, plan, frame.UseConfig{Owner: owner, Scope: scope})
			if err == nil {
				err = call(use)
			}
			_, closeErr := owner.Close(ctx)
			return errors.Join(err, closeErr)
		}},
	}
	for _, failure := range []error{nil, unknown} {
		outcome = failure
		for _, path := range paths {
			beforeCalls, beforeClosed := calls.Load(), closed.Load()
			err := path.run()
			if failure == nil && err != nil || failure != nil && !errors.Is(err, failure) {
				t.Fatalf("%s changed the original outcome: %v", path.name, err)
			}
			if calls.Load() != beforeCalls+1 || closed.Load() != beforeClosed+1 {
				t.Fatalf("%s retried business work or returned before resource exit", path.name)
			}
			if err := frame.Borrow(ctx, lastUse, selection, func(context.Context, []func() error) error {
				t.Fatal("completed path leaked an unowned callable")
				return nil
			}); !errors.Is(err, frame.ErrClosed) {
				t.Fatalf("%s retained an open borrow: %v", path.name, err)
			}
		}
	}
	host.Admission().FenceScope(scope.ID())
	for _, path := range paths {
		if err := path.run(); !errors.Is(err, frame.ErrFenced) {
			t.Fatalf("%s bypassed the original authorization: %v", path.name, err)
		}
	}
	scope, err = host.Admission().OpenScope(ctx, scope.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Withdraw(ctx, candidates[0].ID()); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if err := path.run(); !errors.Is(err, frame.ErrStaleBinding) {
			t.Fatalf("%s bypassed the original capture: %v", path.name, err)
		}
	}
	if calls.Load() != 4 || closed.Load() != 4 {
		t.Fatalf("denied path executed or repeated cleanup: calls=%d closed=%d", calls.Load(), closed.Load())
	}
}

func TestBorrowedHandlerCanRequestItsOwnDisableOrReplacementWithoutWaitingOnItself(t *testing.T) {
	for _, action := range []string{"disable", "replace"} {
		t.Run(action, func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, frame.HostConfig{})
			contract, _ := frame.NewContract[func(context.Context) error]("conformance.self-change", "1")
			if err := host.Register(contract); err != nil {
				t.Fatal(err)
			}
			var plugin *frame.Plugin
			var calls, closes atomic.Int64
			change := func(ctx context.Context) error {
				if action == "disable" {
					return host.SetEnabled(ctx, "self", false)
				}
				return host.Replace(ctx, "self", plugin, nil)
			}
			plugin = installNative(t, host, "self", frame.Unit{
				Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "change", Contract: contract.Ref()}}},
				Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
					if _, err := setup.Cleanup("original provider", func() error { closes.Add(1); return nil }); err != nil {
						return nil, err
					}
					return []frame.ExportValue{frame.Provide("change", contract, func(ctx context.Context) error { calls.Add(1); return change(ctx) })}, nil
				},
			})
			root := source("self", plugin, "main", "change", contract.Ref())
			plan, err := host.Plan(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			owner, scope := ownerAndScope(t, host)
			use, err := host.Acquire(ctx, plan, frame.UseConfig{Owner: owner, Scope: scope})
			if err != nil {
				t.Fatal(err)
			}
			err = frame.BorrowExport(ctx, use, root, func(callbackCtx context.Context, request func(context.Context) error) error {
				err := request(callbackCtx)
				if closes.Load() != 0 {
					t.Fatal("self change closed the provider before its original borrow returned")
				}
				return err
			})
			if action == "disable" && err != nil || action == "replace" && !errors.Is(err, frame.ErrInUse) {
				t.Fatalf("self change waited for its own Use instead of accepting disable or refusing replacement: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatal("self change replayed the original handler")
			}
			if _, err := use.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if err := change(ctx); err != nil {
				t.Fatalf("same change did not succeed after the original Use exited: %v", err)
			}
			if err := host.WaitRetired(ctx, "self"); err != nil {
				t.Fatal(err)
			}
			if closes.Load() != 1 {
				t.Fatalf("original provider exit count=%d", closes.Load())
			}
		})
	}
}
