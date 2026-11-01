package buildcheck_test

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/buildcheck"
	"github.com/Tangerg/frame/value"
)

func TestAroundContinuationKeepsItsOriginalRemainderAfterPointChanges(t *testing.T) {
	e, ctx := engine(t, "input")
	entered, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	var first atomic.Bool
	install(t, ctx, e, "blocking-wrapper", frame.Contribute("outer", e.Contracts.Around, buildcheck.AroundMetadata{Name: "outer", Order: -100}, buildcheck.Around(func(ctx context.Context, source buildcheck.SourceRevision, next buildcheck.Next) (buildcheck.Result, error) {
		if first.CompareAndSwap(false, true) {
			close(entered)
			select {
			case <-resume:
			case <-ctx.Done():
				return buildcheck.Result{}, ctx.Err()
			}
		}
		return next(ctx, source)
	})))
	marked := func(mark string) buildcheck.Around {
		return func(ctx context.Context, source buildcheck.SourceRevision, next buildcheck.Next) (buildcheck.Result, error) {
			result, err := next(ctx, source)
			result.Trace = append(result.Trace, mark)
			return result, err
		}
	}
	var instanceID string
	plugin, err := frame.Define(frame.Definition{Revision: value.Hash([]byte("mutable-around")), Units: []frame.Unit{{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "tail", Contract: e.Contracts.Around.Ref()}}},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			instanceID = setup.InstanceID()
			return []frame.ExportValue{frame.Contribute("tail", e.Contracts.Around, buildcheck.AroundMetadata{Name: "tail", Order: 20}, marked("original remainder"))}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Host.Install(ctx, frame.InstallationConfig{ID: "mutable-around", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	root := frame.ExportRef{InstallationID: "mutable-around", Release: plugin.Revision(), UnitID: "main", Export: "tail", Contract: e.Contracts.Around.Ref()}
	if _, err := e.Host.Activate(ctx, root); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result buildcheck.Result
		err    error
	}
	finished := make(chan outcome, 1)
	go func() { result, err := e.Run(ctx); finished <- outcome{result, err} }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("around did not reach its original admitted call")
	}
	snapshot, err := e.Host.Snapshot(ctx, e.Contracts.Around.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := frame.Candidates(snapshot, e.Contracts.Around)
	if err != nil {
		t.Fatal(err)
	}
	var originalID string
	for _, candidate := range candidates {
		if candidate.Source() == root {
			originalID = candidate.ID()
		}
	}
	if originalID == "" {
		t.Fatal("original remainder was not published")
	}
	if err := e.Host.Withdraw(ctx, originalID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Host.Publish(ctx, instanceID, "replace-tail", frame.Contribute("tail", e.Contracts.Around, buildcheck.AroundMetadata{Name: "tail", Order: 20}, marked("new remainder"))); err != nil {
		t.Fatal(err)
	}
	release()
	firstResult := <-finished
	if firstResult.err != nil || !slices.Contains(firstResult.result.Trace, "original remainder") || slices.Contains(firstResult.result.Trace, "new remainder") {
		t.Fatalf("next was redirected by a later Point publication: %+v, %v", firstResult.result, firstResult.err)
	}
	secondResult, err := e.Run(ctx)
	if err != nil || !slices.Contains(secondResult.Trace, "new remainder") || slices.Contains(secondResult.Trace, "original remainder") {
		t.Fatalf("fresh invocation did not capture the replacement: %+v, %v", secondResult, err)
	}
}
