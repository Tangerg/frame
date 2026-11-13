package browser_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func TestFullRealmHistoryRefusesNewEffectsAndPreservesOriginalCleanup(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "history.db"), true)
	plan := e.start("publisher")
	acquisition, grant := e.acquire(plan, "original-use")
	publication := browser.PublicationRequest{
		OperationID: "dynamic", Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution,
		Object: browser.ObjectRef{ObjectID: "dynamic-object", Export: browser.LocalExport{
			InstanceID: plan.Units[0].Instance.ID, Name: "dynamic", Contract: e.first.Ref(),
		}},
		MetadataJSON: `{"label":"dynamic"}`,
	}
	published, err := e.bridge.Publish(e.ctx, e.credentials.Token, publication)
	if err != nil || published.Decision != browser.Accepted {
		t.Fatalf("publish before capacity: %+v, %v", published, err)
	}
	blocked := e.selectionRequest(plan, "unadmitted-use")
	const defaultHistoryBytes = 256 << 20
	const fillerID = "reserved-history"
	fillerDigest := value.Hash([]byte(fillerID))
	var used int64
	if err := e.store.Read(e.ctx, func(reader sqlite.Reader) error {
		return reader.QueryRowContext(e.ctx, `SELECT coalesce((SELECT sum(history_bytes) FROM frame_realms),0)+coalesce((SELECT sum(history_bytes) FROM frame_realm_receipts),0)`).Scan(&used)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ReserveRealmReceipt(e.ctx, fillerID, fillerDigest, defaultHistoryBytes-used-int64(len(fillerID)+len(fillerDigest.String()))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, snapshotRequest(e.first.Ref().Name)); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("new capture escaped history capacity: %v", err)
	}
	if _, err := e.bridge.Acquire(e.ctx, e.credentials.Token, blocked); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("new Use escaped history capacity: %v", err)
	}
	replay := browser.SnapshotRequest{OperationID: blocked.SnapshotID, Points: []string{e.first.Ref().Name, e.second.Ref().Name}}
	if _, err := e.bridge.Snapshot(e.ctx, e.credentials.Token, replay); err != nil {
		t.Fatalf("refused admission consumed the original selection: %v", err)
	}
	replayed, err := e.bridge.Acquire(e.ctx, e.credentials.Token, acquisition)
	if err != nil || !reflect.DeepEqual(replayed, grant) {
		t.Fatalf("full history prevented original admission observation: %+v, %v", replayed, err)
	}
	publication.OperationID = "unadmitted-publication"
	if _, err := e.bridge.Publish(e.ctx, e.credentials.Token, publication); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("new publication escaped history capacity: %v", err)
	}
	if _, err := e.host.Publication(plan.Units[0].Instance.ID, publication.OperationID); !errors.Is(err, framehost.ErrUnavailable) {
		t.Fatalf("capacity rejection followed a publication effect: %v", err)
	}
	if _, err := e.bridge.OpenRealm(e.ctx, "another-realm", "abcdef0123456789abcdef0123456789"); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("new Realm escaped history capacity: %v", err)
	}
	resumed, err := e.bridge.OpenRealm(e.ctx, "open-test", "0123456789abcdef0123456789abcdef")
	if err != nil || resumed != e.credentials {
		t.Fatalf("original Realm could not resume at capacity: %+v, %v", resumed, err)
	}
	if err := e.bridge.CloseSnapshot(e.ctx, e.credentials.Token, blocked.SnapshotID); err != nil {
		t.Fatalf("accepted snapshot could not close at capacity: %v", err)
	}
	withdrawal := browser.Withdrawal{Authority: plan.Authority, PublicationID: published.Publication.ID}
	if result, err := e.bridge.Withdraw(e.ctx, e.credentials.Token, withdrawal); err != nil || result.Decision != browser.Accepted {
		t.Fatalf("accepted publication could not withdraw at capacity: %+v, %v", result, err)
	}
	proof := executionProof(plan)
	proof.ScopeKind, proof.PublicationID = browser.PublicationScope, published.Publication.ID
	proof.CloseID, proof.ProofID = "publication-close", "publication-proof"
	if result, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof); err != nil || !result.Recorded {
		t.Fatalf("accepted publication could not release at capacity: %+v, %v", result, err)
	}
	if result, err := e.bridge.Prove(e.ctx, e.credentials.Token, useProof(plan, grant)); err != nil || !result.Recorded {
		t.Fatalf("accepted Use could not release at capacity: %+v, %v", result, err)
	}
	if result, err := e.bridge.Withdraw(e.ctx, e.credentials.Token, withdrawal); err != nil || result.Decision != browser.Accepted {
		t.Fatalf("late withdrawal could not observe proven cleanup: %+v, %v", result, err)
	}
}
