package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestLocalExitPreservesMaximumLengthAttemptIdentity(t *testing.T) {
	store, _, _ := testStore(t)
	ref := framehost.UnitRef{InstallationID: "provider", Release: value.Hash(nil), UnitID: "main"}
	binding := framehost.BindingSnapshot{Roots: []framehost.ExportRef{}, UnitRoots: []framehost.UnitRef{ref}, Units: []framehost.UnitBinding{{Source: ref, Configuration: value.Hash(nil)}}}
	attempt := Attempt{ID: strings.Repeat("<", frame.MaxNameBytes), Epoch: store.Epoch(), Source: ref, Binding: binding, Realm: "runner", Profile: framehost.LocalProcess}
	proof := attempt.LocalExit("not-started", `{"started":false}`)
	if err := store.RecordAttempt(t.Context(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseAttempt(context.Background(), attempt.ID, proof); err != nil {
		t.Fatalf("approved attempt could not settle its local exit: %v", err)
	}
	record, err := store.Attempt(t.Context(), attempt.ID)
	if err != nil || record.State != AttemptReleased || record.Proof == nil || record.Proof.ExitID != attempt.ID {
		t.Fatalf("local exit replaced its original attempt identity: %+v %v", record, err)
	}
}

func TestExitEvidenceIsBoundedByTheProofNotTheRequestBudget(t *testing.T) {
	store, _, _ := testStore(t)
	ref := framehost.UnitRef{InstallationID: "provider", Release: value.Hash(nil), UnitID: "main"}
	binding := framehost.BindingSnapshot{UnitRoots: []framehost.UnitRef{ref}, Units: []framehost.UnitBinding{{Source: ref, Configuration: value.Hash(nil)}}}
	attempt := Attempt{ID: "bounded", Epoch: store.Epoch(), Source: ref, Binding: binding, Realm: "runner", Profile: framehost.LocalProcess}
	if err := store.RecordAttempt(t.Context(), attempt); err != nil {
		t.Fatal(err)
	}
	oversized := attempt.LocalExit("not-started", strings.Repeat("x", MaxExitEvidenceBytes+1))
	if err := store.ReleaseAttempt(t.Context(), attempt.ID, oversized); err == nil {
		t.Fatal("evidence beyond its bound was recorded")
	}
	if err := store.RecordExitProof(t.Context(), attempt.ID, "child", "proof", []byte(strings.Repeat("x", MaxExitEvidenceBytes+1))); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("subordinate evidence beyond its bound: %v", err)
	}
	maximal := attempt.LocalExit("not-started", strings.Repeat("x", MaxExitEvidenceBytes))
	if err := store.ReleaseAttempt(t.Context(), attempt.ID, maximal); err != nil {
		t.Fatalf("evidence within its bound could not settle: %v", err)
	}
}
