package frame_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type publicationMetadata func()

func (m publicationMetadata) MarshalJSON() ([]byte, error) {
	m()
	return []byte(`"metadata"`), nil
}

func scopedPublisher(t *testing.T) (*frame.Host, frame.ScopeGrant, *frame.RunningUnit, frame.Point[string, string]) {
	t.Helper()
	host := testHost(t, frame.HostConfig{})
	descriptor := []byte(`{"protocol":"publication-test"}`)
	ref := frame.ContractRef{Name: "publication.value", Version: "1", Kind: frame.ServiceKind, Mode: frame.PortableMode, Digest: framevalue.Hash(descriptor)}
	value, err := frame.BindContract[string](ref, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	point, err := frame.NewPortablePoint[string]("publication.point", "1", []byte(`{"type":"string"}`), value)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(point, value); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	plugin := installNative(t, host, "publisher", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "initial", Contract: point.Ref()}}},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			_, err := setup.Cleanup("publication-owner", func() error { <-release; return nil })
			if err != nil {
				return nil, err
			}
			return []frame.ExportValue{frame.Contribute("initial", point, "initial", "value")}, nil
		},
	})
	grant, err := host.Admission().OpenScope(t.Context(), "publisher-scope")
	if err != nil {
		t.Fatal(err)
	}
	running, err := host.StartUnit(t.Context(), grant, frame.UnitRef{InstallationID: "publisher", Release: plugin.Revision(), UnitID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unblock()
		if err := running.Stop(testContext(t)); err != nil {
			t.Error(err)
		}
	})
	return host, grant, running, point
}

func TestPublicationRejectsFencedGrantBeforeMetadataPreparation(t *testing.T) {
	host, grant, running, point := scopedPublisher(t)
	host.Admission().FenceScope(grant.ID())
	if err := host.Admission().ValidateScope(grant); !errors.Is(err, frame.ErrFenced) {
		t.Fatalf("original grant remained valid: %v", err)
	}
	prepared := false
	_, err := host.Publish(t.Context(), running.ID(), "revoked-publication", frame.ExportValue{
		Name: "late", Contract: point.Ref(), Value: "value", Metadata: publicationMetadata(func() { prepared = true }),
	})
	if err == nil || prepared {
		t.Fatalf("revoked publisher reached metadata preparation: prepared=%v error=%v", prepared, err)
	}
	if _, err := host.Publication(running.ID(), "revoked-publication"); !errors.Is(err, frame.ErrUnavailable) {
		t.Fatalf("revoked publisher retained a publication: %v", err)
	}
}

func TestPublicationRechecksOriginalGrantAfterMetadataPreparation(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	host, grant, running, point := scopedPublisher(t)
	prepared := false
	_, err := host.Publish(t.Context(), running.ID(), "revoked-during-preparation", frame.ExportValue{
		Name: "late", Contract: point.Ref(), Value: "value", Metadata: publicationMetadata(func() {
			prepared = true
			host.Admission().FenceScope(grant.ID())
		}),
	})
	if err == nil || !prepared {
		t.Fatalf("publication crossed its original grant: prepared=%v error=%v", prepared, err)
	}
	if _, err := host.Publication(running.ID(), "revoked-during-preparation"); !errors.Is(err, frame.ErrUnavailable) {
		t.Fatalf("late publication entered the catalog: %v", err)
	}
}

func TestPublicationReplayRetainsItsOriginalIdentity(t *testing.T) {
	host, _, running, point := scopedPublisher(t)
	value := frame.Contribute("dynamic", point, "metadata", "value")
	first, err := host.Publish(t.Context(), running.ID(), "original", value)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := host.Publish(t.Context(), running.ID(), "original", value)
	if err != nil || replayed != first {
		t.Fatalf("replay replaced its original publication: first=%+v replay=%+v error=%v", first, replayed, err)
	}
	value.Metadata = "different"
	if _, err := host.Publish(t.Context(), running.ID(), "original", value); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("publication identity accepted a different request: %v", err)
	}
}
