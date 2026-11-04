package browser

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
)

func TestUseProofFencesLiveDescendantsBeforeReadingProjection(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "state.sqlite"), Admission: frame.NewAdmission()})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	realm := Realm{ID: "realm", Epoch: "1"}
	execution := Execution{ID: "execution", Generation: "1"}
	bridge := &Bridge{config: BridgeConfig{Store: store}, uses: map[string]*browserUse{
		"parent":     {grant: UseGrant{ID: "parent", Realm: realm, Execution: execution}},
		"child":      {grant: UseGrant{ID: "child", ParentUseID: "parent", Realm: realm, Execution: execution}},
		"grandchild": {grant: UseGrant{ID: "grandchild", ParentUseID: "child", Realm: realm, Execution: execution}},
		"unrelated":  {grant: UseGrant{ID: "unrelated", Realm: realm, Execution: execution}},
	}}
	proof := sqlite.ExitProof{ExitID: "close"}
	if err := bridge.releaseUseTree(ctx, "parent", sqlite.Attempt{ID: execution.ID, Realm: string(RealmID(realm))}, proof); err == nil {
		t.Fatal("closed projection unexpectedly succeeded")
	}
	for _, id := range []string{"parent", "child", "grandchild"} {
		if !bridge.uses[id].released() {
			t.Errorf("%s can admit descendants while the release projection is unavailable", id)
		}
	}
	if bridge.uses["unrelated"].released() {
		t.Fatal("parent proof fenced an unrelated Use")
	}
}
