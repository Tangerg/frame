package host_test

import (
	"context"
	"errors"
	"runtime"
	"testing"

	framehost "github.com/Tangerg/frame/host"
)

func TestInterruptedAdmissionReadAndRecoveryRequireRecovery(t *testing.T) {
	cause := errors.New("original admission callback interrupted")
	for _, boundary := range []struct {
		name    string
		restore bool
	}{
		{name: "retained dependency read"},
		{name: "restoration", restore: true},
	} {
		for _, interruption := range []struct {
			name       string
			run        func()
			panicValue any
		}{
			{name: "panic", run: func() { panic(cause) }, panicValue: cause},
			{name: "goexit", run: runtime.Goexit},
		} {
			t.Run(boundary.name+"/"+interruption.name, func(t *testing.T) {
				ctx := testContext(t)
				admission := framehost.NewAdmission()
				repaired := false
				owner := &retainedUseOwner{read: func(context.Context, framehost.InstallationID) ([]framehost.Dependency, error) {
					if !repaired {
						interruption.run()
					}
					return nil, nil
				}}
				if err := admission.RegisterUseOwner(owner); err != nil {
					t.Fatal(err)
				}
				read := func() error {
					_, err := admission.Retained(ctx, "original")
					return err
				}
				apply := func() error { return admission.Under(ctx, read) }
				if boundary.restore {
					apply = func() error {
						return admission.Recover(ctx, func() error {
							interruption.run()
							return nil
						})
					}
				}
				finished := make(chan any, 1)
				go func() {
					defer func() { finished <- recover() }()
					_ = apply()
					t.Error("interrupted callback returned normally")
				}()
				select {
				case recovered := <-finished:
					if recovered != interruption.panicValue {
						t.Fatalf("original interruption changed: %v", recovered)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if err := admission.RecoveryError(); !errors.Is(err, framehost.ErrRecoveryRequired) {
					t.Errorf("interrupted %s left admission open: %v", boundary.name, err)
				}
				called := false
				if err := admission.Under(ctx, func() error { called = true; return nil }); called || !errors.Is(err, framehost.ErrRecoveryRequired) {
					t.Errorf("new work escaped the interrupted owner: called=%t, error=%v", called, err)
				}
				repaired = true
				if err := admission.Recover(ctx, read); err != nil {
					t.Fatalf("original owner could not recover: %v", err)
				}
				if err := admission.Under(ctx, func() error { return nil }); err != nil {
					t.Fatalf("completed recovery did not reopen admission: %v", err)
				}
			})
		}
	}
}
