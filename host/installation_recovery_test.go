package host_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func TestRestorationCannotRollBackCommittedConfiguration(t *testing.T) {
	store, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "restoration.db"), Admission: framehost.NewAdmission()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	host := testHost(t, framehost.Config{Authority: store})
	prepared := make(chan []byte, 1)
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("restoration-original-authority")), Units: []framehost.Unit{{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			prepared <- setup.Configuration()
			return nil, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	installed, err := host.Install(t.Context(), framehost.InstallationConfig{ID: "worker", Plugin: plugin, Configuration: []byte("original"), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	previous, err := store.Installations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	updated := []byte("committed update")
	if err := host.Replace(t.Context(), "worker", plugin, updated); err != nil {
		t.Fatal(err)
	}
	if len(previous) != 1 || !bytes.Equal(previous[0].Configuration, []byte("original")) {
		t.Fatalf("previous read did not retain the superseded configuration: %+v", previous)
	}
	if err := host.Restore(t.Context(), []*framehost.Plugin{plugin}); err != nil {
		t.Fatal(err)
	}
	if _, err := host.ActivateUnit(t.Context(), "", installed.Unit("main")); err != nil {
		t.Fatal(err)
	}
	if actual := <-prepared; !bytes.Equal(actual, updated) {
		t.Errorf("restoration executed a superseded configuration: prepared=%q committed=%q", actual, updated)
	}
	current, found := host.Installation("worker")
	durable, err := store.Installations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(durable) != 1 || !bytes.Equal(current.Configuration, durable[0].Configuration) {
		t.Fatalf("restoration advanced a competing installation record: host=%q durable=%+v", current.Configuration, durable)
	}
}

func TestRestorationReadAndProjectionShareInstallationAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newRestorationStore()
		host := testHost(t, framehost.Config{Authority: installationAuthority(store)})
		plugin := installNative(t, host, "worker")
		reading, resume := make(chan struct{}), make(chan struct{})
		resumeRead := sync.OnceFunc(func() { close(resume) })
		defer resumeRead()
		store.read = func(context.Context) error {
			if _, found := host.Installation("worker"); !found {
				t.Error("authority read lost the current installation")
			}
			close(reading)
			<-resume
			return nil
		}
		restored := make(chan error, 1)
		go func() { restored <- host.Restore(t.Context(), []*framehost.Plugin{plugin}) }()
		<-reading
		updated := []byte("committed after restoration")
		changed := make(chan error, 1)
		go func() { changed <- host.Replace(t.Context(), "worker", plugin, updated) }()
		synctest.Wait()
		select {
		case err := <-changed:
			t.Fatalf("installation mutation bypassed restoration admission: %v", err)
		default:
		}
		resumeRead()
		if err := <-restored; err != nil {
			t.Fatal(err)
		}
		if err := <-changed; err != nil {
			t.Fatal(err)
		}
		store.read = nil
		records, err := store.Installations(t.Context())
		current, found := host.Installation("worker")
		if err != nil || len(records) != 1 || !found || !bytes.Equal(current.Configuration, updated) || !bytes.Equal(records[0].Configuration, updated) {
			t.Fatalf("restoration overwrote the later commit: host=%+v durable=%+v error=%v", current, records, err)
		}
	})
}

func TestHostCloseJoinsOriginalRestorationRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newRestorationStore()
		host := testHost(t, framehost.Config{Authority: installationAuthority(store)})
		plugin := installNative(t, host, "worker")
		committed, _ := host.Installation("worker")
		committed.Configuration = []byte("unprojected committed configuration")
		store.setRecords(committed)
		reading, resume := make(chan context.Context, 1), make(chan struct{})
		resumeRead := sync.OnceFunc(func() { close(resume) })
		defer resumeRead()
		store.read = func(ctx context.Context) error {
			reading <- ctx
			<-resume
			return nil
		}
		restored := make(chan error, 1)
		go func() { restored <- host.Restore(t.Context(), []*framehost.Plugin{plugin}) }()
		original := <-reading
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		outcome, err := host.Close(canceled)
		synctest.Wait()
		if outcome.Released || outcome.ActiveWork != 1 || !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked original read was reported released: %+v %v", outcome, err)
		}
		if !errors.Is(frame.ContextError(original), frame.ErrClosed) {
			t.Fatal("Host close did not cancel the original restoration read")
		}
		resumeRead()
		if err := <-restored; !errors.Is(err, frame.ErrClosed) {
			t.Fatalf("restoration projected after Host close: %v", err)
		}
		if current, _ := host.Installation("worker"); len(current.Configuration) != 0 {
			t.Fatalf("closed Host accepted a late authority read: %+v", current)
		}
		if outcome, err := host.Close(t.Context()); !outcome.Released || outcome.ActiveWork != 0 || err != nil {
			t.Fatalf("completed original read remained active: %+v %v", outcome, err)
		}
	})
}

func TestRestorationReadFailurePreservesRecoveryAndCancellation(t *testing.T) {
	failure := errors.New("test: committed installations unavailable")
	for _, test := range []struct {
		name     string
		cancel   bool
		readErr  error
		recovery bool
	}{
		{name: "storage failure", readErr: failure, recovery: true},
		{name: "request cancellation", cancel: true, readErr: context.Canceled},
		{name: "cancellation after successful read", cancel: true},
		{name: "cancellation with storage failure", cancel: true, readErr: errors.Join(context.Canceled, failure), recovery: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newRestorationStore()
			host := testHost(t, framehost.Config{Authority: installationAuthority(store)})
			plugin := installNative(t, host, "worker")
			record, _ := host.Installation("worker")
			record.Configuration = []byte("durable recovery target")
			store.setRecords(record)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store.read = func(context.Context) error {
				if test.cancel {
					cancel()
				}
				return test.readErr
			}
			err := host.Restore(ctx, []*framehost.Plugin{plugin})
			if err == nil || test.readErr != nil && !errors.Is(err, test.readErr) || test.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("restoration lost the original read failure: %v", err)
			}
			if recovery := host.Admission().RecoveryError(); errors.Is(recovery, framehost.ErrRecoveryRequired) != test.recovery {
				t.Errorf("read failure established the wrong barrier: %v", recovery)
			}
			if current, _ := host.Installation("worker"); len(current.Configuration) != 0 {
				t.Fatalf("failed restoration partially published: %+v", current)
			}
			if store.confirmed != 0 {
				t.Error("failed restoration confirmed records it never projected")
			}
		})
	}
}

func TestRestorationConfirmsItsProjectionToTheAuthority(t *testing.T) {
	store := newRestorationStore()
	host := testHost(t, framehost.Config{Authority: installationAuthority(store)})
	plugin := installNative(t, host, "worker")
	if err := host.Restore(t.Context(), []*framehost.Plugin{plugin}); err != nil {
		t.Fatal(err)
	}
	if store.confirmed != 1 {
		t.Fatalf("restoration confirmed its projection %d times", store.confirmed)
	}
}

func TestInterruptedRestorationReadFencesOriginalAdmission(t *testing.T) {
	failure := errors.New("test: interrupted installation read")
	for _, interrupt := range []struct {
		name       string
		run        func()
		panicValue any
	}{
		{"panic", func() { panic(failure) }, failure},
		{"goexit", runtime.Goexit, nil},
	} {
		t.Run(interrupt.name, func(t *testing.T) {
			store := newRestorationStore()
			host := testHost(t, framehost.Config{Authority: installationAuthority(store)})
			plugin := installNative(t, host, "worker")
			store.read = func(context.Context) error { interrupt.run(); return nil }
			done := make(chan any, 1)
			go func() {
				defer func() { done <- recover() }()
				err := host.Restore(t.Context(), []*framehost.Plugin{plugin})
				t.Errorf("original storage interruption returned normally: %v", err)
			}()
			if recovered := <-done; recovered != interrupt.panicValue {
				t.Fatalf("original read interruption changed: %v", recovered)
			}
			if err := host.Admission().Under(t.Context(), func() error {
				t.Error("interrupted restoration left normal admission open")
				return nil
			}); !errors.Is(err, framehost.ErrRecoveryRequired) {
				t.Fatalf("original admission was not fenced: %v", err)
			}
			if outcome, err := host.Close(t.Context()); !outcome.Released || outcome.ActiveWork != 0 || err != nil {
				t.Fatalf("interrupted original read stranded Host work: %+v %v", outcome, err)
			}
		})
	}
}

func TestRestorationRequiresOriginalExecutionExitProof(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newRestorationStore()
		host := testHost(t, framehost.Config{Authority: installationAuthority(store)})
		var execution frame.ResourceHandle
		plugin := installNative(t, host, "worker", framehost.Unit{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
			Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
				var err error
				execution, err = setup.Own(frame.Resource{Name: "original execution", Close: func(context.Context) (frame.ReleaseState, error) {
					return frame.ResourceUnconfirmed, nil
				}})
				return nil, err
			},
		})
		root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
		if _, err := host.ActivateUnit(t.Context(), "", root); err != nil {
			t.Fatal(err)
		}
		if err := host.Retire(t.Context(), "worker"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		var reads int
		store.read = func(context.Context) error { reads++; return nil }
		if err := host.Restore(t.Context(), []*framehost.Plugin{plugin}); !errors.Is(err, framehost.ErrInUse) || reads != 0 {
			t.Fatalf("restoration read past an unconfirmed original exit: reads=%d error=%v", reads, err)
		}
		if err := execution.ConfirmReleased(nil); err != nil {
			t.Fatal(err)
		}
		if err := host.WaitRetired(t.Context(), "worker"); err != nil {
			t.Fatal(err)
		}
		if err := host.Restore(t.Context(), []*framehost.Plugin{plugin}); err != nil || reads != 1 || len(host.Observations()) != 0 {
			t.Fatalf("original proof did not permit resource-free restoration: reads=%d error=%v", reads, err)
		}
	})
}

func TestVolatileHostHasNoInstallationRestorationAuthority(t *testing.T) {
	host := testHost(t, framehost.Config{})
	if err := host.Restore(t.Context(), nil); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("volatile Host accepted installation restoration: %v", err)
	}
}

type restorationStore struct {
	mu        sync.Mutex
	records   map[framehost.InstallationID]framehost.InstallationRecord
	read      func(context.Context) error
	confirmed int
}

func newRestorationStore(records ...framehost.InstallationRecord) *restorationStore {
	store := new(restorationStore)
	store.setRecords(records...)
	return store
}

func (s *restorationStore) setRecords(records ...framehost.InstallationRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = make(map[framehost.InstallationID]framehost.InstallationRecord, len(records))
	for _, record := range records {
		s.records[record.ID] = record.Clone()
	}
}

func (s *restorationStore) CommitInstallation(ctx context.Context, change framehost.InstallationChange) (framehost.CommitState, error) {
	if err := frame.ContextError(ctx); err != nil {
		return framehost.NotCommitted, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if change.After != nil {
		s.records[change.After.ID] = change.After.Clone()
	} else {
		delete(s.records, change.Before.ID)
	}
	return framehost.Committed, nil
}

func (*restorationStore) ConfirmInstallation(context.Context, framehost.InstallationID) error {
	return nil
}

func (s *restorationStore) ConfirmRestoredInstallations(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.confirmed++
	return nil
}

func (s *restorationStore) Installations(ctx context.Context) ([]framehost.InstallationRecord, error) {
	if s.read != nil {
		if err := s.read(ctx); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]framehost.InstallationRecord, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record.Clone())
	}
	return records, nil
}
