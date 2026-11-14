package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestCanceledInstallationPreparationReleasesItsPrivateReceipt(t *testing.T) {
	admission := framehost.NewAdmission()
	config := testConfig(filepath.Join(t.TempDir(), "private-receipts.db"), admission)
	config.MaxSubmissionsPerOwner = 1
	projections := 0
	config.Owners[0].ProjectInstallation = func(context.Context, Executor, framehost.InstallationChange) error {
		projections++
		return nil
	}
	store, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	host, err := framehost.New(t.Context(), framehost.Config{Authority: store})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash(nil)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var canceled atomic.Bool
	installTransactionFault(t, store, transactionFault{afterExec: func(statement string) error {
		if statement == "COMMIT" && canceled.CompareAndSwap(false, true) {
			cancel()
		}
		return nil
	}})
	installation := framehost.InstallationConfig{ID: "example", Plugin: plugin}
	if _, err := host.Install(ctx, installation); !errors.Is(err, context.Canceled) {
		t.Fatalf("original cancellation was lost: %v", err)
	}
	if projections != 0 {
		t.Fatalf("canceled installation ran %d business projections", projections)
	}
	records, err := store.Installations(t.Context())
	if err != nil || len(records) != 0 {
		t.Fatalf("canceled installation retained facts: records=%v error=%v", records, err)
	}
	var receipts int
	if err := store.Read(t.Context(), func(reader Reader) error {
		return reader.QueryRowContext(t.Context(), `SELECT count(*) FROM frame_submissions WHERE owner_id=?`, store.control.id).Scan(&receipts)
	}); err != nil || receipts != 0 {
		t.Fatalf("canceled installation retained its private receipt: count=%d error=%v", receipts, err)
	}
	if admission.RecoveryError() != nil {
		t.Fatalf("known rejection fenced the Host: %v", admission.RecoveryError())
	}
	if _, err := host.Install(t.Context(), installation); err != nil {
		t.Fatalf("canceled installation occupied the private receipt budget: %v", err)
	}
	if projections != 1 {
		t.Fatalf("later installation ran %d business projections, want one", projections)
	}
}
