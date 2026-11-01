package browser_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
)

func snapshotBridge(t *testing.T) (*bridgeEnvironment, *browser.Bridge) {
	t.Helper()
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	bridge, err := browser.NewBridge(browser.BridgeConfig{Host: e.host, Store: e.store, Source: testModules{}, Points: []frame.ContractRef{e.first.Ref()}, MaxPending: 2})
	if err != nil {
		t.Fatal(err)
	}
	return e, bridge
}

func TestBridgeSealingRealmReleasesUnconsumedSnapshots(t *testing.T) {
	e, bridge := snapshotBridge(t)
	original, err := bridge.OpenRealm(e.ctx, "original", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		snapshot, err := bridge.Snapshot(e.ctx, original.Token, []string{e.first.Ref().Name})
		if err != nil || len(snapshot.Points) != 1 || len(snapshot.Points[0].Candidates) != 0 {
			t.Fatalf("empty selection snapshot = %+v, %v", snapshot, err)
		}
	}
	if err := bridge.SealRealm(e.ctx, original.Token); err != nil {
		t.Fatal(err)
	}
	next, err := bridge.OpenRealm(e.ctx, "next", "abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Snapshot(e.ctx, next.Token, []string{e.first.Ref().Name}); err != nil {
		t.Fatalf("sealed Realm retained snapshot capacity: %v", err)
	}
}

func TestBridgeConcurrentSnapshotCannotRetainASealedRealmLease(t *testing.T) {
	e, bridge := snapshotBridge(t)
	for iteration := range 16 {
		original, err := bridge.OpenRealm(e.ctx, fmt.Sprintf("realm-%d", iteration), fmt.Sprintf("%032d", iteration))
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 8)
		var calls sync.WaitGroup
		for range 8 {
			calls.Go(func() {
				<-start
				_, err := bridge.Snapshot(e.ctx, original.Token, []string{e.first.Ref().Name})
				results <- err
			})
		}
		close(start)
		if err := bridge.SealRealm(e.ctx, original.Token); err != nil {
			t.Fatal(err)
		}
		calls.Wait()
		close(results)
		for err := range results {
			if err != nil && !errors.Is(err, frame.ErrFenced) && !errors.Is(err, frame.ErrCapacity) {
				t.Fatalf("concurrent snapshot failed outside its admission contract: %v", err)
			}
		}
	}
	next, err := bridge.OpenRealm(e.ctx, "after-race", "abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Snapshot(e.ctx, next.Token, []string{e.first.Ref().Name}); err != nil {
		t.Fatalf("snapshot inserted into a sealed Realm: %v", err)
	}
}

func TestBridgeSnapshotCloseIsIdempotentAndRealmBound(t *testing.T) {
	e, bridge := snapshotBridge(t)
	first, err := bridge.OpenRealm(e.ctx, "first", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	other, err := bridge.OpenRealm(e.ctx, "other", "abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := bridge.Snapshot(e.ctx, first.Token, []string{e.first.Ref().Name})
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.CloseSnapshot(e.ctx, other.Token, snapshot.ID); !errors.Is(err, frame.ErrFenced) {
		t.Fatalf("another Realm closed snapshot: %v", err)
	}
	for range 8 {
		if err := bridge.CloseSnapshot(e.ctx, first.Token, snapshot.ID); err != nil {
			t.Fatal(err)
		}
		snapshot, err = bridge.Snapshot(e.ctx, first.Token, []string{e.first.Ref().Name})
		if err != nil {
			t.Fatalf("abandoned empty selection retained capacity: %v", err)
		}
	}
	if err := bridge.CloseSnapshot(e.ctx, first.Token, snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if err := bridge.CloseSnapshot(e.ctx, first.Token, snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if err := bridge.SealRealm(e.ctx, first.Token); err != nil {
		t.Fatal(err)
	}
	if err := bridge.CloseSnapshot(e.ctx, first.Token, snapshot.ID); err != nil {
		t.Fatalf("cleanup-only Realm cannot close original snapshot: %v", err)
	}
}

func TestBridgeSnapshotClosePreservesAcquiredUseAndReplay(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("snapshot-use")
	request, grant := e.acquire(plan, "original-acquire")
	for range 2 {
		if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, request.SnapshotID); err != nil {
			t.Fatal(err)
		}
	}
	replayed, err := e.bridge.Acquire(e.ctx, e.credentials.Token, request)
	if err != nil || !reflect.DeepEqual(replayed, grant) {
		t.Fatalf("snapshot close replaced original acquisition: %+v %v", replayed, err)
	}
	if err := e.bridge.ValidateUse(e.ctx, e.credentials.Token, grant); err != nil {
		t.Fatalf("snapshot close revoked accepted Use: %v", err)
	}
	proof := useProof(plan, grant)
	for range 2 {
		ack, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof)
		if err != nil || !ack.Recorded {
			t.Fatalf("original Use release proof: %+v %v", ack, err)
		}
	}
	uses, err := e.store.Uses(e.ctx)
	if err != nil || len(uses) != 1 || !uses[0].Released {
		t.Fatalf("original Use protection was not released: %+v %v", uses, err)
	}
}

func TestBridgeSnapshotCloseWinsBeforeAcquisition(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("snapshot-closed")
	request := e.selectionRequest(plan, "closed-acquire")
	if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, request.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.Acquire(e.ctx, e.credentials.Token, request); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("closed selection created a Use: %v", err)
	}
	uses, err := e.store.Uses(e.ctx)
	if err != nil || len(uses) != 0 {
		t.Fatalf("closed selection created durable approval: %+v %v", uses, err)
	}
}

func TestBridgeFailedAcquisitionConsumesSnapshotAndReplaysOriginalRefusal(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("snapshot-refused")
	request := e.selectionRequest(plan, "refused-acquire")
	request.Selections[0].Candidates = []string{"not-captured"}
	for range 2 {
		if _, err := e.bridge.Acquire(e.ctx, e.credentials.Token, request); !errors.Is(err, frame.ErrStaleBinding) {
			t.Fatalf("original refusal was not preserved: %v", err)
		}
	}
	request.OperationID = "different-acquire"
	request.Selections[0].Candidates = nil
	if _, err := e.bridge.Acquire(e.ctx, e.credentials.Token, request); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("consumed snapshot admitted another operation: %v", err)
	}
	if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, request.SnapshotID); err != nil {
		t.Fatal(err)
	}
}

func TestBridgeSnapshotCloseAndAcquireHaveOneAdmissionWinner(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("snapshot-race")
	for iteration := range 8 {
		request := e.selectionRequest(plan, fmt.Sprintf("race-acquire-%d", iteration))
		start := make(chan struct{})
		closed := make(chan error, 1)
		go func() { <-start; closed <- e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, request.SnapshotID) }()
		close(start)
		grant, err := e.bridge.Acquire(e.ctx, e.credentials.Token, request)
		if closeErr := <-closed; closeErr != nil {
			t.Fatal(closeErr)
		}
		if errors.Is(err, frame.ErrStaleBinding) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		replayed, err := e.bridge.Acquire(e.ctx, e.credentials.Token, request)
		if err != nil || !reflect.DeepEqual(replayed, grant) {
			t.Fatal("close raced the accepted operation's receipt")
		}
		if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, useProof(plan, grant)); err != nil {
			t.Fatal(err)
		}
	}
	uses, err := e.store.Uses(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, use := range uses {
		if !use.Released {
			t.Fatal("snapshot race lost its original release proof")
		}
	}
}

func TestBridgeCleanupOnlyReconnectReleasesSnapshots(t *testing.T) {
	e, bridge := snapshotBridge(t)
	original, err := bridge.OpenRealm(e.ctx, "original", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := bridge.Snapshot(e.ctx, original.Token, []string{e.first.Ref().Name}); err != nil {
			t.Fatal(err)
		}
	}
	authority := original.Authority
	authority.Epoch = "different"
	result, err := bridge.Reconnect(e.ctx, original.Token, browser.ReconnectRequest{Authority: authority, Realm: original.Realm})
	if err != nil || result.Mode != browser.CleanupOnly {
		t.Fatalf("cleanup-only reconnect = %+v, %v", result, err)
	}
	next, err := bridge.OpenRealm(e.ctx, "next", "abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Snapshot(e.ctx, next.Token, []string{e.first.Ref().Name}); err != nil {
		t.Fatalf("cleanup-only Realm retained selection capacity: %v", err)
	}
}

func TestControlHTTPCloseSnapshotReleasesOnlyTheOriginalLease(t *testing.T) {
	e, bridge := snapshotBridge(t)
	original, err := bridge.OpenRealm(e.ctx, "original", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := bridge.Snapshot(e.ctx, original.Token, []string{e.first.Ref().Name})
	if err != nil {
		t.Fatal(err)
	}
	call := func(input any) *httptest.ResponseRecorder {
		encoded, err := json.Marshal(map[string]any{"method": "closeSnapshot", "input": input})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/control", bytes.NewReader(encoded))
		request.Header.Set("Authorization", "Bearer "+original.Token)
		response := httptest.NewRecorder()
		bridge.Handler().ServeHTTP(response, request)
		return response
	}
	for range 2 {
		if response := call(snapshot.ID); response.Code != http.StatusOK || response.Body.String() != "null" {
			t.Fatalf("close original snapshot = %d %s", response.Code, response.Body.String())
		}
	}
	if response := call(map[string]string{"id": snapshot.ID}); response.Code != http.StatusBadRequest {
		t.Fatalf("unpublished close shape accepted: %d", response.Code)
	}
	for range 2 {
		if _, err := bridge.Snapshot(e.ctx, original.Token, []string{e.first.Ref().Name}); err != nil {
			t.Fatalf("HTTP close retained snapshot capacity: %v", err)
		}
	}
}

func TestBridgeRealmSealingRetainsAcquiredUseUntilOriginalProof(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("snapshot-seal-proof")
	_, grant := e.acquire(plan, "original-acquire")
	pending, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, []string{e.first.Ref().Name})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.bridge.SealRealm(e.ctx, e.credentials.Token); err != nil {
		t.Fatal(err)
	}
	if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, pending.ID); err != nil {
		t.Fatal(err)
	}
	uses, err := e.store.Uses(e.ctx)
	if err != nil || len(uses) != 1 || uses[0].Released {
		t.Fatalf("Realm sealing replaced the original release proof: %+v %v", uses, err)
	}
	proof := useProof(plan, grant)
	for range 2 {
		ack, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof)
		if err != nil || !ack.Recorded {
			t.Fatalf("sealed Realm lost original Use proof: %+v %v", ack, err)
		}
	}
	uses, err = e.store.Uses(e.ctx)
	if err != nil || len(uses) != 1 || !uses[0].Released {
		t.Fatalf("original proof did not release the acquired Use: %+v %v", uses, err)
	}
}
