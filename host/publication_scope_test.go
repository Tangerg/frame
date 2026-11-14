package host_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
)

type publicationMetadata func()

func (m publicationMetadata) MarshalJSON() ([]byte, error) {
	m()
	return []byte(`"metadata"`), nil
}

func scopedPublisher(t *testing.T) (framehost.Host, framehost.ScopeGrant, framehost.RunningUnit, framehost.Point[string, string]) {
	t.Helper()
	host := testHost(t, framehost.Config{})
	descriptor := []byte(`{"protocol":"publication-test"}`)
	ref := framehost.ContractRef{Name: "publication.value", Version: "1", Kind: framehost.ServiceKind, Mode: framehost.PortableMode, Digest: framevalue.Hash(descriptor)}
	value, err := framehost.BindContract[string](ref, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	point, err := framehost.NewPortablePoint[string]("publication.point", "1", []byte(`{"type":"string"}`), value)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(point, value); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	plugin := installNative(t, host, "publisher", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "initial", Contract: point.Ref()}}},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			_, err := setup.Cleanup("publication-owner", func() error { <-release; return nil })
			if err != nil {
				return nil, err
			}
			return []framehost.ExportValue{framehost.Contribute("initial", point, "initial", "value")}, nil
		},
	})
	grant, err := host.Admission().OpenScope(t.Context(), "publisher-scope")
	if err != nil {
		t.Fatal(err)
	}
	running, err := host.StartUnit(t.Context(), grant, framehost.UnitRef{InstallationID: "publisher", Release: plugin.Revision(), UnitID: "main"})
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
	if err := host.Admission().ValidateScope(grant); !errors.Is(err, framehost.ErrFenced) {
		t.Fatalf("original grant remained valid: %v", err)
	}
	prepared := false
	_, err := host.PreparePublication(t.Context(), running.ID(), framehost.ExportValue{
		Name: "late", Contract: point.Ref(), Value: "value", Metadata: publicationMetadata(func() { prepared = true }),
	})
	if err == nil || prepared {
		t.Fatalf("revoked publisher reached metadata preparation: prepared=%v error=%v", prepared, err)
	}
	if _, err := host.Publication(running.ID(), "revoked-publication"); !errors.Is(err, framehost.ErrUnavailable) {
		t.Fatalf("revoked publisher retained a publication: %v", err)
	}
}

func TestPublicationRechecksOriginalGrantAfterMetadataPreparation(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	host, grant, running, point := scopedPublisher(t)
	prepared := false
	request, err := host.PreparePublication(t.Context(), running.ID(), framehost.ExportValue{
		Name: "late", Contract: point.Ref(), Value: "value", Metadata: publicationMetadata(func() {
			prepared = true
			host.Admission().FenceScope(grant.ID())
		}),
	})
	if err == nil || !prepared || request.Valid() {
		t.Fatalf("revoked preparation produced publication authority: prepared=%v request=%v error=%v", prepared, request, err)
	}
	if _, err := host.Publication(running.ID(), "revoked-during-preparation"); !errors.Is(err, framehost.ErrUnavailable) {
		t.Fatalf("late publication entered the catalog: %v", err)
	}
}

func preparePublication(t *testing.T, host framehost.Host, instanceID string, value framehost.ExportValue) framehost.PublicationRequest {
	t.Helper()
	request, err := host.PreparePublication(t.Context(), instanceID, value)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestPublicationReplayRetainsItsOriginalIdentity(t *testing.T) {
	host, _, running, point := scopedPublisher(t)
	value := framehost.Contribute("dynamic", point, "metadata", "value")
	request := preparePublication(t, host, running.ID(), value)
	first, err := host.Publish(t.Context(), "original", request)
	if err != nil {
		t.Fatal(err)
	}
	copied := request
	if replayed, err := host.Publish(t.Context(), "original", copied); err != nil || replayed != first {
		t.Fatalf("copied request replaced its original prepared value: %+v, %v", replayed, err)
	}
	value.Value = "mutated after preparation"
	value.Metadata = "mutated after preparation"
	replayed, err := host.Publish(t.Context(), "original", request)
	if err != nil || replayed != first {
		t.Fatalf("replay replaced its original publication: first=%+v replay=%+v error=%v", first, replayed, err)
	}
	for _, replacement := range []framehost.ExportValue{
		framehost.Contribute("dynamic", point, "different", "value"),
		framehost.Contribute("dynamic", point, "metadata", "another implementation"),
		framehost.Contribute("dynamic", point, "metadata", "value"),
	} {
		other := preparePublication(t, host, running.ID(), replacement)
		if _, err := host.Publish(t.Context(), "original", other); !errors.Is(err, framehost.ErrStaleBinding) {
			t.Fatalf("publication identity accepted a different request: %v", err)
		}
	}
	if err := host.Withdraw(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Publish(t.Context(), "original", request); !errors.Is(err, framehost.ErrStaleBinding) {
		t.Fatalf("withdrawn original was republished by replay: %v", err)
	}
	next, err := host.Publish(t.Context(), "new-membership", request)
	if err != nil || next.ID == first.ID {
		t.Fatalf("new operation reused a withdrawn membership: %+v, %v", next, err)
	}
}

func TestPublicationRequestCannotCrossHosts(t *testing.T) {
	host, _, running, point := scopedPublisher(t)
	request := preparePublication(t, host, running.ID(), framehost.Contribute("dynamic", point, "metadata", "value"))
	other := testHost(t, framehost.Config{})
	if _, err := other.Publish(t.Context(), "foreign", request); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("another host accepted publication authority: %v", err)
	}
}
