package frame_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestInstanceObservationProjectsOriginalCloseEvidence(t *testing.T) {
	ctx := testContext(t)
	host, err := frame.NewHost(context.Background(), frame.HostConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var handle *frame.ResourceHandle
	t.Cleanup(func() {
		if handle != nil {
			if err := handle.ConfirmReleased(nil); err != nil {
				t.Error(err)
			}
		}
		outcome, err := host.Close(testContext(t))
		if !outcome.Released {
			t.Errorf("host did not release after original proof: %+v, %v", outcome, err)
		}
	})
	service, err := frame.NewContract[string]("diagnostics.output", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	cleanupFailure := errors.New("original connection still lacks termination evidence")
	var setup *frame.Setup
	var cleanupCalls atomic.Int64
	plugin := installNative(t, host, "diagnostics", frame.Unit{
		Declaration: frame.UnitDeclaration{
			ID:      "main",
			Profile: frame.NativeGo,
			Exports: []frame.ExportDeclaration{{Name: "output", Contract: service.Ref()}},
		},
		Setup: func(_ context.Context, current *frame.Setup) ([]frame.ExportValue, error) {
			setup = current
			var err error
			handle, err = current.Own(frame.Resource{
				Name: "original connection",
				Close: func(context.Context) (frame.ReleaseState, error) {
					cleanupCalls.Add(1)
					return frame.ResourceUnconfirmed, cleanupFailure
				},
			})
			if err != nil {
				return nil, err
			}
			return []frame.ExportValue{frame.Provide("output", service, "value")}, nil
		},
	})
	if _, err := host.Activate(ctx, source("diagnostics", plugin, "main", "output", service.Ref())); err != nil {
		t.Fatal(err)
	}
	ready := host.Observations()
	if len(ready) != 1 || ready[0].State != frame.Ready || ready[0].Close != nil {
		t.Fatalf("ready observation invented an exit: %+v", ready)
	}
	if err := host.RetireInstance(ctx, setup.InstanceID()); err != nil {
		t.Fatal(err)
	}
	exit := setup.Lifetime().BeginClose(frame.ErrClosed)
	original, err := exit.Wait(ctx)
	if !errors.Is(err, frame.ErrUnconfirmedRelease) || original.Released {
		t.Fatalf("cleanup failure became release proof: %+v, %v", original, err)
	}
	observations := host.Observations()
	if len(observations) != 1 || observations[0].State != frame.CloseFailed {
		t.Fatalf("unconfirmed instance was hidden: %+v", observations)
	}
	observed := observations[0].Close
	if observed == nil || observed.ID != original.ID || observed.Released ||
		!slices.Equal(observed.Pending, []string{"original connection"}) ||
		!slices.Equal(observed.Failures, []frame.CloseFailure{{Resource: "original connection", Detail: cleanupFailure.Error()}}) {
		t.Fatalf("observation lost original exit evidence: %+v", observed)
	}
	observed.ID = 0
	observed.Pending[0] = "changed projection"
	observed.Failures[0].Detail = "changed projection"
	fresh := host.Observations()[0].Close
	if fresh.ID != original.ID || !slices.Equal(fresh.Pending, original.Pending) || !slices.Equal(fresh.Failures, original.Failures) {
		t.Fatalf("mutable observation changed the original exit: %+v", fresh)
	}
	if err := handle.ConfirmReleased(nil); err != nil {
		t.Fatal(err)
	}
	if err := host.WaitInstanceRetired(ctx, setup.InstanceID()); err != nil {
		t.Fatal(err)
	}
	final, err := exit.WaitReleased(ctx)
	if !final.Released || final.ID != original.ID || err == nil || !slices.Equal(final.Failures, original.Failures) {
		t.Fatalf("late proof replaced original diagnostics: %+v, %v", final, err)
	}
	if cleanupCalls.Load() != 1 || len(host.Observations()) != 0 {
		t.Fatalf("late proof replayed cleanup or retained a terminal instance: calls=%d observations=%+v", cleanupCalls.Load(), host.Observations())
	}
}

type diagnosticUseOwner struct {
	id     string
	values []frame.Dependency
	reads  int
}

func (o *diagnosticUseOwner) ID() string { return o.id }

func (o *diagnosticUseOwner) Held(context.Context, frame.InstallationID) ([]frame.Dependency, error) {
	o.reads++
	return o.values, nil
}

func TestRetainedDependenciesIdentifyTheirOriginalOwners(t *testing.T) {
	admission := frame.NewAdmission()
	dependency := frame.Dependency{InstallationID: "original", Release: value.Hash([]byte("release"))}
	documents := &diagnosticUseOwner{id: "consumer.documents", values: []frame.Dependency{dependency}}
	tasks := &diagnosticUseOwner{id: "consumer.tasks", values: []frame.Dependency{dependency, dependency}}
	for _, owner := range []*diagnosticUseOwner{tasks, documents} {
		if err := admission.RegisterUseOwner(owner); err != nil {
			t.Fatal(err)
		}
	}
	retained, err := admission.Retained(t.Context(), "original")
	if err != nil {
		t.Fatal(err)
	}
	expected := []frame.RetainedDependency{
		{OwnerID: documents.ID(), Dependency: dependency},
		{OwnerID: tasks.ID(), Dependency: dependency},
	}
	if !slices.Equal(retained, expected) {
		t.Fatalf("retention lost original owner identity: %+v", retained)
	}
	retained[0].Dependency.Release = value.Hash([]byte("changed projection"))
	retained[0].OwnerID = "changed projection"
	if documents.values[0] != dependency {
		t.Fatal("diagnostic projection mutated its business owner")
	}
	admission.RequireRecovery(errors.New("another owner needs recovery"))
	documents.values = nil
	fresh, err := admission.Retained(t.Context(), "original")
	if err != nil || !slices.Equal(fresh, expected[1:]) {
		t.Fatalf("diagnostics did not read current original records behind recovery: %+v, %v", fresh, err)
	}
	held, err := admission.Held(t.Context(), "original")
	if err != nil || !slices.Equal(held, []frame.Dependency{dependency}) {
		t.Fatalf("destructive commands observed a different retention source: %+v, %v", held, err)
	}
	if documents.reads != 3 || tasks.reads != 3 {
		t.Fatalf("retention reads were cached or duplicated: documents=%d tasks=%d", documents.reads, tasks.reads)
	}
}

func TestScopeObservationDoesNotReopenAFencedGenerationDuringRecovery(t *testing.T) {
	ctx := testContext(t)
	admission := frame.NewAdmission()
	const id frame.ScopeID = "original-scope"
	if observed, exists := admission.Scope(id); exists {
		t.Fatalf("unknown scope was invented: %+v", observed)
	}
	grant, err := admission.OpenScope(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	original, exists := admission.Scope(id)
	if !exists || original.ID != id || original.Generation != grant.Generation() || original.Fenced {
		t.Fatalf("scope observation does not identify original grant: %+v", original)
	}
	admission.RequireRecovery(errors.New("restore original resource evidence"))
	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	maintenance := make(chan error, 1)
	go func() {
		maintenance <- admission.Maintenance(ctx, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	fenced := make(chan struct{})
	go func() {
		admission.FenceScope(id)
		close(fenced)
	}()
	select {
	case <-fenced:
	case <-ctx.Done():
		t.Fatal("scope fence waited for the occupied admission gate")
	}
	observed, exists := admission.Scope(id)
	if !exists || observed.Generation != original.Generation || !observed.Fenced {
		t.Fatalf("recovery hid the original local fence: %+v", observed)
	}
	if err := admission.ValidateScope(grant); !errors.Is(err, frame.ErrFenced) {
		t.Fatalf("fenced grant remained usable: %v", err)
	}
	close(release)
	released = true
	if err := <-maintenance; err != nil {
		t.Fatal(err)
	}
	if _, err := admission.OpenScope(ctx, id); !errors.Is(err, frame.ErrRecoveryRequired) {
		t.Fatalf("scope reopened before recovery completed: %v", err)
	}
	if err := admission.Recover(ctx, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	recovered, _ := admission.Scope(id)
	if recovered != observed || !errors.Is(admission.ValidateScope(grant), frame.ErrFenced) {
		t.Fatalf("recovery reopened the original scope: %+v", recovered)
	}
	renewed, err := admission.OpenScope(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := admission.Scope(id)
	if current.Fenced || current.Generation != original.Generation+1 || renewed.Generation() != current.Generation {
		t.Fatalf("explicit reopening did not create a new generation: %+v", current)
	}
	if err := admission.ValidateScope(renewed); err != nil {
		t.Fatal(err)
	}
	if err := admission.ValidateScope(grant); !errors.Is(err, frame.ErrFenced) {
		t.Fatalf("new scope generation revived an old grant: %v", err)
	}
}

func TestInstallationRestorationPreservesScopeRevocation(t *testing.T) {
	for _, change := range []string{"fence", "disable", "remove"} {
		t.Run(change, func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, frame.HostConfig{})
			var grant frame.ScopeGrant
			plugin := installNative(t, host, "original", frame.Unit{
				Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
				Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
					grant = setup.ScopeGrant()
					return nil, nil
				},
			})
			plan, err := host.PlanUnit(ctx, "", frame.UnitRef{InstallationID: "original", Release: plugin.Revision(), UnitID: "main"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := host.ActivatePlan(ctx, "", plan); err != nil {
				t.Fatal(err)
			}
			if err := host.Retire(ctx, "original"); err != nil {
				t.Fatal(err)
			}
			if err := host.WaitRetired(ctx, "original"); err != nil {
				t.Fatal(err)
			}
			record, found := host.Installation("original")
			if !found {
				t.Fatal("retirement erased the installation")
			}
			records := []frame.InstallationRecord{record}
			switch change {
			case "fence":
				if err := host.SetEnabled(ctx, "original", false); err != nil {
					t.Fatal(err)
				}
			case "disable":
				records[0].Enabled = false
			case "remove":
				records = nil
			}
			if err := host.Restore(ctx, records, []*frame.Plugin{plugin}); err != nil {
				t.Fatal(err)
			}
			if err := host.Admission().ValidateScope(grant); !errors.Is(err, frame.ErrFenced) {
				t.Fatalf("restoration changed original revocation: %v", err)
			}
			if _, err := host.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if err := host.Restore(ctx, records, []*frame.Plugin{plugin}); !errors.Is(err, frame.ErrClosed) {
				t.Fatalf("closed host accepted restoration: %v", err)
			}
		})
	}
}
