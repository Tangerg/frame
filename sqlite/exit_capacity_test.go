package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestLocalExitPreservesMaximumLengthAttemptIdentity(t *testing.T) {
	store, _, _ := testStore(t)
	ref := framehost.UnitRef{InstallationID: "provider", Release: value.Hash(nil), UnitID: "main"}
	binding := framehost.BindingSnapshot{FormatVersion: 1, Roots: []framehost.ExportRef{}, UnitRoots: []framehost.UnitRef{ref}, Units: []framehost.UnitBinding{{Source: ref, Configuration: value.Hash(nil)}}}
	attempt := Attempt{ID: strings.Repeat("<", frame.MaxNameBytes), Epoch: store.Epoch(), Source: ref, Binding: binding, Realm: "runner", Profile: framehost.LocalProcess}
	proof := attempt.LocalExit("not-started", `{"started":false}`)
	if err := store.CheckExitCapacity(proof); err != nil {
		t.Fatalf("legal attempt identity cannot carry its original local exit: %v", err)
	}
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
