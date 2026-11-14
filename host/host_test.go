package host_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testHost(t *testing.T, config framehost.Config) framehost.Host {
	t.Helper()
	host, err := framehost.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		outcome, err := host.Close(ctx)
		if err != nil || !outcome.Released {
			t.Errorf("host close: %+v, %v", outcome, err)
		}
	})
	return host
}

func installNative(t *testing.T, host framehost.Host, id string, units ...framehost.Unit) *framehost.Plugin {
	t.Helper()
	plugin, err := framehost.Define(framehost.Definition{Revision: framevalue.Hash([]byte(id)), Units: units})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(testContext(t), framehost.InstallationConfig{ID: framehost.InstallationID(id), Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return plugin
}

func source(id string, plugin *framehost.Plugin, unit, name string, contract framehost.ContractRef) framehost.ExportRef {
	return framehost.ExportRef{InstallationID: framehost.InstallationID(id), Release: plugin.Revision(), UnitID: framehost.UnitID(unit), Export: name, Contract: contract}
}

func ownerAndScope(t *testing.T, host framehost.Host) (frame.Lifetime, framehost.ScopeGrant) {
	t.Helper()
	owner := frame.NewLifetime(context.Background())
	t.Cleanup(func() {
		_, err := owner.Close(testContext(t))
		if err != nil {
			t.Error(err)
		}
	})
	scope, err := host.Admission().OpenScope(testContext(t), "test-caller")
	if err != nil {
		t.Fatal(err)
	}
	return owner, scope
}

func TestExactSlotsAndFrozenOptionalAbsence(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	service, _ := framehost.NewContract[string]("example.text", "1")
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	makeProvider := func(id, text string) (*framehost.Plugin, framehost.ExportRef) {
		plugin := installNative(t, host, id, framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "text", Contract: service.Ref()}}}, Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Provide("text", service, text)}, nil
		}})
		return plugin, source(id, plugin, "main", "text", service.Ref())
	}
	_, left := makeProvider("left", "L")
	_, right := makeProvider("right", "R")
	optional, _ := framehost.NewContract[string]("example.optional", "1")
	if err := host.Register(optional); err != nil {
		t.Fatal(err)
	}
	var setups atomic.Int64
	consumer := installNative(t, host, "consumer", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "text", Contract: service.Ref()}}, Imports: []framehost.ImportDeclaration{{Slot: "left", Contract: service.Ref()}, {Slot: "right", Contract: service.Ref()}, {Slot: "optional", Contract: optional.Ref(), Optional: true}}, Fixed: []framehost.SlotProviders{{Slot: "left", Providers: []framehost.ExportRef{left}}, {Slot: "right", Providers: []framehost.ExportRef{right}}}}, Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
		setups.Add(1)
		leftValue, err := framehost.Import(setup, "left", service)
		if err != nil {
			return nil, err
		}
		rightValue, err := framehost.Import(setup, "right", service)
		if err != nil {
			return nil, err
		}
		_, found, err := framehost.OptionalImport(setup, "optional", optional)
		if err != nil || found {
			return nil, errors.New("optional binding changed")
		}
		if _, found, err := framehost.OptionalImport(setup, "optional", framehost.Contract[string]{}); !errors.Is(err, frame.ErrInvalid) || found {
			return nil, errors.New("optional absence admitted an undeclared contract")
		}
		return []framehost.ExportValue{framehost.Provide("text", service, leftValue+rightValue)}, nil
	}})
	root := source("consumer", consumer, "main", "text", service.Ref())
	binding, err := host.Activate(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	installNative(t, host, "late", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "optional", Contract: optional.Ref()}}}, Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
		return []framehost.ExportValue{framehost.Provide("optional", optional, "late")}, nil
	}})
	again, err := host.Plan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := binding.Identity()
	againBinding, err := again.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := againBinding.Identity()
	if before != after || setups.Load() != 1 {
		t.Fatalf("active setup was rebound: %s %s %d", before, after, setups.Load())
	}
	owner, scope := ownerAndScope(t, host)
	use, err := acquireExports(host, ctx, binding, framehost.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := framehost.BorrowExport(ctx, use, root, func(_ context.Context, value string) error {
		if value != "LR" {
			t.Fatalf("slots mixed: %s", value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := use.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := host.Replace(ctx, "consumer", consumer, nil); err != nil {
		t.Fatalf("returned Use.Close still retained installation: %v", err)
	}
}

func TestOptionalImportRefusesMissingSetup(t *testing.T) {
	contract, err := framehost.NewContract[string]("example.optional", "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := framehost.OptionalImport(framehost.Setup{}, "optional", contract); !errors.Is(err, frame.ErrInvalid) || found {
		t.Fatalf("missing setup was accepted: found=%v, %v", found, err)
	}
}

func TestPointReadSetMetadataAndWithdrawal(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	point, _ := framehost.NewPoint[map[string]string, func() string]("example.formatters", "1")
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{"label": "first"}
	var setup framehost.Setup
	plugin := installNative(t, host, "format", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "one", Contract: point.Ref()}}}, Setup: func(_ context.Context, s framehost.Setup) ([]framehost.ExportValue, error) {
		setup = s
		return []framehost.ExportValue{framehost.Contribute("one", point, metadata, func() string { return "one" })}, nil
	}})
	if _, err := host.Activate(ctx, source("format", plugin, "main", "one", point.Ref())); err != nil {
		t.Fatal(err)
	}
	metadata["label"] = "mutated"
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := framehost.Candidates(snapshot, point)
	if err != nil {
		t.Fatal(err)
	}
	if view, err := candidates[0].Metadata(); err != nil || view["label"] != "first" {
		t.Fatal("metadata aliased setup")
	}
	copy, err := candidates[0].Metadata()
	if err != nil {
		t.Fatal(err)
	}
	copy["label"] = "consumer mutation"
	if view, err := candidates[0].Metadata(); err != nil || view["label"] != "first" {
		t.Fatal("candidate exposes mutable metadata")
	}
	selection, err := framehost.Select(snapshot, point, "first@1", candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	plan, err := framehost.Capture(snapshot, selection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	owner, scope := ownerAndScope(t, host)
	use, err := host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Withdraw(ctx, candidates[0].ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Scope: scope}); !errors.Is(err, framehost.ErrStaleBinding) {
		t.Fatalf("old read set accepted: %v", err)
	}
	if err := framehost.Borrow(ctx, use, selection, func(_ context.Context, values []func() string) error {
		if values[0]() != "one" {
			t.Fatal("old use value changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dynamic, err := host.Publish(ctx, "publish-2", preparePublication(t, host, setup.InstanceID(), framehost.Contribute("dynamic", point, map[string]string{"label": "two"}, func() string { return "two" })))
	if err != nil {
		t.Fatal(err)
	}
	if dynamic.Source.Export != "dynamic" {
		t.Fatal("wrong dynamic identity")
	}
	next, _ := host.Snapshot(ctx, point.Ref())
	dynamicCandidates, _ := framehost.Candidates(next, point)
	nextSelection, _ := framehost.Select(next, point, "all@1", dynamicCandidates...)
	nextPlan, err := framehost.Capture(next, nextSelection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nextPlan.Snapshot(); err == nil {
		t.Fatal("dynamic contribution gained durable reconstruction")
	}
	if _, err := use.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLateReadyCannotEscapeScopeFence(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	contract, _ := framehost.NewContract[string]("example.late", "1")
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	resume := make(chan struct{})
	released := make(chan struct{})
	plugin := installNative(t, host, "late", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}}, Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
		if _, err := setup.Cleanup("late resource", func() error { close(released); return nil }); err != nil {
			return nil, err
		}
		close(started)
		<-resume
		return []framehost.ExportValue{framehost.Provide("value", contract, "too late")}, nil
	}})
	activated := make(chan error, 1)
	go func() {
		_, err := host.Activate(ctx, source("late", plugin, "main", "value", contract.Ref()))
		activated <- err
	}()
	<-started
	if err := host.SetEnabled(ctx, "late", false); err != nil {
		t.Fatal(err)
	}
	close(resume)
	if err := <-activated; err == nil {
		t.Fatal("late ready published after disable")
	}
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("late actual resource leaked")
	}
	snapshot, _ := host.Snapshot(ctx, contract.Ref())
	values, _ := snapshot.Candidates(contract.Ref())
	if len(values) != 0 {
		t.Fatal("fenced source remained public")
	}
}

func TestParentCloseRacingChildAcquireLeavesNoReference(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	point, _ := framehost.NewPoint[string, string]("example.children", "1")
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "child", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: point.Ref()}}}, Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
		return []framehost.ExportValue{framehost.Contribute("value", point, "metadata", "value")}, nil
	}})
	if _, err := host.Activate(ctx, source("child", plugin, "main", "value", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := host.Snapshot(ctx, point.Ref())
	candidates, _ := framehost.Candidates(snapshot, point)
	selection, _ := framehost.Select(snapshot, point, "first@1", candidates[0])
	plan, _ := framehost.Capture(snapshot, selection.Ref())
	owner, scope := ownerAndScope(t, host)
	for range 100 {
		parent, err := host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		var child framehost.Use
		var acquireErr error
		var group sync.WaitGroup
		group.Add(2)
		go func() { defer group.Done(); child, acquireErr = host.AcquireChild(ctx, parent, plan) }()
		go func() { defer group.Done(); _, _ = parent.Close(ctx) }()
		group.Wait()
		if acquireErr != nil && !errors.Is(acquireErr, frame.ErrClosed) {
			t.Fatal(acquireErr)
		}
		if child.Valid() {
			if outcome, err := child.Close(ctx); err != nil || !outcome.Released {
				t.Fatalf("child close: %+v %v", outcome, err)
			}
		}
		if _, err := parent.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.Replace(ctx, "child", plugin, nil); err != nil {
		t.Fatalf("closed child leaked binding: %v", err)
	}
}

func TestPortablePointHasIndependentMetadataAndValueIdentity(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	descriptor := []byte(`{"protocol":"example","methods":["format"]}`)
	valueRef := framehost.ContractRef{Name: "example.portable.format", Version: "1", Digest: framevalue.Hash(descriptor), Kind: framehost.ServiceKind, Mode: framehost.PortableMode}
	valueContract, err := framehost.BindContract[func() string](valueRef, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	type Metadata struct {
		Priority int `json:"priority"`
	}
	point, err := framehost.NewPortablePoint[Metadata]("example.portable.formatters", "1", []byte(`{"type":"object","properties":{"priority":{"type":"integer"}},"required":["priority"],"additionalProperties":false}`), valueContract)
	if err != nil {
		t.Fatal(err)
	}
	if point.Ref().Digest == valueRef.Digest || point.ValueContract() != valueRef {
		t.Fatal("point relabelled callable contract")
	}
	if err := host.Register(valueContract, point); err != nil {
		t.Fatal(err)
	}
	metadata, _ := framevalue.ParseJSON([]byte(`{"priority":2}`))
	plugin := installNative(t, host, "portable", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "format", Contract: point.Ref()}}}, Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
		actual, err := setup.ValueContract(point.Ref())
		if err != nil || actual != valueRef {
			return nil, errors.New("wrong value contract")
		}
		return []framehost.ExportValue{{Name: "format", Contract: point.Ref(), Metadata: metadata, Value: func() string { return "ok" }}}, nil
	}})
	if _, err := host.Activate(ctx, source("portable", plugin, "main", "format", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := host.Snapshot(ctx, point.Ref())
	candidates, err := framehost.Candidates(snapshot, point)
	if err != nil {
		t.Fatal(err)
	}
	if view, err := candidates[0].Metadata(); err != nil || view.Priority != 2 {
		t.Fatal("portable metadata not decoded through original schema")
	}
	if _, err := host.PrepareMetadata(point.Ref(), []byte(`{"priority":"2"}`)); err == nil {
		t.Fatal("metadata schema ignored")
	}
}

func acquireExports(host framehost.Host, ctx context.Context, binding framehost.BindingSnapshot, config framehost.UseConfig) (framehost.Use, error) {
	plan, err := host.PlanWith(ctx, config.Realm, nil, binding.Roots...)
	if err != nil {
		return framehost.Use{}, err
	}
	return host.Acquire(ctx, plan, config)
}

func TestReleaseDeclarationsAreImmutableAcrossInstallationTransitions(t *testing.T) {
	h := testHost(t, framehost.Config{})
	plugin := installNative(t, h, "original")
	before, _ := h.Installation("original")
	changed, err := framehost.Define(framehost.Definition{Revision: plugin.Revision(), Units: []framehost.Unit{{Declaration: framehost.UnitDeclaration{ID: "new", Profile: framehost.NativeGo}, Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) { return nil, nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, actual := range []error{
		h.Update(t.Context(), before, framehost.InstallationConfig{ID: before.ID, Plugin: changed, Enabled: true}),
		h.Replace(t.Context(), before.ID, changed, nil),
	} {
		if !errors.Is(actual, frame.ErrContractMismatch) {
			t.Fatalf("conflicting release: %v", actual)
		}
	}
	if _, err := h.Install(t.Context(), framehost.InstallationConfig{ID: "second", Plugin: changed}); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("same release declared by another installation: %v", err)
	}
	if err := h.Replace(t.Context(), before.ID, plugin, before.Configuration); err != nil {
		t.Fatalf("equivalent replacement was refused: %v", err)
	}
	if after, _ := h.Installation(before.ID); after.Release != before.Release {
		t.Fatalf("equivalent replacement advanced release: %s", after.Release)
	}
}

type pausedInstallationStore struct {
	entered, resume chan struct{}
	paused          bool
}

func (s *pausedInstallationStore) CommitInstallation(context.Context, framehost.InstallationChange) (framehost.CommitState, error) {
	if s.paused {
		close(s.entered)
		<-s.resume
	}
	return framehost.CommitSucceeded, nil
}
func (*pausedInstallationStore) ConfirmInstallation(context.Context, framehost.InstallationID) error {
	return nil
}
func (*pausedInstallationStore) Installations(context.Context) ([]framehost.InstallationRecord, error) {
	return nil, nil
}

func TestHostCloseJoinsAdmittedInstallationChanges(t *testing.T) {
	for _, action := range []string{"install", "update", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			ctx := testContext(t)
			store := &pausedInstallationStore{entered: make(chan struct{}), resume: make(chan struct{})}
			h := testHost(t, framehost.Config{Authority: installationAuthority(store)})
			plugin, err := framehost.Define(framehost.Definition{Revision: framevalue.Hash([]byte("original"))})
			if err != nil {
				t.Fatal(err)
			}
			config := framehost.InstallationConfig{ID: "change", Plugin: plugin, Enabled: true}
			var before framehost.InstallationRecord
			if action != "install" {
				before, err = h.Install(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
			}
			store.paused = true
			finished := make(chan error, 1)
			go func() {
				var err error
				switch action {
				case "install":
					_, err = h.Install(ctx, config)
				case "update":
					config.Enabled = false
					err = h.Update(ctx, before, config)
				case "uninstall":
					err = h.Uninstall(ctx, config.ID)
				}
				finished <- err
			}()
			select {
			case <-store.entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			outcome, waitErr := h.Close(canceled)
			close(store.resume)
			if err = <-finished; err != nil {
				t.Fatal(err)
			}
			if outcome.Released || !errors.Is(waitErr, context.Canceled) {
				t.Fatalf("in-flight commit was reported released: %+v %v", outcome, waitErr)
			}
			if outcome, err = h.Close(ctx); !outcome.Released || err != nil {
				t.Fatalf("settled close: %+v %v", outcome, err)
			}
		})
	}
}

type waitingMetadata struct{ entered, resume chan struct{} }

func (m waitingMetadata) MarshalJSON() ([]byte, error) {
	close(m.entered)
	<-m.resume
	return []byte(`"metadata"`), nil
}

func TestActivationRegistrationEndsWhenPreparationReturns(t *testing.T) {
	ctx := testContext(t)
	h := testHost(t, framehost.Config{})
	descriptor := []byte(`{"protocol":"example"}`)
	ref := framehost.ContractRef{Name: "stage.value", Version: "1", Kind: framehost.ServiceKind, Mode: framehost.PortableMode, Digest: framevalue.Hash(descriptor)}
	value, err := framehost.BindContract[string](ref, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	point, err := framehost.NewPortablePoint[string]("stage.point", "1", []byte(`{"type":"string"}`), value)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Register(point, value); err != nil {
		t.Fatal(err)
	}
	metadata := waitingMetadata{make(chan struct{}), make(chan struct{})}
	prepared := make(chan framehost.Setup, 1)
	plugin := installNative(t, h, "p", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "u", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "e", Contract: point.Ref()}}}, Setup: func(_ context.Context, s framehost.Setup) ([]framehost.ExportValue, error) {
		prepared <- s
		return []framehost.ExportValue{{Name: "e", Contract: point.Ref(), Metadata: metadata, Value: "v"}}, nil
	}})
	finished := make(chan error, 1)
	go func() { _, err := h.Activate(ctx, source("p", plugin, "u", "e", point.Ref())); finished <- err }()
	select {
	case <-metadata.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	setup := <-prepared
	err = setup.OnActivate(func(context.Context) error { t.Error("late activation ran"); return nil })
	close(metadata.resume)
	if !errors.Is(err, frame.ErrClosed) {
		t.Errorf("late activation registration: %v", err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
