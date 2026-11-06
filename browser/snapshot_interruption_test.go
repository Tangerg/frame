package browser_test

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

type interruptedSnapshotMetadata string

const (
	snapshotCodecPanic  interruptedSnapshotMetadata = "panic"
	snapshotCodecGoexit interruptedSnapshotMetadata = "goexit"
)

var errSnapshotCodec = errors.New("metadata codec interrupted")

func (m interruptedSnapshotMetadata) MarshalJSON() ([]byte, error) {
	if m == snapshotCodecPanic {
		panic(errSnapshotCodec)
	}
	runtime.Goexit()
	return nil, nil
}

type snapshotMetadataDriver struct {
	point    frame.ContractRef
	metadata interruptedSnapshotMetadata
}

func (snapshotMetadataDriver) Profile() frame.Profile { return "snapshot-metadata" }
func (snapshotMetadataDriver) RealmBound() bool       { return true }
func (d snapshotMetadataDriver) Prepare(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
	return []frame.ExportValue{{Name: "metadata", Contract: d.point, Metadata: d.metadata, Value: "content"}}, nil
}

func TestSnapshotInterruptionRetainsFailureAndReleasesCapacity(t *testing.T) {
	for _, interruption := range []interruptedSnapshotMetadata{snapshotCodecPanic, snapshotCodecGoexit} {
		t.Run(string(interruption), func(t *testing.T) {
			point, err := frame.NewPoint[interruptedSnapshotMetadata, string]("test.snapshot-metadata", "1")
			if err != nil {
				t.Fatal(err)
			}
			e := openConfiguredBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true, func(config *browser.BridgeConfig) {
				config.Points = append(config.Points, point.Ref())
				config.MaxPending = 1
			})
			if err := e.host.Register(point); err != nil {
				t.Fatal(err)
			}
			driver := snapshotMetadataDriver{point.Ref(), interruption}
			if err := e.host.RegisterDriver(driver); err != nil {
				t.Fatal(err)
			}
			plugin, err := frame.Define(frame.Definition{Revision: value.Hash([]byte("snapshot-metadata")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "metadata", Profile: driver.Profile(), Exports: []frame.ExportDeclaration{{Name: "metadata", Contract: point.Ref()}}}}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.host.Install(e.ctx, frame.InstallationConfig{ID: "metadata", Plugin: plugin, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			e.credentials, err = e.bridge.OpenRealm(e.ctx, "open", "0123456789abcdef0123456789abcdef")
			if err != nil {
				t.Fatal(err)
			}
			root := frame.ExportRef{InstallationID: "metadata", Release: plugin.Revision(), UnitID: "metadata", Export: "metadata", Contract: point.Ref()}
			if _, err := e.host.ActivateIn(e.ctx, browser.RealmID(e.credentials.Realm), root); err != nil {
				t.Fatal(err)
			}
			request := snapshotRequest(point.Ref().Name)
			done := make(chan struct{})
			var returned bool
			var panicked any
			go func() {
				defer close(done)
				defer func() { panicked = recover() }()
				_, _ = e.bridge.Snapshot(e.ctx, e.credentials.Token, request)
				returned = true
			}()
			select {
			case <-done:
			case <-e.ctx.Done():
				t.Fatal(e.ctx.Err())
			}
			if returned || (interruption == snapshotCodecPanic && panicked != errSnapshotCodec) || (interruption == snapshotCodecGoexit && panicked != nil) {
				t.Fatalf("codec interruption changed: returned=%v panic=%v", returned, panicked)
			}
			for range 2 {
				replayed, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, request)
				if !errors.Is(err, frame.ErrUnavailable) || replayed.ID != "" {
					t.Fatalf("interrupted capture replayed as success: %+v, %v", replayed, err)
				}
			}
			changed := request
			changed.Points = []string{e.first.Ref().Name}
			if _, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, changed); !errors.Is(err, sqlite.ErrConflict) {
				t.Fatalf("failed operation accepted a different capture: %v", err)
			}
			if _, err := e.store.RealmReceipt(e.ctx, request.OperationID); err != nil {
				t.Fatalf("interruption did not retain its receipt: %v", err)
			}
			fresh := snapshotRequest(e.first.Ref().Name)
			captured, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, fresh)
			if err != nil || captured.ID != fresh.OperationID {
				t.Fatalf("interrupted capture retained lease capacity: %+v, %v", captured, err)
			}
			if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, request.OperationID); err != nil {
				t.Fatal(err)
			}
			if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, fresh.OperationID); err != nil {
				t.Fatal(err)
			}
		})
	}
}
