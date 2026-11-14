package browser

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func TestPublicationProofReplayCompletesLocalRetirement(t *testing.T) {
	ctx := t.Context()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "state.sqlite"), Admission: framehost.NewAdmission()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	realm := Realm{ID: "realm", Epoch: "1"}
	source := framehost.UnitRef{InstallationID: "publisher", Release: value.Hash([]byte("publisher")), UnitID: "view"}
	attempt := sqlite.Attempt{
		ID: "execution", Epoch: store.Epoch(), Realm: string(RealmID(realm)), Profile: framehost.BrowserJS,
		Scope: "scope", ScopeGeneration: 1, Generation: 1, Source: source,
		Binding: framehost.BindingSnapshot{Roots: []framehost.ExportRef{}, UnitRoots: []framehost.UnitRef{source}, Units: []framehost.UnitBinding{{Source: source, Configuration: value.Hash(nil)}}},
	}
	if err := store.RecordAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	publication := &controlOperation{execution: attempt.ID, controlRequestIdentity: controlRequestIdentity{Kind: publishOperation, Realm: realm}}
	unrelated := &controlOperation{}
	bridge := &Bridge{
		config:       BridgeConfig{Store: store, MaxMessageBytes: defaultMaxMessageBytes},
		authority:    Authority{ID: store.ID(), Epoch: strconv.FormatUint(store.Epoch(), 10)},
		signer:       newRealmSigner(),
		publications: map[string]*controlOperation{"publication": publication, "unrelated": unrelated},
	}
	proof := CloseProof{
		CloseID: "close", ProofID: "proof", Authority: bridge.authority, Realm: realm,
		Execution: Execution{ID: attempt.ID, Generation: "1"}, ScopeID: string(attempt.Scope), ScopeGeneration: "1",
		ScopeKind: PublicationScope, PublicationID: "publication", Instances: []string{attempt.ID}, Status: frame.ResourceReleased,
	}
	encoded, err := marshalControl(proof)
	if err != nil {
		t.Fatal(err)
	}
	// A lost commit acknowledgement can leave the durable proof ahead of its
	// local projection. Replaying that proof must finish the original cleanup.
	if err := store.RecordExitProof(ctx, attempt.ID, string(proof.ScopeKind)+":"+proof.PublicationID, proof.ProofID, encoded); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ack, err := bridge.Prove(ctx, bridge.signer.token(realm), proof)
		if err != nil || !ack.Recorded {
			t.Fatalf("replay: %+v, %v", ack, err)
		}
		if bridge.publications[proof.PublicationID] != nil {
			t.Fatal("acknowledged proof retained its publication projection")
		}
		if bridge.publications["unrelated"] != unrelated {
			t.Fatal("proof retired an unrelated publication")
		}
	}
}

func TestUseProofFencesLiveDescendantsBeforeReadingProjection(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "state.sqlite"), Admission: framehost.NewAdmission()})
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
