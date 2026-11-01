package frame_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

type retainedUseOwner struct {
	read func(context.Context, frame.InstallationID) ([]frame.Dependency, error)
}

func (retainedUseOwner) ID() string { return "consumer.documents" }
func (o *retainedUseOwner) Held(ctx context.Context, id frame.InstallationID) ([]frame.Dependency, error) {
	return o.read(ctx, id)
}

func TestUnavailableUseOwnerFencesAdmissionUntilRecovered(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, frame.HostConfig{})
	plugin := installNative(t, host, "retained", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
		return nil, nil
	}})
	unavailable := errors.New("original business store is unavailable")
	owner := &retainedUseOwner{read: func(context.Context, frame.InstallationID) ([]frame.Dependency, error) {
		return nil, unavailable
	}}
	if err := host.Admission().RegisterUseOwner(owner); err != nil {
		t.Fatal(err)
	}
	if err := host.Uninstall(ctx, "retained"); !errors.Is(err, unavailable) || !errors.Is(err, frame.ErrRecoveryRequired) {
		t.Fatalf("unknown business retention was treated as unused: %v", err)
	}
	if err := host.Admission().RecoveryError(); !errors.Is(err, unavailable) || !errors.Is(err, frame.ErrRecoveryRequired) {
		t.Fatalf("owner failure did not retain a recovery barrier: %v", err)
	}
	if _, err := host.Install(ctx, frame.InstallationConfig{ID: "after-failure", Plugin: plugin, Enabled: true}); !errors.Is(err, frame.ErrRecoveryRequired) {
		t.Fatalf("new admission escaped the required owner: %v", err)
	}
	restore := func() error {
		_, err := host.Admission().Held(ctx, "retained")
		return err
	}
	if err := host.Admission().Recover(ctx, restore); !errors.Is(err, unavailable) {
		t.Fatalf("failed restoration cleared the owner failure: %v", err)
	}
	owner.read = func(context.Context, frame.InstallationID) ([]frame.Dependency, error) { return nil, nil }
	if err := host.Admission().Recover(ctx, restore); err != nil {
		t.Fatalf("repaired owner could not recover: %v", err)
	}
	if err := host.Admission().RecoveryError(); err != nil {
		t.Fatalf("completed owner restoration retained a stale barrier: %v", err)
	}
	if err := host.Uninstall(ctx, "retained"); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(ctx, frame.InstallationConfig{ID: "after-recovery", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidUseOwnerProjectionRequiresRecovery(t *testing.T) {
	for _, test := range []struct {
		name       string
		dependency frame.Dependency
	}{
		{"invalid release", frame.Dependency{InstallationID: "original", Release: value.Digest{}}},
		{"different installation", frame.Dependency{InstallationID: "another", Release: value.Hash([]byte("release"))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			admission := frame.NewAdmission()
			owner := &retainedUseOwner{read: func(context.Context, frame.InstallationID) ([]frame.Dependency, error) {
				return []frame.Dependency{test.dependency}, nil
			}}
			if err := admission.RegisterUseOwner(owner); err != nil {
				t.Fatal(err)
			}
			if _, err := admission.Held(t.Context(), "original"); !errors.Is(err, frame.ErrInvalid) || !errors.Is(err, frame.ErrRecoveryRequired) {
				t.Fatalf("invalid required owner projection was accepted: %v", err)
			}
			if err := admission.RecoveryError(); !errors.Is(err, frame.ErrRecoveryRequired) {
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
			admission := frame.NewAdmission()
			owner := &retainedUseOwner{read: func(context.Context, frame.InstallationID) ([]frame.Dependency, error) {
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
				if !errors.Is(err, storageFailure) || !errors.Is(admission.RecoveryError(), frame.ErrRecoveryRequired) {
					t.Fatalf("cancellation hid the owner failure: %v; recovery: %v", err, admission.RecoveryError())
				}
			} else if err := admission.RecoveryError(); err != nil {
				t.Fatalf("caller cancellation poisoned the owner: %v", err)
			}
		})
	}
}
