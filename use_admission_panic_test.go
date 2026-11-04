package frame_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
)

func TestPanickingUseApprovalReleasesLocalHoldsAndFencesAdmission(t *testing.T) {
	panicCause := errors.New("approval interrupted")
	protection := &useRetirementProtection{
		approve:  func() error { panic(panicCause) },
		releases: make(chan recordedUseRelease, 1),
	}
	host := testHost(t, frame.HostConfig{ExecutionProtection: protection})
	plugin := installNative(t, host, "provider", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup:       func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { return nil, nil },
	})
	owner, scope := ownerAndScope(t, host)
	root := frame.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}
	func() {
		defer func() {
			if recovered := recover(); recovered != panicCause {
				t.Errorf("approval panic changed: %v", recovered)
			}
		}()
		_, _ = host.AcquireUnit(t.Context(), root, frame.UseConfig{Owner: owner, Scope: scope, Realm: "realm"})
		t.Error("approval panic was swallowed")
	}()
	if err := host.Admission().RecoveryError(); !errors.Is(err, frame.ErrRecoveryRequired) {
		t.Errorf("interrupted approval left admission open: %v", err)
	}
	if outcome, err := host.Close(testContext(t)); err != nil || !outcome.Released {
		t.Fatalf("interrupted approval retained its provider: %+v, %v", outcome, err)
	}
}
