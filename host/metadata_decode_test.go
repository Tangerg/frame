package host_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

var metadataDecodeUnavailable atomic.Bool
var errMetadataDecode = errors.New("metadata decoder is unavailable")

type fallibleMetadata struct{ value.JSON }

func (m *fallibleMetadata) UnmarshalJSON(raw []byte) error {
	if metadataDecodeUnavailable.Load() {
		return errMetadataDecode
	}
	return m.JSON.UnmarshalJSON(raw)
}

func TestMetadataReadReportsOriginalCodecFailure(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	capability, err := frame.NewCapability("metadata.codec", frame.MethodDescriptor{Name: "read", Kind: frame.Query, Input: []byte(`{"type":"string"}`), Output: []byte(`{"type":"string"}`)})
	if err != nil {
		t.Fatal(err)
	}
	service, err := framehost.NewPortableContract[string]("1", capability)
	if err != nil {
		t.Fatal(err)
	}
	point, err := framehost.NewPortablePoint[fallibleMetadata]("metadata.fallible", "1", []byte(`true`), service)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service, point); err != nil {
		t.Fatal(err)
	}
	encoded, err := value.ParseJSON([]byte(`"published"`))
	if err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "metadata", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
		Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Contribute("member", point, fallibleMetadata{encoded}, "callable")}, nil
		},
	})
	if _, err := host.Activate(ctx, source("metadata", plugin, "main", "member", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	metadataDecodeUnavailable.Store(true)
	t.Cleanup(func() { metadataDecodeUnavailable.Store(false) })
	candidates, err := framehost.Candidates(snapshot, point)
	if err != nil {
		t.Fatalf("listing candidates invoked a codec: %v", err)
	}
	if _, err := candidates[0].Metadata(); !errors.Is(err, errMetadataDecode) {
		t.Fatalf("metadata read lost its decoder error: %v", err)
	}
	refs, err := snapshot.Candidates(point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := refs[0].MetadataJSON(); err != nil || string(raw) != encoded.String() {
		t.Fatalf("typed decoding changed the admitted wire value: %s, %v", raw, err)
	}
	selection, err := framehost.Select(snapshot, point, "one", candidates...)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := framehost.Capture(snapshot, selection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	_, scope := ownerAndScope(t, host)
	if err := host.With(ctx, scope, plan, func(access *framehost.Access) error {
		_, err := selection.One(access)
		return err
	}); !errors.Is(err, errMetadataDecode) {
		t.Fatalf("selected metadata lost its decoder error: %v", err)
	}
}
