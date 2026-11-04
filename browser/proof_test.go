package browser_test

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestFailedExecutionProofRemainsInCompletePoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	e := openBridge(t, path, true)
	plan := e.start("component")
	before, err := e.bridge.Poll(e.ctx, e.credentials.Token)
	if err != nil || len(before.Fences) != 0 {
		t.Fatalf("ready execution was already fenced: %+v %v", before, err)
	}
	fault, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := fault.Exec(`DROP TRIGGER IF EXISTS refuse_release`); err != nil {
			t.Error(err)
		}
		if err := fault.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := fault.Exec(`CREATE TRIGGER refuse_release BEFORE UPDATE OF state ON frame_attempts BEGIN SELECT RAISE(ABORT, 'injected release failure'); END`); err != nil {
		t.Fatal(err)
	}
	proof := executionProof(plan)
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof); err == nil {
		t.Fatal("fault did not reach durable release")
	}
	for range 2 {
		pending, err := e.bridge.Poll(e.ctx, e.credentials.Token)
		if err != nil || len(pending.Plans) != 0 || len(pending.Fences) != 1 || pending.Fences[0].ScopeID != plan.ScopeID || pending.Fences[0].ScopeGeneration != plan.ScopeGeneration {
			t.Fatalf("unsettled exit disappeared from complete projection: %+v %v", pending, err)
		}
	}
	if _, err := fault.Exec(`DROP TRIGGER refuse_release`); err != nil {
		t.Fatal(err)
	}
	ack, err := e.bridge.Prove(e.ctx, e.credentials.Token, proof)
	if err != nil || !ack.Recorded {
		t.Fatalf("original proof did not settle: %+v %v", ack, err)
	}
	pending, err := e.bridge.Poll(e.ctx, e.credentials.Token)
	if err != nil || len(pending.Plans) != 0 || len(pending.Fences) != 0 {
		t.Fatalf("settled execution remained pending: %+v %v", pending, err)
	}
}
