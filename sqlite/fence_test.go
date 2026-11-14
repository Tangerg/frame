package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestRefusedProtectionWritesDoNotFenceTheStore(t *testing.T) {
	store, _, a := testStore(t)
	ctx := t.Context()
	ref := framehost.UnitRef{InstallationID: "one", Release: value.Hash([]byte("release")), UnitID: "u"}
	binding := framehost.BindingSnapshot{Roots: []framehost.ExportRef{}, UnitRoots: []framehost.UnitRef{ref}, Units: []framehost.UnitBinding{{Source: ref, Configuration: value.Hash(nil)}}}
	orphan := framehost.UseApproval{ID: "orphan", ParentID: "missing", ExecutionID: "execution", Realm: "realm", Scope: "scope", ScopeGeneration: 1, Epoch: store.Epoch(), Binding: binding}
	err := a.Admit(ctx, func(ctx context.Context) error { return store.ApproveUse(ctx, orphan) })
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("use without its parent: %v", err)
	}
	unknown := ExitProof{AttemptID: "execution", Realm: "realm", ExitID: "exit", Kind: "test", Evidence: []byte("{}")}
	if err := store.ReleaseRealmUse(ctx, "unknown", "realm", "execution", unknown); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("release of an unknown use: %v", err)
	}
	if err := a.RecoveryError(); err != nil {
		t.Fatalf("a refused write fenced the store: %v", err)
	}
}

func TestRealmUseReleaseRequiresProofOfTheOriginalExecution(t *testing.T) {
	store, _, _ := testStore(t)
	proof := ExitProof{AttemptID: "execution", Realm: "realm", ExitID: "exit", Kind: "test", Evidence: []byte("{}")}
	for name, mutate := range map[string]func(*ExitProof){
		"another execution": func(p *ExitProof) { p.AttemptID = "other" },
		"another realm":     func(p *ExitProof) { p.Realm = "other" },
		"missing kind":      func(p *ExitProof) { p.Kind = "" },
		"missing evidence":  func(p *ExitProof) { p.Evidence = nil },
	} {
		forged := proof
		mutate(&forged)
		if err := store.ReleaseRealmUse(t.Context(), "use", "realm", "execution", forged); !errors.Is(err, frame.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
