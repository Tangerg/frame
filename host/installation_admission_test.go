package host_test

import (
	"context"
	"errors"
	"testing"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

type observedInstallationStore struct {
	resultStore
	commits int
}

func (s *observedInstallationStore) CommitInstallation(ctx context.Context, change framehost.InstallationChange) (framehost.CommitState, error) {
	s.commits++
	return s.resultStore.CommitInstallation(ctx, change)
}

func TestInstallationRevalidatesAfterRetentionRead(t *testing.T) {
	for _, boundary := range []struct {
		name         string
		cancellation bool
	}{
		{name: "cancellation", cancellation: true},
		{name: "recovery"},
	} {
		for _, persistence := range []struct {
			name      string
			withStore bool
		}{
			{name: "memory"},
			{name: "store", withStore: true},
		} {
			for _, operation := range []struct {
				name    string
				replace bool
			}{
				{name: "uninstall"},
				{name: "replace", replace: true},
			} {
				t.Run(boundary.name+"/"+persistence.name+"/"+operation.name, func(t *testing.T) {
					storage := &observedInstallationStore{resultStore: resultStore{state: framehost.CommitSucceeded}}
					config := framehost.Config{}
					if persistence.withStore {
						config.Authority = installationAuthority(storage)
					}
					host := testHost(t, config)
					original, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("original"))})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := host.Install(t.Context(), framehost.InstallationConfig{ID: "original", Plugin: original}); err != nil {
						t.Fatal(err)
					}
					replacement, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("replacement"))})
					if err != nil {
						t.Fatal(err)
					}
					apply := func(ctx context.Context) error { return host.Uninstall(ctx, "original") }
					if operation.replace {
						apply = func(ctx context.Context) error { return host.Replace(ctx, "original", replacement, nil) }
					}
					ctx, cancel := context.WithCancelCause(t.Context())
					defer cancel(nil)
					cause := errors.New("admission invalidated during retention read")
					expected := framehost.ErrRecoveryRequired
					invalidate := func() { host.Admission().RequireRecovery(cause) }
					if boundary.cancellation {
						expected = context.Canceled
						invalidate = func() { cancel(cause) }
					}
					owner := &retainedUseOwner{read: func(context.Context, framehost.InstallationID) ([]framehost.Dependency, error) {
						invalidate()
						return nil, nil
					}}
					if err := host.Admission().RegisterUseOwner(owner); err != nil {
						t.Fatal(err)
					}
					commits := storage.commits
					err = apply(ctx)
					if !errors.Is(err, expected) || !errors.Is(err, cause) || framehost.InstallationConclusion(err) != framehost.CommitRejected {
						t.Errorf("invalidated retention read authorized a commit: conclusion=%s, error=%v", framehost.InstallationConclusion(err), err)
					}
					if record, found := host.Installation("original"); !found || record.Release != original.Revision() {
						t.Errorf("invalidated request changed the original installation: found=%t, record=%+v", found, record)
					}
					if storage.commits != commits {
						t.Errorf("invalidated request entered storage: commits=%d, original=%d", storage.commits, commits)
					}
					if err := host.Admission().RecoveryError(); boundary.cancellation && err != nil {
						t.Errorf("cancellation before commit required recovery: %v", err)
					} else if !boundary.cancellation && !errors.Is(err, cause) {
						t.Errorf("original recovery barrier changed: %v", err)
					}
					owner.read = func(context.Context, framehost.InstallationID) ([]framehost.Dependency, error) { return nil, nil }
					if !boundary.cancellation {
						if err := host.Admission().Recover(t.Context(), func() error { return nil }); err != nil {
							t.Fatal(err)
						}
					}
					if err := apply(t.Context()); err != nil {
						t.Errorf("new request could not apply its own transition: %v", err)
					}
				})
			}
		}
	}
}
