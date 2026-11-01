package frame_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
	"github.com/Tangerg/frame/jsonvalue"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testHost(t *testing.T, config frame.HostConfig) *frame.Host {
	t.Helper()
	host, err := frame.NewHost(context.Background(), config)
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

func installNative(t *testing.T, host *frame.Host, id string, units ...frame.Unit) *frame.Plugin {
	t.Helper()
	plugin, err := frame.Define(frame.Definition{Revision: integrity.Hash([]byte(id)), Units: units})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(testContext(t), frame.InstallationConfig{ID: frame.InstallationID(id), Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return plugin
}

func source(id string, plugin *frame.Plugin, unit, name string, contract frame.ContractRef) frame.ExportRef {
	return frame.ExportRef{InstallationID: frame.InstallationID(id), Release: plugin.Revision(), UnitID: frame.UnitID(unit), Export: name, Contract: contract}
}

func ownerAndScope(t *testing.T, host *frame.Host) (*frame.Lifetime, frame.ScopeGrant) {
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
	host := testHost(t, frame.HostConfig{})
	service, _ := frame.NewContract[string]("example.text", "1")
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	makeProvider := func(id, text string) (*frame.Plugin, frame.ExportRef) {
		plugin := installNative(t, host, id, frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "text", Contract: service.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{frame.Provide("text", service, text)}, nil
		}})
		return plugin, source(id, plugin, "main", "text", service.Ref())
	}
	_, left := makeProvider("left", "L")
	_, right := makeProvider("right", "R")
	optional, _ := frame.NewContract[string]("example.optional", "1")
	if err := host.Register(optional); err != nil {
		t.Fatal(err)
	}
	var setups atomic.Int64
	consumer := installNative(t, host, "consumer", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "text", Contract: service.Ref()}}, Imports: []frame.ImportDeclaration{{Slot: "left", Contract: service.Ref(), Providers: []frame.ExportRef{left}}, {Slot: "right", Contract: service.Ref(), Providers: []frame.ExportRef{right}}, {Slot: "optional", Contract: optional.Ref(), Optional: true}}}, Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
		setups.Add(1)
		leftValue, err := frame.Import(setup, "left", service)
		if err != nil {
			return nil, err
		}
		rightValue, err := frame.Import(setup, "right", service)
		if err != nil {
			return nil, err
		}
		_, found, err := frame.OptionalImport(setup, "optional", optional)
		if err != nil || found {
			return nil, errors.New("optional binding changed")
		}
		if _, found, err := frame.OptionalImport(setup, "optional", frame.Contract[string]{}); !errors.Is(err, frame.ErrInvalid) || found {
			return nil, errors.New("optional absence admitted an undeclared contract")
		}
		return []frame.ExportValue{frame.Provide("text", service, leftValue+rightValue)}, nil
	}})
	root := source("consumer", consumer, "main", "text", service.Ref())
	binding, err := host.Activate(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	installNative(t, host, "late", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "optional", Contract: optional.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
		return []frame.ExportValue{frame.Provide("optional", optional, "late")}, nil
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
	use, err := acquireExports(host, ctx, binding, frame.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := frame.BorrowExport(ctx, use, root, func(_ context.Context, value string) error {
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
	contract, err := frame.NewContract[string]("example.optional", "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := frame.OptionalImport(nil, "optional", contract); !errors.Is(err, frame.ErrInvalid) || found {
		t.Fatalf("missing setup was accepted: found=%v, %v", found, err)
	}
}

func TestPointReadSetMetadataAndWithdrawal(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, frame.HostConfig{})
	point, _ := frame.NewPoint[map[string]string, func() string]("example.formatters", "1")
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{"label": "first"}
	var setup *frame.Setup
	plugin := installNative(t, host, "format", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "one", Contract: point.Ref()}}}, Setup: func(_ context.Context, s *frame.Setup) ([]frame.ExportValue, error) {
		setup = s
		return []frame.ExportValue{frame.Contribute("one", point, metadata, func() string { return "one" })}, nil
	}})
	if _, err := host.Activate(ctx, source("format", plugin, "main", "one", point.Ref())); err != nil {
		t.Fatal(err)
	}
	metadata["label"] = "mutated"
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := frame.Candidates(snapshot, point)
	if err != nil {
		t.Fatal(err)
	}
	if candidates[0].Metadata()["label"] != "first" {
		t.Fatal("metadata aliased setup")
	}
	copy := candidates[0].Metadata()
	copy["label"] = "consumer mutation"
	if candidates[0].Metadata()["label"] != "first" {
		t.Fatal("candidate exposes mutable metadata")
	}
	selection, err := frame.Select(snapshot, point, "first@1", candidates[0])
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
	if err := host.Withdraw(ctx, candidates[0].ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Acquire(ctx, plan, frame.UseConfig{Owner: owner, Scope: scope}); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("old read set accepted: %v", err)
	}
	if err := frame.Borrow(ctx, use, selection, func(_ context.Context, values []func() string) error {
		if values[0]() != "one" {
			t.Fatal("old use value changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dynamic, err := host.Publish(ctx, setup.InstanceID(), "publish-2", frame.Contribute("dynamic", point, map[string]string{"label": "two"}, func() string { return "two" }))
	if err != nil {
		t.Fatal(err)
	}
	if dynamic.Source.Export != "dynamic" {
		t.Fatal("wrong dynamic identity")
	}
	next, _ := host.Snapshot(ctx, point.Ref())
	dynamicCandidates, _ := frame.Candidates(next, point)
	nextSelection, _ := frame.Select(next, point, "all@1", dynamicCandidates...)
	nextPlan, err := frame.Capture(next, nextSelection.Ref())
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
	host := testHost(t, frame.HostConfig{})
	contract, _ := frame.NewContract[string]("example.late", "1")
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	resume := make(chan struct{})
	released := make(chan struct{})
	plugin := installNative(t, host, "late", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}}, Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
		if _, err := setup.Cleanup("late resource", func() error { close(released); return nil }); err != nil {
			return nil, err
		}
		close(started)
		<-resume
		return []frame.ExportValue{frame.Provide("value", contract, "too late")}, nil
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
	host := testHost(t, frame.HostConfig{})
	point, _ := frame.NewPoint[string, string]("example.children", "1")
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "child", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "value", Contract: point.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
		return []frame.ExportValue{frame.Contribute("value", point, "metadata", "value")}, nil
	}})
	if _, err := host.Activate(ctx, source("child", plugin, "main", "value", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := host.Snapshot(ctx, point.Ref())
	candidates, _ := frame.Candidates(snapshot, point)
	selection, _ := frame.Select(snapshot, point, "first@1", candidates[0])
	plan, _ := frame.Capture(snapshot, selection.Ref())
	owner, scope := ownerAndScope(t, host)
	for range 100 {
		parent, err := host.Acquire(ctx, plan, frame.UseConfig{Owner: owner, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		var child *frame.Use
		var acquireErr error
		var group sync.WaitGroup
		group.Add(2)
		go func() { defer group.Done(); child, acquireErr = host.AcquireChild(ctx, parent, plan) }()
		go func() { defer group.Done(); _, _ = parent.Close(ctx) }()
		group.Wait()
		if acquireErr != nil && !errors.Is(acquireErr, frame.ErrClosed) {
			t.Fatal(acquireErr)
		}
		if child != nil {
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
	host := testHost(t, frame.HostConfig{})
	descriptor := []byte(`{"protocol":"example","methods":["format"]}`)
	valueRef := frame.ContractRef{Name: "example.portable.format", Version: "1", Digest: integrity.Hash(descriptor), Kind: frame.ServiceKind, Mode: frame.PortableMode}
	valueContract, err := frame.BindContract[func() string](valueRef, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	type Metadata struct {
		Priority int `json:"priority"`
	}
	point, err := frame.NewPortablePoint[Metadata]("example.portable.formatters", "1", []byte(`{"type":"object","properties":{"priority":{"type":"integer"}},"required":["priority"],"additionalProperties":false}`), valueContract)
	if err != nil {
		t.Fatal(err)
	}
	if point.Ref().Digest == valueRef.Digest || point.ValueContract() != valueRef {
		t.Fatal("point relabelled callable contract")
	}
	if err := host.Register(valueContract, point); err != nil {
		t.Fatal(err)
	}
	metadata, _ := jsonvalue.Parse([]byte(`{"priority":2}`))
	plugin := installNative(t, host, "portable", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "format", Contract: point.Ref()}}}, Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
		actual, err := setup.ValueContract(point.Ref())
		if err != nil || actual != valueRef {
			return nil, errors.New("wrong value contract")
		}
		return []frame.ExportValue{{Name: "format", Contract: point.Ref(), Metadata: metadata, Value: func() string { return "ok" }, Durable: true}}, nil
	}})
	if _, err := host.Activate(ctx, source("portable", plugin, "main", "format", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := host.Snapshot(ctx, point.Ref())
	candidates, err := frame.Candidates(snapshot, point)
	if err != nil {
		t.Fatal(err)
	}
	if candidates[0].Metadata().Priority != 2 {
		t.Fatal("portable metadata not decoded through original schema")
	}
	if _, err := host.DecodeMetadata(point.Ref(), []byte(`{"priority":"2"}`)); err == nil {
		t.Fatal("metadata schema ignored")
	}
}

func acquireExports(host *frame.Host, ctx context.Context, binding frame.BindingSnapshot, config frame.UseConfig) (*frame.Use, error) {
	plan, err := host.PlanWith(ctx, config.Realm, nil, binding.Roots...)
	if err != nil {
		return nil, err
	}
	return host.Acquire(ctx, plan, config)
}
