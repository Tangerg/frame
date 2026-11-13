package host_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

type retainedUseOwner struct {
	read func(context.Context, framehost.InstallationID) ([]framehost.Dependency, error)
}

func (retainedUseOwner) ID() string { return "consumer.documents" }
func (o *retainedUseOwner) Held(ctx context.Context, id framehost.InstallationID) ([]framehost.Dependency, error) {
	return o.read(ctx, id)
}

func TestUnavailableUseOwnerFencesAdmissionUntilRecovered(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	plugin := installNative(t, host, "retained", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo}, Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
		return nil, nil
	}})
	unavailable := errors.New("original business store is unavailable")
	owner := &retainedUseOwner{read: func(context.Context, framehost.InstallationID) ([]framehost.Dependency, error) {
		return nil, unavailable
	}}
	if err := host.Admission().RegisterUseOwner(owner); err != nil {
		t.Fatal(err)
	}
	if err := host.Uninstall(ctx, "retained"); !errors.Is(err, unavailable) || !errors.Is(err, framehost.ErrRecoveryRequired) {
		t.Fatalf("unknown business retention was treated as unused: %v", err)
	}
	if err := host.Admission().RecoveryError(); !errors.Is(err, unavailable) || !errors.Is(err, framehost.ErrRecoveryRequired) {
		t.Fatalf("owner failure did not retain a recovery barrier: %v", err)
	}
	if _, err := host.Install(ctx, framehost.InstallationConfig{ID: "after-failure", Plugin: plugin, Enabled: true}); !errors.Is(err, framehost.ErrRecoveryRequired) {
		t.Fatalf("new admission escaped the required owner: %v", err)
	}
	restore := func() error {
		_, err := host.Admission().Held(ctx, "retained")
		return err
	}
	if err := host.Admission().Recover(ctx, restore); !errors.Is(err, unavailable) {
		t.Fatalf("failed restoration cleared the owner failure: %v", err)
	}
	owner.read = func(context.Context, framehost.InstallationID) ([]framehost.Dependency, error) { return nil, nil }
	if err := host.Admission().Recover(ctx, restore); err != nil {
		t.Fatalf("repaired owner could not recover: %v", err)
	}
	if err := host.Admission().RecoveryError(); err != nil {
		t.Fatalf("completed owner restoration retained a stale barrier: %v", err)
	}
	if err := host.Uninstall(ctx, "retained"); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(ctx, framehost.InstallationConfig{ID: "after-recovery", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidUseOwnerProjectionRequiresRecovery(t *testing.T) {
	for _, test := range []struct {
		name       string
		dependency framehost.Dependency
	}{
		{"invalid release", framehost.Dependency{InstallationID: "original", Release: value.Digest{}}},
		{"different installation", framehost.Dependency{InstallationID: "another", Release: value.Hash([]byte("release"))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			admission := framehost.NewAdmission()
			owner := &retainedUseOwner{read: func(context.Context, framehost.InstallationID) ([]framehost.Dependency, error) {
				return []framehost.Dependency{test.dependency}, nil
			}}
			if err := admission.RegisterUseOwner(owner); err != nil {
				t.Fatal(err)
			}
			if _, err := admission.Held(t.Context(), "original"); !errors.Is(err, frame.ErrInvalid) || !errors.Is(err, framehost.ErrRecoveryRequired) {
				t.Fatalf("invalid required owner projection was accepted: %v", err)
			}
			if err := admission.RecoveryError(); !errors.Is(err, framehost.ErrRecoveryRequired) {
				t.Fatalf("invalid owner did not fence admission: %v", err)
			}
		})
	}
}

func TestUseOwnerCancellationDoesNotHideConcurrentFailure(t *testing.T) {
	for _, test := range []struct {
		name        string
		storageFail bool
	}{
		{"request cancellation", false},
		{"cancellation and storage failure", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			requestCause := errors.New("caller ended its wait")
			storageFailure := errors.New("business store read failed")
			admission := framehost.NewAdmission()
			owner := &retainedUseOwner{read: func(context.Context, framehost.InstallationID) ([]framehost.Dependency, error) {
				cancel(requestCause)
				failure := errors.Join(ctx.Err(), context.Cause(ctx))
				if test.storageFail {
					failure = errors.Join(failure, storageFailure)
				}
				return nil, fmt.Errorf("read original rows: %w", failure)
			}}
			if err := admission.RegisterUseOwner(owner); err != nil {
				t.Fatal(err)
			}
			_, err := admission.Held(ctx, "original")
			if !errors.Is(err, context.Canceled) || !errors.Is(err, requestCause) {
				t.Fatalf("request cancellation was lost: %v", err)
			}
			if test.storageFail {
				if !errors.Is(err, storageFailure) || !errors.Is(admission.RecoveryError(), framehost.ErrRecoveryRequired) {
					t.Fatalf("cancellation hid the owner failure: %v; recovery: %v", err, admission.RecoveryError())
				}
			} else if err := admission.RecoveryError(); err != nil {
				t.Fatalf("caller cancellation poisoned the owner: %v", err)
			}
		})
	}
}

func TestDestructiveReadSealsItsBusinessOwnerSet(t *testing.T) {
	a := framehost.NewAdmission()
	entered, resume := make(chan struct{}), make(chan struct{})
	original := &retainedUseOwner{read: func(context.Context, framehost.InstallationID) ([]framehost.Dependency, error) {
		close(entered)
		<-resume
		return nil, nil
	}}
	if err := a.RegisterUseOwner(original); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- a.Under(t.Context(), func() error { return a.CheckUnused(t.Context(), "install") })
	}()
	<-entered
	late := &diagnosticUseOwner{id: "late", values: []framehost.Dependency{{InstallationID: "install", Release: value.Hash(nil)}}}
	err := a.RegisterUseOwner(late)
	close(resume)
	if !errors.Is(err, frame.ErrClosed) {
		t.Errorf("late business owner entered destructive decision: %v", err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionProofRequiresAnActiveUncancelledContext(t *testing.T) {
	admission := framehost.NewAdmission()
	for _, test := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "missing context"},
		{name: "unadmitted context", ctx: context.Background()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := admission.CheckAdmitted(test.ctx); !errors.Is(err, frame.ErrInvalid) {
				t.Fatalf("invalid admission context: %v", err)
			}
		})
	}
	var retained context.Context
	cause := errors.New("original admitted request cancelled")
	if err := admission.Admit(t.Context(), func(admitted context.Context) error {
		retained = admitted
		if err := admission.CheckAdmitted(admitted); err != nil {
			t.Fatal(err)
		}
		cancelled, cancel := context.WithCancelCause(admitted)
		cancel(cause)
		if err := admission.CheckAdmitted(cancelled); !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
			t.Errorf("cancelled capability still authorized work: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := admission.CheckAdmitted(retained); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("retained admission capability: %v", err)
	}
}
