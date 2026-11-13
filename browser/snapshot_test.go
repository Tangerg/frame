package browser_test

import (
	"bytes"
	"crypto/rand"
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
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
)

func snapshotRequest(points ...string) browser.SnapshotRequest {
	return browser.SnapshotRequest{OperationID: rand.Text(), Points: points}
}

func snapshotBridge(t *testing.T) (*bridgeEnvironment, *browser.Bridge) {
	t.Helper()
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	bridge, err := browser.NewBridge(browser.BridgeConfig{Host: e.host, Store: e.store, Source: testModules{}, Points: []framehost.ContractRef{e.first.Ref()}, MaxPending: 2})
	if err != nil {
		t.Fatal(err)
	}
	return e, bridge
}

func TestSnapshotCapacityCountsLeasesInsteadOfPoints(t *testing.T) {
	e := openConfiguredBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true, func(config *browser.BridgeConfig) { config.MaxPending = 1 })
	credentials, err := e.bridge.OpenRealm(e.ctx, "one", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	request := snapshotRequest(e.first.Ref().Name, e.second.Ref().Name)
	snapshot, err := e.bridge.Snapshot(e.ctx, credentials.Token, request)
	if err != nil || len(snapshot.Points) != 2 {
		t.Fatalf("one lease could not capture two registered Points: %+v %v", snapshot, err)
	}
	if _, err := e.bridge.Snapshot(e.ctx, credentials.Token, snapshotRequest(e.first.Ref().Name)); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("live lease did not occupy its original slot: %v", err)
	}
	if err := e.bridge.CloseSnapshot(e.ctx, credentials.Token, request.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.Snapshot(e.ctx, credentials.Token, snapshotRequest(e.first.Ref().Name, e.second.Ref().Name)); err != nil {
		t.Fatalf("released lease did not return capacity: %v", err)
	}
}

type lostSnapshotResponse struct {
	header http.Header
	body   []byte
}

func (w *lostSnapshotResponse) Header() http.Header { return w.header }
func (w *lostSnapshotResponse) WriteHeader(int)     {}
func (w *lostSnapshotResponse) Write(body []byte) (int, error) {
	w.body = append(w.body, body...)
	return 0, errors.New("response lost before reaching the caller")
}

func TestLostSnapshotResponseReplaysOriginalLeaseWithoutTakingAnotherRealmSlot(t *testing.T) {
	e, bridge := snapshotBridge(t)
	first, err := bridge.OpenRealm(e.ctx, "first", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	other, err := bridge.OpenRealm(e.ctx, "other", "abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	input := snapshotRequest(e.first.Ref().Name)
	body, err := json.Marshal(struct {
		Method string                  `json:"method"`
		Input  browser.SnapshotRequest `json:"input"`
	}{"snapshot", input})
	if err != nil {
		t.Fatal(err)
	}
	var original browser.Snapshot
	for range 3 {
		request := httptest.NewRequest(http.MethodPost, "/control", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+first.Token)
		lost := &lostSnapshotResponse{header: make(http.Header)}
		bridge.Handler().ServeHTTP(lost, request)
		if err := json.Unmarshal(lost.body, &original); err != nil || original.ID != input.OperationID {
			t.Fatalf("lost response = %s, %v", lost.body, err)
		}
	}
	replayed, err := bridge.Snapshot(e.ctx, first.Token, input)
	if err != nil || !reflect.DeepEqual(replayed, original) {
		t.Fatalf("original capture was replaced: %+v, %v", replayed, err)
	}
	if _, err := bridge.Snapshot(e.ctx, other.Token, snapshotRequest(e.first.Ref().Name)); err != nil {
		t.Fatalf("one lost capture consumed another Realm's capacity: %v", err)
	}
	if _, err := bridge.Snapshot(e.ctx, first.Token, snapshotRequest(e.first.Ref().Name)); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("two live leases did not enforce the configured limit: %v", err)
	}
	if err := bridge.CloseSnapshot(e.ctx, first.Token, input.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Snapshot(e.ctx, first.Token, input); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("closed operation recaptured a lease: %v", err)
	}
}

func TestSnapshotOperationPreservesProvenanceAndRejectsChangedRequests(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	e.start("snapshot-owner")
	input := snapshotRequest(e.first.Ref().Name, e.second.Ref().Name)
	original, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, input)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, input)
	if err != nil || !reflect.DeepEqual(replayed, original) {
		t.Fatalf("retry changed original candidate provenance: %+v, %v", replayed, err)
	}
	original.Points[0].Candidates[0].Token = "caller-mutated"
	again, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, input)
	if err != nil || !reflect.DeepEqual(again, replayed) {
		t.Fatalf("returned projection changed the owned lease: %+v, %v", again, err)
	}
	changed := input
	changed.Points = []string{e.second.Ref().Name}
	if _, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, changed); !errors.Is(err, sqlite.ErrConflict) {
		t.Fatalf("changed capture reused original identity: %v", err)
	}
	other, err := e.bridge.OpenRealm(e.ctx, "other", "abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	for _, closeOriginal := range []bool{false, true} {
		if closeOriginal {
			if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, input.OperationID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := e.bridge.Snapshot(e.ctx, other.Token, input); !errors.Is(err, framehost.ErrFenced) {
			t.Fatalf("another Realm observed original capture: %v", err)
		}
		if err := e.bridge.CloseSnapshot(e.ctx, other.Token, input.OperationID); !errors.Is(err, framehost.ErrFenced) {
			t.Fatalf("another Realm closed original capture: %v", err)
		}
	}
}

func TestSnapshotConcurrentRetriesShareOneCapture(t *testing.T) {
	e, bridge := snapshotBridge(t)
	credentials, err := bridge.OpenRealm(e.ctx, "capture", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	input := snapshotRequest(e.first.Ref().Name)
	start := make(chan struct{})
	var calls sync.WaitGroup
	for range 8 {
		calls.Go(func() {
			<-start
			capture, err := bridge.Snapshot(e.ctx, credentials.Token, input)
			if err != nil || capture.ID != input.OperationID {
				t.Errorf("retry = %+v, %v", capture, err)
			}
		})
	}
	close(start)
	calls.Wait()
	if _, err := bridge.Snapshot(e.ctx, credentials.Token, snapshotRequest(e.first.Ref().Name)); err != nil {
		t.Fatalf("concurrent retries consumed another lease: %v", err)
	}
}

func TestSnapshotCloseBeforeCapturePreventsLateCreation(t *testing.T) {
	e, bridge := snapshotBridge(t)
	credentials, err := bridge.OpenRealm(e.ctx, "capture", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		input := snapshotRequest(e.first.Ref().Name)
		if err := bridge.CloseSnapshot(e.ctx, credentials.Token, input.OperationID); err != nil {
			t.Fatal(err)
		}
		if _, err := bridge.Snapshot(e.ctx, credentials.Token, input); !errors.Is(err, frame.ErrClosed) {
			t.Fatalf("delayed capture revived closed identity: %v", err)
		}
	}
	if _, err := bridge.Snapshot(e.ctx, credentials.Token, snapshotRequest(e.first.Ref().Name)); err != nil {
		t.Fatalf("completed closes consumed live capacity: %v", err)
	}
}

func TestBridgeSealingRealmReleasesUnconsumedSnapshots(t *testing.T) {
	e, bridge := snapshotBridge(t)
	original, err := bridge.OpenRealm(e.ctx, "original", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		snapshot, err := bridge.Snapshot(e.ctx, original.Token, snapshotRequest(e.first.Ref().Name))
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
	if _, err := bridge.Snapshot(e.ctx, next.Token, snapshotRequest(e.first.Ref().Name)); err != nil {
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
				_, err := bridge.Snapshot(e.ctx, original.Token, snapshotRequest(e.first.Ref().Name))
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
			if err != nil && !errors.Is(err, framehost.ErrFenced) && !errors.Is(err, frame.ErrCapacity) {
				t.Fatalf("concurrent snapshot failed outside its admission contract: %v", err)
			}
		}
	}
	next, err := bridge.OpenRealm(e.ctx, "after-race", "abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Snapshot(e.ctx, next.Token, snapshotRequest(e.first.Ref().Name)); err != nil {
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
	snapshot, err := bridge.Snapshot(e.ctx, first.Token, snapshotRequest(e.first.Ref().Name))
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.CloseSnapshot(e.ctx, other.Token, snapshot.ID); !errors.Is(err, framehost.ErrFenced) {
		t.Fatalf("another Realm closed snapshot: %v", err)
	}
	for range 8 {
		if err := bridge.CloseSnapshot(e.ctx, first.Token, snapshot.ID); err != nil {
			t.Fatal(err)
		}
		snapshot, err = bridge.Snapshot(e.ctx, first.Token, snapshotRequest(e.first.Ref().Name))
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
	if _, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, browser.SnapshotRequest{OperationID: request.SnapshotID, Points: []string{e.first.Ref().Name, e.second.Ref().Name}}); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("consumed capture was recreated: %v", err)
	}
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
	if _, err := e.bridge.Acquire(e.ctx, e.credentials.Token, request); !errors.Is(err, framehost.ErrStaleBinding) {
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
		if _, err := e.bridge.Acquire(e.ctx, e.credentials.Token, request); !errors.Is(err, framehost.ErrStaleBinding) {
			t.Fatalf("original refusal was not preserved: %v", err)
		}
	}
	request.OperationID = "different-acquire"
	request.Selections[0].Candidates = nil
	if _, err := e.bridge.Acquire(e.ctx, e.credentials.Token, request); !errors.Is(err, framehost.ErrStaleBinding) {
		t.Fatalf("consumed snapshot admitted another operation: %v", err)
	}
	if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, request.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, browser.SnapshotRequest{OperationID: request.SnapshotID, Points: []string{e.first.Ref().Name, e.second.Ref().Name}}); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("refused acquisition recreated its consumed capture: %v", err)
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
		if errors.Is(err, framehost.ErrStaleBinding) {
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
		if _, err := bridge.Snapshot(e.ctx, original.Token, snapshotRequest(e.first.Ref().Name)); err != nil {
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
	if _, err := bridge.Snapshot(e.ctx, next.Token, snapshotRequest(e.first.Ref().Name)); err != nil {
		t.Fatalf("cleanup-only Realm retained selection capacity: %v", err)
	}
}

func TestControlHTTPCloseSnapshotReleasesOnlyTheOriginalLease(t *testing.T) {
	e, bridge := snapshotBridge(t)
	original, err := bridge.OpenRealm(e.ctx, "original", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := bridge.Snapshot(e.ctx, original.Token, snapshotRequest(e.first.Ref().Name))
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
		if _, err := bridge.Snapshot(e.ctx, original.Token, snapshotRequest(e.first.Ref().Name)); err != nil {
			t.Fatalf("HTTP close retained snapshot capacity: %v", err)
		}
	}
}

func TestBridgeRealmSealingRetainsAcquiredUseUntilOriginalProof(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("snapshot-seal-proof")
	_, grant := e.acquire(plan, "original-acquire")
	pending, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, snapshotRequest(e.first.Ref().Name))
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
