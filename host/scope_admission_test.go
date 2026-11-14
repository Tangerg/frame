package host_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	framehost "github.com/Tangerg/frame/host"
)

func TestScopeFencedDuringApprovalIsRefused(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	protection := &useRetirementProtection{releases: make(chan recordedUseRelease, 1)}
	host := testHost(t, framehost.Config{Authority: protectionAuthority(protection)})
	plugin := installNative(t, host, "approval-provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup:       func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) { return nil, nil },
	})
	owner, scope := ownerAndScope(t, host)
	protection.approve = func() error {
		host.Admission().FenceScope(scope.ID())
		return nil
	}
	use, err := host.AcquireUnit(t.Context(), framehost.UnitRef{InstallationID: "approval-provider", Release: plugin.Revision(), UnitID: "main"}, framehost.UseConfig{Owner: owner, Scope: scope, Realm: "realm"})
	t.Logf("scope valid=%v, acquired use=%t, acquire error=%v", host.Admission().ValidateScope(scope), use.Valid(), err)
	if use.Valid() || !errors.Is(err, framehost.ErrFenced) {
		t.Errorf("acquisition published authority after its original scope was fenced: use=%t, error=%v", use.Valid(), err)
	}
}

func TestScopeFencedDuringAcceptedReadDoesNotPrepare(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	entered, resume := make(chan struct{}), make(chan struct{})
	var accepted framehost.BindingSnapshot
	var reads, prepared atomic.Int64
	host := testHost(t, framehost.Config{ReadAcceptedBinding: func(context.Context, string, framehost.ScopeGrant) (framehost.BindingSnapshot, error) {
		if reads.Add(1) == 1 {
			close(entered)
			<-resume
		}
		return accepted.Clone(), nil
	}})
	plugin := installNative(t, host, "reader-provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			prepared.Add(1)
			return nil, nil
		},
	})
	root := framehost.UnitRef{InstallationID: "reader-provider", Release: plugin.Revision(), UnitID: "main"}
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
		_, err := host.AcquireRecord(t.Context(), "original-business-record", framehost.UseConfig{Owner: owner, Scope: scope})
		done <- err
	}()
	<-entered
	host.Admission().FenceScope(scope.ID())
	close(resume)
	err = <-done
	t.Logf("scope was fenced before original read returned; preparation calls=%d, acquire error=%v", prepared.Load(), err)
	if prepared.Load() != 0 || !errors.Is(err, framehost.ErrFenced) {
		t.Errorf("fenced accepted read did not refuse preparation: setups=%d, error=%v", prepared.Load(), err)
	}
}
