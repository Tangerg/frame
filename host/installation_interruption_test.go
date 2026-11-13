package host_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

type interruptedInstallationStore struct {
	committed *framehost.InstallationRecord
	commit    func()
	confirm   func()
}

func (s *interruptedInstallationStore) CommitInstallation(_ context.Context, change framehost.InstallationChange) (framehost.CommitState, error) {
	s.committed = nil
	if change.After != nil {
		record := change.After.Clone()
		s.committed = &record
	}
	if s.commit != nil {
		s.commit()
	}
	return framehost.CommitSucceeded, nil
}

func (s *interruptedInstallationStore) ConfirmInstallation(context.Context, framehost.InstallationID) error {
	if s.confirm != nil {
		s.confirm()
	}
	return nil
}

func (s *interruptedInstallationStore) Installations(context.Context) ([]framehost.InstallationRecord, error) {
	if s.committed == nil {
		return nil, nil
	}
	return []framehost.InstallationRecord{s.committed.Clone()}, nil
}

func TestInterruptedInstallationWriteRequiresRecovery(t *testing.T) {
	cause := errors.New("installation storage interrupted")
	for _, operation := range []string{"install", "disable", "uninstall"} {
		for _, phase := range []string{"commit", "confirm"} {
			for _, interrupt := range []struct {
				name       string
				run        func()
				panicValue any
			}{
				{name: "panic", run: func() { panic(cause) }, panicValue: cause},
				{name: "goexit", run: runtime.Goexit},
			} {
				t.Run(operation+"/"+phase+"/"+interrupt.name, func(t *testing.T) {
					ctx := testContext(t)
					store := &interruptedInstallationStore{}
					host := testHost(t, framehost.Config{Installations: store})
					prepared := make(chan string, 1)
					var cleanups atomic.Int32
					plugin, err := framehost.Define(framehost.Definition{
						Revision: value.Hash([]byte("interrupted-installation")),
						Units: []framehost.Unit{{
							Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
							Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
								prepared <- setup.InstanceID()
								_, err := setup.Cleanup("original resource", func() error { cleanups.Add(1); return nil })
								return nil, err
							},
						}},
					})
					if err != nil {
						t.Fatal(err)
					}
					apply := func() error {
						_, err := host.Install(ctx, framehost.InstallationConfig{ID: "worker", Plugin: plugin, Enabled: true})
						return err
					}
					var instanceID string
					if operation != "install" {
						if err := apply(); err != nil {
							t.Fatal(err)
						}
						root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
						if _, err := host.ActivateUnit(ctx, "", root); err != nil {
							t.Fatal(err)
						}
						instanceID = <-prepared
						if operation == "disable" {
							apply = func() error { return host.SetEnabled(ctx, "worker", false) }
						} else {
							apply = func() error { return host.Uninstall(ctx, "worker") }
						}
					}
					if phase == "commit" {
						store.commit = interrupt.run
					} else {
						store.confirm = interrupt.run
					}
					done := make(chan any, 1)
					go func() {
						defer func() { done <- recover() }()
						if err := apply(); err != nil {
							t.Errorf("interruption became a normal error: %v", err)
						}
						t.Error("interrupted storage returned normally")
					}()
					select {
					case recovered := <-done:
						if recovered != interrupt.panicValue {
							t.Fatalf("original interruption changed: %v", recovered)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if operation == "uninstall" && store.committed != nil || operation != "uninstall" && (store.committed == nil || store.committed.Enabled != (operation == "install")) {
						t.Fatalf("original committed record was lost: %+v", store.committed)
					}
					if err := host.Admission().RecoveryError(); !errors.Is(err, framehost.ErrRecoveryRequired) {
						t.Fatalf("interrupted installation left admission open: %v", err)
					}
					admitted := false
					err = host.Admission().Under(ctx, func() error { admitted = true; return nil })
					if admitted || !errors.Is(err, framehost.ErrRecoveryRequired) {
						t.Fatalf("new work passed the recovery barrier: admitted=%t, error=%v", admitted, err)
					}
					if operation == "install" {
						return
					}
					if phase == "confirm" {
						if err := host.WaitInstanceRetired(ctx, instanceID); err != nil {
							t.Fatalf("confirmation interruption stranded accepted retirement: %v", err)
						}
						if cleanups.Load() != 1 {
							t.Fatalf("accepted retirement lost its original cleanup: %d", cleanups.Load())
						}
						return
					}
					observations := host.Observations()
					if len(observations) != 1 || observations[0].ID != instanceID || observations[0].State != framehost.Ready || cleanups.Load() != 0 {
						t.Fatalf("unknown commit retired an unconfirmed transition: cleanups=%d, observations=%+v", cleanups.Load(), observations)
					}
				})
			}
		}
	}
}

func TestInterruptedEnableRecoversOriginalTransition(t *testing.T) {
	for _, interruption := range []struct {
		name string
		run  func()
	}{
		{name: "panic", run: func() { panic("original enable interrupted") }},
		{name: "goexit", run: runtime.Goexit},
	} {
		t.Run(interruption.name, func(t *testing.T) {
			store := &interruptedInstallationStore{}
			host := testHost(t, framehost.Config{Installations: store})
			var prepared atomic.Int64
			plugin := installNative(t, host, "provider", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
				Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
					prepared.Add(1)
					return nil, nil
				},
			})
			if err := host.SetEnabled(t.Context(), "provider", false); err != nil {
				t.Fatal(err)
			}
			store.commit = interruption.run
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { _ = recover() }()
				_ = host.SetEnabled(t.Context(), "provider", true)
				t.Error("interrupted enable returned")
			}()
			<-done
			store.commit = nil
			if err := host.Admission().Recover(t.Context(), func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := host.SetEnabled(t.Context(), "provider", false); !errors.Is(err, framehost.ErrRecoveryRequired) {
				t.Fatalf("another operation replaced the original unresolved enable: %v", err)
			}
			records, err := store.Installations(t.Context())
			if err != nil || len(records) != 1 || !records[0].Enabled {
				t.Fatalf("interrupted enable did not actually commit: %+v, %v", records, err)
			}
			if err := host.Restore(t.Context(), records, []*framehost.Plugin{plugin}); err != nil {
				t.Fatal(err)
			}
			if err := host.Admission().Recover(t.Context(), func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := host.ActivateUnit(t.Context(), "", records[0].Unit("main")); err != nil || prepared.Load() != 1 {
				t.Fatalf("recovered enable retained the old fence: setups=%d, error=%v", prepared.Load(), err)
			}
		})
	}
}
