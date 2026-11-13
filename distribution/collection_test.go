package distribution

import (
	"context"
	"errors"
	"testing"

	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
)

func TestInvalidatedRetentionReadCannotWithdrawRelease(t *testing.T) {
	for _, boundary := range []struct {
		name         string
		cancellation bool
	}{
		{name: "cancellation", cancellation: true},
		{name: "recovery"},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("collection invalidated during retention read")
			var invalidate func()
			store := storeFixture(t, func(context.Context, framevalue.Digest) (bool, error) {
				invalidate()
				return false, nil
			})
			expected := framehost.ErrRecoveryRequired
			invalidate = func() { store.admission.RequireRecovery(cause) }
			if boundary.cancellation {
				expected = context.Canceled
				invalidate = func() { cancel(cause) }
			}
			_, _, archive, _ := packageFixture(t)
			release := installArchive(t, store, archive)
			if err := store.Remove(ctx, release.Digest); !errors.Is(err, expected) || !errors.Is(err, cause) {
				t.Errorf("invalidated retention read withdrew a release: %v", err)
			}
			if !boundary.cancellation {
				if err := store.admission.RecoveryError(); !errors.Is(err, cause) {
					t.Errorf("original recovery barrier changed: %v", err)
				}
				if err := store.admission.Recover(t.Context(), func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			lease, err := store.Acquire(t.Context(), release.Digest)
			if err != nil {
				t.Fatalf("invalidated collection lost the original release: %v", err)
			}
			defer lease.Close()
			if _, err := lease.Read(t.Context(), "theme.css"); err != nil {
				t.Fatalf("invalidated collection changed original content: %v", err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			invalidate = func() {}
			if err := store.Remove(t.Context(), release.Digest); err != nil {
				t.Fatalf("new collection could not withdraw the original release: %v", err)
			}
		})
	}
}
