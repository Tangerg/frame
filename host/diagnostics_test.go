package host_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

type instanceExitDiagnostic struct {
	lifetime frame.Lifetime
	once     sync.Once
	forward  func()
}

func (e *instanceExitDiagnostic) Error() string {
	if outcome, closing := e.lifetime.CloseOutcome(); closing && outcome.Released {
		e.once.Do(e.forward)
	}
	return "original instance cleanup diagnostic"
}

func TestConfirmedInstanceExitAllowsSuccessorBeforeDiagnosticForwarding(t *testing.T) {
	for _, lateProof := range []bool{false, true} {
		name := "cleanup proves release"
		if lateProof {
			name = "late release proof"
		}
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			host, err := framehost.New(ctx, framehost.Config{})
			if err != nil {
				t.Fatal(err)
			}
			var resource frame.ResourceHandle
			t.Cleanup(func() {
				if resource.Valid() {
					if err := resource.ConfirmReleased(nil); err != nil {
						t.Error(err)
					}
				}
				if outcome, err := host.Close(ctx); !outcome.Released || err != nil && !errors.Is(err, frame.ErrCloseFailed) {
					t.Errorf("host close: %+v, %v", outcome, err)
				}
			})
			var prepares atomic.Int32
			var originalID, successorID string
			diagnostic := &instanceExitDiagnostic{}
			plugin := installNative(t, host, "successor", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
				Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
					if prepares.Add(1) != 1 {
						successorID = setup.InstanceID()
						return nil, nil
					}
					originalID, diagnostic.lifetime = setup.InstanceID(), setup.Lifetime()
					var err error
					resource, err = setup.Own(frame.Resource{Name: "original execution", Close: func(context.Context) (frame.ReleaseState, error) {
						if lateProof {
							return frame.ResourceUnconfirmed, diagnostic
						}
						return frame.ResourceReleased, diagnostic
					}})
					return nil, err
				},
			})
			root := framehost.UnitRef{InstallationID: "successor", Release: plugin.Revision(), UnitID: "main"}
			var successorErr error
			diagnostic.forward = func() { _, successorErr = host.ActivateUnit(ctx, "", root) }
			if _, err := host.ActivateUnit(ctx, "", root); err != nil {
				t.Fatal(err)
			}
			if err := host.RetireInstance(ctx, originalID); err != nil {
				t.Fatal(err)
			}
			exit := diagnostic.lifetime.BeginClose(frame.ErrClosed)
			outcome, err := exit.Wait(ctx)
			if lateProof {
				if outcome.Released || !errors.Is(err, frame.ErrUnconfirmedRelease) {
					t.Fatalf("unconfirmed cleanup became exit proof: %+v, %v", outcome, err)
				}
				if _, err := host.ActivateUnit(ctx, "", root); !errors.Is(err, framehost.ErrInUse) || prepares.Load() != 1 {
					t.Fatalf("successor overlapped unconfirmed resources: prepares=%d error=%v", prepares.Load(), err)
				}
				if err := resource.ConfirmReleased(nil); err != nil {
					t.Fatal(err)
				}
			} else if !outcome.Released || !errors.Is(err, frame.ErrCloseFailed) {
				t.Fatalf("original exit lost its conclusion: %+v, %v", outcome, err)
			}
			if err := host.WaitInstanceRetired(ctx, originalID); err != nil {
				t.Fatal(err)
			}
			if successorErr != nil || prepares.Load() != 2 || successorID == "" || successorID == originalID {
				t.Fatalf("confirmed exit still blocked its successor: prepares=%d successor=%q error=%v", prepares.Load(), successorID, successorErr)
			}
		})
	}
}

func TestInstanceObservationProjectsOriginalCloseEvidence(t *testing.T) {
	ctx := testContext(t)
	host, err := framehost.New(context.Background(), framehost.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var handle frame.ResourceHandle
	t.Cleanup(func() {
		if handle.Valid() {
			if err := handle.ConfirmReleased(nil); err != nil {
				t.Error(err)
			}
		}
		outcome, err := host.Close(testContext(t))
		if !outcome.Released {
			t.Errorf("host did not release after original proof: %+v, %v", outcome, err)
		}
	})
	service, err := framehost.NewContract[string]("diagnostics.output", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	cleanupFailure := errors.New("original connection still lacks termination evidence")
	var setup framehost.Setup
	var cleanupCalls atomic.Int64
	plugin := installNative(t, host, "diagnostics", framehost.Unit{
		Declaration: framehost.UnitDeclaration{
			ID:      "main",
			Profile: framehost.NativeGo,
			Exports: []framehost.ExportDeclaration{{Name: "output", Contract: service.Ref()}},
		},
		Setup: func(_ context.Context, current framehost.Setup) ([]framehost.ExportValue, error) {
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
			return []framehost.ExportValue{framehost.Provide("output", service, "value")}, nil
		},
	})
	if _, err := host.Activate(ctx, source("diagnostics", plugin, "main", "output", service.Ref())); err != nil {
		t.Fatal(err)
	}
	ready := host.Observations()
	if len(ready) != 1 || ready[0].State != framehost.Ready || ready[0].Close != nil {
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
	if len(observations) != 1 || observations[0].State != framehost.CloseFailed {
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
	values []framehost.Dependency
	reads  int
}

func (o *diagnosticUseOwner) ID() string { return o.id }

func (o *diagnosticUseOwner) Held(context.Context, framehost.InstallationID) ([]framehost.Dependency, error) {
	o.reads++
	return o.values, nil
}

func TestRetainedDependenciesIdentifyTheirOriginalOwners(t *testing.T) {
	admission := framehost.NewAdmission()
	dependency := framehost.Dependency{InstallationID: "original", Release: value.Hash([]byte("release"))}
	documents := &diagnosticUseOwner{id: "consumer.documents", values: []framehost.Dependency{dependency}}
	tasks := &diagnosticUseOwner{id: "consumer.tasks", values: []framehost.Dependency{dependency, dependency}}
	for _, owner := range []*diagnosticUseOwner{tasks, documents} {
		if err := admission.RegisterUseOwner(owner); err != nil {
			t.Fatal(err)
		}
	}
	retained, err := admission.Retained(t.Context(), "original")
	if err != nil {
		t.Fatal(err)
	}
	expected := []framehost.RetainedDependency{
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
	if err != nil || !slices.Equal(held, []framehost.Dependency{dependency}) {
		t.Fatalf("destructive commands observed a different retention source: %+v, %v", held, err)
	}
	if documents.reads != 3 || tasks.reads != 3 {
		t.Fatalf("retention reads were cached or duplicated: documents=%d tasks=%d", documents.reads, tasks.reads)
	}
}

func TestScopeObservationDoesNotReopenAFencedGenerationDuringRecovery(t *testing.T) {
	ctx := testContext(t)
	admission := framehost.NewAdmission()
	const id framehost.ScopeID = "original-scope"
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
	if err := admission.ValidateScope(grant); !errors.Is(err, framehost.ErrFenced) {
		t.Fatalf("fenced grant remained usable: %v", err)
	}
	close(release)
	released = true
	if err := <-maintenance; err != nil {
		t.Fatal(err)
	}
	if _, err := admission.OpenScope(ctx, id); !errors.Is(err, framehost.ErrRecoveryRequired) {
		t.Fatalf("scope reopened before recovery completed: %v", err)
	}
	if err := admission.Recover(ctx, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	recovered, _ := admission.Scope(id)
	if recovered != observed || !errors.Is(admission.ValidateScope(grant), framehost.ErrFenced) {
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
	if err := admission.ValidateScope(grant); !errors.Is(err, framehost.ErrFenced) {
		t.Fatalf("new scope generation revived an old grant: %v", err)
	}
}

func TestInstallationRestorationPreservesScopeRevocation(t *testing.T) {
	for _, change := range []string{"fence", "disable", "remove"} {
		t.Run(change, func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, framehost.Config{})
			var grant framehost.ScopeGrant
			plugin := installNative(t, host, "original", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
				Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
					grant = setup.ScopeGrant()
					return nil, nil
				},
			})
			plan, err := host.PlanUnit(ctx, "", framehost.UnitRef{InstallationID: "original", Release: plugin.Revision(), UnitID: "main"}, nil)
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
			records := []framehost.InstallationRecord{record}
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
			if err := host.Restore(ctx, records, []*framehost.Plugin{plugin}); err != nil {
				t.Fatal(err)
			}
			if err := host.Admission().ValidateScope(grant); !errors.Is(err, framehost.ErrFenced) {
				t.Fatalf("restoration changed original revocation: %v", err)
			}
			if _, err := host.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if err := host.Restore(ctx, records, []*framehost.Plugin{plugin}); !errors.Is(err, frame.ErrClosed) {
				t.Fatalf("closed host accepted restoration: %v", err)
			}
		})
	}
}
