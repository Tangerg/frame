package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func TestUseReleasePersistsOriginalCloseWithoutDiagnostics(t *testing.T) {
	const requestBytes = 4096
	admission := framehost.NewAdmission()
	store, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "state.sqlite"), Admission: admission, MaxRequestBytes: requestBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	host, err := framehost.New(t.Context(), framehost.Config{Authority: store})
	if err != nil {
		t.Fatal(err)
	}
	owner := frame.NewLifetime(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if outcome, err := owner.Close(ctx); !outcome.Released {
			t.Errorf("application did not release: %v", err)
		}
		if outcome, err := host.Close(ctx); !outcome.Released {
			t.Errorf("Host did not release: %v", err)
		}
	})
	contract, err := framehost.NewContract[string]("value", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash(nil), Units: []framehost.Unit{{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}},
		Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Provide("value", contract, "value")}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(t.Context(), framehost.InstallationConfig{ID: "provider", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	const realm framehost.RealmID = "application"
	root := framehost.ExportRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main", Export: "value", Contract: contract.Ref()}
	if _, err := host.ActivateIn(t.Context(), realm, root); err != nil {
		t.Fatal(err)
	}
	plan, err := host.PlanWith(t.Context(), realm, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := admission.OpenScope(t.Context(), "scope")
	if err != nil {
		t.Fatal(err)
	}
	use, err := host.Acquire(t.Context(), plan, framehost.UseConfig{Owner: owner, Scope: scope, Realm: realm, ExecutionID: "execution"})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_, err := use.Lifetime().Own(frame.Resource{Name: strings.Repeat("name", 1<<19), Close: func(context.Context) (frame.ReleaseState, error) {
			return frame.ResourceReleased, errors.New(strings.Repeat("diagnostic", 1<<10))
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	outcome, err := use.Lifetime().Close(t.Context())
	if !outcome.Released || !errors.Is(err, frame.ErrCloseFailed) {
		t.Fatalf("local release did not retain its diagnostics: released=%t error=%v", outcome.Released, err)
	}
	encoded, err := json.Marshal(outcome)
	if err != nil || len(encoded) <= requestBytes {
		t.Fatalf("diagnostic fixture did not exceed request budget: bytes=%d error=%v", len(encoded), err)
	}
	retired, err := use.WaitRetired(t.Context())
	if !retired.Released || errors.Is(err, frame.ErrCapacity) || admission.RecoveryError() != nil {
		t.Fatalf("diagnostics blocked durable retirement: released=%t error=%v recovery=%v", retired.Released, err, admission.RecoveryError())
	}
	record, err := store.Use(t.Context(), use.ID())
	if err != nil || !record.Released || record.Proof == nil || record.Proof.Host == nil || record.Proof.Host.CloseID != outcome.ID {
		t.Fatalf("durable release lost its original Close: %+v %v", record, err)
	}
	if err := store.SettleUse(t.Context(), use.ID(), *record.Proof.Host); err != nil {
		t.Fatalf("original release did not replay: %v", err)
	}
	if err := store.SettleUse(t.Context(), use.ID(), frame.ReleaseProof{CloseID: outcome.ID + 1}); !errors.Is(err, sqlite.ErrConflict) {
		t.Fatalf("another Close replaced original release: %v", err)
	}
}
