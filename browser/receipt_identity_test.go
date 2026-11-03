package browser_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/sqlite"
)

func TestSharedStoreReservesSnapshotForItsOriginalRealm(t *testing.T) {
	e := openConfiguredBridge(t, filepath.Join(t.TempDir(), "receipt-identity.db"), true, func(config *browser.BridgeConfig) {
		config.MaxPending = 1
		config.Points = config.Points[:1]
	})
	other, err := browser.NewBridge(browser.BridgeConfig{Host: e.host, Store: e.store, Source: testModules{}, Points: []frame.ContractRef{e.first.Ref()}, MaxPending: 1})
	if err != nil {
		t.Fatal(err)
	}
	bridges := []*browser.Bridge{e.bridge, other}
	credentials := make([]browser.Credentials, len(bridges))
	for i, bridge := range bridges {
		credentials[i], err = bridge.OpenRealm(e.ctx, fmt.Sprintf("open-realm-%d", i), "0123456789abcdef0123456789abcdef")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := bridge.SealRealm(e.ctx, credentials[i].Token); err != nil {
				t.Error(err)
			}
		})
	}
	type result struct {
		bridge   int
		snapshot browser.Snapshot
		err      error
	}
	for iteration := range 8 {
		request := browser.SnapshotRequest{OperationID: fmt.Sprintf("shared-capture-%d", iteration), Points: []string{e.first.Ref().Name}}
		start := make(chan struct{})
		completed := make(chan result, len(bridges))
		for i, bridge := range bridges {
			go func() {
				<-start
				snapshot, err := bridge.Snapshot(e.ctx, credentials[i].Token, request)
				completed <- result{bridge: i, snapshot: snapshot, err: err}
			}()
		}
		close(start)
		var accepted []result
		for range bridges {
			select {
			case outcome := <-completed:
				if outcome.err == nil {
					accepted = append(accepted, outcome)
					continue
				}
				if errors.Is(outcome.err, sqlite.ErrIndeterminate) || !errors.Is(outcome.err, sqlite.ErrConflict) && !errors.Is(outcome.err, frame.ErrFenced) {
					t.Fatalf("another Realm was not refused before capture: %v", outcome.err)
				}
			case <-e.ctx.Done():
				t.Fatal(e.ctx.Err())
			}
		}
		if len(accepted) != 1 || accepted[0].snapshot.ID != request.OperationID {
			t.Fatalf("one operation acquired %d snapshot owners: %+v", len(accepted), accepted)
		}
		winner := accepted[0]
		if err := bridges[winner.bridge].CloseSnapshot(e.ctx, credentials[winner.bridge].Token, winner.snapshot.ID); err != nil {
			t.Fatalf("original snapshot could not close: %v", err)
		}
		for i, bridge := range bridges {
			fresh := snapshotRequest(e.first.Ref().Name)
			snapshot, err := bridge.Snapshot(e.ctx, credentials[i].Token, fresh)
			if err != nil || snapshot.ID != fresh.OperationID {
				t.Fatalf("bridge %d retained a stranded operation or lease: %+v, %v", i, snapshot, err)
			}
			if err := bridge.CloseSnapshot(e.ctx, credentials[i].Token, snapshot.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
}
