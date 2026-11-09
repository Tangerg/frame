package frame_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
)

func TestScopeFencedDuringApprovalIsRefused(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	protection := &useRetirementProtection{releases: make(chan recordedUseRelease, 1)}
	host := testHost(t, frame.HostConfig{ExecutionProtection: protection})
	plugin := installNative(t, host, "approval-provider", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup:       func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { return nil, nil },
	})
	owner, scope := ownerAndScope(t, host)
	protection.approve = func() error {
		host.Admission().FenceScope(scope.ID())
		return nil
	}
	use, err := host.AcquireUnit(t.Context(), frame.UnitRef{InstallationID: "approval-provider", Release: plugin.Revision(), UnitID: "main"}, frame.UseConfig{Owner: owner, Scope: scope, Realm: "realm"})
	t.Logf("scope valid=%v, acquired use=%t, acquire error=%v", host.Admission().ValidateScope(scope), use != nil, err)
	if use != nil || !errors.Is(err, frame.ErrFenced) {
		t.Errorf("acquisition published authority after its original scope was fenced: use=%t, error=%v", use != nil, err)
	}
}

func TestScopeFencedDuringAcceptedReadDoesNotPrepare(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	entered, resume := make(chan struct{}), make(chan struct{})
	var accepted frame.BindingSnapshot
	var reads, prepared atomic.Int64
	host := testHost(t, frame.HostConfig{ReadAcceptedBinding: func(context.Context, string, frame.ScopeGrant) (frame.BindingSnapshot, error) {
		if reads.Add(1) == 1 {
			close(entered)
			<-resume
		}
		return accepted.Clone(), nil
	}})
	plugin := installNative(t, host, "reader-provider", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			prepared.Add(1)
			return nil, nil
		},
	})
	root := frame.UnitRef{InstallationID: "reader-provider", Release: plugin.Revision(), UnitID: "main"}
	plan, err := host.PlanUnit(t.Context(), "", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err = plan.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	owner, scope := ownerAndScope(t, host)
	done := make(chan error, 1)
	go func() {
		_, err := host.AcquireRecord(t.Context(), "original-business-record", frame.UseConfig{Owner: owner, Scope: scope})
		done <- err
	}()
	<-entered
	host.Admission().FenceScope(scope.ID())
	close(resume)
	err = <-done
	t.Logf("scope was fenced before original read returned; preparation calls=%d, acquire error=%v", prepared.Load(), err)
	if prepared.Load() != 0 || !errors.Is(err, frame.ErrFenced) {
		t.Errorf("fenced accepted read did not refuse preparation: setups=%d, error=%v", prepared.Load(), err)
	}
}
