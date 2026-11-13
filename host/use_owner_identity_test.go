package host_test

import (
	"context"
	"sync/atomic"
	"testing"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

type observedUseOwner struct {
	admission framehost.Admission
	id        string
	reads     atomic.Int32
}

func (o *observedUseOwner) ID() string {
	o.reads.Add(1)
	_ = o.admission.RecoveryError()
	return o.id
}

func (*observedUseOwner) Held(_ context.Context, id framehost.InstallationID) ([]framehost.Dependency, error) {
	return []framehost.Dependency{{InstallationID: id, Release: value.Hash(nil)}}, nil
}

func TestUseOwnerIdentityIsFrozenBeforeStorageReads(t *testing.T) {
	ctx := testContext(t)
	admission := framehost.NewAdmission()
	owners := []*observedUseOwner{
		{admission: admission, id: "owner-z"},
		{admission: admission, id: "owner-a"},
	}
	for _, owner := range owners {
		if err := admission.RegisterUseOwner(owner); err != nil {
			t.Fatal(err)
		}
		owner.id = "changed"
	}
	type result struct {
		values []framehost.RetainedDependency
		err    error
	}
	done := make(chan result, 1)
	go func() {
		values, err := admission.Retained(ctx, "installation")
		done <- result{values: values, err: err}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.values) != 2 || result.values[0].OwnerID != "owner-a" || result.values[1].OwnerID != "owner-z" {
			t.Fatalf("registered identities changed during storage reads: %+v", result.values)
		}
	case <-ctx.Done():
		t.Fatal("owner enumeration blocked on an external identity callback")
	}
	for _, owner := range owners {
		if owner.reads.Load() != 1 {
			t.Fatalf("registration re-read owner identity: %d", owner.reads.Load())
		}
	}
}
