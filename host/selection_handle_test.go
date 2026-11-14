package host_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

type selectionHandles struct {
	host       framehost.Host
	point      framehost.Point[string, string]
	snapshot   framehost.Snapshot
	selections []framehost.Selection[string, string]
	plans      []framehost.BindingPlan
	sources    []framehost.ExportRef
}

func newSelectionHandles(t *testing.T) selectionHandles {
	t.Helper()
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	point, err := framehost.NewPoint[string, string]("selection.handles", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "providers", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "first", Contract: point.Ref()}, {Name: "second", Contract: point.Ref()}}},
		Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Contribute("first", point, "first", "first"), framehost.Contribute("second", point, "second", "second")}, nil
		},
	})
	if _, err := host.ActivateUnit(ctx, "", framehost.UnitRef{InstallationID: "providers", Release: plugin.Revision(), UnitID: "main"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := framehost.Candidates(snapshot, point)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates: %d, %v", len(candidates), err)
	}
	fixture := selectionHandles{host: host, point: point, snapshot: snapshot}
	for _, candidate := range candidates {
		selected, err := framehost.Select(snapshot, point, "original", candidate)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := framehost.Capture(snapshot, selected.Ref())
		if err != nil {
			t.Fatal(err)
		}
		fixture.selections = append(fixture.selections, selected)
		fixture.plans = append(fixture.plans, plan)
		fixture.sources = append(fixture.sources, candidate.Source())
	}
	return fixture
}

type admissionWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *admissionWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestAcquireKeepsItsOriginalPlanAcrossAdmissionWait(t *testing.T) {
	fixture := newSelectionHandles(t)
	ctx := testContext(t)
	owner, scope := ownerAndScope(t, fixture.host)
	held, release := make(chan struct{}), make(chan struct{})
	var resume sync.Once
	t.Cleanup(func() { resume.Do(func() { close(release) }) })
	gate := make(chan error, 1)
	go func() {
		gate <- fixture.host.Admission().Under(ctx, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-gate:
		t.Fatalf("admission did not enter its original operation: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	wait := &admissionWaitContext{Context: ctx, waiting: make(chan struct{})}
	type acquired struct {
		use framehost.Use
		err error
	}
	completed := make(chan acquired, 1)
	plan := fixture.plans[0]
	go func() {
		use, err := fixture.host.Acquire(wait, plan, framehost.UseConfig{Owner: owner, Scope: scope})
		completed <- acquired{use, err}
	}()
	select {
	case <-wait.waiting:
	case result := <-completed:
		t.Fatalf("acquisition did not wait for admission: %v", result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	plan = fixture.plans[1]
	resume.Do(func() { close(release) })
	if err := <-gate; err != nil {
		t.Fatal(err)
	}
	var result acquired
	select {
	case result = <-completed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	t.Cleanup(func() { _, _ = result.use.Close(context.Background()) })
	if roots := result.use.Binding().Roots; len(roots) != 1 || roots[0] != fixture.sources[0] {
		t.Fatalf("original binding changed: %v", roots)
	}
	if err := framehost.BorrowExport(ctx, result.use, fixture.sources[0], func(context.Context, string) error { return nil }); err != nil {
		t.Errorf("original binding did not retain its selected implementation: %v", err)
	}
	called := false
	err := framehost.BorrowExport(ctx, result.use, fixture.sources[1], func(context.Context, string) error { called = true; return nil })
	if called || !errors.Is(err, frame.ErrContractMismatch) {
		t.Errorf("replacement plan introduced another implementation: called=%t, %v", called, err)
	}
}

func TestSnapshotCopiesRetainCandidateProvenance(t *testing.T) {
	fixture := newSelectionHandles(t)
	copied := fixture.snapshot
	candidates, err := fixture.snapshot.Candidates(fixture.point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := framehost.SelectRefs(copied, fixture.point.Ref(), "copied", candidates[0]); err != nil {
		t.Fatalf("copy lost its original catalog read: %v", err)
	}
}

func TestReplacingSnapshotCannotRewriteExistingSelection(t *testing.T) {
	fixture := newSelectionHandles(t)
	original := fixture.snapshot
	fresh, err := fixture.host.Snapshot(t.Context(), fixture.point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	fixture.snapshot = fresh
	if _, err := framehost.Capture(fixture.snapshot, fixture.selections[0].Ref()); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("another catalog read rewrote selection provenance: %v", err)
	}
	if _, err := framehost.Capture(original, fixture.selections[0].Ref()); err != nil {
		t.Fatalf("replacing a handle invalidated the original selection: %v", err)
	}
}

func TestUnissuedSelectionHandlesCannotAdmitWork(t *testing.T) {
	fixture := newSelectionHandles(t)
	ctx := testContext(t)
	var snapshot framehost.Snapshot
	if _, err := snapshot.Candidates(fixture.point.Ref()); !errors.Is(err, frame.ErrInvalid) {
		t.Errorf("unissued snapshot returned candidates: %v", err)
	}
	if _, err := snapshot.Revision(fixture.point.Ref()); !errors.Is(err, frame.ErrInvalid) {
		t.Errorf("unissued snapshot returned a catalog revision: %v", err)
	}
	if _, err := framehost.Capture(snapshot); !errors.Is(err, frame.ErrInvalid) {
		t.Errorf("unissued snapshot produced a plan: %v", err)
	}
	var plan framehost.BindingPlan
	if _, err := plan.Snapshot(); !errors.Is(err, frame.ErrInvalid) {
		t.Errorf("unissued plan produced a binding: %v", err)
	}
	owner, scope := ownerAndScope(t, fixture.host)
	if _, err := fixture.host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Scope: scope}); !errors.Is(err, frame.ErrInvalid) {
		t.Errorf("unissued plan admitted a use: %v", err)
	}
	if _, err := fixture.host.ActivatePlan(ctx, "", plan); !errors.Is(err, frame.ErrInvalid) {
		t.Errorf("unissued plan admitted activation: %v", err)
	}
}
