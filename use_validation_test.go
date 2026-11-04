package frame_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
)

func TestInvalidUseIdentityIsRejectedBeforePreparation(t *testing.T) {
	for _, test := range []struct {
		name      string
		realm     frame.RealmID
		execution string
	}{
		{"realm", "invalid realm", "execution"},
		{"execution", "realm", "invalid execution"},
	} {
		t.Run(test.name, func(t *testing.T) {
			protection := &useRetirementProtection{approve: func() error { return nil }, releases: make(chan recordedUseRelease, 1)}
			host := testHost(t, frame.HostConfig{ExecutionProtection: protection})
			var prepared atomic.Int32
			plugin := installNative(t, host, "provider", frame.Unit{
				Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
				Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
					prepared.Add(1)
					return nil, nil
				},
			})
			owner, scope := ownerAndScope(t, host)
			root := frame.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}
			use, err := host.AcquireUnit(t.Context(), root, frame.UseConfig{Owner: owner, Scope: scope, Realm: test.realm, ExecutionID: test.execution})
			if use != nil || !errors.Is(err, frame.ErrInvalid) {
				t.Fatalf("invalid use identity was admitted: %v", err)
			}
			if prepared.Load() != 0 {
				t.Fatalf("invalid use identity started %d plugin preparations", prepared.Load())
			}
		})
	}
}
