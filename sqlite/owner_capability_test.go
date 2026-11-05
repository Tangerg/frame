package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func TestRebindingAnOwnerHandlePreservesTheRegisteredAuthority(t *testing.T) {
	admission := frame.NewAdmission()
	dependency := frame.Dependency{InstallationID: "provider", Release: value.Hash([]byte("original provider"))}
	store, err := sqlite.Open(t.Context(), sqlite.Config{
		Path: filepath.Join(t.TempDir(), "owner-capability.db"), Admission: admission,
		Owners: []sqlite.OwnerSchema{
			{ID: "retained", References: func(context.Context, sqlite.Reader, frame.InstallationID) ([]frame.Dependency, error) {
				return []frame.Dependency{dependency}, nil
			}},
			{ID: "unretained", References: func(context.Context, sqlite.Reader, frame.InstallationID) ([]frame.Dependency, error) {
				return nil, nil
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.Owner("retained")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Owner("unretained")
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.RegisterUseOwner(owner); err != nil {
		t.Fatal(err)
	}
	if err := admission.CheckUnused(t.Context(), dependency.InstallationID); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("fixture did not retain the original provider: %v", err)
	}
	owner = other
	if owner.ID() != "unretained" {
		t.Fatal("caller could not rebind its local capability")
	}
	original, err := store.Owner("retained")
	if err != nil {
		t.Fatal(err)
	}
	if original.ID() != "retained" {
		t.Errorf("rebound handle rewrote the Store's original owner: %q", original.ID())
	}
	if err := admission.CheckUnused(t.Context(), dependency.InstallationID); !errors.Is(err, frame.ErrInUse) {
		t.Errorf("rebound handle discarded the registered owner's retained dependency: %v", err)
	}
	issued, err := original.Prepare(t.Context(), []byte("original owner request"))
	if err != nil || issued.Owner != "retained" {
		t.Fatalf("original lookup issued another owner's intent: %+v %v", issued, err)
	}
}

func TestZeroOwnerRefusesEveryOperation(t *testing.T) {
	var owner sqlite.Owner
	if owner.ID() != "" {
		t.Fatal("zero owner invented a registered identity")
	}
	if err := frame.NewAdmission().RegisterUseOwner(owner); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("zero owner entered the retained-use registry: %v", err)
	}
	id := sqlite.SubmissionID{Store: "store", Owner: "owner", Sequence: 1}
	request := []byte("original request")
	mutation := func(context.Context, sqlite.Executor) ([]byte, error) {
		t.Error("zero owner ran a mutation")
		return nil, nil
	}
	for _, operation := range []struct {
		name string
		call func() error
	}{
		{"held", func() error { _, err := owner.Held(t.Context(), "provider"); return err }},
		{"prepare", func() error { _, err := owner.Prepare(t.Context(), request); return err }},
		{"prepare under", func() error { _, err := owner.PrepareUnder(t.Context(), request); return err }},
		{"commit", func() error { _, err := owner.Commit(t.Context(), id, request, mutation, nil); return err }},
		{"commit under", func() error { _, err := owner.CommitUnder(t.Context(), id, request, mutation, nil); return err }},
		{"settlement", func() error { _, err := owner.Settlement(t.Context(), id, request); return err }},
		{"recover prepared", func() error { return owner.RecoverPrepared(t.Context()) }},
		{"pending", func() error { _, err := owner.Pending(t.Context()); return err }},
		{"confirm", func() error { return owner.Confirm(t.Context(), id) }},
		{"retain evidence", func() error { return owner.RetainEvidence(t.Context(), id, "holder") }},
		{"release evidence", func() error { return owner.ReleaseEvidence(t.Context(), id, "holder") }},
		{"compact", func() error { return owner.Compact(t.Context(), 1) }},
		{"compact settled", func() error { return owner.CompactSettled(t.Context()) }},
		{"compact settled under", func() error { return owner.CompactSettledUnder(t.Context()) }},
		{"update", func() error {
			return owner.Update(t.Context(), func(context.Context, sqlite.Executor) error {
				t.Error("zero owner ran an update")
				return nil
			})
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("zero owner panicked instead of refusing admission: %v", recovered)
				}
			}()
			if err := operation.call(); !errors.Is(err, frame.ErrInvalid) {
				t.Fatalf("zero owner was not refused at the boundary: %v", err)
			}
		})
	}
}
