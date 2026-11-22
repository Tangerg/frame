package host_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/frame"
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
	return framehost.Committed, nil
}

func (*interruptedInstallationStore) ConfirmRestoredInstallations(context.Context) error { return nil }

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

func TestCommittedWithdrawalRetiresBeforeStorageConfirmation(t *testing.T) {
	for _, pending := range []bool{false, true} {
		phase := "ready"
		if pending {
			phase = "preparing"
		}
		for _, operation := range []struct {
			name   string
			change func(context.Context, framehost.Host, *framehost.Plugin) error
		}{
			{"disable", func(ctx context.Context, host framehost.Host, _ *framehost.Plugin) error {
				return host.Disable(ctx, "worker")
			}},
			{"replace", func(ctx context.Context, host framehost.Host, plugin *framehost.Plugin) error {
				return host.Replace(ctx, "worker", plugin, []byte("next configuration"))
			}},
			{"update", func(ctx context.Context, host framehost.Host, plugin *framehost.Plugin) error {
				original, _ := host.Installation("worker")
				return host.Update(ctx, original, framehost.InstallationConfig{ID: "worker", Plugin: plugin, Configuration: []byte("next configuration"), Enabled: true})
			}},
			{"uninstall", func(ctx context.Context, host framehost.Host, _ *framehost.Plugin) error {
				return host.Uninstall(ctx, "worker")
			}},
		} {
			t.Run(phase+"/"+operation.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					storage := &interruptedInstallationStore{}
					host := testHost(t, framehost.Config{Authority: installationAuthority(storage)})
					entered := make(chan struct{})
					var cleanups atomic.Int32
					plugin := installNative(t, host, "worker", framehost.Unit{
						Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
						Setup: func(ctx context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
							if _, err := setup.Cleanup("original resource", func() error { cleanups.Add(1); return nil }); err != nil {
								return nil, err
							}
							close(entered)
							if pending {
								<-ctx.Done()
								return nil, frame.ContextError(ctx)
							}
							return nil, nil
						},
					})
					activated := make(chan error, 1)
					go func() {
						_, err := host.ActivateUnit(t.Context(), "", framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"})
						activated <- err
					}()
					<-entered
					if !pending {
						if err := <-activated; err != nil {
							t.Fatal(err)
						}
					}
					confirming, confirm := make(chan struct{}), make(chan struct{})
					storage.confirm = func() { close(confirming); <-confirm }
					changed := make(chan error, 1)
					go func() { changed <- operation.change(t.Context(), host, plugin) }()
					<-confirming
					synctest.Wait()
					if cleanups.Load() != 1 || len(host.Observations()) != 0 {
						t.Errorf("committed withdrawal left retirement behind storage confirmation: cleanups=%d observations=%+v", cleanups.Load(), host.Observations())
					}
					close(confirm)
					if err := <-changed; err != nil {
						t.Fatalf("original committed transition did not confirm: %v", err)
					}
					if pending {
						if err := <-activated; err == nil {
							t.Error("withdrawn preparation published readiness")
						}
					}
					if err := host.WaitRetired(t.Context(), "worker"); err != nil || cleanups.Load() != 1 {
						t.Fatalf("original retirement was replaced or repeated: cleanups=%d error=%v", cleanups.Load(), err)
					}
				})
			})
		}
	}
}

func TestCommittedWithdrawalDrainsUsesDuringStorageConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		storage := &interruptedInstallationStore{}
		host := testHost(t, framehost.Config{Authority: installationAuthority(storage)})
		var cleanups atomic.Int32
		plugin := installNative(t, host, "worker", framehost.Unit{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
			Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
				_, err := setup.Cleanup("original resource", func() error { cleanups.Add(1); return nil })
				return nil, err
			},
		})
		owner, scope := ownerAndScope(t, host)
		use, err := host.AcquireUnit(t.Context(), framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}, framehost.UseConfig{Owner: owner, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		confirming, confirm := make(chan struct{}), make(chan struct{})
		storage.confirm = func() { close(confirming); <-confirm }
		changed := make(chan error, 1)
		go func() { changed <- host.Disable(t.Context(), "worker") }()
		<-confirming
		synctest.Wait()
		work, err := use.Enter(t.Context())
		if err != nil {
			close(confirm)
			<-changed
			t.Fatalf("withdrawal revoked a draining Use: %v", err)
		}
		type closeResult struct {
			outcome frame.CloseOutcome
			err     error
		}
		closed := make(chan closeResult, 1)
		go func() { outcome, err := use.Close(t.Context()); closed <- closeResult{outcome, err} }()
		synctest.Wait()
		if cleanups.Load() != 0 {
			t.Errorf("withdrawal released an active borrow: cleanups=%d", cleanups.Load())
		}
		work.Finish()
		result := <-closed
		synctest.Wait()
		if !result.outcome.Released || result.err != nil || cleanups.Load() != 1 {
			t.Errorf("drained Use did not finish its original retirement during confirmation: cleanups=%d outcome=%+v error=%v", cleanups.Load(), result.outcome, result.err)
		}
		close(confirm)
		if err := <-changed; err != nil {
			t.Fatal(err)
		}
		if err := host.WaitRetired(t.Context(), "worker"); err != nil || cleanups.Load() != 1 {
			t.Fatalf("confirmation repeated the original retirement: cleanups=%d error=%v", cleanups.Load(), err)
		}
	})
}

func TestCommittedWithdrawalRetainsOriginalExitProof(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		storage := &interruptedInstallationStore{}
		host := testHost(t, framehost.Config{Authority: installationAuthority(storage)})
		var preparations, cleanups atomic.Int32
		var original frame.ResourceHandle
		var originalLifetime frame.Lifetime
		plugin := installNative(t, host, "worker", framehost.Unit{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
			Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
				generation := preparations.Add(1)
				resource, err := setup.Own(frame.Resource{Name: "original execution", Close: func(context.Context) (frame.ReleaseState, error) {
					cleanups.Add(1)
					if generation == 1 {
						return frame.ResourceUnconfirmed, nil
					}
					return frame.ResourceReleased, nil
				}})
				if generation == 1 {
					original, originalLifetime = resource, setup.Lifetime()
				}
				return nil, err
			},
		})
		root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
		if _, err := host.ActivateUnit(t.Context(), "", root); err != nil {
			t.Fatal(err)
		}
		confirming, confirm := make(chan struct{}), make(chan struct{})
		storage.confirm = func() { close(confirming); <-confirm }
		changed := make(chan error, 1)
		go func() { changed <- host.Disable(t.Context(), "worker") }()
		<-confirming
		synctest.Wait()
		if outcome, closing := originalLifetime.CloseOutcome(); !closing || outcome.Released || cleanups.Load() != 1 {
			t.Errorf("withdrawal did not retain the original unconfirmed exit: closing=%t cleanups=%d outcome=%+v", closing, cleanups.Load(), outcome)
		}
		close(confirm)
		if err := <-changed; err != nil {
			t.Fatal(err)
		}
		storage.confirm = nil
		if err := host.Enable(t.Context(), "worker"); err != nil {
			t.Fatal(err)
		}
		if _, err := host.ActivateUnit(t.Context(), "", root); !errors.Is(err, framehost.ErrInUse) || preparations.Load() != 1 {
			t.Errorf("confirmation substituted for original exit proof: preparations=%d error=%v", preparations.Load(), err)
		}
		if err := original.ConfirmReleased(nil); err != nil {
			t.Fatal(err)
		}
		if err := host.WaitRetired(t.Context(), "worker"); err != nil {
			t.Fatal(err)
		}
		if _, err := host.ActivateUnit(t.Context(), "", root); err != nil || preparations.Load() != 2 {
			t.Fatalf("late original proof did not admit fresh preparation: preparations=%d error=%v", preparations.Load(), err)
		}
		if err := host.Retire(t.Context(), "worker"); err != nil {
			t.Fatal(err)
		}
		if err := host.WaitRetired(t.Context(), "worker"); err != nil || cleanups.Load() != 2 {
			t.Fatalf("late proof replaced or repeated original cleanup: cleanups=%d error=%v", cleanups.Load(), err)
		}
	})
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
					host := testHost(t, framehost.Config{Authority: installationAuthority(store)})
					prepared := make(chan string, 1)
					var cleanups atomic.Int32
					plugin, err := framehost.Define(framehost.Definition{
						Revision: value.Hash([]byte("interrupted-installation")),
						Units: []framehost.Unit{{
							Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
							Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
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
							apply = func() error { return host.Disable(ctx, "worker") }
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
			host := testHost(t, framehost.Config{Authority: installationAuthority(store)})
			var prepared atomic.Int64
			plugin := installNative(t, host, "provider", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
				Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
					prepared.Add(1)
					return nil, nil
				},
			})
			if err := host.Disable(t.Context(), "provider"); err != nil {
				t.Fatal(err)
			}
			store.commit = interruption.run
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { _ = recover() }()
				_ = host.Enable(t.Context(), "provider")
				t.Error("interrupted enable returned")
			}()
			<-done
			store.commit = nil
			if err := host.Admission().Recover(t.Context(), func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := host.Disable(t.Context(), "provider"); !errors.Is(err, framehost.ErrRecoveryRequired) {
				t.Fatalf("another operation replaced the original unresolved enable: %v", err)
			}
			records, err := store.Installations(t.Context())
			if err != nil || len(records) != 1 || !records[0].Enabled {
				t.Fatalf("interrupted enable did not actually commit: %+v, %v", records, err)
			}
			if err := host.Restore(t.Context(), []*framehost.Plugin{plugin}); err != nil {
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
