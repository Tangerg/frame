package host_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

// durableDouble completes a test's persistence double into a Durable. The
// half a test does not exercise accepts every request.
type durableDouble struct {
	framehost.InstallationStore
	framehost.ExecutionProtection
	admission framehost.Admission
}

func (d durableDouble) Admission() framehost.Admission { return d.admission }
func (durableDouble) Epoch() uint64                    { return 1 }

func installationAuthority(store framehost.InstallationStore) framehost.Durable {
	return durableDouble{InstallationStore: store, ExecutionProtection: acceptedProtection{}, admission: framehost.NewAdmission()}
}

func protectionAuthority(protection framehost.ExecutionProtection) framehost.Durable {
	return durableDouble{InstallationStore: acceptedInstallations{}, ExecutionProtection: protection, admission: framehost.NewAdmission()}
}

type acceptedProtection struct{}

func (acceptedProtection) ApproveUse(context.Context, framehost.UseApproval) error { return nil }
func (acceptedProtection) SettleUse(context.Context, string, frame.ReleaseProof) error {
	return nil
}

type acceptedInstallations struct{}

func (acceptedInstallations) CommitInstallation(context.Context, framehost.InstallationChange) (framehost.CommitState, error) {
	return framehost.CommitSucceeded, nil
}
func (acceptedInstallations) ConfirmInstallation(context.Context, framehost.InstallationID) error {
	return nil
}
func (acceptedInstallations) Installations(context.Context) ([]framehost.InstallationRecord, error) {
	return nil, nil
}

type authorityProbeDriver struct {
	framehost.Driver
	called *bool
}

func (d authorityProbeDriver) Profile() framehost.Profile {
	*d.called = true
	return "test.authority-admission"
}

func TestHostRejectsUnissuedAuthorityBeforeDriverCallbacks(t *testing.T) {
	for _, test := range []struct {
		name      string
		authority framehost.Authority
	}{
		{"unissued admission", framehost.Admission{}},
		{"unissued store", sqlite.Store{}},
		{"typed nil durable", (*durableDouble)(nil)},
		{"durable without epoch", zeroEpochDurable{protectionAuthority(acceptedProtection{}).(durableDouble)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			host, err := framehost.New(t.Context(), framehost.Config{Authority: test.authority, Drivers: []framehost.Driver{authorityProbeDriver{called: &called}}})
			if host.Valid() {
				_, _ = host.Close(t.Context())
			}
			if host.Valid() || !errors.Is(err, frame.ErrInvalid) {
				t.Errorf("unissued authority admitted: host=%t, error=%v", host.Valid(), err)
			}
			if called {
				t.Error("driver callback ran before authority validation")
			}
		})
	}
}

type zeroEpochDurable struct{ durableDouble }

func (zeroEpochDurable) Epoch() uint64 { return 0 }

func TestHostDerivesAdmissionAndEpochFromItsDurableAuthority(t *testing.T) {
	admission := framehost.NewAdmission()
	store, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "host.db"), Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	host := testHost(t, framehost.Config{Authority: store})
	if host.Admission() != admission || host.Epoch() != store.Epoch() || host.Durable() != framehost.Durable(store) {
		t.Fatalf("host kept another authority: epoch=%d, store epoch=%d", host.Epoch(), store.Epoch())
	}
	volatile := testHost(t, framehost.Config{Authority: admission})
	if volatile.Admission() != admission || volatile.Durable() != nil || volatile.Epoch() != 1 {
		t.Fatalf("volatile host gained durable authority: epoch=%d", volatile.Epoch())
	}
}

type resultStore struct {
	state   framehost.CommitState
	confirm error
}

func (s resultStore) CommitInstallation(context.Context, framehost.InstallationChange) (framehost.CommitState, error) {
	return s.state, nil
}
func (s resultStore) ConfirmInstallation(context.Context, framehost.InstallationID) error {
	return s.confirm
}
func (s resultStore) Installations(context.Context) ([]framehost.InstallationRecord, error) {
	return nil, nil
}

func TestInstallationReportsTheOriginalCommit(t *testing.T) {
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("test")), Units: []framehost.Unit{{Declaration: framehost.UnitDeclaration{ID: "sample", Profile: framehost.NativeGo}, Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) { return nil, nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []framehost.CommitState{framehost.CommitRejected, framehost.CommitIndeterminate, framehost.CommitSucceeded} {
		t.Run(string(state), func(t *testing.T) {
			host, err := framehost.New(t.Context(), framehost.Config{Authority: installationAuthority(resultStore{state, errors.New("confirmation unavailable")})})
			if err != nil {
				t.Fatal(err)
			}
			record, err := host.Install(t.Context(), framehost.InstallationConfig{ID: "sample", Plugin: plugin})
			if err == nil || framehost.InstallationConclusion(err) != state {
				t.Fatalf("state %s: %v", state, err)
			}
			if (record.ID != "") != (state == framehost.CommitSucceeded) {
				t.Fatal("only committed records may be returned")
			}
		})
	}
}
