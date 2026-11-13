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

type authorityProbeDriver struct {
	framehost.Driver
	called *bool
}

func (d authorityProbeDriver) Profile() framehost.Profile {
	*d.called = true
	return "test.authority-admission"
}

func TestHostRejectsMismatchedDurableAuthorityBeforeDriverCallbacks(t *testing.T) {
	admission := framehost.NewAdmission()
	open := func(name string) sqlite.Store {
		store, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), name), Admission: admission})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
		return store
	}
	first, second := open("first.db"), open("second.db")
	for _, test := range []struct {
		name   string
		config framehost.Config
	}{
		{"zero installation authority", framehost.Config{Admission: admission, Installations: sqlite.Store{}}},
		{"zero protection authority", framehost.Config{Admission: admission, ExecutionProtection: sqlite.Store{}}},
		{"installation admission", framehost.Config{Admission: framehost.NewAdmission(), Installations: first}},
		{"protection admission", framehost.Config{Admission: framehost.NewAdmission(), ExecutionProtection: first}},
		{"installation epoch", framehost.Config{Admission: admission, Epoch: first.Epoch() + 1, Installations: first}},
		{"protection epoch", framehost.Config{Admission: admission, Epoch: first.Epoch() + 1, ExecutionProtection: first}},
		{"split authorities", framehost.Config{Admission: admission, Epoch: first.Epoch(), Installations: first, ExecutionProtection: second}},
		{"implicit admission", framehost.Config{Installations: first, ExecutionProtection: first}},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			test.config.Drivers = []framehost.Driver{authorityProbeDriver{called: &called}}
			host, err := framehost.New(t.Context(), test.config)
			if host.Valid() {
				_, _ = host.Close(t.Context())
			}
			if host.Valid() || !errors.Is(err, frame.ErrInvalid) {
				t.Errorf("mismatched durable authority admitted: host=%t, error=%v", host.Valid(), err)
			}
			if called {
				t.Error("driver callback ran before authority validation")
			}
		})
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
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("test")), Units: []framehost.Unit{{Declaration: framehost.UnitDeclaration{ID: "sample", Profile: framehost.NativeGo}, Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) { return nil, nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []framehost.CommitState{framehost.CommitRejected, framehost.CommitIndeterminate, framehost.CommitSucceeded} {
		t.Run(string(state), func(t *testing.T) {
			host, err := framehost.New(t.Context(), framehost.Config{Installations: resultStore{state, errors.New("confirmation unavailable")}})
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
