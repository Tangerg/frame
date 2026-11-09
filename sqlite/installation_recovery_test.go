package sqlite

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestRecoveredInstallationEnableAdvancesOriginalAuthority(t *testing.T) {
	store, _, admission := testStore(t)
	host, err := frame.NewHost(t.Context(), frame.HostConfig{Admission: admission, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = host.Close(ctx)
	})
	var setups atomic.Int64
	plugin, err := frame.Define(frame.Definition{Revision: value.Hash([]byte("original-enable")), Units: []frame.Unit{{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup:       func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { setups.Add(1); return nil, nil },
	}}})
	if err != nil {
		t.Fatal(err)
	}
	const installation frame.InstallationID = "original-enable"
	if _, err := host.Install(t.Context(), frame.InstallationConfig{ID: installation, Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := host.SetEnabled(t.Context(), installation, false); err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int64
	var lostAck, failedReconciliation atomic.Bool
	ackLost := errors.New("test: enable COMMIT acknowledgement was lost")
	readUnavailable := errors.New("test: original settlement temporarily unavailable")
	installTransactionFault(t, store, transactionFault{
		afterExec: func(statement string) error {
			if statement == "COMMIT" && commits.Add(1) == 2 {
				lostAck.Store(true)
				return ackLost
			}
			return nil
		},
		beforeExec: func(statement string) error {
			if statement == "BEGIN IMMEDIATE" && lostAck.Load() && failedReconciliation.CompareAndSwap(false, true) {
				return readUnavailable
			}
			return nil
		},
	})
	enableErr := host.SetEnabled(t.Context(), installation, true)
	if frame.InstallationConclusion(enableErr) != frame.CommitIndeterminate {
		t.Fatalf("expected committed enable with unavailable acknowledgement: %v", enableErr)
	}
	records, err := store.RecoverInstallations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !records[0].Enabled {
		t.Fatalf("enable did not actually commit: %+v", records)
	}
	if err := host.Restore(t.Context(), records, []*frame.Plugin{plugin}); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmRestoredInstallations(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := admission.Recover(t.Context(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	retryErr := host.SetEnabled(t.Context(), installation, true)
	_, activateErr := host.ActivateUnit(t.Context(), "", records[0].Unit("main"))
	restored, _ := host.Installation(installation)
	t.Logf("enable=%v; durable enabled=%t; restored enabled=%t; recovery=%v; retry enable=%v; activate=%v; setup calls=%d", enableErr, records[0].Enabled, restored.Enabled, admission.RecoveryError(), retryErr, activateErr, setups.Load())
	if activateErr != nil {
		t.Error("successful SQLite/Host recovery left the original committed enable permanently fenced")
	}
}
